package formation

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
)

// createFormation creates the tmux session with orchestrator, planner, and worker windows.
// On partial failure, it rolls back by killing the tmux session.
func createFormation(cfg model.Config) (retErr error) {
	// Kill existing session if any
	if tmux.SessionExists() {
		slog.Debug("createFormation: killing pre-existing session before creation")
		if err := tmux.KillSession(); err != nil {
			return fmt.Errorf("kill existing session: %w", err)
		}
	}

	// Create the session and apply server-level hardening atomically in a
	// single tmux invocation. Chaining avoids both:
	//   - the "empty server exits before we can disable exit-empty" race when
	//     set-option is issued before any session exists, and
	//   - the "user's tmux.conf exit-unattached=on destroys the session
	//     between new-session and the follow-up set-option" race.
	slog.Debug("createFormation: creating session with server hardening (atomic)")

	if err := tmux.CreateSessionWithServerOptions("orchestrator", map[string]string{
		"exit-empty":      "off",
		"exit-unattached": "off",
	}); err != nil {
		return fmt.Errorf("create session with server options: %w", err)
	}

	// Rollback: if any subsequent step fails, destroy the partially-created session
	defer func() {
		if retErr != nil {
			slog.Warn("createFormation: rolling back due to error", "error", retErr)
			if tmux.SessionExists() {
				if killErr := tmux.KillSession(); killErr != nil {
					slog.Warn("createFormation: rollback kill session failed", "error", killErr)
				}
			}
		}
	}()

	// Harden session: prevent user-level tmux config from destroying the detached session.
	if err := tmux.SetSessionOption("destroy-unattached", "off"); err != nil {
		return fmt.Errorf("set destroy-unattached: %w", err)
	}

	// Set remain-on-exit for orchestrator window.
	orchWindow := fmt.Sprintf("=%s:0", tmux.GetSessionName())
	if err := tmux.SetWindowOption(orchWindow, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("set remain-on-exit for orchestrator: %w", err)
	}

	orchPane := fmt.Sprintf("=%s:0.0", tmux.GetSessionName())
	if err := setAgentVars(orchPane, "orchestrator", "orchestrator", resolveModel(cfg, "orchestrator")); err != nil {
		return err
	}

	// Window 1: planner
	if err := tmux.CreateWindow("planner"); err != nil {
		return fmt.Errorf("create planner window: %w", err)
	}

	// Set remain-on-exit for planner window
	plannerWindow := fmt.Sprintf("=%s:1", tmux.GetSessionName())
	if err := tmux.SetWindowOption(plannerWindow, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("set remain-on-exit for planner: %w", err)
	}

	plannerPane := fmt.Sprintf("=%s:1.0", tmux.GetSessionName())
	if err := setAgentVars(plannerPane, "planner", "planner", resolveModel(cfg, "planner")); err != nil {
		return err
	}

	// Window 2: workers
	workerCount := max(cfg.Agents.Workers.Count, 1)

	if err := tmux.CreateWindow("workers"); err != nil {
		return fmt.Errorf("create workers window: %w", err)
	}

	// Set remain-on-exit for workers window
	workerWindow := fmt.Sprintf("=%s:2", tmux.GetSessionName())
	if err := tmux.SetWindowOption(workerWindow, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("set remain-on-exit for workers: %w", err)
	}

	// Limit scrollback buffer for worker panes to reduce memory usage
	if err := tmux.SetWindowOption(workerWindow, "history-limit", "500"); err != nil {
		return fmt.Errorf("set history-limit for workers: %w", err)
	}

	panes, err := tmux.SetupWorkerGrid(workerWindow, workerCount)
	if err != nil {
		return fmt.Errorf("setup worker grid: %w", err)
	}

	for i, pane := range panes {
		agentID := fmt.Sprintf("worker%d", i+1)
		workerModel := resolveModel(cfg, agentID)
		if err := setAgentVars(pane, agentID, "worker", workerModel); err != nil {
			return err
		}
	}

	// Launch agents in each pane
	allPanes := make([]string, 0, 2+cfg.Agents.Workers.Count)
	allPanes = append(allPanes, orchPane, plannerPane)
	allPanes = append(allPanes, panes...)

	// Wait for each pane's shell to be ready before sending commands.
	// Orchestrator + planner are required; worker panes are best-effort
	// with automatic cleanup on failure.
	requiredPanes := allPanes[:2] // orchestrator + planner
	optionalPanes := allPanes[2:] // workers
	timeout := shellReadyTimeout(cfg)
	readyPanes, err := preparePanes(requiredPanes, optionalPanes, timeout, waitForShellReady, killPaneByTarget)
	if err != nil {
		return err
	}

	for _, pane := range readyPanes {
		if err := tmux.SendCommand(pane, agent.LaunchCommand); err != nil {
			return fmt.Errorf("launch agent in %s: %w", pane, err)
		}
	}

	// Auto-accept Claude Code workspace trust dialog.
	// Claude Code does not expose an env var or CLI flag to bypass the trust
	// dialog in interactive mode. --dangerously-skip-permissions only covers
	// per-tool permission checks, not project-level trust. Sending Enter after
	// a brief delay accepts the dialog if it appears; if the project is already
	// trusted or the dialog has not appeared, the Enter is harmless (empty input
	// is ignored by Claude Code's interactive prompt).
	autoAcceptTrustDialog(readyPanes)

	// Select orchestrator window so `tmux attach` lands there
	if err := tmux.SelectWindow(fmt.Sprintf("=%s:0", tmux.GetSessionName())); err != nil {
		return fmt.Errorf("select orchestrator window: %w", err)
	}

	return nil
}

