// Package agent provides agent lifecycle management and command execution.
package agent

import (
	"context"
	"crypto/sha256"
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
	"github.com/msageha/maestro_v2/internal/tmux"
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
	stableCheckRounds  = 1 // 安定性判定に必要なラウンド数
	lastLineMaxDisplay = 80
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
	maestroDir string
	config     model.WatcherConfig
	logger     *log.Logger
	logFile    io.Closer
	logLevel   LogLevel
	busyRegex  *regexp.Regexp
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

	return &Executor{
		maestroDir: maestroDir,
		config:     applyDefaults(watcherCfg),
		logger:     log.New(w, "", 0),
		logFile:    closer,
		logLevel:   parseLogLevel(logLevel),
		busyRegex:  busyRegex,
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
		ctx = context.Background()
	}

	paneTarget, err := tmux.FindPaneByAgentID(req.AgentID)
	if err != nil {
		e.log(LogLevelError, "delivery_error agent_id=%s error=pane_not_found: %v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("find pane for %s: %w", req.AgentID, err)}
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
func (e *Executor) execIsBusy(ctx context.Context, paneTarget string) ExecResult {
	verdict := e.detectBusy(ctx, paneTarget)
	return ExecResult{Success: verdict != VerdictIdle}
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
	if err := tmux.SendCtrlC(paneTarget); err != nil {
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
	if err := tmux.SetUserVar(paneTarget, "status", "idle"); err != nil {
		e.log(LogLevelWarn, "set_status_idle_failed agent_id=%s error=%v", req.AgentID, err)
	}

	e.log(LogLevelInfo, "interrupt_success agent_id=%s task_id=%s lease_epoch=%d",
		req.AgentID, req.TaskID, req.LeaseEpoch)
	return ExecResult{Success: true}
}

// execWithClear delivers a message with prior /clear (Worker mode).
func (e *Executor) execWithClear(ctx context.Context, req ExecRequest, paneTarget string) ExecResult {
	e.log(LogLevelInfo, "delivery_start agent_id=%s task_id=%s command_id=%s lease_epoch=%d attempt=%d",
		req.AgentID, req.TaskID, req.CommandID, req.LeaseEpoch, req.Attempt)

	// Orchestrator: never /clear, fall through to deliver mode
	if req.AgentID == "orchestrator" {
		return e.execDeliver(ctx, req, paneTarget)
	}

	// Check if pane process has been restarted (PID changed)
	currentPID, err := tmux.GetPanePID(paneTarget)
	if err != nil {
		e.log(LogLevelWarn, "get_pane_pid_failed agent_id=%s error=%v", req.AgentID, err)
		// Continue with default behavior
	} else {
		storedPID, _ := tmux.GetUserVar(paneTarget, "clear_ready_pid")
		if storedPID != "" && storedPID != currentPID {
			// Process restarted, reset clear_ready flag
			e.log(LogLevelInfo, "pane_process_restarted agent_id=%s old_pid=%s new_pid=%s, resetting clear_ready",
				req.AgentID, storedPID, currentPID)
			tmux.SetUserVar(paneTarget, "clear_ready", "")
			tmux.SetUserVar(paneTarget, "clear_ready_pid", "")
		}
	}

	// Check if this worker pane is ready for /clear (has active conversation)
	clearReady, err := tmux.GetUserVar(paneTarget, "clear_ready")
	if err != nil || clearReady != "true" {
		// First dispatch: skip /clear, use deliver mode
		e.log(LogLevelDebug, "first_dispatch agent_id=%s clear_ready=%s, using deliver mode",
			req.AgentID, clearReady)

		result := e.execDeliver(ctx, req, paneTarget)

		// On success, mark this pane as clear-ready for future dispatches
		if result.Success {
			if err := tmux.SetUserVar(paneTarget, "clear_ready", "true"); err != nil {
				e.log(LogLevelWarn, "set_clear_ready_failed agent_id=%s error=%v", req.AgentID, err)
			}
			// Store current PID to detect future process restarts
			if currentPID != "" {
				if err := tmux.SetUserVar(paneTarget, "clear_ready_pid", currentPID); err != nil {
					e.log(LogLevelWarn, "set_clear_ready_pid_failed agent_id=%s error=%v", req.AgentID, err)
				}
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
		tmux.SetUserVar(paneTarget, "clear_ready", "")
		tmux.SetUserVar(paneTarget, "clear_ready_pid", "")

		return ExecResult{Error: fmt.Errorf("with_clear: %w", err), Retryable: true}
	}

	// Step 4: Busy detection with retry
	verdict := e.detectBusyWithRetry(ctx, req, paneTarget)
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
	e.log(LogLevelInfo, "delivery_start agent_id=%s task_id=%s command_id=%s lease_epoch=%d attempt=%d",
		req.AgentID, req.TaskID, req.CommandID, req.LeaseEpoch, req.Attempt)

	// Orchestrator: strict busy check, no retry, immediate failure if busy
	if req.AgentID == "orchestrator" {
		verdict := e.detectBusy(ctx, paneTarget)
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
	verdict := e.detectBusyWithRetry(ctx, req, paneTarget)
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
	if err := tmux.SendTextAndSubmit(ctx, paneTarget, req.Message); err != nil {
		e.log(LogLevelError, "delivery_error agent_id=%s task_id=%s error=send_text: %v",
			req.AgentID, req.TaskID, err)
		return ExecResult{Error: fmt.Errorf("send message: %w", err), Retryable: true}
	}

	// Update @status to busy
	if err := tmux.SetUserVar(paneTarget, "status", "busy"); err != nil {
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
		preClearContent, err := tmux.CapturePaneJoined(paneTarget, promptReadyLines)
		preClearHashValid := err == nil
		if err != nil {
			e.log(LogLevelWarn, "clear_confirm pre_capture error=%v attempt=%d (hash check disabled)", err, attempt)
		}
		preClearHash := contentHash(preClearContent)

		// Send /clear with double-enter for reliability
		if err := tmux.SendCommand(paneTarget, "/clear"); err != nil {
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

		// Wait 500ms before sending second Enter
		if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
			return fmt.Errorf("clear_confirm: wait cancelled: %w", err)
		}

		// Send second Enter to ensure /clear execution.
		// Commands starting with `/` often trigger completion prompts for the user,
		// requiring a second Enter to confirm the command.
		if err := tmux.SendKeys(paneTarget, "Enter"); err != nil {
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

		content, err := tmux.CapturePaneJoined(paneTarget, promptReadyLines)
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

// clearTextVisible checks whether "/clear" text is visible near the bottom of the pane content.
// This is the primary signal for detecting that /clear was NOT processed as a command
// and instead remains as literal text in the input field.
func clearTextVisible(content string) bool {
	lines := strings.Split(content, "\n")
	// Check the last 6 non-blank lines (covers input line + a few status lines)
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 6; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "/clear") {
			return true
		}
		checked++
	}
	return false
}

// --- Busy Detection ---

// detectBusy performs one round of the 3-stage busy detection algorithm.
// Returns VerdictUndecided if ctx is cancelled during the activity probe sleep.
func (e *Executor) detectBusy(ctx context.Context, paneTarget string) BusyVerdict {
	// Stage 1: pane_current_command — quick gate
	cmd, err := tmux.GetPaneCurrentCommand(paneTarget)
	if err != nil {
		e.log(LogLevelDebug, "busy_detection pane_current_command error=%v", err)
		return VerdictUndecided
	}
	e.log(LogLevelDebug, "busy_detection started; pane_current_command=%s", cmd)

	// If the pane is running only a shell, no agent CLI is active → idle.
	if tmux.IsShellCommand(cmd) {
		e.log(LogLevelDebug, "busy_detection pane running shell %q → idle", cmd)
		return VerdictIdle
	}

	// Stage 2: Pattern hint from last busyHintLines lines.
	// Uses CapturePane (no -J) to preserve line boundaries for regex matching.
	content, err := tmux.CapturePane(paneTarget, busyHintLines)
	if err != nil {
		e.log(LogLevelDebug, "busy_detection capture_pane error=%v", err)
		return VerdictUndecided
	}

	patternMatched := e.busyRegex != nil && e.busyRegex.MatchString(content)

	hintStr := "not_matched"
	if patternMatched {
		hintStr = "matched"
	}
	e.log(LogLevelDebug, "busy_detection busy_pattern_hint=%s", hintStr)

	// Stage 3: Activity probe (hash comparison over idle_stable_sec).
	// Uses CapturePaneJoined (-J) for width-independent hash stability.
	joinedContent, err := tmux.CapturePaneJoined(paneTarget, busyHintLines)
	if err != nil {
		e.log(LogLevelDebug, "busy_detection joined capture error=%v", err)
		return VerdictUndecided
	}
	hashA := contentHash(joinedContent)
	if err := sleepCtx(ctx, time.Duration(e.config.IdleStableSec)*time.Second); err != nil {
		e.log(LogLevelDebug, "busy_detection activity_probe sleep cancelled: %v", err)
		return VerdictUndecided
	}

	joinedContent2, err := tmux.CapturePaneJoined(paneTarget, busyHintLines)
	if err != nil {
		e.log(LogLevelDebug, "busy_detection second capture error=%v", err)
		return VerdictUndecided
	}
	hashB := contentHash(joinedContent2)

	hashChanged := hashA != hashB

	var verdict BusyVerdict
	switch {
	case hashChanged:
		verdict = VerdictBusy
	case !patternMatched:
		verdict = VerdictIdle
	default:
		verdict = VerdictUndecided
	}

	e.log(LogLevelDebug, "busy_detection activity_probe hash_changed=%v verdict=%s",
		hashChanged, verdict)
	return verdict
}

// detectBusyWithRetry runs busy detection with a retry loop on VerdictBusy.
// Returns VerdictUndecided if ctx is cancelled during retries.
func (e *Executor) detectBusyWithRetry(ctx context.Context, req ExecRequest, paneTarget string) BusyVerdict {
	verdict := e.detectBusy(ctx, paneTarget)
	if verdict != VerdictBusy {
		return verdict
	}

	for i := 1; i <= e.config.BusyCheckMaxRetries; i++ {
		e.log(LogLevelDebug, "busy_retry retry=%d/%d agent_id=%s",
			i, e.config.BusyCheckMaxRetries, req.AgentID)
		if err := sleepCtx(ctx, time.Duration(e.config.BusyCheckInterval)*time.Second); err != nil {
			e.log(LogLevelDebug, "busy_retry sleep cancelled: %v", err)
			return VerdictUndecided
		}

		verdict = e.detectBusy(ctx, paneTarget)
		if verdict != VerdictBusy {
			return verdict
		}
	}

	return VerdictBusy
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

		content1, err := tmux.CapturePaneJoined(paneTarget, promptReadyLines)
		if err != nil {
			return fmt.Errorf("capture pane for stability round %d: %w", round, err)
		}
		h1 := contentHash(content1)

		if err := sleepCtx(ctx, time.Duration(e.config.IdleStableSec)*time.Second); err != nil {
			return fmt.Errorf("wait_stable sleep cancelled (round %d): %w", round, err)
		}

		content2, err := tmux.CapturePaneJoined(paneTarget, promptReadyLines)
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
	finalContent, err := tmux.CapturePane(paneTarget, promptReadyLines)
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

// contentHash returns a hex-encoded SHA-256 hash of the content.
func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// isPromptReady checks whether the pane content indicates Claude Code is at its input prompt.
// It inspects the last non-blank line of the captured pane output.
//
// Primary check: the line contains '❯' (U+276F HEAVY RIGHT-POINTING ANGLE QUOTATION MARK
// ORNAMENT), which is the character Claude Code uses in its input prompt.
// Fallback check: the line starts with '>' (ASCII 0x3E). This covers older Claude Code
// versions or terminal environments where the Unicode character is not rendered.
//
// The fallback is intentionally broad; false positives (e.g. markdown blockquotes) are
// mitigated by callers performing stability checks before invoking this function.
// maxPromptSearchLines limits how many non-blank lines (from the bottom)
// are checked for the ❯ prompt character. This accommodates Claude Code's
// status bar (typically 1–2 lines below the prompt) while bounding the
// search to avoid false positives from agent output that happens to
// contain ❯ in earlier lines.
const maxPromptSearchLines = 6

// ansiEscape matches ANSI escape sequences including CSI (with private params),
// OSC, and charset designators.
var ansiEscape = regexp.MustCompile(`\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\][^\x07]*(?:\x07|\x1b\\)|\([B0UK]|[>=])`)

// stripANSI removes ANSI escape sequences from s.
func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

func isPromptReady(content string) bool {
	lines := strings.Split(content, "\n")
	// Scan bottom-up, checking up to maxPromptSearchLines non-blank lines for ❯.
	// Claude Code's TUI may show a status bar below the prompt,
	// so the prompt line is not necessarily the last non-blank line.
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < maxPromptSearchLines; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "❯") {
			return true
		}
		checked++
	}
	// Fallback: check only the last non-blank line for '>'.
	// Limiting to the last line avoids false positives from markdown
	// blockquotes or shell output on earlier lines.
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		return strings.HasPrefix(trimmed, ">")
	}
	return false
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
	maxRetries := e.config.WaitReadyMaxRetries
	interval := time.Duration(e.config.WaitReadyIntervalSec) * time.Second

	for i := 0; i <= maxRetries; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait_ready cancelled at attempt %d: %w", i, err)
		}

		content, err := tmux.CapturePane(paneTarget, promptReadyLines)
		if err != nil {
			e.log(LogLevelDebug, "wait_ready capture error=%v attempt=%d", err, i)
			if i < maxRetries {
				if err := sleepCtx(ctx, interval); err != nil {
					return fmt.Errorf("wait_ready sleep cancelled: %w", err)
				}
				continue
			}
			// Capture itself kept failing — this is a tmux error, not a prompt issue.
			return fmt.Errorf("wait_ready: capture pane failed after %d attempts: %w", i+1, err)
		}

		if isPromptReady(content) {
			e.log(LogLevelDebug, "wait_ready prompt detected attempt=%d", i)
			return nil
		}

		if i < maxRetries {
			e.log(LogLevelDebug, "wait_ready not ready attempt=%d/%d", i, maxRetries)
			if err := sleepCtx(ctx, interval); err != nil {
				return fmt.Errorf("wait_ready sleep cancelled: %w", err)
			}
		}
	}

	// Fallback: prompt not detected, but proceed with a warning.
	// The subsequent detectBusyWithRetry() will catch if the agent is actually busy.
	e.log(LogLevelInfo, "wait_ready prompt_fallback pane=%s: prompt not detected after %d attempts, proceeding (detectBusy will guard)",
		paneTarget, maxRetries+1)
	return nil
}

// lastNonBlankLine returns the last non-blank line from content, truncated to lastLineMaxDisplay chars.
func lastNonBlankLine(content string) string {
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if len(trimmed) > lastLineMaxDisplay {
			return trimmed[:lastLineMaxDisplay] + "..."
		}
		return trimmed
	}
	return "<empty>"
}

// --- Envelope Builders ---

// BuildWorkerEnvelope creates the delivery envelope for a Worker task.
// Format matches spec §5.8.1 Worker 向けタスク配信エンベロープ.
func BuildWorkerEnvelope(task model.Task, workerID string, leaseEpoch, attempt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[maestro] task_id:%s command_id:%s lease_epoch:%d attempt:%d\n",
		task.ID, task.CommandID, leaseEpoch, attempt)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "purpose: %s\n", task.Purpose)
	fmt.Fprintf(&sb, "content: %s\n", task.Content)
	fmt.Fprintf(&sb, "acceptance_criteria: %s\n", task.AcceptanceCriteria)
	constraintsStr := "なし"
	if len(task.Constraints) > 0 {
		constraintsStr = strings.Join(task.Constraints, ", ")
	}
	fmt.Fprintf(&sb, "constraints: %s\n", constraintsStr)
	toolsHintStr := "なし"
	if len(task.ToolsHint) > 0 {
		toolsHintStr = strings.Join(task.ToolsHint, ", ")
	}
	fmt.Fprintf(&sb, "tools_hint: %s\n", toolsHintStr)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "完了時: maestro result write %s --task-id %s --command-id %s --lease-epoch %d --status <completed|failed> --summary \"...\"\n",
		workerID, task.ID, task.CommandID, leaseEpoch)
	sb.WriteString("失敗時に部分変更あり: 上記に加えて --partial-changes --no-retry-safe")
	return sb.String()
}

