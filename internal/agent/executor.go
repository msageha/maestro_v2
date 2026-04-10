// Package agent provides agent lifecycle management and command execution.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// ExecMode represents the agent execution mode.
type ExecMode string

const (
	// ModeDeliver sends a message with busy check (used by Planner/Orchestrator).
	ModeDeliver ExecMode = "deliver"
	// ModeWithClear sends /clear before delivery (used by Workers).
	ModeWithClear ExecMode = "with_clear"
	// ModeInterrupt interrupts a running task.
	ModeInterrupt ExecMode = "interrupt"
	// ModeIsBusy queries the busy state without delivering.
	ModeIsBusy ExecMode = "is_busy"
	// ModeClear resets context without delivery.
	ModeClear ExecMode = "clear"
)

// busyVerdict is the result of busy detection.
type busyVerdict int

const (
	// VerdictIdle indicates the agent is not busy.
	VerdictIdle busyVerdict = iota
	// VerdictBusy indicates the agent is currently processing.
	VerdictBusy
	// VerdictUndecided indicates the busy state could not be determined.
	VerdictUndecided
)

func (v busyVerdict) String() string {
	switch v {
	case VerdictIdle:
		return "idle"
	case VerdictBusy:
		return "busy"
	default:
		return "undecided"
	}
}

// Sentinel errors for specific executor conditions.
var (
	// ErrBusyUndecided is returned when busy detection is inconclusive.
	// This is a transient condition safe for immediate retry.
	ErrBusyUndecided = errors.New("agent busy: undecided_after_probes")
)

// ExecutorConfig holds tunable constants for Executor behavior.
// Use DefaultExecutorConfig() to get production defaults; tests can
// override individual fields for fast or deterministic runs.
type ExecutorConfig struct {
	// PromptReadyLines is the number of lines captured for prompt detection and stability hash.
	PromptReadyLines int
	// BusyHintLines is the number of lines captured for busy pattern matching.
	BusyHintLines int
	// StableCheckRounds is the number of consecutive stable hash rounds required.
	StableCheckRounds int
	// DefaultExecTimeout is the fallback timeout when ExecRequest.Context is nil.
	DefaultExecTimeout time.Duration
	// ClaudeLaunchTimeout is the timeout for waiting after re-launching Claude.
	ClaudeLaunchTimeout time.Duration
}

// DefaultExecutorConfig returns production-safe defaults.
func DefaultExecutorConfig() ExecutorConfig {
	return ExecutorConfig{
		PromptReadyLines:    12, // 12 lines to accommodate status bars
		BusyHintLines:       5,
		StableCheckRounds:   1,
		DefaultExecTimeout:  5 * time.Minute,
		ClaudeLaunchTimeout: 60 * time.Second,
	}
}

// ExecRequest contains parameters for executing a message delivery.
type ExecRequest struct {
	Context    context.Context // nil defaults to context.Background()
	AgentID    string
	Message    string
	Mode       ExecMode
	TaskID     string
	CommandID  string
	LeaseEpoch int
	Attempt    int
	WorkingDir string // Target working directory (worktree mode). Empty = no change.
}

// ExecResult contains the outcome of an execution attempt.
type ExecResult struct {
	Success   bool
	Retryable bool
	Error     error
}

// logLevel controls logging verbosity.
type logLevel int

const (
	logLevelDebug logLevel = iota
	logLevelInfo
	logLevelWarn
	logLevelError
)

func parseLogLevel(s string) logLevel {
	switch strings.ToLower(s) {
	case "debug":
		return logLevelDebug
	case "info":
		return logLevelInfo
	case "warn", "warning":
		return logLevelWarn
	case "error":
		return logLevelError
	default:
		return logLevelInfo
	}
}

// Executor handles message delivery to agents via tmux panes.
// It delegates send/clear operations to messageDeliverer and busy detection
// to busyDetector, acting as a facade for the overall dispatch workflow.
type Executor struct {
	maestroDir   string
	execCfg      ExecutorConfig
	config       model.WatcherConfig
	logger       *log.Logger
	logFile      io.Closer
	logLevel     logLevel
	paneIO       PaneIO
	busyDetector *busyDetector
	paneState    *paneStateManager
	deliverer    *messageDeliverer
}

