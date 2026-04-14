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

// ExecMode is an alias for model.ExecMode.
type ExecMode = model.ExecMode

// Mode constants are re-exported from model for backward compatibility.
const (
	ModeDeliver   = model.ModeDeliver
	ModeWithClear = model.ModeWithClear
	ModeInterrupt = model.ModeInterrupt
	ModeIsBusy    = model.ModeIsBusy
	ModeClear     = model.ModeClear
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

// ExecRequest is an alias for model.ExecRequest.
type ExecRequest = model.ExecRequest

// ExecResult is an alias for model.ExecResult.
type ExecResult = model.ExecResult

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
// It delegates send/clear operations to messageDeliverer, busy detection
// to busyDetector, and process lifecycle to ClaudeProcessManager,
// acting as a facade for the overall dispatch workflow.
type Executor struct {
	maestroDir     string
	execCfg        ExecutorConfig
	config         model.WatcherConfig
	logger         *log.Logger
	logFile        io.Closer
	logLevel       logLevel
	paneIO         PaneIO
	busyDetector   *busyDetector
	paneState      *paneStateManager
	deliverer      *messageDeliverer
	processManager *ClaudeProcessManager
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
	e.processManager = newClaudeProcessManager(paneIO, ps, &e.config, execCfg, logger, ll)
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

// CleanupPaneMutex removes the per-pane delivery mutex for the given pane target.
// Call this when a pane is no longer in use to prevent unbounded growth of
// the internal sync.Map.
func (e *Executor) CleanupPaneMutex(paneTarget string) {
	e.deliverer.removePaneMutex(paneTarget)
}

// Default values for WatcherConfig fields when unset or non-positive.
const (
	defaultBusyCheckInterval      = 2   // seconds between busy-detection probes
	defaultBusyCheckMaxRetries    = 30  // max busy-detection retry attempts
	defaultIdleStableSec          = 5   // seconds of stability before declaring idle
	defaultCooldownAfterClear     = 3   // seconds to wait after /clear
	defaultWaitReadyIntervalSec   = 2   // seconds between prompt-readiness polls
	defaultWaitReadyMaxRetries    = 15  // max prompt-readiness poll attempts
	defaultClearConfirmTimeoutSec = 5   // seconds to wait for /clear confirmation
	defaultClearConfirmPollMs     = 250 // milliseconds between /clear confirmation polls
	defaultClearMaxAttempts       = 3   // max /clear retry attempts
	defaultClearRetryBackoffMs    = 500 // milliseconds backoff between /clear retries
	defaultClearSecondEnterDelayMs = 500 // milliseconds delay before second Enter after /clear
)

func applyDefaults(cfg model.WatcherConfig) model.WatcherConfig {
	if cfg.BusyCheckInterval <= 0 {
		cfg.BusyCheckInterval = defaultBusyCheckInterval
	}
	if cfg.BusyCheckMaxRetries <= 0 {
		cfg.BusyCheckMaxRetries = defaultBusyCheckMaxRetries
	}
	if cfg.IdleStableSec <= 0 {
		cfg.IdleStableSec = defaultIdleStableSec
	}
	if cfg.CooldownAfterClear <= 0 {
		cfg.CooldownAfterClear = defaultCooldownAfterClear
	}
	if cfg.WaitReadyIntervalSec <= 0 {
		cfg.WaitReadyIntervalSec = defaultWaitReadyIntervalSec
	}
	if cfg.WaitReadyMaxRetries <= 0 {
		cfg.WaitReadyMaxRetries = defaultWaitReadyMaxRetries
	}
	if cfg.ClearConfirmTimeoutSec <= 0 {
		cfg.ClearConfirmTimeoutSec = defaultClearConfirmTimeoutSec
	}
	if cfg.ClearConfirmPollMs <= 0 {
		cfg.ClearConfirmPollMs = defaultClearConfirmPollMs
	}
	if cfg.ClearMaxAttempts <= 0 {
		cfg.ClearMaxAttempts = defaultClearMaxAttempts
	}
	if cfg.ClearRetryBackoffMs <= 0 {
		cfg.ClearRetryBackoffMs = defaultClearRetryBackoffMs
	}
	if cfg.ClearSecondEnterDelayMs <= 0 {
		cfg.ClearSecondEnterDelayMs = defaultClearSecondEnterDelayMs
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

	if err := e.processManager.waitReady(ctx, paneTarget); err != nil {
		e.log(logLevelWarn, "clear_wait_ready_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("wait ready before /clear: %w", err), Retryable: true}
	}

	if err := e.clearAndConfirm(ctx, paneTarget); err != nil {
		return ExecResult{Error: fmt.Errorf("clear: %w", err), Retryable: true}
	}

	// Verify prompt readiness after clear (strict — no subsequent busy detection)
	if err := e.processManager.waitStable(ctx, paneTarget, false); err != nil {
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
	if err := e.processManager.waitReady(ctx, paneTarget); err != nil {
		e.log(logLevelWarn, "interrupt_wait_ready_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("wait ready before /clear (interrupt): %w", err), Retryable: true}
	}

	// Step 3+4: /clear with confirmation (replaces send + cooldown + waitStable)
	if err := e.clearAndConfirm(ctx, paneTarget); err != nil {
		return ExecResult{Error: fmt.Errorf("interrupt: %w", err), Retryable: true}
	}

	// Verify prompt readiness after clear (strict — no subsequent busy detection)
	if err := e.processManager.waitStable(ctx, paneTarget, false); err != nil {
		e.log(logLevelWarn, "interrupt_post_stable_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("interrupt: post-clear stability: %w", err), Retryable: true}
	}

	// Step 5: Set @status="idle" — the agent is now idle after interrupt.
	if err := e.paneState.SetStatus(paneTarget, "idle"); err != nil {
		e.log(logLevelWarn, "set_status_idle_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("interrupt succeeded but set_status failed: %w", err), Retryable: true}
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
		if err := e.processManager.ensureWorkingDir(ctx, paneTarget, req.WorkingDir); err != nil {
			e.log(logLevelError, "working_dir_change_failed agent_id=%s error=%v", req.AgentID, err)
			return ExecResult{Error: fmt.Errorf("ensure working dir: %w", err), Retryable: true}
		}
	}

	// Ensure Claude is actually running (not crashed back to shell).
	if err := e.processManager.ensureClaudeRunning(ctx, paneTarget, req.AgentID); err != nil {
		e.log(logLevelError, "ensure_claude_running_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("ensure claude running: %w", err), Retryable: true}
	}

	// Check if pane process has been restarted (PID changed)
	_, currentPID, err := e.checkProcessRestart(paneTarget, req.AgentID)
	if err != nil {
		return ExecResult{Error: fmt.Errorf("check process restart: %w", err), Retryable: true}
	}

	// First dispatch: skip /clear, use deliver mode
	if !e.paneState.IsClearReady(paneTarget) {
		return e.execFirstDispatch(ctx, req, paneTarget, currentPID)
	}

	// Subsequent dispatches: use full /clear workflow
	return e.execClearAndDeliver(ctx, req, paneTarget)
}

// checkProcessRestart detects pane process restarts and logs accordingly.
// Returns (restarted, currentPID, error).
func (e *Executor) checkProcessRestart(paneTarget, agentID string) (bool, string, error) {
	restarted, currentPID, err := e.paneState.DetectProcessRestart(paneTarget)
	if err != nil {
		e.log(logLevelWarn, "detect_process_restart_error agent_id=%s error=%v", agentID, err)
		return restarted, currentPID, fmt.Errorf("detect process restart: %w", err)
	}
	if restarted {
		e.log(logLevelInfo, "pane_process_restarted agent_id=%s new_pid=%s, resetting clear_ready",
			agentID, currentPID)
	}
	return restarted, currentPID, nil
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
	if err := e.processManager.waitReady(ctx, paneTarget); err != nil {
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
		if err := e.processManager.ensureClaudeRunning(ctx, paneTarget, req.AgentID); err != nil {
			e.log(logLevelError, "ensure_claude_running_failed agent_id=orchestrator error=%v", err)
			return ExecResult{Error: fmt.Errorf("ensure claude running: %w", err), Retryable: true}
		}
		verdict := e.busyDetector.DetectBusy(ctx, paneTarget)
		e.log(logLevelDebug, "busy_detection agent_id=orchestrator verdict=%s", verdict)
		if verdict != VerdictIdle {
			e.log(logLevelWarn, "delivery_failure agent_id=orchestrator reason=orchestrator_busy verdict=%s", verdict)
			if verdict == VerdictUndecided {
				return ExecResult{
					Error:     fmt.Errorf("orchestrator busy: %w", ErrBusyUndecided),
					Retryable: true,
				}
			}
			return ExecResult{
				Error:     fmt.Errorf("orchestrator busy (verdict=%s)", verdict),
				Retryable: true,
			}
		}
		return e.sendAndConfirm(req, paneTarget)
	}

	// Planner/other: ensure Claude is running before delivery
	if err := e.processManager.ensureClaudeRunning(ctx, paneTarget, req.AgentID); err != nil {
		e.log(logLevelError, "ensure_claude_running_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("ensure claude running: %w", err), Retryable: true}
	}

	// Planner/other: busy detection with retry
	verdict := e.busyDetector.DetectBusyWithRetry(ctx, paneTarget, req.AgentID)
	if verdict != VerdictIdle {
		if verdict == VerdictUndecided {
			e.log(logLevelWarn, "delivery_failure agent_id=%s task_id=%s reason=undecided_after_probes",
				req.AgentID, req.TaskID)
			return ExecResult{
				Error:     fmt.Errorf("agent %s busy: %w", req.AgentID, ErrBusyUndecided),
				Retryable: true,
			}
		}
		e.log(logLevelWarn, "delivery_failure agent_id=%s task_id=%s reason=busy_timeout",
			req.AgentID, req.TaskID)
		return ExecResult{
			Error:     fmt.Errorf("agent %s busy: timeout", req.AgentID),
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
