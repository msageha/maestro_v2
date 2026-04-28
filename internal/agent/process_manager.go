package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// ClaudeProcessManager manages Claude process lifecycle: startup, recovery,
// working directory changes, and prompt readiness detection.
// Extracted from Executor to separate process management from dispatch routing.
type ClaudeProcessManager struct {
	paneIO    PaneIO
	paneState *paneStateManager
	execCfg   ExecutorConfig
	config    *model.WatcherConfig
	logger    *log.Logger
	logLevel  logLevel
	// dirExists checks whether a path resolves to a real directory. Defaults
	// to directoryExists (os.Stat). Tests use synthetic paths like
	// "/project/worktree1" that never exist on the filesystem, so they
	// override this with a no-op so the stale-cwd detection in
	// ensureWorkingDir does not force a respawn on every same-cwd call.
	dirExists func(path string) bool
}

func newClaudeProcessManager(paneIO PaneIO, paneState *paneStateManager, cfg *model.WatcherConfig, execCfg ExecutorConfig, logger *log.Logger, ll logLevel) *ClaudeProcessManager {
	return &ClaudeProcessManager{
		paneIO:    paneIO,
		paneState: paneState,
		execCfg:   execCfg,
		config:    cfg,
		logger:    logger,
		logLevel:  ll,
		dirExists: directoryExists,
	}
}

// ensureClaudeRunning checks whether the pane is running a shell (indicating
// Claude has crashed or exited) and re-launches Claude if necessary.
// This guards against the scenario where Claude crashes back to the shell
// but the pane PID stays the same (shell PID unchanged), which
// DetectProcessRestart cannot detect.
// Returns nil if Claude is confirmed running, or a retryable error on failure.
func (pm *ClaudeProcessManager) ensureClaudeRunning(ctx context.Context, paneTarget, agentID string) error {
	cmd, err := pm.paneIO.GetPaneCurrentCommand(paneTarget)
	if err != nil {
		pm.log(logLevelError, "ensure_claude_running_check_failed agent_id=%s error=%v", agentID, err)
		return fmt.Errorf("%w: %w", ErrCheckPaneCommand, err)
	}

	if !pm.paneIO.IsShellCommand(cmd) {
		// Not a shell -- Claude (or another process) is running
		return nil
	}

	pm.log(logLevelWarn, "claude_not_running agent_id=%s pane_command=%s, re-launching", agentID, cmd)

	// Reset clear_ready since we are starting a fresh Claude session
	if resetErr := pm.paneState.ResetClearReady(paneTarget); resetErr != nil {
		pm.log(logLevelError, "ensure_claude_running_reset_clear_ready agent_id=%s error=%v", agentID, resetErr)
	}

	// Re-launch Claude in the pane using the resolved binary path to prevent
	// version skew between the daemon binary and the pane's PATH resolution.
	if sendErr := pm.paneIO.SendCommand(paneTarget, ResolvedLaunchCommand()); sendErr != nil {
		return fmt.Errorf("%w: %w", ErrRelaunch, sendErr)
	}

	// Wait for Claude prompt readiness (fail-closed)
	launchCtx, cancel := context.WithTimeout(ctx, pm.execCfg.ClaudeLaunchTimeout)
	defer cancel()
	if waitErr := pm.waitReadyStrict(launchCtx, paneTarget); waitErr != nil {
		return fmt.Errorf("%w after re-launch: %w", ErrWaitClaudeReady, waitErr)
	}

	// Claude is back up; clear any "evicted" sentinel set during a
	// daemon-driven respawn-to-project-root so subsequent shell
	// detections in status.go are once again real crash signals.
	if err := pm.paneState.ResetAgentState(paneTarget); err != nil {
		pm.log(logLevelWarn, "ensure_claude_running_reset_agent_state agent_id=%s error=%v", agentID, err)
	}

	pm.log(logLevelInfo, "claude_relaunched agent_id=%s", agentID)
	return nil
}