// NewExecutor creates a new Executor that logs to .maestro/logs/agent_executor.log.
func NewExecutor(maestroDir string, watcherCfg model.WatcherConfig, logLevel string) (*Executor, error) {
	logPath := filepath.Join(maestroDir, "logs", "agent_executor.log")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600) //nolint:gosec // logPath is constructed from a controlled application log directory
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", logPath, err)
	}
	return newExecutor(maestroDir, watcherCfg, logLevel, logFile, logFile, DefaultExecutorConfig())
}

// newExecutor is the internal constructor accepting injectable writer/closer (used by NewExecutor and tests).
func newExecutor(maestroDir string, watcherCfg model.WatcherConfig, logLevel string, w io.Writer, closer io.Closer, execCfg ExecutorConfig) (*Executor, error) {
	var busyRegex *regexp.Regexp
	if watcherCfg.BusyPatterns != "" {
		re, err := regexp.Compile(watcherCfg.BusyPatterns)
		if err != nil {
			return nil, fmt.Errorf("compile busy_patterns %q: %w", watcherCfg.BusyPatterns, err)
		}
		busyRegex = re
	}

	cfg := applyDefaults(watcherCfg)
	logger := log.New(w, "", 0)
	ll := parseLogLevel(logLevel)
	paneIO := NewTmuxPaneIO()
	ps := newPaneStateManager(paneIO)

	bd := newBusyDetector(paneIO, busyRegex, busyDetectorConfig{
		IdleStableSec:       cfg.IdleStableSec,
		BusyCheckMaxRetries: cfg.BusyCheckMaxRetries,
		BusyCheckInterval:   cfg.BusyCheckInterval,
		BusyHintLines:       execCfg.BusyHintLines,
	}, logger, ll)

	e := &Executor{
		maestroDir:   maestroDir,
		execCfg:      execCfg,
		config:       cfg,
		logger:       logger,
		logFile:      closer,
		logLevel:     ll,
		paneIO:       paneIO,
		busyDetector: bd,
		paneState:    ps,
	}
	e.deliverer = newMessageDeliverer(paneIO, ps, &e.config, execCfg, logger, ll)
	return e, nil
}

// Close releases the log file handle.
func (e *Executor) Close() error {
	if e.logFile != nil {
		return e.logFile.Close()
	}
	return nil
}

func applyDefaults(cfg model.WatcherConfig) model.WatcherConfig {
	if cfg.BusyCheckInterval <= 0 {
		cfg.BusyCheckInterval = 2
	}
	if cfg.BusyCheckMaxRetries <= 0 {
		cfg.BusyCheckMaxRetries = 30
	}
	if cfg.IdleStableSec <= 0 {
		cfg.IdleStableSec = 5
	}
	if cfg.CooldownAfterClear <= 0 {
		cfg.CooldownAfterClear = 3
	}
	if cfg.WaitReadyIntervalSec <= 0 {
		cfg.WaitReadyIntervalSec = 2
	}
	if cfg.WaitReadyMaxRetries <= 0 {
		cfg.WaitReadyMaxRetries = 15
	}
	if cfg.ClearConfirmTimeoutSec <= 0 {
		cfg.ClearConfirmTimeoutSec = 5
	}
	if cfg.ClearConfirmPollMs <= 0 {
		cfg.ClearConfirmPollMs = 250
	}
	if cfg.ClearMaxAttempts <= 0 {
		cfg.ClearMaxAttempts = 3
	}
	if cfg.ClearRetryBackoffMs <= 0 {
		cfg.ClearRetryBackoffMs = 500
	}
	if cfg.ClearSecondEnterDelayMs <= 0 {
		cfg.ClearSecondEnterDelayMs = 500
	}
	return cfg
}

