package plan

import (
	"fmt"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon"
	"github.com/msageha/maestro_v2/internal/model"
)

// PlanStateReader implements daemon.StateReader by reading state/commands/ YAML files.
type PlanStateReader struct {
	stateManager *StateManager
}

func NewPlanStateReader(sm *StateManager) *PlanStateReader {
	return &PlanStateReader{stateManager: sm}
}

func (r *PlanStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return "", err
	}

	status, ok := state.TaskStates[taskID]
	if !ok {
		return "", fmt.Errorf("task %s not found in command %s", taskID, commandID)
	}
	return status, nil
}

func (r *PlanStateReader) GetCommandPhases(commandID string) ([]daemon.PhaseInfo, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return nil, err
	}

	if len(state.Phases) == 0 {
		return nil, nil
	}

	var phases []daemon.PhaseInfo
	for _, p := range state.Phases {
		// Convert depends_on_phases (names) to phase IDs
		phaseNameToID := make(map[string]string)
		for _, sp := range state.Phases {
			phaseNameToID[sp.Name] = sp.PhaseID
		}

		var depIDs []string
		for _, depName := range p.DependsOnPhases {
			if id, ok := phaseNameToID[depName]; ok {
				depIDs = append(depIDs, id)
			}
		}

		// Collect required task IDs for this phase
		phaseTaskSet := make(map[string]bool)
		for _, tid := range p.TaskIDs {
			phaseTaskSet[tid] = true
		}
		var requiredTaskIDs []string
		for _, tid := range state.RequiredTaskIDs {
			if phaseTaskSet[tid] {
				requiredTaskIDs = append(requiredTaskIDs, tid)
			}
		}

		isSystemCommit := state.SystemCommitTaskID != nil && phaseTaskSet[*state.SystemCommitTaskID]

		phases = append(phases, daemon.PhaseInfo{
			ID:               p.PhaseID,
			Name:             p.Name,
			Status:           p.Status,
			DependsOn:        depIDs,
			FillDeadlineAt:   p.FillDeadlineAt,
			RequiredTaskIDs:  requiredTaskIDs,
			SystemCommitTask: isSystemCommit,
		})
	}

	return phases, nil
}

func (r *PlanStateReader) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return nil, err
	}

	deps, ok := state.TaskDependencies[taskID]
	if !ok {
		return nil, nil
	}
	return deps, nil
}

func (r *PlanStateReader) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	r.stateManager.LockCommand(commandID)
	defer r.stateManager.UnlockCommand(commandID)

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for i := range state.Phases {
		if state.Phases[i].PhaseID == phaseID {
			if err := model.ValidatePhaseTransition(state.Phases[i].Status, newStatus); err != nil {
				return fmt.Errorf("phase %s in command %s: %w", phaseID, commandID, err)
			}
			state.Phases[i].Status = newStatus
			if model.IsPhaseTerminal(newStatus) {
				state.Phases[i].CompletedAt = &now
			}
			if newStatus == model.PhaseStatusActive {
				state.Phases[i].ActivatedAt = &now
			}
			if newStatus == model.PhaseStatusAwaitingFill {
				if state.Phases[i].Constraints != nil && state.Phases[i].Constraints.TimeoutMinutes > 0 {
					deadline := time.Now().UTC().Add(time.Duration(state.Phases[i].Constraints.TimeoutMinutes) * time.Minute).Format(time.RFC3339)
					state.Phases[i].FillDeadlineAt = &deadline
				}
			}
			break
		}
	}

	state.UpdatedAt = now
	return r.stateManager.SaveState(state)
}

func (r *PlanStateReader) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	r.stateManager.LockCommand(commandID)
	defer r.stateManager.UnlockCommand(commandID)

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return err
	}

	if state.TaskStates == nil {
		state.TaskStates = make(map[string]model.Status)
	}

	if currentStatus, exists := state.TaskStates[taskID]; exists {
		if err := model.ValidateTaskStateTransition(currentStatus, newStatus); err != nil {
			return fmt.Errorf("task %s in command %s: %w", taskID, commandID, err)
		}
	}

	state.TaskStates[taskID] = newStatus

	if cancelledReason != "" {
		if state.CancelledReasons == nil {
			state.CancelledReasons = make(map[string]string)
		}
		state.CancelledReasons[taskID] = cancelledReason
	}

	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return r.stateManager.SaveState(state)
}

func (r *PlanStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		if !r.stateManager.StateExists(commandID) {
			return false, daemon.ErrStateNotFound
		}
		return false, err
	}
	return state.Cancel.Requested, nil
}

func (r *PlanStateReader) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return false, false, err
	}

	if state.SystemCommitTaskID == nil || *state.SystemCommitTaskID != taskID {
		return false, false, nil
	}

	// Non-phased command: check all user tasks (except self) are terminal
	if len(state.Phases) == 0 {
		for tid, s := range state.TaskStates {
			if tid == taskID {
				continue
			}
			if !model.IsTerminal(s) {
				return true, false, nil
			}
		}
		return true, true, nil
	}

	// Phased command: check all user phases are terminal
	for _, phase := range state.Phases {
		if !model.IsPhaseTerminal(phase.Status) {
			return true, false, nil
		}
	}
	return true, true, nil
}