// ensureWorkingDir ensures the agent's Claude Code process is running in the
// specified working directory. If the current CWD (tracked via @cwd tmux user
// variable) differs from workingDir, the method exits the current Claude
// process, changes the shell CWD, and re-launches Claude.
//
// This is called transparently by the executor before task delivery, so that
// Workers run in their worktree directory without being aware of worktrees.
//
// 2026-04-28: even when the requested cwd matches the tracked cwd, we still
// re-stat the tracked path. The Phase B publish/cleanup pipeline removes a
// worktree directory after its worker reports completion, which leaves the
// pane's @cwd label pointing at a deleted path. Claude Code's Stop hook can
// then fire from that pane and `posix_spawn '/bin/sh'` surfaces an
// `ENOENT, no such file or directory` warning (node.js reports the chdir
// failure as if the binary itself was missing). Forcing a respawn when the
// tracked cwd no longer exists puts the pane back into a real directory
// before the next dispatch and silences the spurious warning.
func (pm *ClaudeProcessManager) ensureWorkingDir(ctx context.Context, paneTarget, workingDir string) error {
	if workingDir == "" {
		return nil
	}

	// Validate path: reject control characters to prevent injection via SendCommand
	if containsControlChars(workingDir) {
		return fmt.Errorf("%w: %q", ErrControlChars, workingDir)
	}

	// Check current CWD from tmux user variable
	currentCWD := pm.paneState.GetCWD(paneTarget)
	dirExists := pm.dirExists
	if dirExists == nil {
		dirExists = directoryExists
	}
	staleCWD := currentCWD != "" && !dirExists(currentCWD)
	if currentCWD == workingDir && !staleCWD {
		pm.log(logLevelDebug, "working_dir unchanged cwd=%s", workingDir)
		return nil
	}

	if staleCWD {
		pm.log(logLevelInfo,
			"working_dir_stale_cwd_respawn pane=%s tracked_cwd=%q new=%q "+
				"(tracked cwd no longer exists on disk; forcing respawn to flush stale @cwd)",
			paneTarget, currentCWD, workingDir)
	} else {
		pm.log(logLevelInfo, "working_dir_change old=%q new=%q", currentCWD, workingDir)
	}

	// Step 1: Kill the current pane process and respawn a fresh shell in the
	// target working directory.
	if err := pm.paneIO.RespawnPane(paneTarget, workingDir); err != nil {
		return fmt.Errorf("%w: %w", ErrRespawnPane, err)
	}
	markCWDUnknownOnError := func(err error) error {
		if err == nil {
			return err
		}
		if resetErr := pm.paneState.ResetCWD(paneTarget); resetErr != nil {
			pm.log(logLevelWarn, "reset_cwd_after_working_dir_failure_failed cwd=%s reset_error=%v original_error=%v", workingDir, resetErr, err)
			return errors.Join(err, fmt.Errorf("reset tracked cwd failed: %w", resetErr))
		}
		pm.log(logLevelWarn, "working_dir_change_incomplete cwd=%s error=%v tracked_cwd_reset=true", workingDir, err)
		return err
	}

	// Step 2: Wait for the fresh shell to be ready
	if err := pm.waitForShell(ctx, paneTarget); err != nil {
		return markCWDUnknownOnError(fmt.Errorf("wait for shell after respawn: %w", err))
	}

	// Step 3: Reset clear_ready state before re-launching Claude.
	// RespawnPane changes the pane PID, so clear_ready_pid is now stale.
	// Reset early (matching ensureClaudeRunning) to prevent DetectProcessRestart
	// from seeing a stale PID between here and the launch.
	if err := pm.paneState.ResetClearReady(paneTarget); err != nil {
		pm.log(logLevelError, "reset_clear_ready_after_respawn_failed error=%v", err)
		return markCWDUnknownOnError(fmt.Errorf("reset clear ready after respawn: %w", err))
	}

	// Step 4: Re-launch Claude using the resolved binary path to prevent version skew.
	if err := pm.paneIO.SendCommand(paneTarget, ResolvedLaunchCommand()); err != nil {
		return markCWDUnknownOnError(fmt.Errorf("re-launch claude: %w", err))
	}

	// Step 5: Wait for Claude prompt readiness (fail-closed: error on timeout)
	// Use a generous timeout since Claude startup can take a few seconds.
	launchCtx, cancel := context.WithTimeout(ctx, pm.execCfg.ClaudeLaunchTimeout)
	defer cancel()
	if err := pm.waitReadyStrict(launchCtx, paneTarget); err != nil {
		return markCWDUnknownOnError(fmt.Errorf("wait for claude ready: %w", err))
	}

	// Step 6: Update CWD tracking (clear_ready was already reset in Step 3)
	if err := pm.paneState.SetCWD(paneTarget, workingDir); err != nil {
		pm.log(logLevelWarn, "set_cwd_failed cwd=%s error=%v", workingDir, err)
		return fmt.Errorf("set tracked cwd after working dir change: %w", err)
	}

	// Step 7: Clear @agent_state="evicted" if it was set by an earlier
	// respawn-to-project-root. Claude is now confirmed running again, so
	// status.go can resume treating shell detections as real crashes.
	if err := pm.paneState.ResetAgentState(paneTarget); err != nil {
		pm.log(logLevelWarn, "ensure_working_dir_reset_agent_state cwd=%s error=%v", workingDir, err)
	}

	pm.log(logLevelInfo, "working_dir_changed cwd=%s", workingDir)
	return nil
}

