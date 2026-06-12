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

	// ErrAgentBusy is returned when the agent is confirmed busy (actively processing).
	// This is a normal operational state, not a failure. The caller should retry later.
	ErrAgentBusy = errors.New("agent busy")

	// ErrUserComposing is returned when the orchestrator pane's input box
	// holds actively-changing user text. Pasting now would interleave the
	// notification with the human's draft and the trailing Enter would
	// submit it half-written. Transient; the notification queue retries on
	// a later scan.
	ErrUserComposing = errors.New("user composing in pane")
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
	// UserComposingProbeInterval is the gap between the two prompt-input
	// captures in detectActiveUserInput (orchestrator user-composing guard).
	// Long enough for a typing human to change the input text, short enough
	// not to delay notification delivery noticeably.
	UserComposingProbeInterval time.Duration
}

// DefaultExecutorConfig returns production-safe defaults.
func DefaultExecutorConfig() ExecutorConfig {
	return ExecutorConfig{
		PromptReadyLines:           12, // 12 lines to accommodate status bars
		BusyHintLines:              5,
		StableCheckRounds:          1,
		DefaultExecTimeout:         5 * time.Minute,
		ClaudeLaunchTimeout:        60 * time.Second,
		UserComposingProbeInterval: 2 * time.Second,
	}
}

// ExecRequest is an alias for model.ExecRequest.
type ExecRequest = model.ExecRequest

// ExecResult is an alias for model.ExecResult.
type ExecResult = model.ExecResult

// logLevel controls logging verbosity.
// This is intentionally a private type within the agent package, separate from
// core.LogLevel in the daemon package. The agent package does not import
// daemon/core to maintain package boundary isolation.
type logLevel int

const (
	logLevelDebug logLevel = iota
	logLevelInfo
	logLevelWarn
	logLevelError
)

func (l logLevel) String() string {
	switch l {
	case logLevelDebug:
		return "debug"
	case logLevelInfo:
		return "info"
	case logLevelWarn:
		return "warn"
	case logLevelError:
		return "error"
	default:
		return "unknown"
	}
}

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
	e.processManager = newClaudeProcessManager(paneIO, ps, &e.config, execCfg, logger, ll, maestroDir)
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

// RespawnPaneToProjectRoot kills the agent process inside the worker's
// tmux pane and respawns the shell into the project root. Phase B uses
// this just before `git worktree remove` so the pane never holds a cwd
// the daemon is about to delete — claude-code's Stop hook posix_spawn
// '/bin/sh' fails with ENOENT when its cwd has been removed.
//
// onlyIfCWDUnder, when non-empty, restricts the eviction to panes whose
// current working directory is inside that directory. Phase B cleanup
// passes the worktree path being removed so a worker that has already
// been re-assigned to a different command's worktree is left untouched
// (E2E 2026-06-11: the orphan cleanup of a failed command evicted a
// worker two minutes after it was dispatched for a new command, killing
// the freshly launched claude and stalling the task until the lease hard
// cap). An empty string keeps the unconditional behaviour.
//
// No-op when the worker pane cannot be located (worker never started, or
// already torn down). Errors are returned so the caller can decide
// whether to skip the corresponding cleanup. ensureWorkingDir on the
// next dispatch will detect the post-respawn shell and re-launch claude,
// so this is safe to call between turns.
func (e *Executor) RespawnPaneToProjectRoot(workerID, onlyIfCWDUnder string) error {
	paneTarget, err := e.paneIO.FindPaneByAgentID(workerID)
	if err != nil {
		e.log(logLevelDebug,
			"respawn_to_project_root_skip worker=%s reason=pane_not_found error=%v",
			workerID, err)
		return nil
	}
	if onlyIfCWDUnder != "" {
		cwd, cwdErr := e.paneIO.GetPaneCurrentPath(paneTarget)
		switch {
		case cwdErr != nil:
			// Fall through to evict: a transient tmux query failure must not
			// leave a pane sitting in a directory that is about to be
			// deleted (the ENOENT failure mode this hook exists to prevent).
			e.log(logLevelWarn,
				"respawn_to_project_root_cwd_query_failed worker=%s error=%v (evicting anyway)",
				workerID, cwdErr)
		case !isPathUnder(cwd, onlyIfCWDUnder):
			e.log(logLevelInfo,
				"respawn_to_project_root_skip worker=%s reason=cwd_outside_target cwd=%s target=%s "+
					"(pane already re-assigned elsewhere; leaving its process alone)",
				workerID, cwd, onlyIfCWDUnder)
			return nil
		}
	}
	projectRoot := projectRootFromMaestroDir(e.maestroDir)
	if projectRoot == "" {
		// Defensive: maestroDir was unset (newExecutor path used by some
		// tests). Skip silently rather than respawning into "/" which
		// would surprise the operator.
		e.log(logLevelDebug,
			"respawn_to_project_root_skip worker=%s reason=no_project_root", workerID)
		return nil
	}
	return e.processManager.respawnToProjectRoot(paneTarget, workerID, projectRoot)
}