// sleepCtx blocks until duration expires or context is cancelled.
// Used to allow daemon shutdown to interrupt long-running waits
// (e.g., retry cooldowns, prompt readiness polling).
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Execute dispatches the request based on its Mode.
func (e *Executor) Execute(req ExecRequest) ExecResult {
	ctx := req.Context
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), e.execCfg.DefaultExecTimeout)
		defer cancel()
	}
	req.Context = ctx

	paneTarget, err := e.paneIO.FindPaneByAgentID(req.AgentID)
	if err != nil {
		e.log(logLevelError, "delivery_error agent_id=%s error=pane_not_found: %v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("find pane for %s: %w", req.AgentID, err)}
	}

	// Log delivery_start once for delivery-related modes (prevents duplication
	// when execWithClear delegates to execDeliver).
	if req.Mode == ModeWithClear || req.Mode == ModeDeliver {
		e.log(logLevelInfo, "delivery_start agent_id=%s task_id=%s command_id=%s lease_epoch=%d attempt=%d",
			req.AgentID, req.TaskID, req.CommandID, req.LeaseEpoch, req.Attempt)
	}

	switch req.Mode {
	case ModeIsBusy:
		return e.execIsBusy(ctx, paneTarget)
	case ModeClear:
		return e.execClear(ctx, req, paneTarget)
	case ModeInterrupt:
		return e.execInterrupt(ctx, req, paneTarget)
	case ModeWithClear:
		return e.execWithClear(ctx, req, paneTarget)
	case ModeDeliver:
		return e.execDeliver(ctx, req, paneTarget)
	default:
		return ExecResult{Error: fmt.Errorf("unknown exec mode: %s", req.Mode)}
	}
}

// execIsBusy checks agent busy state. Returns Success=true if busy, false if idle.
// VerdictUndecided is NOT treated as busy — it returns Success=false with
// ErrBusyUndecided so callers can distinguish it from a confirmed idle state.
// This prevents false positive lease extensions when the verdict is inconclusive.
func (e *Executor) execIsBusy(ctx context.Context, paneTarget string) ExecResult {
	verdict := e.busyDetector.DetectBusy(ctx, paneTarget)
	switch verdict {
	case VerdictBusy:
		return ExecResult{Success: true}
	case VerdictUndecided:
		return ExecResult{
			Success:   false,
			Error:     fmt.Errorf("%w", ErrBusyUndecided),
			Retryable: true,
		}
	default: // VerdictIdle
		return ExecResult{Success: false}
	}
}