// directoryExists reports whether path resolves to an existing directory on
// disk. Used by ensureWorkingDir to detect a stale @cwd label whose
// underlying worktree was cleaned up between dispatches. Errors other than
// "does not exist" (e.g. permission denied) are treated as "exists" to
// avoid forcing a respawn on transient filesystem hiccups.
func directoryExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return !os.IsNotExist(err)
	}
	return info.IsDir()
}

// respawnToProjectRoot replaces the pane's current process with a fresh
// shell anchored at the project root. Used by the daemon's Phase B
// before worktree cleanup to evict the pane from a cwd that's about to
// be deleted. The cwd tracker is reset so the next ensureWorkingDir call
// treats the pane as needing a respawn into whatever working dir the
// next dispatch supplies — that path also re-launches claude, so this
// helper does not need to relaunch it itself.
//
// 2026-04-28 retest2: also sets @agent_state="evicted" so status.go
// distinguishes "daemon evicted the pane on purpose" (sit in shell, no
// claude process) from "claude crashed back to shell". Without this
// flag, `maestro status` flipped the worker to "dead" between cleanup
// and the next dispatch even though the daemon owns the gap.
func (pm *ClaudeProcessManager) respawnToProjectRoot(paneTarget, workerID, projectRoot string) error {
	pm.log(logLevelInfo,
		"respawn_to_project_root worker=%s pane=%s target=%s "+
			"(evicting pane from a cwd the daemon is about to delete; "+
			"prevents Stop hook posix_spawn '/bin/sh' ENOENT)",
		workerID, paneTarget, projectRoot)
	if err := pm.paneIO.RespawnPane(paneTarget, projectRoot); err != nil {
		return fmt.Errorf("%w to project root %s: %w", ErrRespawnPane, projectRoot, err)
	}
	if err := pm.paneState.ResetCWD(paneTarget); err != nil {
		// Resetting the tracker is observability — failure here just means
		// the next ensureWorkingDir will see a stale @cwd label, which
		// the stale-cwd guard already handles. Surface at warn so we
		// notice if it becomes a pattern.
		pm.log(logLevelWarn, "respawn_to_project_root_reset_cwd_failed pane=%s error=%v", paneTarget, err)
	}
	if err := pm.paneState.ResetClearReady(paneTarget); err != nil {
		pm.log(logLevelWarn, "respawn_to_project_root_reset_clear_ready_failed pane=%s error=%v", paneTarget, err)
	}
	if err := pm.paneState.SetAgentState(paneTarget, AgentStateEvicted); err != nil {
		pm.log(logLevelWarn, "respawn_to_project_root_set_agent_state_failed pane=%s error=%v", paneTarget, err)
	}
	return nil
}

// waitForShell polls a tmux pane until its current command is a known shell,
// indicating the pane has returned to the shell prompt (e.g., after Claude exits).
func (pm *ClaudeProcessManager) waitForShell(ctx context.Context, paneTarget string) error {
	const maxAttempts = 30         // 30 x 500ms = 15s max wait for shell to appear after respawn
	const pollInterval = 500       // milliseconds; balances responsiveness vs tmux query overhead
	const maxConsecutiveErrors = 5 // consecutive tmux query failures before giving up (transient error tolerance)

	consecutiveErrors := 0
	for i := 0; i < maxAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waitForShell cancelled: %w", err)
		}

		cmd, err := pm.paneIO.GetPaneCurrentCommand(paneTarget)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= maxConsecutiveErrors {
				return fmt.Errorf("waitForShell: %d %w, last: %w", consecutiveErrors, ErrConsecutiveErrors, err)
			}
		} else {
			consecutiveErrors = 0
			if pm.paneIO.IsShellCommand(cmd) {
				pm.log(logLevelDebug, "waitForShell detected shell command=%s", cmd)
				return nil
			}
		}

		if err := sleepCtx(ctx, time.Duration(pollInterval)*time.Millisecond); err != nil {
			return fmt.Errorf("waitForShell sleep cancelled: %w", err)
		}
	}

	return fmt.Errorf("waitForShell: shell not detected after %d attempts", maxAttempts)
}

