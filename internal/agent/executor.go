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
	ModeDeliver   ExecMode = "deliver"    // Direct delivery with busy check (Planner/Orchestrator)
	ModeWithClear ExecMode = "with_clear" // /clear + delivery (Workers)
	ModeInterrupt ExecMode = "interrupt"  // Interrupt running task
	ModeIsBusy    ExecMode = "is_busy"    // Query busy state only
	ModeClear     ExecMode = "clear"      // Context reset without delivery
)

// BusyVerdict is the result of busy detection.
type BusyVerdict int

const (
	VerdictIdle BusyVerdict = iota
	VerdictBusy
	VerdictUndecided
)

func (v BusyVerdict) String() string {
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

const (
	promptReadyLines   = 12 // プロンプト検出+安定性ハッシュ用 (12 lines to accommodate status bars)
	busyHintLines      = 5  // busy パターンマッチ用
	stableCheckRounds  = 1  // 安定性判定に必要なラウンド数
	defaultExecTimeout = 5 * time.Minute // Context未設定時のデフォルトタイムアウト
)

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

// LogLevel controls logging verbosity.
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

func parseLogLevel(s string) LogLevel {
	switch strings.ToLower(s) {
	case "debug":
		return LogLevelDebug
	case "info":
		return LogLevelInfo
	case "warn", "warning":
		return LogLevelWarn
	case "error":
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

// Executor handles message delivery to agents via tmux panes.
type Executor struct {
	maestroDir   string
	config       model.WatcherConfig
	logger       *log.Logger
	logFile      io.Closer
	logLevel     LogLevel
	paneIO       PaneIO
	busyDetector *BusyDetector
	paneState    *PaneStateManager
}

// NewExecutor creates a new Executor that logs to .maestro/logs/agent_executor.log.
func NewExecutor(maestroDir string, watcherCfg model.WatcherConfig, logLevel string) (*Executor, error) {
	logPath := filepath.Join(maestroDir, "logs", "agent_executor.log")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", logPath, err)
	}
	return newExecutor(maestroDir, watcherCfg, logLevel, logFile, logFile)
}

// newExecutor is the internal constructor that accepts an io.Writer for testing.
func newExecutor(maestroDir string, watcherCfg model.WatcherConfig, logLevel string, w io.Writer, closer io.Closer) (*Executor, error) {
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

	bd := NewBusyDetector(paneIO, busyRegex, BusyDetectorConfig{
		IdleStableSec:       cfg.IdleStableSec,
		BusyCheckMaxRetries: cfg.BusyCheckMaxRetries,
		BusyCheckInterval:   cfg.BusyCheckInterval,
	}, logger, ll)

	return &Executor{
		maestroDir:   maestroDir,
		config:       cfg,
		logger:       logger,
		logFile:      closer,
		logLevel:     ll,
		paneIO:       paneIO,
		busyDetector: bd,
		paneState:    NewPaneStateManager(paneIO),
	}, nil
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

// sleepCtx sleeps for d or returns early if ctx is cancelled.
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
		ctx, cancel = context.WithTimeout(context.Background(), defaultExecTimeout)
		defer cancel()
	}
	req.Context = ctx

	paneTarget, err := e.paneIO.FindPaneByAgentID(req.AgentID)
	if err != nil {
		e.log(LogLevelError, "delivery_error agent_id=%s error=pane_not_found: %v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("find pane for %s: %w", req.AgentID, err)}
	}

	// Log delivery_start once for delivery-related modes (prevents duplication
	// when execWithClear delegates to execDeliver).
	if req.Mode == ModeWithClear || req.Mode == ModeDeliver {
		e.log(LogLevelInfo, "delivery_start agent_id=%s task_id=%s command_id=%s lease_epoch=%d attempt=%d",
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
	e.log(LogLevelDebug, "clear_operation agent_id=%s mode=clear", req.AgentID)

	if err := e.waitReady(ctx, paneTarget); err != nil {
		e.log(LogLevelWarn, "clear_wait_ready_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("wait ready before /clear: %w", err), Retryable: true}
	}

	if err := e.clearAndConfirm(ctx, paneTarget); err != nil {
		return ExecResult{Error: fmt.Errorf("clear: %w", err), Retryable: true}
	}

	// Verify prompt readiness after clear (strict — no subsequent busy detection)
	if err := e.waitStable(ctx, paneTarget, false); err != nil {
		e.log(LogLevelWarn, "clear_post_stable_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("clear: post-clear stability: %w", err), Retryable: true}
	}
	return ExecResult{Success: true}
}

// execInterrupt interrupts a running task: C-c → cooldown → /clear → cooldown → stability.
func (e *Executor) execInterrupt(ctx context.Context, req ExecRequest, paneTarget string) ExecResult {
	e.log(LogLevelInfo, "interrupt_start agent_id=%s task_id=%s lease_epoch=%d",
		req.AgentID, req.TaskID, req.LeaseEpoch)

	if req.AgentID == "orchestrator" {
		e.log(LogLevelError, "delivery_error agent_id=orchestrator error=cannot_interrupt_orchestrator")
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
		e.log(LogLevelWarn, "interrupt_wait_ready_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("wait ready before /clear (interrupt): %w", err), Retryable: true}
	}

	// Step 3+4: /clear with confirmation (replaces send + cooldown + waitStable)
	if err := e.clearAndConfirm(ctx, paneTarget); err != nil {
		return ExecResult{Error: fmt.Errorf("interrupt: %w", err), Retryable: true}
	}

	// Verify prompt readiness after clear (strict — no subsequent busy detection)
	if err := e.waitStable(ctx, paneTarget, false); err != nil {
		e.log(LogLevelWarn, "interrupt_post_stable_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("interrupt: post-clear stability: %w", err), Retryable: true}
	}

	// Step 5: Set @status="idle" — the agent is now idle after interrupt.
	if err := e.paneState.SetStatus(paneTarget, "idle"); err != nil {
		e.log(LogLevelWarn, "set_status_idle_failed agent_id=%s error=%v", req.AgentID, err)
	}

	e.log(LogLevelInfo, "interrupt_success agent_id=%s task_id=%s lease_epoch=%d",
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
	// This may restart Claude in a different directory, resetting clear_ready.
	if req.WorkingDir != "" {
		if err := e.ensureWorkingDir(ctx, paneTarget, req.WorkingDir); err != nil {
			e.log(LogLevelError, "working_dir_change_failed agent_id=%s error=%v", req.AgentID, err)
			return ExecResult{Error: fmt.Errorf("ensure working dir: %w", err), Retryable: true}
		}
	}

	// Check if pane process has been restarted (PID changed)
	restarted, currentPID, err := e.paneState.DetectProcessRestart(paneTarget)
	if err != nil {
		e.log(LogLevelWarn, "detect_process_restart_error agent_id=%s error=%v", req.AgentID, err)
		// Continue with default behavior — IsClearReady will re-check state
	}
	if restarted {
		e.log(LogLevelInfo, "pane_process_restarted agent_id=%s new_pid=%s, resetting clear_ready",
			req.AgentID, currentPID)
	}

	// Check if this worker pane is ready for /clear (has active conversation)
	if !e.paneState.IsClearReady(paneTarget) {
		// First dispatch: skip /clear, use deliver mode
		e.log(LogLevelDebug, "first_dispatch agent_id=%s, using deliver mode", req.AgentID)

		result := e.execDeliver(ctx, req, paneTarget)

		// On success, mark this pane as clear-ready for future dispatches
		if result.Success {
			if err := e.paneState.SetClearReady(paneTarget, currentPID); err != nil {
				e.log(LogLevelWarn, "set_clear_ready_failed agent_id=%s error=%v", req.AgentID, err)
			}
		}

		return result
	}

	// Subsequent dispatches: use full /clear workflow
	// Step 1: Wait for prompt readiness
	if err := e.waitReady(ctx, paneTarget); err != nil {
		e.log(LogLevelWarn, "with_clear_wait_ready_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("wait ready before /clear: %w", err), Retryable: true}
	}

	// Step 2+3: /clear with confirmation (replaces send + cooldown + waitStable)
	e.log(LogLevelDebug, "clear_operation agent_id=%s mode=with_clear", req.AgentID)
	if err := e.clearAndConfirm(ctx, paneTarget); err != nil {
		e.log(LogLevelWarn, "delivery_failure agent_id=%s task_id=%s reason=clear_not_confirmed error=%v",
			req.AgentID, req.TaskID, err)

		// Reset clear_ready on /clear failure - conversation might have been lost
		e.log(LogLevelInfo, "clear_failed_reset agent_id=%s, resetting clear_ready for next dispatch", req.AgentID)
		if err := e.paneState.ResetClearReady(paneTarget); err != nil {
			e.log(LogLevelWarn, "reset_clear_ready_failed agent_id=%s error=%v", req.AgentID, err)
		}

		return ExecResult{Error: fmt.Errorf("with_clear: %w", err), Retryable: true}
	}

	// Step 4: Busy detection with retry
	verdict := e.busyDetector.DetectBusyWithRetry(ctx, paneTarget, req.AgentID)
	if verdict != VerdictIdle {
		e.log(LogLevelWarn, "delivery_failure agent_id=%s task_id=%s verdict=%s",
			req.AgentID, req.TaskID, verdict)

		// Return sentinel error for VerdictUndecided (safe for immediate retry)
		if verdict == VerdictUndecided {
			return ExecResult{
				Error:     fmt.Errorf("%w", ErrBusyUndecided),
				Retryable: true,
			}
		}

		// VerdictBusy: keep lease to prevent duplicate dispatch
		return ExecResult{
			Error:     fmt.Errorf("agent %s busy: timeout", req.AgentID),
			Retryable: true,
		}
	}

	// Step 5: Deliver
	return e.sendAndConfirm(req, paneTarget)
}

// execDeliver delivers a message without /clear (Planner/Orchestrator).
func (e *Executor) execDeliver(ctx context.Context, req ExecRequest, paneTarget string) ExecResult {
	// Orchestrator: strict busy check, no retry, immediate failure if busy
	if req.AgentID == "orchestrator" {
		verdict := e.busyDetector.DetectBusy(ctx, paneTarget)
		e.log(LogLevelDebug, "busy_detection agent_id=orchestrator verdict=%s", verdict)
		if verdict != VerdictIdle {
			e.log(LogLevelWarn, "delivery_failure agent_id=orchestrator reason=orchestrator_busy verdict=%s", verdict)
			return ExecResult{
				Error:     fmt.Errorf("orchestrator busy (verdict=%s)", verdict),
				Retryable: true,
			}
		}
		return e.sendAndConfirm(req, paneTarget)
	}

	// Planner/other: busy detection with retry
	verdict := e.busyDetector.DetectBusyWithRetry(ctx, paneTarget, req.AgentID)
	if verdict != VerdictIdle {
		reason := "busy_timeout"
		if verdict == VerdictUndecided {
			reason = "undecided_after_probes"
		}
		e.log(LogLevelWarn, "delivery_failure agent_id=%s task_id=%s reason=%s",
			req.AgentID, req.TaskID, reason)
		return ExecResult{
			Error:     fmt.Errorf("agent %s busy: %s", req.AgentID, reason),
			Retryable: true,
		}
	}

	return e.sendAndConfirm(req, paneTarget)
}

// sendAndConfirm sends the message and updates @status to busy.
func (e *Executor) sendAndConfirm(req ExecRequest, paneTarget string) ExecResult {
	// Send message via paste-buffer + Enter for reliable multi-line delivery
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if err := e.paneIO.SendTextAndSubmit(ctx, paneTarget, req.Message); err != nil {
		e.log(LogLevelError, "delivery_error agent_id=%s task_id=%s error=send_text: %v",
			req.AgentID, req.TaskID, err)
		return ExecResult{Error: fmt.Errorf("send message: %w", err), Retryable: true}
	}

	// Update @status to busy
	if err := e.paneState.SetStatus(paneTarget, "busy"); err != nil {
		e.log(LogLevelWarn, "set_status_failed agent_id=%s error=%v", req.AgentID, err)
	}

	e.log(LogLevelInfo, "delivery_success agent_id=%s task_id=%s command_id=%s lease_epoch=%d",
		req.AgentID, req.TaskID, req.CommandID, req.LeaseEpoch)
	return ExecResult{Success: true}
}

// --- Clear Confirmation ---

// clearAndConfirm sends /clear and confirms it was processed by the target application.
// It retries up to ClearMaxAttempts times. Returns nil on confirmed clear, or an error
// if all attempts fail (fail-closed: caller must NOT proceed with delivery).
//
// Confirmation checks (per poll):
//  1. "/clear" text is NOT visible near the bottom of the pane (primary signal —
//     directly detects the production failure mode where /clear remains as unprocessed
//     text in the input field).
//  2. Pane content hash has changed from pre-clear snapshot (secondary signal).
//  3. Pane content is stable across two consecutive polls.
func (e *Executor) clearAndConfirm(ctx context.Context, paneTarget string) error {
	timeout := time.Duration(e.config.ClearConfirmTimeoutSec) * time.Second
	pollInterval := time.Duration(e.config.ClearConfirmPollMs) * time.Millisecond
	maxAttempts := e.config.ClearMaxAttempts
	backoffMs := e.config.ClearRetryBackoffMs

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("clear_and_confirm cancelled before attempt %d: %w", attempt, err)
		}

		// Capture pre-clear hash
		preClearContent, err := e.paneIO.CapturePaneJoined(paneTarget, promptReadyLines)
		preClearHashValid := err == nil
		if err != nil {
			e.log(LogLevelWarn, "clear_confirm pre_capture error=%v attempt=%d (hash check disabled)", err, attempt)
		}
		preClearHash := contentHash(preClearContent)

		// Send /clear with double-enter for reliability
		if err := e.paneIO.SendCommand(paneTarget, "/clear"); err != nil {
			e.log(LogLevelWarn, "clear_confirm send_clear error=%v attempt=%d", err, attempt)
			if attempt < maxAttempts {
				backoff := time.Duration(backoffMs*(1<<(attempt-1))) * time.Millisecond
				if err := sleepCtx(ctx, backoff); err != nil {
					return fmt.Errorf("clear_confirm backoff cancelled: %w", err)
				}
				continue
			}
			return fmt.Errorf("clear_confirm: send /clear failed after %d attempts: %w", maxAttempts, err)
		}

		// Wait before sending second Enter (configurable; default 500ms).
		// Claude's /clear command may trigger a completion prompt, requiring a
		// second Enter. The delay ensures the first Enter is processed.
		secondEnterDelay := time.Duration(e.config.ClearSecondEnterDelayMs) * time.Millisecond
		if err := sleepCtx(ctx, secondEnterDelay); err != nil {
			return fmt.Errorf("clear_confirm: wait cancelled: %w", err)
		}

		// Send second Enter to ensure /clear execution.
		if err := e.paneIO.SendKeys(paneTarget, "Enter"); err != nil {
			e.log(LogLevelWarn, "clear_confirm send_second_enter error=%v attempt=%d", err, attempt)
			if attempt < maxAttempts {
				backoff := time.Duration(backoffMs*(1<<(attempt-1))) * time.Millisecond
				if err := sleepCtx(ctx, backoff); err != nil {
					return fmt.Errorf("clear_confirm backoff cancelled: %w", err)
				}
				continue
			}
			return fmt.Errorf("clear_confirm: send second Enter failed after %d attempts: %w", maxAttempts, err)
		}

		// Poll for confirmation within timeout window
		confirmed, err := e.pollClearConfirmation(ctx, paneTarget, preClearHash, preClearHashValid, timeout, pollInterval)
		if err != nil {
			return err // context cancelled
		}
		if confirmed {
			e.log(LogLevelDebug, "clear_confirm confirmed attempt=%d", attempt)
			return nil
		}

		// Not confirmed — retry with backoff
		e.log(LogLevelWarn, "clear_confirm not_confirmed attempt=%d/%d", attempt, maxAttempts)
		if attempt < maxAttempts {
			backoff := time.Duration(backoffMs*(1<<(attempt-1))) * time.Millisecond
			e.log(LogLevelDebug, "clear_confirm retry_backoff=%v", backoff)
			if err := sleepCtx(ctx, backoff); err != nil {
				return fmt.Errorf("clear_confirm backoff cancelled: %w", err)
			}
		}
	}

	return fmt.Errorf("clear_confirm: /clear not confirmed after %d attempts", maxAttempts)
}

// pollClearConfirmation polls the pane within the timeout window to confirm /clear was processed.
// Returns (true, nil) if confirmed, (false, nil) if timed out, or (false, err) if ctx cancelled.
//
// Confirmation requires ALL of:
//  1. "/clear" text is NOT visible near the bottom of the pane (primary signal)
//  2. Pane content hash has changed from pre-clear snapshot (mandatory when preClearHashValid)
//  3. Pane content is stable across two consecutive polls (debounce)
//
// When preClearHashValid is false (pre-capture failed), hash change is not required but
// stability (3 consecutive polls) is demanded as a stricter fallback.
func (e *Executor) pollClearConfirmation(
	ctx context.Context,
	paneTarget string,
	preClearHash string,
	preClearHashValid bool,
	timeout, pollInterval time.Duration,
) (bool, error) {
	deadline := time.Now().Add(timeout)
	stableCount := 0
	hashChanged := false
	var prevPollHash string

	for time.Now().Before(deadline) {
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return false, fmt.Errorf("clear_confirm poll cancelled: %w", err)
		}

		content, err := e.paneIO.CapturePaneJoined(paneTarget, promptReadyLines)
		if err != nil {
			e.log(LogLevelDebug, "clear_confirm poll capture error=%v", err)
			stableCount = 0
			prevPollHash = ""
			hashChanged = false
			continue
		}

		currentHash := contentHash(content)

		// Check 1 (primary): "/clear" text must NOT be visible near the bottom of the pane.
		if clearTextVisible(content) {
			e.log(LogLevelDebug, "clear_confirm /clear text still visible")
			stableCount = 0
			prevPollHash = currentHash
			continue
		}

		// Check 2 (mandatory when valid): hash must differ from pre-clear state.
		if preClearHashValid && currentHash != preClearHash {
			hashChanged = true
		}

		// Check 3: stability — consecutive polls with same hash.
		if prevPollHash != "" && currentHash == prevPollHash {
			stableCount++
		} else {
			stableCount = 1
		}
		prevPollHash = currentHash

		// Confirmation logic:
		// - With valid pre-clear hash: require hash change + 2 stable polls (debounce)
		// - Without valid pre-clear hash: require 3 stable polls (stricter debounce as fallback)
		if preClearHashValid {
			if hashChanged && stableCount >= 2 {
				return true, nil
			}
		} else {
			if stableCount >= 3 {
				return true, nil
			}
		}
	}

	return false, nil
}

// waitStable confirms pane content is stable over stableCheckRounds consecutive
// rounds of hash comparison, then verifies the prompt is ready.
// Worst-case duration: stableCheckRounds × IdleStableSec (default 1 × 5s = ~5s).
//
// softPromptCheck controls how prompt detection failure is handled:
//   - true:  log a warning and proceed (safe when caller runs detectBusyWithRetry afterwards)
//   - false: return an error (required when no subsequent busy detection exists)
func (e *Executor) waitStable(ctx context.Context, paneTarget string, softPromptCheck bool) error {
	for round := 0; round < stableCheckRounds; round++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait_stable cancelled before round %d: %w", round, err)
		}

		content1, err := e.paneIO.CapturePaneJoined(paneTarget, promptReadyLines)
		if err != nil {
			return fmt.Errorf("capture pane for stability round %d: %w", round, err)
		}
		h1 := contentHash(content1)

		if err := sleepCtx(ctx, time.Duration(e.config.IdleStableSec)*time.Second); err != nil {
			return fmt.Errorf("wait_stable sleep cancelled (round %d): %w", round, err)
		}

		content2, err := e.paneIO.CapturePaneJoined(paneTarget, promptReadyLines)
		if err != nil {
			return fmt.Errorf("capture pane for stability round %d: %w", round, err)
		}
		h2 := contentHash(content2)

		if h1 != h2 {
			return fmt.Errorf("pane content not stable after %ds (round %d)", e.config.IdleStableSec, round)
		}
		e.log(LogLevelDebug, "wait_stable round=%d passed", round)
	}

	// Verify prompt is ready after stability confirmed.
	// Uses CapturePane (no -J) to preserve line boundaries for prompt detection.
	finalContent, err := e.paneIO.CapturePane(paneTarget, promptReadyLines)
	if err != nil {
		if softPromptCheck {
			e.log(LogLevelWarn, "wait_stable prompt_check capture error=%v (non-fatal, soft mode)", err)
			return nil
		}
		return fmt.Errorf("capture pane for prompt check: %w", err)
	}
	if !isPromptReady(finalContent) {
		if softPromptCheck {
			e.log(LogLevelInfo, "wait_stable prompt_not_detected pane=%s last_line=%q (proceeding — detectBusy will guard delivery)",
				paneTarget, lastNonBlankLine(finalContent))
			return nil
		}
		return fmt.Errorf("pane stable but no prompt detected (last line: %q)", lastNonBlankLine(finalContent))
	}
	e.log(LogLevelDebug, "wait_stable prompt confirmed")
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
		e.log(LogLevelInfo, "wait_ready prompt_fallback pane=%s: prompt not detected after retries, proceeding (detectBusy will guard)",
			paneTarget)
	}
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
		e.log(LogLevelDebug, "working_dir unchanged cwd=%s", workingDir)
		return nil
	}

	e.log(LogLevelInfo, "working_dir_change old=%q new=%q", currentCWD, workingDir)

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
	launchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := e.waitReadyStrict(launchCtx, paneTarget); err != nil {
		return fmt.Errorf("ensureWorkingDir: wait for Claude ready: %w", err)
	}

	// Step 5: Update CWD tracking and reset clear_ready state
	if err := e.paneState.SetCWD(paneTarget, workingDir); err != nil {
		e.log(LogLevelWarn, "set_cwd_failed cwd=%s error=%v", workingDir, err)
	}
	// Reset clear_ready since we started a fresh Claude session
	if err := e.paneState.ResetClearReady(paneTarget); err != nil {
		e.log(LogLevelWarn, "reset_clear_ready_failed error=%v", err)
	}

	e.log(LogLevelInfo, "working_dir_changed cwd=%s", workingDir)
	return nil
}

// waitForShell polls a tmux pane until its current command is a known shell,
// indicating the pane has returned to the shell prompt (e.g., after Claude exits).
func (e *Executor) waitForShell(ctx context.Context, paneTarget string) error {
	const maxAttempts = 30        // 30 × 500ms = 15s max
	const pollInterval = 500      // milliseconds
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
				e.log(LogLevelDebug, "waitForShell detected shell command=%s", cmd)
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

		content, err := e.paneIO.CapturePane(paneTarget, promptReadyLines)
		if err != nil {
			e.log(LogLevelDebug, "waitReadyCore capture error=%v attempt=%d", err, i)
			if i < maxRetries {
				if err := sleepCtx(ctx, interval); err != nil {
					return false, fmt.Errorf("waitReadyCore sleep cancelled: %w", err)
				}
				continue
			}
			return false, fmt.Errorf("waitReadyCore: capture pane failed after %d attempts: %w", i+1, err)
		}

		if isPromptReady(content) {
			e.log(LogLevelDebug, "waitReadyCore prompt detected attempt=%d", i)
			return true, nil
		}

		if i < maxRetries {
			e.log(LogLevelDebug, "waitReadyCore not ready attempt=%d/%d", i, maxRetries)
			if err := sleepCtx(ctx, interval); err != nil {
				return false, fmt.Errorf("waitReadyCore sleep cancelled: %w", err)
			}
		}
	}

	return false, nil
}

// --- Logging ---

// logf is the shared log formatting function used by Executor and BusyDetector.
// It checks the level threshold, formats the message with timestamp, level, and
// component prefix, then writes to the provided logger.
func logf(logger *log.Logger, minLevel, level LogLevel, component, format string, args ...any) {
	if level < minLevel {
		return
	}
	levelStr := "INFO"
	switch level {
	case LogLevelDebug:
		levelStr = "DEBUG"
	case LogLevelWarn:
		levelStr = "WARN"
	case LogLevelError:
		levelStr = "ERROR"
	}
	msg := fmt.Sprintf(format, args...)
	logger.Printf("%s %s %s: %s", time.Now().Format(time.RFC3339), levelStr, component, msg)
}

func (e *Executor) log(level LogLevel, format string, args ...any) {
	logf(e.logger, e.logLevel, level, "agent_executor", format, args...)
}
