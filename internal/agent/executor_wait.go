package agent

import (
	"context"
	"fmt"
	"time"
)

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