// waitForShellReady polls a tmux pane until its current command is a known
// shell, indicating the pane is ready to receive input.
func waitForShellReady(ctx context.Context, pane string) error {
	const maxConsecutiveErrors = 5
	consecutiveErrors := 0
	var lastErr error

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waitForShellReady cancelled: %w", err)
		}
		cmd, err := tmux.GetPaneCurrentCommand(pane)
		if err != nil {
			consecutiveErrors++
			lastErr = err
			slog.Warn("GetPaneCurrentCommand failed", "pane", pane, "attempt", consecutiveErrors, "max_attempts", maxConsecutiveErrors, "error", err)
			if consecutiveErrors >= maxConsecutiveErrors {
				return fmt.Errorf("waitForShellReady: %d consecutive errors, last: %w", consecutiveErrors, lastErr)
			}
		} else {
			consecutiveErrors = 0
			if tmux.IsShellCommand(cmd) {
				return nil
			}
		}
		t := time.NewTimer(100 * time.Millisecond)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("waitForShellReady cancelled: %w", ctx.Err())
		}
	}
}

func setAgentVars(pane, agentID, role, agentModel string) error {
	vars := map[string]string{
		"agent_id": agentID,
		"role":     role,
		"model":    agentModel,
		"status":   "idle",
	}
	for k, v := range vars {
		if err := tmux.SetUserVar(pane, k, v); err != nil {
			return fmt.Errorf("set @%s on %s: %w", k, pane, err)
		}
	}
	return nil
}

const defaultShellReadyTimeoutSec = 10

// shellReadyTimeout returns the configured shell readiness timeout or the default (10s).
func shellReadyTimeout(cfg model.Config) time.Duration {
	if cfg.Watcher.ShellReadyTimeoutSec > 0 {
		return time.Duration(cfg.Watcher.ShellReadyTimeoutSec) * time.Second
	}
	return defaultShellReadyTimeoutSec * time.Second
}

// killPaneByTarget kills a tmux pane by its target identifier.
// Best-effort: errors are logged but not propagated.
// Package-level variable for test overriding.
var killPaneByTarget = func(pane string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "tmux", "kill-pane", "-t", pane).Run(); err != nil { //nolint:gosec // "tmux" is a fixed command; pane target is derived from internal formation setup
		slog.Warn("kill-pane failed", "pane", pane, "error", err)
	}
}

// preparePanes waits for panes to become ready with automatic cleanup of failed panes.
// Required panes (orchestrator + planner) must all succeed; optional panes (workers)
// are killed on failure and excluded from the result. Returns the list of ready panes.
func preparePanes(
	requiredPanes []string,
	optionalPanes []string,
	timeout time.Duration,
	waitFn func(ctx context.Context, pane string) error,
	killFn func(pane string),
) ([]string, error) {
	readyPanes := make([]string, 0, len(requiredPanes)+len(optionalPanes))

	// Required panes must all succeed
	for _, pane := range requiredPanes {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := waitFn(ctx, pane)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("required pane %s not ready: %w", pane, err)
		}
		readyPanes = append(readyPanes, pane)
	}

	// Optional panes: cleanup on failure, continue with remaining
	var failedCount int
	for _, pane := range optionalPanes {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := waitFn(ctx, pane)
		cancel()
		if err != nil {
			slog.Warn("worker pane not ready, cleaning up", "pane", pane, "error", err)
			killFn(pane)
			failedCount++
			continue
		}
		readyPanes = append(readyPanes, pane)
	}

	if failedCount > 0 {
		slog.Warn("formation started with partial workers",
			"ready_workers", len(readyPanes)-len(requiredPanes),
			"failed_workers", failedCount)
	}

	return readyPanes, nil
}

// trustDialogPollInterval is how frequently each pane is checked for Claude
// Code's workspace trust dialog.
const trustDialogPollInterval = 500 * time.Millisecond