// paneRuntime reads the @runtime pane user variable, falling back to the
// default runtime (claude-code) when unset or on read error. This is a
// fail-open read: a missing/failing runtime lookup must not break readiness
// detection for the default runtime.
func (pm *ClaudeProcessManager) paneRuntime(paneTarget string) string {
	runtime, err := pm.paneIO.GetUserVar(paneTarget, "runtime")
	if err != nil {
		pm.log(logLevelDebug, "pane_runtime_read_failed pane=%s error=%v (defaulting)", paneTarget, err)
		return model.DefaultRuntime()
	}
	if runtime == "" {
		return model.DefaultRuntime()
	}
	return runtime
}

// isAgentReady reports whether the pane's agent runtime appears to be at an
// interactive-ready state. The check is runtime-specific:
//
//   - claude-code: look for the '❯' prompt marker (isPromptReady on content).
//   - codex / gemini / other non-claude runtimes: no universal prompt glyph
//     is available, so readiness is inferred from (a) the pane's current
//     command not being a shell (the runtime binary is actually running) and
//     (b) the pane has rendered at least one non-blank line (the TUI has
//     painted something). This is strictly weaker than the claude check, but
//     it is the best reliable signal across codex (Rust TUI) and gemini
//     (Node TUI) without coupling to private prompt characters that may
//     change across versions.
//
// Callers that need stronger confirmation (e.g., post-/clear for claude) use
// waitStable, which adds content-stability debouncing on top of this check.
func (pm *ClaudeProcessManager) isAgentReady(paneTarget, runtime, content string) bool {
	switch runtime {
	case model.RuntimeClaudeCode, "":
		return isPromptReady(content)
	default:
		cmd, err := pm.paneIO.GetPaneCurrentCommand(paneTarget)
		if err != nil {
			pm.log(logLevelDebug, "is_agent_ready current_command_failed pane=%s runtime=%s error=%v",
				paneTarget, runtime, err)
			return false
		}
		if pm.paneIO.IsShellCommand(cmd) {
			return false
		}
		return lastNonBlankLine(content) != "<empty>"
	}
}

// waitStable confirms pane content is stable over StableCheckRounds consecutive
// rounds of hash comparison, then verifies the prompt is ready.
// Worst-case duration: StableCheckRounds x IdleStableSec (default 1 x 5s = ~5s).
//
// softPromptCheck controls how prompt detection failure is handled:
//   - true:  log a warning and proceed (safe when caller runs detectBusyWithRetry afterwards)
//   - false: return an error (required when no subsequent busy detection exists)
func (pm *ClaudeProcessManager) waitStable(ctx context.Context, paneTarget string, softPromptCheck bool) error {
	runtime := pm.paneRuntime(paneTarget)
	for round := 0; round < pm.execCfg.StableCheckRounds; round++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait_stable cancelled before round %d: %w", round, err)
		}

		content1, err := pm.paneIO.CapturePaneJoined(paneTarget, pm.execCfg.PromptReadyLines)
		if err != nil {
			return fmt.Errorf("%w for stability round %d: %w", ErrCapturePane, round, err)
		}
		h1 := contentHash(content1)

		if err := sleepCtx(ctx, time.Duration(pm.config.IdleStableSec)*time.Second); err != nil {
			return fmt.Errorf("wait_stable sleep cancelled (round %d): %w", round, err)
		}

		content2, err := pm.paneIO.CapturePaneJoined(paneTarget, pm.execCfg.PromptReadyLines)
		if err != nil {
			return fmt.Errorf("%w for stability round %d: %w", ErrCapturePane, round, err)
		}
		h2 := contentHash(content2)

		if h1 != h2 {
			return fmt.Errorf("%w after %ds (round %d)", ErrNotStable, pm.config.IdleStableSec, round)
		}
		pm.log(logLevelDebug, "wait_stable round=%d passed", round)
	}

	// Verify prompt is ready after stability confirmed.
	// Uses CapturePane (no -J) to preserve line boundaries for prompt detection.
	finalContent, err := pm.paneIO.CapturePane(paneTarget, pm.execCfg.PromptReadyLines)
	if err != nil {
		if softPromptCheck {
			pm.log(logLevelWarn, "wait_stable prompt_check capture error=%v (non-fatal, soft mode)", err)
			return nil
		}
		return fmt.Errorf("%w for prompt check: %w", ErrCapturePane, err)
	}
	if !pm.isAgentReady(paneTarget, runtime, finalContent) {
		if softPromptCheck {
			pm.log(logLevelInfo, "wait_stable prompt_not_detected runtime=%s pane=%s last_line=%q (proceeding -- detectBusy will guard delivery)",
				runtime, paneTarget, lastNonBlankLine(finalContent))
			return nil
		}
		return fmt.Errorf("pane stable but %w (runtime=%s, last line: %q)", ErrNoPrompt, runtime, lastNonBlankLine(finalContent))
	}
	pm.log(logLevelDebug, "wait_stable prompt confirmed runtime=%s", runtime)
	return nil
}

