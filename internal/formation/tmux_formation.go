package formation

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
)

// createFormation creates the tmux session with orchestrator, planner, and worker windows.
// On partial failure, it rolls back by killing the tmux session.
func createFormation(cfg model.Config) (retErr error) {
	// Kill existing session if any
	if tmux.SessionExists() {
		fmt.Println("[debug] createFormation: killing pre-existing session before creation")
		if err := tmux.KillSession(); err != nil {
			return fmt.Errorf("kill existing session: %w", err)
		}
	}

	// Window 0: orchestrator
	if err := tmux.CreateSession("orchestrator"); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	// Rollback: if any subsequent step fails, destroy the partially-created session
	defer func() {
		if retErr != nil {
			log.Printf("WARN createFormation: rolling back due to error: %v", retErr)
			if tmux.SessionExists() {
				if killErr := tmux.KillSession(); killErr != nil {
					log.Printf("WARN createFormation: rollback kill session failed: %v", killErr)
				}
			}
		}
	}()

	fmt.Println("[debug] createFormation: applying session hardening options")

	// Harden server: prevent tmux server from exiting when the last session is destroyed.
	if err := tmux.SetServerOption("exit-empty", "off"); err != nil {
		return fmt.Errorf("set exit-empty: %w", err)
	}

	// Defense in depth: explicitly disable exit-unattached.
	if err := tmux.SetServerOption("exit-unattached", "off"); err != nil {
		return fmt.Errorf("set exit-unattached: %w", err)
	}

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

	// Wait for each pane's shell to be ready before sending commands
	for _, pane := range allPanes {
		paneCtx, paneCancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := waitForShellReady(paneCtx, pane)
		paneCancel()
		if err != nil {
			return fmt.Errorf("pane %s shell not ready: %w", pane, err)
		}
	}

	for _, pane := range allPanes {
		if err := tmux.SendCommand(pane, "maestro agent launch"); err != nil {
			return fmt.Errorf("launch agent in %s: %w", pane, err)
		}
	}

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
			fmt.Fprintf(os.Stderr, "Warning: GetPaneCurrentCommand failed for pane %s (attempt %d/%d): %v\n",
				pane, consecutiveErrors, maxConsecutiveErrors, err)
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
		// Worker: boost overrides all, then per-worker override, then default
		if cfg.Agents.Workers.Boost {
			return "opus"
		}
		if m, ok := cfg.Agents.Workers.Models[agentID]; ok {
			return m
		}
		if m := cfg.Agents.Workers.DefaultModel; m != "" {
			return m
		}
	}
	return "sonnet"
}