// trustDialogPollTimeout is the maximum time we keep polling a pane looking
// for the trust dialog. The dialog can take well over 30s to appear on first
// launch (Claude Code binary initialization, network auth, model handshake),
// and a short timeout caused the automation to silently give up — leaving
// every pane stuck on "Is this a project you created or one you trust?"
// until the operator hit Enter manually. Five minutes covers slow starts
// while still bounding the watcher goroutine's lifetime.
const trustDialogPollTimeout = 5 * time.Minute

// trustDialogCaptureLines controls how many lines the trust-dialog poller
// pulls from each pane.
//
// This must be 0 (i.e. no `-S` flag to capture-pane). Claude Code renders
// its workspace trust dialog on the terminal's *alternate screen*, and an
// alt-screen has no scrollback: asking tmux for history lines via `-S -N`
// returns content from the underlying primary screen instead, completely
// missing the dialog that is actually visible to the operator.
//
// Older versions of this file set this to 80, then 200, on the theory that
// pulling more history would catch the dialog if it had scrolled. In
// practice the larger window just pulled more of the pre-dialog shell
// transcript and the dialog was never detected — the poll loop ran for the
// full 5 minutes while the dialog sat on screen. Keeping this at 0 captures
// exactly the visible pane, which is the alt-screen when the dialog is up.
const trustDialogCaptureLines = 0

// trustDialogMarkers are substrings that reliably identify Claude Code's
// workspace trust dialog. Across CLI versions the dialog phrasing has
// shifted — "Do you trust the files in this folder?" on older builds,
// "Is this a project you created or one you trust?" / "Yes, I trust this
// folder" on newer ones. Matching any of these substrings (case-insensitive)
// is treated as positive detection. Markers are intentionally short so they
// survive further wording tweaks; each one is specific enough that it will
// not false-match idle REPL output or shell prompts.
//
// We also match the two short menu options ("yes, proceed" / "no, exit")
// because on narrow panes the long question line wraps in a way that — even
// after -J joining — can get split across our capture boundary, but the
// options almost always render on their own short lines near the prompt.
var trustDialogMarkers = []string{
	"trust the files",   // "Do you trust the files in this folder?"
	"do you trust",      // older variant
	"one you trust",     // "Is this a project you created or one you trust?"
	"trust this folder", // "Yes, I trust this folder"
	"yes, proceed",      // menu option on newer builds
	"no, exit",          // menu option on newer builds
}

// trustDialogMaxEnterSends is the number of Enter keystrokes the auto-accept
// loop sends in "fast retry" mode (one per trustDialogPollInterval = 500 ms).
// After this many attempts the loop switches to "slow retry" mode
// (trustDialogSlowRetryInterval between attempts) instead of giving up.
//
// Background: Enter keystrokes sent before Claude Code's TUI has finished
// switching the terminal into raw/alt-screen mode and reading stdin are
// silently discarded. On slow-starting panes (Orchestrator, workers) all fast
// retries can be consumed before the TUI becomes stdin-ready, leaving the
// dialog on screen. Switching to slow retries instead of abandoning the pane
// means the loop keeps trying for the full trustDialogPollTimeout window,
// so an Enter eventually lands once the TUI is ready.
const trustDialogMaxEnterSends = 8

// trustDialogSlowRetryInterval is the minimum gap between Enter keystrokes
// once the fast-retry budget (trustDialogMaxEnterSends) is exhausted. Slower
// sends reduce log noise while still retrying often enough that the dialog is
// cleared within a few seconds of Claude Code becoming stdin-ready.
const trustDialogSlowRetryInterval = 3 * time.Second

// trustDialogPaneState tracks per-pane progress for autoAcceptTrustDialog.
// sawDialog distinguishes "never saw the dialog" (project already trusted)
// from "dialog appeared and we attempted Enter" — only the latter case cares
// about whether the dialog has now cleared.
type trustDialogPaneState struct {
	sawDialog  bool
	enterSent  int
	lastSentAt time.Time
}