// isPathUnder reports whether path is dir itself or located inside dir.
// Both sides are cleaned and symlink-resolved (best effort) before the
// comparison so /tmp vs /private/tmp style aliases on macOS compare equal.
func isPathUnder(path, dir string) bool {
	p := canonicalizePath(path)
	d := canonicalizePath(dir)
	rel, err := filepath.Rel(d, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// canonicalizePath cleans path and resolves symlinks when possible. When
// the path no longer exists (e.g. a worktree already removed) the cleaned
// absolute form is returned unchanged.
func canonicalizePath(path string) string {
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved
	}
	return clean
}

// projectRootFromMaestroDir derives the project root from the maestro
// data directory. The daemon binds maestroDir to "<root>/.maestro", so
// the parent directory is the project root.
func projectRootFromMaestroDir(maestroDir string) string {
	if maestroDir == "" {
		return ""
	}
	return filepath.Dir(maestroDir)
}

// Default values for WatcherConfig fields when unset or non-positive.
//
// defaultClearConfirmTimeoutSec / defaultClearMaxAttempts have explicit
// budgets (8 s × 7 attempts = 56 s) sized to absorb Claude Code TUI
// cold-start (~40-50 s); the previous 5 s × 5 attempts dropped
// clear_confirm dispatch every few minutes in workspace benches.
const (
	defaultBusyCheckInterval      = 2   // seconds between busy-detection probes
	defaultBusyCheckMaxRetries    = 30  // max busy-detection retry attempts
	defaultIdleStableSec          = 5   // seconds of stability before declaring idle
	defaultCooldownAfterClear     = 3   // seconds to wait after /clear
	defaultWaitReadyIntervalSec   = 2   // seconds between prompt-readiness polls
	defaultWaitReadyMaxRetries    = 15  // max prompt-readiness poll attempts
	defaultClearConfirmTimeoutSec = 8   // seconds to wait for /clear confirmation
	defaultClearConfirmPollMs     = 250 // milliseconds between /clear confirmation polls
	defaultClearMaxAttempts       = 7   // max /clear retry attempts
	defaultClearRetryBackoffMs    = 500 // milliseconds backoff between /clear retries
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

// stampRunOnMainVar writes the @run_on_main pane variable ("1" for
// RunOnMain dispatches, "" otherwise) and reads it back to confirm the
// value landed. The read-back matters because worker_policy_hook.sh
// only fails closed when its *read* of the flag fails — a write that
// silently did not land reads back as empty and disables the guard
// entirely, leaving a RunOnMain Worker on the main checkout with no
// mutation protection.
func (e *Executor) stampRunOnMainVar(paneTarget string, runOnMain bool) error {
	want := ""
	if runOnMain {
		want = "1"
	}
	if err := e.paneIO.SetUserVar(paneTarget, "run_on_main", want); err != nil {
		return fmt.Errorf("set @run_on_main=%q: %w", want, err)
	}
	got, err := e.paneIO.GetUserVar(paneTarget, "run_on_main")
	if err != nil {
		return fmt.Errorf("read back @run_on_main: %w", err)
	}
	if got != want {
		return fmt.Errorf("read back @run_on_main: got %q, want %q", got, want)
	}
	return nil
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

	// run_on_main hard guard: stamp the pane with @run_on_main so the
	// PreToolUse policy hook can deny Write/Edit while the Worker is
	// pointed at the main worktree (read-only verification mode). For a
	// RunOnMain dispatch this flag is the ONLY mechanical mutation guard
	// (the pane already points at the main checkout), so a stamp failure
	// must abort the dispatch as retryable instead of degrading to
	// fail-open. The var must also be cleared on non-run_on_main
	// dispatches because the same pane is reused across tasks; a stale
	// "1" would lock the next task into read-only mode, so clear
	// failures abort too (availability rather than safety).
	if err := e.stampRunOnMainVar(paneTarget, req.RunOnMain); err != nil {
		e.log(logLevelError, "set_run_on_main_var_failed agent_id=%s run_on_main=%t error=%v",
			req.AgentID, req.RunOnMain, err)
		return ExecResult{Error: fmt.Errorf("stamp @run_on_main: %w", err), Retryable: true}
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
//
// Bug J: first dispatch intentionally bypasses busy detection. By definition,
// !clear_ready means no prior task has been delivered to this pane, so any
// content movement observed on screen is runtime startup (TUI animations,
// welcome banner, status refresh) rather than actual task processing. Running
// hash-based busy detection here interprets those animations as "busy" and
// blocks delivery for minutes — most acutely for codex, whose Rust TUI
// re-renders continuously right after launch.
//
// Use waitReady (prompt-readiness check) instead: it confirms the runtime
// process is alive (non-shell pane command) and has painted at least one
// non-blank line, which is the correct precondition for first delivery across
// claude-code / codex / gemini. execWithClear already ran ensureClaudeRunning
// before arriving here, so we do not repeat that step.
func (e *Executor) execFirstDispatch(ctx context.Context, req ExecRequest, paneTarget, currentPID string) ExecResult {
	e.log(logLevelDebug, "first_dispatch agent_id=%s, using waitReady + send (no busy-detect)", req.AgentID)

	if err := e.processManager.waitReady(ctx, paneTarget); err != nil {
		e.log(logLevelWarn, "first_dispatch_wait_ready_failed agent_id=%s error=%v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("first dispatch wait ready: %w", err), Retryable: true}
	}

	result := e.sendAndConfirm(req, paneTarget)
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

// detectActiveUserInput reports whether a human appears to be actively typing
// in the pane's input box: the prompt line carries non-empty text AND that
// text changes across a short interval. Static non-empty text is NOT treated
// as composing — it may be a placeholder hint or an abandoned draft, and
// deferring on it would block delivery indefinitely; proceeding matches the
// pre-guard behavior. Capture failures fail open (deliver) for the same
// reason.
func (e *Executor) detectActiveUserInput(ctx context.Context, paneTarget string) bool {
	first, ok := e.capturePromptInput(paneTarget)
	if !ok || first == "" {
		return false
	}
	if err := sleepCtx(ctx, e.execCfg.UserComposingProbeInterval); err != nil {
		// Delivery context expired mid-probe with known non-empty input:
		// defer conservatively rather than pasting into a possible draft.
		return true
	}
	second, ok := e.capturePromptInput(paneTarget)
	if !ok {
		return false
	}
	return second != "" && second != first
}

// capturePromptInput captures the pane and extracts the prompt-line input
// text. ok=false on capture failure or when no prompt line is visible.
func (e *Executor) capturePromptInput(paneTarget string) (string, bool) {
	content, err := e.paneIO.CapturePane(paneTarget, e.execCfg.PromptReadyLines)
	if err != nil {
		return "", false
	}
	return promptInputText(content)
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
	// Orchestrator: busy check with retry to avoid false-positive busy
	// detection during transient state transitions (e.g., screen updates
	// right after becoming idle).
	if req.AgentID == "orchestrator" {
		// Ensure Claude is running for orchestrator too
		if err := e.processManager.ensureClaudeRunning(ctx, paneTarget, req.AgentID); err != nil {
			e.log(logLevelError, "ensure_claude_running_failed agent_id=orchestrator error=%v", err)
			return ExecResult{Error: fmt.Errorf("ensure claude running: %w", err), Retryable: true}
		}
		verdict := e.busyDetector.DetectBusyWithRetry(ctx, paneTarget, req.AgentID)
		e.log(logLevelDebug, "busy_detection agent_id=orchestrator verdict=%s", verdict)
		if verdict != VerdictIdle {
			if verdict == VerdictUndecided {
				e.log(logLevelWarn, "delivery_failure agent_id=orchestrator reason=undecided_after_probes verdict=%s", verdict)
				return ExecResult{
					Error:     fmt.Errorf("orchestrator busy: %w", ErrBusyUndecided),
					Retryable: true,
				}
			}
			// VerdictBusy: orchestrator is actively processing — normal operational state
			e.log(logLevelInfo, "agent_busy_retryable agent_id=orchestrator verdict=%s", verdict)
			return ExecResult{
				Error:     fmt.Errorf("orchestrator busy: %w", ErrAgentBusy),
				Retryable: true,
			}
		}
		// User-composing guard. The busy detector's claude fast-path declares
		// idle whenever the prompt glyph is visible — which is also true
		// while a human is typing in the orchestrator pane, the one pane a
		// user actually works in. Pasting now would inject the notification
		// into their draft and the trailing Enter would submit it
		// half-written. Defer only on ACTIVE composition (input text that
		// changes between two captures); static text (abandoned draft,
		// placeholder hint) proceeds as before, so a stale draft can never
		// block notifications indefinitely.
		if e.detectActiveUserInput(ctx, paneTarget) {
			e.log(logLevelInfo, "delivery_deferred_user_composing agent_id=orchestrator")
			return ExecResult{
				Error:     fmt.Errorf("orchestrator busy: %w", ErrUserComposing),
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
		// VerdictBusy: agent is actively processing — normal operational state, not a failure
		e.log(logLevelInfo, "agent_busy_retryable agent_id=%s task_id=%s verdict=%s",
			req.AgentID, req.TaskID, verdict)
		return ExecResult{
			Error:     fmt.Errorf("agent %s busy: %w", req.AgentID, ErrAgentBusy),
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