// BuildPlannerEnvelope creates the delivery envelope for a Planner command.
// Format matches spec §5.8.1 Planner 向けコマンド配信エンベロープ.
func BuildPlannerEnvelope(cmd model.Command, leaseEpoch, attempt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[maestro] command_id:%s lease_epoch:%d attempt:%d\n",
		cmd.ID, leaseEpoch, attempt)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "content: %s\n", cmd.Content)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "タスク分解後: maestro plan submit --command-id %s --tasks-file plan.yaml\n", cmd.ID)
	fmt.Fprintf(&sb, "全タスク完了後: maestro plan complete --command-id %s --summary \"...\"", cmd.ID)
	return sb.String()
}

// BuildOrchestratorNotificationEnvelope creates the envelope for an Orchestrator notification.
// Format matches spec §5.8.1 Orchestrator 向け通知配信エンベロープ.
func BuildOrchestratorNotificationEnvelope(commandID, notificationType string) string {
	terminalStatus := mapNotificationTypeToStatus(notificationType)
	return fmt.Sprintf("[maestro] kind:command_completed command_id:%s status:%s\nresults/planner.yaml を確認してください",
		commandID, terminalStatus)
}

func mapNotificationTypeToStatus(nt string) string {
	switch nt {
	case "command_completed":
		return "completed"
	case "command_failed":
		return "failed"
	case "command_cancelled":
		return "cancelled"
	default:
		return nt
	}
}

// BuildTaskResultNotification creates a side-channel notification for the Planner.
func BuildTaskResultNotification(commandID, taskID, workerID, taskStatus string) string {
	return fmt.Sprintf("[maestro] kind:task_result command_id:%s task_id:%s worker_id:%s status:%s\nresults/%s.yaml を確認してください",
		commandID, taskID, workerID, taskStatus, workerID)
}

// --- Logging ---

func (e *Executor) log(level LogLevel, format string, args ...any) {
	if level < e.logLevel {
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
	e.logger.Printf("%s %s agent_executor: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}
