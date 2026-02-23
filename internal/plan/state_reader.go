package plan

import (
	"fmt"

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
