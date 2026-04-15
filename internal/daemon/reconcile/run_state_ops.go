package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// loadState loads and parses a command state YAML file.
func (r *Run) loadState(path string) (*model.CommandState, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application state directory
	if err != nil {
		return nil, err
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// loadWorktreeState loads and parses a worktree command state YAML file.
func (r *Run) loadWorktreeState(path string) (*model.WorktreeCommandState, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application state directory
	if err != nil {
		return nil, err
	}
	var state model.WorktreeCommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// loadCommandResultFile loads results/planner.yaml.
func (r *Run) loadCommandResultFile(path string) (*model.CommandResultFile, error) {
	var rf model.CommandResultFile
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application results directory
	if err != nil {
		if os.IsNotExist(err) {
			return &rf, nil
		}
		return nil, err
	}
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		return nil, err
	}
	return &rf, nil
}

// updateLastReconciledAt updates last_reconciled_at on a state file.
func (r *Run) updateLastReconciledAt(commandID string) {
	statePath := filepath.Join(r.Deps.MaestroDir, "state", "commands", commandID+".yaml")

	r.Deps.LockMap.WithLock("state:"+commandID, func() {
		state, err := r.loadState(statePath)
		if err != nil {
			return
		}

		now := r.Deps.Clock.Now().UTC().Format(time.RFC3339)
		state.LastReconciledAt = &now
		state.UpdatedAt = now
		if err := yamlutil.AtomicWrite(statePath, state); err != nil {
			r.Log(core.LogLevelError, "update_last_reconciled command=%s error=%v", commandID, err)
		}
	})
}

// quarantineCommandResult removes a specific result from results/planner.yaml and writes it to quarantine/.
func (r *Run) quarantineCommandResult(resultPath string, result model.CommandResult) error {
	quarantineDir := filepath.Join(r.Deps.MaestroDir, "quarantine")
	if err := os.MkdirAll(quarantineDir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for a quarantine directory
		return fmt.Errorf("create quarantine dir: %w", err)
	}

	quarantineFile := filepath.Join(quarantineDir,
		fmt.Sprintf("res_%s_%s.yaml", r.Deps.Clock.Now().UTC().Format("20060102T150405Z"), result.ID))

	if err := yamlutil.AtomicWrite(quarantineFile, result); err != nil {
		return fmt.Errorf("write quarantine file: %w", err)
	}

	r.Deps.LockMap.Lock("result:planner")
	defer r.Deps.LockMap.Unlock("result:planner")

	rf, err := r.loadCommandResultFile(resultPath)
	if err != nil {
		return fmt.Errorf("reload result file: %w", err)
	}

	filtered := make([]model.CommandResult, 0, len(rf.Results))
	for _, res := range rf.Results {
		if res.ID != result.ID {
			filtered = append(filtered, res)
		}
	}
	rf.Results = filtered

	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		return fmt.Errorf("write result file: %w", err)
	}

	return nil
}