// autoAcceptTrustDialog watches each pane for Claude Code's workspace trust
// dialog and sends Enter until the dialog clears. Previous versions used a
// fixed 4s delay + single Enter, which failed when the dialog appeared later
// than expected (slow startup, staggered pane launches). A later iteration
// polled per-pane and deleted the pane after the first Enter, which still
// failed when the first Enter landed before Claude Code's TUI was reading
// stdin — the keystroke was lost during the switch to raw mode and the loop
// never retried.
//
// Per-pane state machine:
//   - "waiting":    dialog has not been seen yet — keep polling until timeout
//     (common "no dialog appears" case when the project is already trusted).
//   - "fast retry": dialog is currently visible — send Enter every
//     trustDialogPollInterval until trustDialogMaxEnterSends attempts.
//   - "slow retry": fast-retry budget exhausted but dialog still visible —
//     send Enter every trustDialogSlowRetryInterval. This handles the race
//     where all fast Enters land before Claude Code's TUI is stdin-ready and
//     are silently discarded; slow retries keep trying for the full
//     trustDialogPollTimeout window so the dialog clears once the TUI is ready.
//   - "accepted":   dialog was seen at some point and is now gone — stop
//     polling this pane (Enter was consumed successfully).
func autoAcceptTrustDialog(panes []string) {
	go func() {
		remaining := make(map[string]*trustDialogPaneState, len(panes))
		for _, p := range panes {
			remaining[p] = &trustDialogPaneState{}
		}

		deadline := time.Now().Add(trustDialogPollTimeout)
		for len(remaining) > 0 && time.Now().Before(deadline) {
			time.Sleep(trustDialogPollInterval)
			for pane, st := range remaining {
				// Prefer CapturePaneJoined: Claude Code's trust dialog wraps
				// its question across visual lines on narrow panes, and the
				// non-joined capture would split a marker like "one you
				// trust" into "one you" + "trust" — breaking substring
				// matching. -J joins wrapped lines so markers survive.
				//
				// trustDialogCaptureLines MUST be 0 here; see its doc comment
				// for why pulling scrollback breaks alt-screen detection.
				content, err := tmux.CapturePaneJoined(pane, trustDialogCaptureLines)
				if err != nil {
					slog.Debug("autoAcceptTrustDialog: capture-pane failed",
						"pane", pane, "error", err)
					continue
				}
				if !containsTrustDialog(content) {
					if st.sawDialog {
						// Dialog appeared earlier and is now gone — accepted.
						slog.Info("autoAcceptTrustDialog: accepted",
							"pane", pane, "enter_sent", st.enterSent)
						delete(remaining, pane)
					}
					// else: dialog never appeared; keep waiting until timeout.
					continue
				}

				st.sawDialog = true
				now := time.Now()
				if st.enterSent >= trustDialogMaxEnterSends {
					// Fast-retry budget exhausted. Switch to slow retry rather
					// than giving up: Enter keystrokes sent before Claude
					// Code's TUI is stdin-ready are silently discarded, so all
					// fast retries may be wasted on slow-starting panes. Slow
					// retries keep trying for the full trustDialogPollTimeout
					// window so the dialog is eventually cleared once the TUI
					// is ready.
					if now.Sub(st.lastSentAt) < trustDialogSlowRetryInterval {
						continue // still inside slow-retry cooldown
					}
					if st.enterSent == trustDialogMaxEnterSends {
						// Log exactly once when switching modes.
						slog.Warn("autoAcceptTrustDialog: switching to slow retry; dialog still visible",
							"pane", pane, "enter_sent", st.enterSent,
							"slow_interval", trustDialogSlowRetryInterval)
					}
				}
				if err := tmux.SendKeys(pane, "Enter"); err != nil {
					slog.Debug("autoAcceptTrustDialog: send Enter failed",
						"pane", pane, "error", err)
					continue
				}
				st.enterSent++
				st.lastSentAt = now
				slog.Debug("autoAcceptTrustDialog: sent Enter",
					"pane", pane, "attempt", st.enterSent)
			}
		}
		if len(remaining) > 0 {
			leftover := make([]string, 0, len(remaining))
			for p := range remaining {
				leftover = append(leftover, p)
			}
			// Elevated to Warn so operators notice when the automation timed
			// out. Two cases are merged here:
			//   - project already trusted (sawDialog=false): harmless, the
			//     dialog simply never appeared.
			//   - dialog appeared but did not clear (sawDialog=true, slow
			//     retry exhausted): the dialog is still on screen and the
			//     operator must hit Enter manually.
			slog.Warn("autoAcceptTrustDialog: timed out before all dialogs cleared",
				"panes", leftover, "timeout", trustDialogPollTimeout)
		}
	}()
}

// containsTrustDialog reports whether the pane content shows Claude Code's
// workspace trust dialog. Matching is case-insensitive so minor wording
// variants between Claude CLI versions do not break detection.
func containsTrustDialog(paneContent string) bool {
	lc := strings.ToLower(paneContent)
	for _, m := range trustDialogMarkers {
		if strings.Contains(lc, m) {
			return true
		}
	}
	return false
}

// resolveModel determines the model for a given agent.
func resolveModel(cfg model.Config, agentID string) string {
	switch agentID {
	case "orchestrator":
		if m := cfg.Agents.Orchestrator.Model; m != "" {
			return m
		}
	case "planner":
		if m := cfg.Agents.Planner.Model; m != "" {
			return m
		}
	default:
		return model.ResolveWorkerModel(agentID, cfg)
	}
	return "sonnet"
}

