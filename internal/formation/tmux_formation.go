package formation

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
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

// trustDialogDelay is the time to wait for Claude Code's workspace trust dialog
// to appear before sending the auto-accept keystroke. Claude CLI startup
// typically takes 1-3 seconds; 4 seconds provides margin for slower machines.
const trustDialogDelay = 4 * time.Second

// autoAcceptTrustDialog sends an Enter keystroke to each pane after a brief
// delay to automatically accept Claude Code's workspace trust dialog.
// If the dialog is not present (project already trusted), the Enter is
// harmless — Claude Code ignores empty input in interactive mode.
func autoAcceptTrustDialog(panes []string) {
	go func() {
		time.Sleep(trustDialogDelay)
		for _, pane := range panes {
			if err := tmux.SendKeys(pane, "Enter"); err != nil {
				slog.Debug("autoAcceptTrustDialog: send Enter failed", "pane", pane, "error", err)
			}
		}
	}()
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