// execClear sends /clear and waits for stability.
func (e *Executor) execClear(ctx context.Context, req ExecRequest, paneTarget string) ExecResult {
	e.log(logLevelDebug, "clear_operation agent_id=%s mode=clear", req.AgentID)

	if err := e.waitReady(ctx, paneTarget); err != nil {
		e.log(logLevelWarn, "clear_wait_ready_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("wait ready before /clear: %w", err), Retryable: true}
	}

	if err := e.clearAndConfirm(ctx, paneTarget); err != nil {
		return ExecResult{Error: fmt.Errorf("clear: %w", err), Retryable: true}
	}

	// Verify prompt readiness after clear (strict — no subsequent busy detection)
	if err := e.waitStable(ctx, paneTarget, false); err != nil {
		e.log(logLevelWarn, "clear_post_stable_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("clear: post-clear stability: %w", err), Retryable: true}
	}
	return ExecResult{Success: true}
}

// execInterrupt interrupts a running task: C-c → cooldown → /clear → cooldown → stability.
func (e *Executor) execInterrupt(ctx context.Context, req ExecRequest, paneTarget string) ExecResult {
	e.log(logLevelInfo, "interrupt_start agent_id=%s task_id=%s lease_epoch=%d",
		req.AgentID, req.TaskID, req.LeaseEpoch)

	if req.AgentID == "orchestrator" {
		e.log(logLevelError, "delivery_error agent_id=orchestrator error=cannot_interrupt_orchestrator")
		return ExecResult{Error: fmt.Errorf("cannot interrupt orchestrator")}
	}

	// Step 1: C-c
	if err := e.paneIO.SendCtrlC(paneTarget); err != nil {
		return ExecResult{Error: fmt.Errorf("send C-c: %w", err), Retryable: true}
	}
	if err := sleepCtx(ctx, time.Duration(e.config.CooldownAfterClear)*time.Second); err != nil {
		return ExecResult{Error: fmt.Errorf("cooldown after C-c cancelled: %w", err), Retryable: true}
	}

	// Step 2: Wait for prompt readiness after C-c
	if err := e.waitReady(ctx, paneTarget); err != nil {
		e.log(logLevelWarn, "interrupt_wait_ready_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("wait ready before /clear (interrupt): %w", err), Retryable: true}
	}

	// Step 3+4: /clear with confirmation (replaces send + cooldown + waitStable)
	if err := e.clearAndConfirm(ctx, paneTarget); err != nil {
		return ExecResult{Error: fmt.Errorf("interrupt: %w", err), Retryable: true}
	}

	// Verify prompt readiness after clear (strict — no subsequent busy detection)
	if err := e.waitStable(ctx, paneTarget, false); err != nil {
		e.log(logLevelWarn, "interrupt_post_stable_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("interrupt: post-clear stability: %w", err), Retryable: true}
	}

	// Step 5: Set @status="idle" — the agent is now idle after interrupt.
	if err := e.paneState.SetStatus(paneTarget, "idle"); err != nil {
		e.log(logLevelWarn, "set_status_idle_failed agent_id=%s error=%v", req.AgentID, err)
	}

	e.log(logLevelInfo, "interrupt_success agent_id=%s task_id=%s lease_epoch=%d",
		req.AgentID, req.TaskID, req.LeaseEpoch)
	return ExecResult{Success: true}
}

// execWithClear delivers a message with prior /clear (Worker mode).
func (e *Executor) execWithClear(ctx context.Context, req ExecRequest, paneTarget string) ExecResult {
	// Orchestrator: never /clear, fall through to deliver mode
	if req.AgentID == "orchestrator" {
		return e.execDeliver(ctx, req, paneTarget)
	}

	// Handle working directory change (worktree mode).
	if req.WorkingDir != "" {
		if err := e.ensureWorkingDir(ctx, paneTarget, req.WorkingDir); err != nil {
			e.log(logLevelError, "working_dir_change_failed agent_id=%s error=%v", req.AgentID, err)
			return ExecResult{Error: fmt.Errorf("ensure working dir: %w", err), Retryable: true}
		}
	}

	// Ensure Claude is actually running (not crashed back to shell).
	if err := e.ensureClaudeRunning(ctx, paneTarget, req.AgentID); err != nil {
		e.log(logLevelError, "ensure_claude_running_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("ensure claude running: %w", err), Retryable: true}
	}

	// Check if pane process has been restarted (PID changed)
	_, currentPID := e.checkProcessRestart(paneTarget, req.AgentID)

	// First dispatch: skip /clear, use deliver mode
	if !e.paneState.IsClearReady(paneTarget) {
		return e.execFirstDispatch(ctx, req, paneTarget, currentPID)
	}

	// Subsequent dispatches: use full /clear workflow
	return e.execClearAndDeliver(ctx, req, paneTarget)
}

// checkProcessRestart detects pane process restarts and logs accordingly.
// Returns (restarted, currentPID).
func (e *Executor) checkProcessRestart(paneTarget, agentID string) (bool, string) {
	restarted, currentPID, err := e.paneState.DetectProcessRestart(paneTarget)
	if err != nil {
		e.log(logLevelWarn, "detect_process_restart_error agent_id=%s error=%v", agentID, err)
		return false, currentPID
	}
	if restarted {
		e.log(logLevelInfo, "pane_process_restarted agent_id=%s new_pid=%s, resetting clear_ready",
			agentID, currentPID)
	}
	return restarted, currentPID
}

// execFirstDispatch handles the first delivery to a worker pane (no /clear needed).
func (e *Executor) execFirstDispatch(ctx context.Context, req ExecRequest, paneTarget, currentPID string) ExecResult {
	e.log(logLevelDebug, "first_dispatch agent_id=%s, using deliver mode", req.AgentID)
	result := e.execDeliver(ctx, req, paneTarget)
	if result.Success {
		if err := e.paneState.SetClearReady(paneTarget, currentPID); err != nil {
			e.log(logLevelError, "set_clear_ready_failed agent_id=%s error=%v", req.AgentID, err)
		}
	}
	return result
}

// execClearAndDeliver performs /clear, busy detection, and delivery for subsequent dispatches.
func (e *Executor) execClearAndDeliver(ctx context.Context, req ExecRequest, paneTarget string) ExecResult {
	// Step 1: Wait for prompt readiness
	if err := e.waitReady(ctx, paneTarget); err != nil {
		e.log(logLevelWarn, "with_clear_wait_ready_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("wait ready before /clear: %w", err), Retryable: true}
	}

	// Step 2+3: /clear with confirmation
	e.log(logLevelDebug, "clear_operation agent_id=%s mode=with_clear", req.AgentID)
	if err := e.clearAndConfirm(ctx, paneTarget); err != nil {
		return e.handleClearFailure(req, paneTarget, err)
	}

	// Step 4: Busy detection with retry
	verdict := e.busyDetector.DetectBusyWithRetry(ctx, paneTarget, req.AgentID)
	if verdict != VerdictIdle {
		return e.handleBusyVerdict(req, verdict)
	}

	// Step 5: Deliver
	return e.sendAndConfirm(req, paneTarget)
}

// handleClearFailure processes a /clear confirmation failure, resetting clear_ready state.
func (e *Executor) handleClearFailure(req ExecRequest, paneTarget string, clearErr error) ExecResult {
	e.log(logLevelWarn, "delivery_failure agent_id=%s task_id=%s reason=clear_not_confirmed error=%v",
		req.AgentID, req.TaskID, clearErr)

	e.log(logLevelInfo, "clear_failed_reset agent_id=%s, resetting clear_ready for next dispatch", req.AgentID)
	if resetErr := e.paneState.ResetClearReady(paneTarget); resetErr != nil {
		e.log(logLevelError, "reset_clear_ready_failed agent_id=%s error=%v", req.AgentID, resetErr)
		return ExecResult{Error: errors.Join(fmt.Errorf("with_clear: %w", clearErr), fmt.Errorf("reset_clear_ready: %w", resetErr)), Retryable: true}
	}

	return ExecResult{Error: fmt.Errorf("with_clear: %w", clearErr), Retryable: true}
}

// handleBusyVerdict converts a non-idle busy verdict to an ExecResult.
func (e *Executor) handleBusyVerdict(req ExecRequest, verdict busyVerdict) ExecResult {
	e.log(logLevelWarn, "delivery_failure agent_id=%s task_id=%s verdict=%s",
		req.AgentID, req.TaskID, verdict)

	if verdict == VerdictUndecided {
		return ExecResult{
			Error:     fmt.Errorf("%w", ErrBusyUndecided),
			Retryable: true,
		}
	}

	return ExecResult{
		Error:     fmt.Errorf("agent %s busy: timeout", req.AgentID),
		Retryable: true,
	}
}

// execDeliver delivers a message without /clear (Planner/Orchestrator).
func (e *Executor) execDeliver(ctx context.Context, req ExecRequest, paneTarget string) ExecResult {
	// Orchestrator: strict busy check, no retry, immediate failure if busy
	if req.AgentID == "orchestrator" {
		// Ensure Claude is running for orchestrator too
		if err := e.ensureClaudeRunning(ctx, paneTarget, req.AgentID); err != nil {
			e.log(logLevelError, "ensure_claude_running_failed agent_id=orchestrator error=%v", err)
			return ExecResult{Error: fmt.Errorf("ensure claude running: %w", err), Retryable: true}
		}
		verdict := e.busyDetector.DetectBusy(ctx, paneTarget)
		e.log(logLevelDebug, "busy_detection agent_id=orchestrator verdict=%s", verdict)
		if verdict != VerdictIdle {
			e.log(logLevelWarn, "delivery_failure agent_id=orchestrator reason=orchestrator_busy verdict=%s", verdict)
			return ExecResult{
				Error:     fmt.Errorf("orchestrator busy (verdict=%s)", verdict),
				Retryable: true,
			}
		}
		return e.sendAndConfirm(req, paneTarget)
	}

	// Planner/other: ensure Claude is running before delivery
	if err := e.ensureClaudeRunning(ctx, paneTarget, req.AgentID); err != nil {
		e.log(logLevelError, "ensure_claude_running_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("ensure claude running: %w", err), Retryable: true}
	}

	// Planner/other: busy detection with retry
	verdict := e.busyDetector.DetectBusyWithRetry(ctx, paneTarget, req.AgentID)
	if verdict != VerdictIdle {
		reason := "busy_timeout"
		if verdict == VerdictUndecided {
			reason = "undecided_after_probes"
		}
		e.log(logLevelWarn, "delivery_failure agent_id=%s task_id=%s reason=%s",
			req.AgentID, req.TaskID, reason)
		return ExecResult{
			Error:     fmt.Errorf("agent %s busy: %s", req.AgentID, reason),
			Retryable: true,
		}
	}

	return e.sendAndConfirm(req, paneTarget)
}

// sendAndConfirm delegates to the messageDeliverer.
func (e *Executor) sendAndConfirm(req ExecRequest, paneTarget string) ExecResult {
	return e.deliverer.sendAndConfirm(req, paneTarget)
}

// clearAndConfirm delegates to the messageDeliverer.
func (e *Executor) clearAndConfirm(ctx context.Context, paneTarget string) error {
	return e.deliverer.clearAndConfirm(ctx, paneTarget)
}

// waitStable confirms pane content is stable over StableCheckRounds consecutive
// rounds of hash comparison, then verifies the prompt is ready.
// Worst-case duration: StableCheckRounds × IdleStableSec (default 1 × 5s = ~5s).
//
// softPromptCheck controls how prompt detection failure is handled:
//   - true:  log a warning and proceed (safe when caller runs detectBusyWithRetry afterwards)
//   - false: return an error (required when no subsequent busy detection exists)
func (e *Executor) waitStable(ctx context.Context, paneTarget string, softPromptCheck bool) error {
	for round := 0; round < e.execCfg.StableCheckRounds; round++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait_stable cancelled before round %d: %w", round, err)
		}

		content1, err := e.paneIO.CapturePaneJoined(paneTarget, e.execCfg.PromptReadyLines)
		if err != nil {
			return fmt.Errorf("capture pane for stability round %d: %w", round, err)
		}
		h1 := contentHash(content1)

		if err := sleepCtx(ctx, time.Duration(e.config.IdleStableSec)*time.Second); err != nil {
			return fmt.Errorf("wait_stable sleep cancelled (round %d): %w", round, err)
		}

		content2, err := e.paneIO.CapturePaneJoined(paneTarget, e.execCfg.PromptReadyLines)
		if err != nil {
			return fmt.Errorf("capture pane for stability round %d: %w", round, err)
		}
		h2 := contentHash(content2)

		if h1 != h2 {
			return fmt.Errorf("pane content not stable after %ds (round %d)", e.config.IdleStableSec, round)
		}
		e.log(logLevelDebug, "wait_stable round=%d passed", round)
	}

	// Verify prompt is ready after stability confirmed.
	// Uses CapturePane (no -J) to preserve line boundaries for prompt detection.
	finalContent, err := e.paneIO.CapturePane(paneTarget, e.execCfg.PromptReadyLines)
	if err != nil {
		if softPromptCheck {
			e.log(logLevelWarn, "wait_stable prompt_check capture error=%v (non-fatal, soft mode)", err)
			return nil
		}
		return fmt.Errorf("capture pane for prompt check: %w", err)
	}
	if !isPromptReady(finalContent) {
		if softPromptCheck {
			e.log(logLevelInfo, "wait_stable prompt_not_detected pane=%s last_line=%q (proceeding — detectBusy will guard delivery)",
				paneTarget, lastNonBlankLine(finalContent))
			return nil
		}
		return fmt.Errorf("pane stable but no prompt detected (last line: %q)", lastNonBlankLine(finalContent))
	}
	e.log(logLevelDebug, "wait_stable prompt confirmed")
	return nil
}

// waitReady polls until the pane shows a Claude Code prompt ('❯' or '>'), indicating readiness.
// It uses WaitReadyIntervalSec and WaitReadyMaxRetries from config for timing.
// Worst-case duration: (WaitReadyMaxRetries+1) × WaitReadyIntervalSec (default 16 × 2s = 32s).
//
// If prompt detection fails after all retries, the function logs at INFO level
// and returns nil (proceeds) instead of blocking dispatch. The caller's
// subsequent detectBusyWithRetry provides a safety net against delivering
// to a busy agent.
func (e *Executor) waitReady(ctx context.Context, paneTarget string) error {
	ready, err := e.waitReadyCore(ctx, paneTarget)
	if err != nil {
		return err
	}
	if !ready {
		// Fallback: prompt not detected, but proceed with a warning.
		// The subsequent detectBusyWithRetry() will catch if the agent is actually busy.
		e.log(logLevelInfo, "wait_ready prompt_fallback pane=%s: prompt not detected after retries, proceeding (detectBusy will guard)",
			paneTarget)
	}
	return nil
}

// --- Claude Process Recovery ---

// ensureClaudeRunning checks whether the pane is running a shell (indicating
// Claude has crashed or exited) and re-launches Claude if necessary.
// This guards against the scenario where Claude crashes back to the shell
// but the pane PID stays the same (shell PID unchanged), which
// DetectProcessRestart cannot detect.
// Returns nil if Claude is confirmed running, or a retryable error on failure.
func (e *Executor) ensureClaudeRunning(ctx context.Context, paneTarget, agentID string) error {
	cmd, err := e.paneIO.GetPaneCurrentCommand(paneTarget)
	if err != nil {
		e.log(logLevelWarn, "ensure_claude_running_check_failed agent_id=%s error=%v", agentID, err)
		// Cannot determine state; proceed optimistically
		return nil
	}

	if !e.paneIO.IsShellCommand(cmd) {
		// Not a shell — Claude (or another process) is running
		return nil
	}

	e.log(logLevelWarn, "claude_not_running agent_id=%s pane_command=%s, re-launching", agentID, cmd)

	// Reset clear_ready since we are starting a fresh Claude session
	if resetErr := e.paneState.ResetClearReady(paneTarget); resetErr != nil {
		e.log(logLevelError, "ensure_claude_running_reset_clear_ready agent_id=%s error=%v", agentID, resetErr)
	}

	// Re-launch Claude
	if sendErr := e.paneIO.SendCommand(paneTarget, "maestro agent launch"); sendErr != nil {
		return fmt.Errorf("ensureClaudeRunning: re-launch: %w", sendErr)
	}

	// Wait for Claude prompt readiness (fail-closed)
	launchCtx, cancel := context.WithTimeout(ctx, e.execCfg.ClaudeLaunchTimeout)
	defer cancel()
	if waitErr := e.waitReadyStrict(launchCtx, paneTarget); waitErr != nil {
		return fmt.Errorf("ensureClaudeRunning: wait for Claude ready: %w", waitErr)
	}

	e.log(logLevelInfo, "claude_relaunched agent_id=%s", agentID)
	return nil
}

// --- Working Directory Management ---

// ensureWorkingDir ensures the agent's Claude Code process is running in the
// specified working directory. If the current CWD (tracked via @cwd tmux user
// variable) differs from workingDir, the method exits the current Claude
// process, changes the shell CWD, and re-launches Claude.
//
// This is called transparently by the executor before task delivery, so that
// Workers run in their worktree directory without being aware of worktrees.
func (e *Executor) ensureWorkingDir(ctx context.Context, paneTarget, workingDir string) error {
	if workingDir == "" {
		return nil
	}

	// Validate path: reject control characters to prevent injection via SendCommand
	if containsControlChars(workingDir) {
		return fmt.Errorf("ensureWorkingDir: working dir contains control characters: %q", workingDir)
	}

	// Check current CWD from tmux user variable
	currentCWD := e.paneState.GetCWD(paneTarget)
	if currentCWD == workingDir {
		e.log(logLevelDebug, "working_dir unchanged cwd=%s", workingDir)
		return nil
	}

	e.log(logLevelInfo, "working_dir_change old=%q new=%q", currentCWD, workingDir)

	// Step 1: Kill the current pane process and respawn a fresh shell in the
	// target working directory. This replaces the fragile Ctrl+C → Ctrl+D →
	// waitForShell → cd sequence which broke when Claude did not exit cleanly.
	if err := e.paneIO.RespawnPane(paneTarget, workingDir); err != nil {
		return fmt.Errorf("ensureWorkingDir: respawn pane: %w", err)
	}

	// Step 2: Wait for the fresh shell to be ready
	if err := e.waitForShell(ctx, paneTarget); err != nil {
		return fmt.Errorf("ensureWorkingDir: wait for shell after respawn: %w", err)
	}

	// Step 3: Re-launch Claude
	if err := e.paneIO.SendCommand(paneTarget, "maestro agent launch"); err != nil {
		return fmt.Errorf("ensureWorkingDir: re-launch: %w", err)
	}

	// Step 4: Wait for Claude prompt readiness (fail-closed: error on timeout)
	// Use a generous timeout since Claude startup can take a few seconds.
	launchCtx, cancel := context.WithTimeout(ctx, e.execCfg.ClaudeLaunchTimeout)
	defer cancel()
	if err := e.waitReadyStrict(launchCtx, paneTarget); err != nil {
		return fmt.Errorf("ensureWorkingDir: wait for Claude ready: %w", err)
	}

	// Step 5: Update CWD tracking and reset clear_ready state
	if err := e.paneState.SetCWD(paneTarget, workingDir); err != nil {
		e.log(logLevelWarn, "set_cwd_failed cwd=%s error=%v", workingDir, err)
	}
	// Reset clear_ready since we started a fresh Claude session
	if err := e.paneState.ResetClearReady(paneTarget); err != nil {
		e.log(logLevelError, "reset_clear_ready_failed error=%v", err)
		return fmt.Errorf("ensureWorkingDir: reset_clear_ready: %w", err)
	}

	e.log(logLevelInfo, "working_dir_changed cwd=%s", workingDir)
	return nil
}

// waitForShell polls a tmux pane until its current command is a known shell,
// indicating the pane has returned to the shell prompt (e.g., after Claude exits).
func (e *Executor) waitForShell(ctx context.Context, paneTarget string) error {
	const maxAttempts = 30   // 30 × 500ms = 15s max
	const pollInterval = 500 // milliseconds
	const maxConsecutiveErrors = 5

	consecutiveErrors := 0
	for i := 0; i < maxAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waitForShell cancelled: %w", err)
		}

		cmd, err := e.paneIO.GetPaneCurrentCommand(paneTarget)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= maxConsecutiveErrors {
				return fmt.Errorf("waitForShell: %d consecutive errors, last: %w", consecutiveErrors, err)
			}
		} else {
			consecutiveErrors = 0
			if e.paneIO.IsShellCommand(cmd) {
				e.log(logLevelDebug, "waitForShell detected shell command=%s", cmd)
				return nil
			}
		}

		if err := sleepCtx(ctx, time.Duration(pollInterval)*time.Millisecond); err != nil {
			return fmt.Errorf("waitForShell sleep cancelled: %w", err)
		}
	}

	return fmt.Errorf("waitForShell: shell not detected after %d attempts", maxAttempts)
}

// waitReadyStrict is like waitReady but fail-closed: returns an error if the
// prompt is not detected after all retries (instead of soft-proceeding).
// Used after re-launching Claude where we must confirm the process started.
func (e *Executor) waitReadyStrict(ctx context.Context, paneTarget string) error {
	ready, err := e.waitReadyCore(ctx, paneTarget)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("waitReadyStrict: Claude prompt not detected after %d attempts", e.config.WaitReadyMaxRetries+1)
	}
	return nil
}

// waitReadyCore is the shared retry loop for prompt detection. It returns
// (true, nil) if the prompt was detected, (false, nil) if retries were
// exhausted without detection, or (false, err) on hard failures (context
// cancellation, persistent capture errors).
func (e *Executor) waitReadyCore(ctx context.Context, paneTarget string) (bool, error) {
	maxRetries := e.config.WaitReadyMaxRetries
	interval := time.Duration(e.config.WaitReadyIntervalSec) * time.Second

	for i := 0; i <= maxRetries; i++ {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("waitReadyCore cancelled at attempt %d: %w", i, err)
		}

		content, err := e.paneIO.CapturePane(paneTarget, e.execCfg.PromptReadyLines)
		if err != nil {
			e.log(logLevelDebug, "waitReadyCore capture error=%v attempt=%d", err, i)
			if i < maxRetries {
				if err := sleepCtx(ctx, interval); err != nil {
					return false, fmt.Errorf("waitReadyCore sleep cancelled: %w", err)
				}
				continue
			}
			return false, fmt.Errorf("waitReadyCore: capture pane failed after %d attempts: %w", i+1, err)
		}

		if isPromptReady(content) {
			e.log(logLevelDebug, "waitReadyCore prompt detected attempt=%d", i)
			return true, nil
		}

		if i < maxRetries {
			e.log(logLevelDebug, "waitReadyCore not ready attempt=%d/%d", i, maxRetries)
			if err := sleepCtx(ctx, interval); err != nil {
				return false, fmt.Errorf("waitReadyCore sleep cancelled: %w", err)
			}
		}
	}

	return false, nil
}

// --- Logging ---

// logf is the shared log formatting function used by Executor and busyDetector.
// It checks the level threshold, formats the message with timestamp, level, and
// component prefix, then writes to the provided logger.
func logf(logger *log.Logger, minLevel, level logLevel, component, format string, args ...any) {
	if level < minLevel {
		return
	}
	levelStr := "[INFO]"
	switch level {
	case logLevelDebug:
		levelStr = "[DEBUG]"
	case logLevelWarn:
		levelStr = "[WARN]"
	case logLevelError:
		levelStr = "[ERROR]"
	}
	msg := fmt.Sprintf(format, args...)
	logger.Printf("%s %s %s: %s", time.Now().Format(time.RFC3339), levelStr, component, msg)
}

func (e *Executor) log(level logLevel, format string, args ...any) {
	logf(e.logger, e.logLevel, level, "agent_executor", format, args...)
}