// waitReady polls until the pane shows a Claude Code prompt, indicating readiness.
// It uses WaitReadyIntervalSec and WaitReadyMaxRetries from config for timing.
// Worst-case duration: (WaitReadyMaxRetries+1) x WaitReadyIntervalSec (default 16 x 2s = 32s).
//
// If prompt detection fails after all retries, the function logs at INFO level
// and returns nil (proceeds) instead of blocking dispatch. The caller's
// subsequent detectBusyWithRetry provides a safety net against delivering
// to a busy agent.
func (pm *ClaudeProcessManager) waitReady(ctx context.Context, paneTarget string) error {
	ready, err := pm.waitReadyCore(ctx, paneTarget)
	if err != nil {
		return err
	}
	if !ready {
		// Fallback: prompt not detected, but proceed with a warning.
		// The subsequent detectBusyWithRetry() will catch if the agent is actually busy.
		pm.log(logLevelInfo, "wait_ready prompt_fallback pane=%s: prompt not detected after retries, proceeding (detectBusy will guard)",
			paneTarget)
	}
	return nil
}

// waitReadyStrict is like waitReady but fail-closed: returns an error if the
// prompt is not detected after all retries (instead of soft-proceeding).
// Used after re-launching the agent runtime where we must confirm the process started.
func (pm *ClaudeProcessManager) waitReadyStrict(ctx context.Context, paneTarget string) error {
	ready, err := pm.waitReadyCore(ctx, paneTarget)
	if err != nil {
		return err
	}
	if !ready {
		runtime := pm.paneRuntime(paneTarget)
		return fmt.Errorf("waitReadyStrict: runtime=%s %w after %d attempts", runtime, ErrPromptNotDetected, pm.config.WaitReadyMaxRetries+1)
	}
	return nil
}

// waitReadyCore is the shared retry loop for agent-runtime readiness detection.
// Runtime is resolved once per call from the @runtime pane user variable; the
// per-runtime readiness check (claude-code prompt glyph vs non-shell + rendered
// content for codex/gemini) is implemented in isAgentReady.
//
// Returns (true, nil) if ready was detected, (false, nil) if retries were
// exhausted without detection, or (false, err) on hard failures (context
// cancellation, persistent capture errors).
func (pm *ClaudeProcessManager) waitReadyCore(ctx context.Context, paneTarget string) (bool, error) {
	maxRetries := pm.config.WaitReadyMaxRetries
	interval := time.Duration(pm.config.WaitReadyIntervalSec) * time.Second
	runtime := pm.paneRuntime(paneTarget)

	for i := 0; i <= maxRetries; i++ {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("waitReadyCore cancelled at attempt %d: %w", i, err)
		}

		content, err := pm.paneIO.CapturePane(paneTarget, pm.execCfg.PromptReadyLines)
		if err != nil {
			pm.log(logLevelDebug, "waitReadyCore capture error=%v attempt=%d runtime=%s", err, i, runtime)
			if i < maxRetries {
				if err := sleepCtx(ctx, interval); err != nil {
					return false, fmt.Errorf("waitReadyCore sleep cancelled: %w", err)
				}
				continue
			}
			return false, fmt.Errorf("waitReadyCore: %w failed after %d attempts: %w", ErrCapturePane, i+1, err)
		}

		if pm.isAgentReady(paneTarget, runtime, content) {
			pm.log(logLevelDebug, "waitReadyCore ready detected attempt=%d runtime=%s", i, runtime)
			return true, nil
		}

		if i < maxRetries {
			pm.log(logLevelDebug, "waitReadyCore not ready attempt=%d/%d runtime=%s", i, maxRetries, runtime)
			if err := sleepCtx(ctx, interval); err != nil {
				return false, fmt.Errorf("waitReadyCore sleep cancelled: %w", err)
			}
		}
	}

	return false, nil
}

func (pm *ClaudeProcessManager) log(level logLevel, format string, args ...any) {
	logf(pm.logger, pm.logLevel, level, "agent_executor", format, args...)
}
