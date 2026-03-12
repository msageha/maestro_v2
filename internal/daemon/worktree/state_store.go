package worktree

import (
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

const maxWorktreeStateBytes = 1 << 20 // 1MB

func (wm *Manager) saveState(commandID string, state *model.WorktreeCommandState) error {
	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	statePath := filepath.Join(stateDir, commandID+".yaml")
	return yamlutil.AtomicWrite(statePath, state)
}

// loadState loads state without acquiring mu. Callers that need thread-safety
// must hold wm.mu themselves. Public read-only methods like GetState and
// GetWorkerPath acquire mu before calling this.
func (wm *Manager) loadState(commandID string) (*model.WorktreeCommandState, error) {
	return wm.loadStateUnlocked(commandID)
}

func (wm *Manager) loadStateUnlocked(commandID string) (*model.WorktreeCommandState, error) {
	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	fi, err := os.Stat(statePath)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxWorktreeStateBytes {
		return nil, fmt.Errorf("worktree state file too large (%d bytes > %d max)", fi.Size(), maxWorktreeStateBytes)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}
	var state model.WorktreeCommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse worktree state: %w", err)
	}
	return &state, nil
}

// setWorkerStatus validates and applies a status transition for a worker.
func (wm *Manager) setWorkerStatus(ws *model.WorktreeState, newStatus model.WorktreeStatus, now string) error {
	if err := model.ValidateWorktreeTransition(ws.Status, newStatus); err != nil {
		wm.log(core.LogLevelError, "invalid_worktree_transition worker=%s from=%s to=%s error=%v",
			ws.WorkerID, ws.Status, newStatus, err)
		return fmt.Errorf("worker %s: %w", ws.WorkerID, err)
	}
	ws.Status = newStatus
	ws.UpdatedAt = now
	return nil
}

// setIntegrationStatus validates and applies a status transition for the integration branch.
func (wm *Manager) setIntegrationStatus(state *model.WorktreeCommandState, newStatus model.IntegrationStatus, now string) error {
	if err := model.ValidateIntegrationTransition(state.Integration.Status, newStatus); err != nil {
		wm.log(core.LogLevelError, "invalid_integration_transition command=%s from=%s to=%s error=%v",
			state.CommandID, state.Integration.Status, newStatus, err)
		return fmt.Errorf("integration %s: %w", state.CommandID, err)
	}
	state.Integration.Status = newStatus
	state.Integration.UpdatedAt = now
	return nil
}
