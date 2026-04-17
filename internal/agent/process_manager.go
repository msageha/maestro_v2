package agent

import (
	"context"
	"fmt"
	"log"
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
}

func newClaudeProcessManager(paneIO PaneIO, paneState *paneStateManager, cfg *model.WatcherConfig, execCfg ExecutorConfig, logger *log.Logger, ll logLevel) *ClaudeProcessManager {
	return &ClaudeProcessManager{
		paneIO:    paneIO,
		paneState: paneState,
		execCfg:   execCfg,
		config:    cfg,
		logger:    logger,
		logLevel:  ll,
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

	// Re-launch Claude
	if sendErr := pm.paneIO.SendCommand(paneTarget, "maestro agent launch"); sendErr != nil {
		return fmt.Errorf("%w: %w", ErrRelaunch, sendErr)
	}

	// Wait for Claude prompt readiness (fail-closed)
	launchCtx, cancel := context.WithTimeout(ctx, pm.execCfg.ClaudeLaunchTimeout)
	defer cancel()
	if waitErr := pm.waitReadyStrict(launchCtx, paneTarget); waitErr != nil {
		return fmt.Errorf("%w after re-launch: %w", ErrWaitClaudeReady, waitErr)
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
	if currentCWD == workingDir {
		pm.log(logLevelDebug, "working_dir unchanged cwd=%s", workingDir)
		return nil
	}

	pm.log(logLevelInfo, "working_dir_change old=%q new=%q", currentCWD, workingDir)

	// Step 1: Kill the current pane process and respawn a fresh shell in the
	// target working directory.
	if err := pm.paneIO.RespawnPane(paneTarget, workingDir); err != nil {
		return fmt.Errorf("%w: %w", ErrRespawnPane, err)
	}

	// Step 2: Wait for the fresh shell to be ready
	if err := pm.waitForShell(ctx, paneTarget); err != nil {
		return fmt.Errorf("wait for shell after respawn: %w", err)
	}

	// Step 3: Reset clear_ready state before re-launching Claude.
	// RespawnPane changes the pane PID, so clear_ready_pid is now stale.
	// Reset early (matching ensureClaudeRunning) to prevent DetectProcessRestart
	// from seeing a stale PID between here and the launch.
	if err := pm.paneState.ResetClearReady(paneTarget); err != nil {
		pm.log(logLevelError, "reset_clear_ready_after_respawn_failed error=%v", err)
		return fmt.Errorf("reset clear ready after respawn: %w", err)
	}

	// Step 4: Re-launch Claude
	if err := pm.paneIO.SendCommand(paneTarget, "maestro agent launch"); err != nil {
		return fmt.Errorf("re-launch claude: %w", err)
	}

	// Step 5: Wait for Claude prompt readiness (fail-closed: error on timeout)
	// Use a generous timeout since Claude startup can take a few seconds.
	launchCtx, cancel := context.WithTimeout(ctx, pm.execCfg.ClaudeLaunchTimeout)
	defer cancel()
	if err := pm.waitReadyStrict(launchCtx, paneTarget); err != nil {
		return fmt.Errorf("wait for claude ready: %w", err)
	}

	// Step 6: Update CWD tracking (clear_ready was already reset in Step 3)
	if err := pm.paneState.SetCWD(paneTarget, workingDir); err != nil {
		pm.log(logLevelWarn, "set_cwd_failed cwd=%s error=%v", workingDir, err)
	}

	pm.log(logLevelInfo, "working_dir_changed cwd=%s", workingDir)
	return nil
}

// waitForShell polls a tmux pane until its current command is a known shell,
// indicating the pane has returned to the shell prompt (e.g., after Claude exits).
func (pm *ClaudeProcessManager) waitForShell(ctx context.Context, paneTarget string) error {
	const maxAttempts = 30   // 30 x 500ms = 15s max
	const pollInterval = 500 // milliseconds
	const maxConsecutiveErrors = 5

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

// waitStable confirms pane content is stable over StableCheckRounds consecutive
// rounds of hash comparison, then verifies the prompt is ready.
// Worst-case duration: StableCheckRounds x IdleStableSec (default 1 x 5s = ~5s).
//
// softPromptCheck controls how prompt detection failure is handled:
//   - true:  log a warning and proceed (safe when caller runs detectBusyWithRetry afterwards)
//   - false: return an error (required when no subsequent busy detection exists)
func (pm *ClaudeProcessManager) waitStable(ctx context.Context, paneTarget string, softPromptCheck bool) error {
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
	if !isPromptReady(finalContent) {
		if softPromptCheck {
			pm.log(logLevelInfo, "wait_stable prompt_not_detected pane=%s last_line=%q (proceeding -- detectBusy will guard delivery)",
				paneTarget, lastNonBlankLine(finalContent))
			return nil
		}
		return fmt.Errorf("pane stable but %w (last line: %q)", ErrNoPrompt, lastNonBlankLine(finalContent))
	}
	pm.log(logLevelDebug, "wait_stable prompt confirmed")
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
// Used after re-launching Claude where we must confirm the process started.
func (pm *ClaudeProcessManager) waitReadyStrict(ctx context.Context, paneTarget string) error {
	ready, err := pm.waitReadyCore(ctx, paneTarget)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("waitReadyStrict: Claude %w after %d attempts", ErrPromptNotDetected, pm.config.WaitReadyMaxRetries+1)
	}
	return nil
}

// waitReadyCore is the shared retry loop for prompt detection. It returns
// (true, nil) if the prompt was detected, (false, nil) if retries were
// exhausted without detection, or (false, err) on hard failures (context
// cancellation, persistent capture errors).
func (pm *ClaudeProcessManager) waitReadyCore(ctx context.Context, paneTarget string) (bool, error) {
	maxRetries := pm.config.WaitReadyMaxRetries
	interval := time.Duration(pm.config.WaitReadyIntervalSec) * time.Second

	for i := 0; i <= maxRetries; i++ {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("waitReadyCore cancelled at attempt %d: %w", i, err)
		}

		content, err := pm.paneIO.CapturePane(paneTarget, pm.execCfg.PromptReadyLines)
		if err != nil {
			pm.log(logLevelDebug, "waitReadyCore capture error=%v attempt=%d", err, i)
			if i < maxRetries {
				if err := sleepCtx(ctx, interval); err != nil {
					return false, fmt.Errorf("waitReadyCore sleep cancelled: %w", err)
				}
				continue
			}
			return false, fmt.Errorf("waitReadyCore: %w failed after %d attempts: %w", ErrCapturePane, i+1, err)
		}

		if isPromptReady(content) {
			pm.log(logLevelDebug, "waitReadyCore prompt detected attempt=%d", i)
			return true, nil
		}

		if i < maxRetries {
			pm.log(logLevelDebug, "waitReadyCore not ready attempt=%d/%d", i, maxRetries)
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
