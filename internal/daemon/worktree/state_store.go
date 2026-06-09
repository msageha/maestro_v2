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

// maxWorktreeStateBytes is the maximum worktree state file size (1MB).
// Why: 1MB accommodates state files with hundreds of workers per command
// while protecting against corrupted or adversarial files consuming unbounded memory.
const maxWorktreeStateBytes = 1 << 20

func (wm *Manager) saveState(commandID string, state *model.WorktreeCommandState) error {
	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	if err := os.MkdirAll(stateDir, 0750); err != nil {
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
	data, err := os.ReadFile(statePath) //nolint:gosec // statePath is derived from maestroDir + commandID; caller controls path
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxWorktreeStateBytes {
		return nil, fmt.Errorf("worktree state file too large (%d bytes > %d max)", len(data), maxWorktreeStateBytes)
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
		wm.Log(core.LogLevelError, "invalid_worktree_transition worker=%s from=%s to=%s error=%v",
			ws.WorkerID, ws.Status, newStatus, err)
		return fmt.Errorf("worker %s: %w", ws.WorkerID, err)
	}
	// Entering conflict from a clean (non-conflict, non-resolving) state marks a
	// FRESH conflict episode — e.g. a later phase re-merge after a prior conflict
	// was escalated and resolved. Reset the per-episode conflict bookkeeping:
	//   - ConflictEscalated → false so R7 can escalate the new conflict (a stale
	//     true would suppress it forever).
	//   - ConflictResolutionAttempts → 0 so the new episode gets its own
	//     resolution budget instead of inheriting an exhausted count and
	//     escalating immediately without attempting resolution.
	// Transitions WITHIN the conflict lifecycle (conflict<->resolving) keep both,
	// so the SAME episode is neither re-escalated on every scan nor given a fresh
	// budget on each Pass-1 stale-resolving reset.
	if newStatus == model.WorktreeStatusConflict &&
		ws.Status != model.WorktreeStatusConflict &&
		ws.Status != model.WorktreeStatusResolving {
		ws.ConflictEscalated = false
		ws.ConflictResolutionAttempts = 0
	}
	ws.Status = newStatus
	ws.UpdatedAt = now
	return nil
}

// setIntegrationStatus validates and applies a status transition for the integration branch.
func (wm *Manager) setIntegrationStatus(state *model.WorktreeCommandState, newStatus model.IntegrationStatus, now string) error {
	if err := model.ValidateIntegrationTransition(state.Integration.Status, newStatus); err != nil {
		wm.Log(core.LogLevelError, "invalid_integration_transition command=%s from=%s to=%s error=%v",
			state.CommandID, state.Integration.Status, newStatus, err)
		return fmt.Errorf("integration %s: %w", state.CommandID, err)
	}
	state.Integration.Status = newStatus
	state.Integration.UpdatedAt = now
	return nil
}
