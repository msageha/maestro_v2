package plan

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
)

func buildCommandState(commandID string, tasks []TaskInput, nameToID map[string]string, phases []model.Phase, now string) (*model.CommandState, error) {
	state := &model.CommandState{
		SchemaVersion:    1,
		FileType:         "state_command",
		CommandID:        commandID,
		CompletionPolicy: defaultCompletionPolicy(),
		TaskTracking: model.TaskTracking{
			TaskDependencies: make(map[string][]string),
			TaskStates:       make(map[string]model.Status),
			CancelledReasons: make(map[string]string),
			AppliedResultIDs: make(map[string]string),
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: make(map[string]string),
		},
		PhaseTracking: model.PhaseTracking{
			Phases: phases,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	var systemCommitTaskID *string

	for _, t := range tasks {
		taskID := nameToID[t.Name]

		if t.Required {
			state.RequiredTaskIDs = append(state.RequiredTaskIDs, taskID)
		} else {
			state.OptionalTaskIDs = append(state.OptionalTaskIDs, taskID)
		}

		// §2.1: tasks enter the lifecycle at `planned`. AdvanceTaskState
		// (BFS over validTaskStateTransitions) walks through ready / dispatched /
		// running / verify_pending automatically as the task progresses, so
		// downstream code that targets verify_pending or completed sees the full
		// §2.1 state path applied without per-step planner orchestration.
		state.SetTaskState(taskID, model.StatusPlanned, now)

		if len(t.BlockedBy) > 0 {
			depIDs := make([]string, 0, len(t.BlockedBy))
			for _, depName := range t.BlockedBy {
				id, ok := nameToID[depName]
				if !ok {
					return nil, fmt.Errorf("blocked_by references unknown task %q", depName)
				}
				depIDs = append(depIDs, id)
			}
			state.TaskDependencies[taskID] = depIDs
		}

		if t.Name == "__system_commit" {
			systemCommitTaskID = &taskID
		}
	}

	state.SystemCommitTaskID = systemCommitTaskID
	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)
	return state, nil
}

// buildPhaseCommandState constructs the CommandState for a phase-based submission.
func buildPhaseCommandState(opts SubmitOptions, phases []PhaseInput, phaseNameToID map[string]string,
	cpd *concretePhaseData, systemCommitTaskID *string, now string) (*model.CommandState, error) {

	state := &model.CommandState{
		SchemaVersion:    1,
		FileType:         "state_command",
		CommandID:        opts.CommandID,
		PlanVersion:      0,
		PlanStatus:       model.PlanStatusPlanning,
		CompletionPolicy: defaultCompletionPolicy(),
		TaskTracking: model.TaskTracking{
			TaskDependencies:   make(map[string][]string),
			TaskStates:         make(map[string]model.Status),
			CancelledReasons:   make(map[string]string),
			AppliedResultIDs:   make(map[string]string),
			SystemCommitTaskID: systemCommitTaskID,
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: make(map[string]string),
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Build phases
	for _, p := range phases {
		phaseModel := model.Phase{
			PhaseID:         phaseNameToID[p.Name],
			Name:            p.Name,
			Type:            p.Type,
			DependsOnPhases: p.DependsOnPhases,
		}

		if p.Type == "concrete" {
			phaseModel.Status = model.PhaseStatusActive
			phaseModel.ActivatedAt = &now

			for _, t := range p.Tasks {
				taskID, ok := cpd.nameToID[t.Name]
				if !ok {
					return nil, fmt.Errorf("task %q not found in nameToID map for phase %q", t.Name, p.Name)
				}
				phaseModel.TaskIDs = append(phaseModel.TaskIDs, taskID)

				if t.Required {
					state.RequiredTaskIDs = append(state.RequiredTaskIDs, taskID)
				} else {
					state.OptionalTaskIDs = append(state.OptionalTaskIDs, taskID)
				}

				// §2.1: tasks enter the lifecycle at `planned`; see buildCommandState above.
				state.SetTaskState(taskID, model.StatusPlanned, now)

				// Convert blocked_by names to IDs
				if len(t.BlockedBy) > 0 {
					depIDs := make([]string, 0, len(t.BlockedBy))
					for _, depName := range t.BlockedBy {
						depID, ok := cpd.nameToID[depName]
						if !ok {
							return nil, fmt.Errorf("blocked_by references unknown task %q in phase %q", depName, p.Name)
						}
						depIDs = append(depIDs, depID)
					}
					state.TaskDependencies[taskID] = depIDs
				}
			}
		} else {
			// Deferred phase
			phaseModel.Status = model.PhaseStatusPending
			if p.Constraints != nil {
				phaseModel.Constraints = &model.PhaseConstraints{
					MaxTasks:           p.Constraints.MaxTasks,
					TimeoutMinutes:     p.Constraints.TimeoutMinutes,
					AllowedBloomLevels: p.Constraints.AllowedBloomLevels,
				}
				if len(phaseModel.Constraints.AllowedBloomLevels) == 0 {
					phaseModel.Constraints.AllowedBloomLevels = []int{1, 2, 3, 4, 5, 6}
				}
			}
		}

		state.Phases = append(state.Phases, phaseModel)
	}

	// Add system commit task to state (outside any phase)
	if systemCommitTaskID != nil {
		state.RequiredTaskIDs = append(state.RequiredTaskIDs, *systemCommitTaskID)
		// §2.1: __system_commit also enters the lifecycle at `planned`.
		state.SetTaskState(*systemCommitTaskID, model.StatusPlanned, now)

		// Register dependencies: system commit blocks on all concrete-phase tasks
		depIDs := make([]string, 0, len(cpd.tasks)-1) // exclude __system_commit itself
		for _, t := range cpd.tasks {
			if t.Name != "__system_commit" {
				depID, ok := cpd.nameToID[t.Name]
				if !ok {
					return nil, fmt.Errorf("system commit dependency task %q not found in nameToID map", t.Name)
				}
				depIDs = append(depIDs, depID)
			}
		}
		if len(depIDs) > 0 {
			state.TaskDependencies[*systemCommitTaskID] = depIDs
		}
	}

	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)
	return state, nil
}

// buildPhaseSubmitResult constructs the SubmitResult for a phase-based submission.
func buildPhaseSubmitResult(commandID string, phases []PhaseInput, phaseNameToID map[string]string,
	cpd *concretePhaseData, systemCommitTaskID *string) *SubmitResult {

	result := &SubmitResult{CommandID: commandID}
	for _, p := range phases {
		pr := SubmitPhaseResult{
			Name:    p.Name,
			PhaseID: phaseNameToID[p.Name],
			Type:    p.Type,
		}
		if p.Type == "concrete" {
			pr.Status = string(model.PhaseStatusActive)
			for _, t := range p.Tasks {
				a := cpd.assignMap[t.Name]
				pr.Tasks = append(pr.Tasks, SubmitTaskResult{
					Name:   t.Name,
					TaskID: cpd.nameToID[t.Name],
					Worker: a.WorkerID,
					Model:  a.Model,
				})
			}
		} else {
			pr.Status = string(model.PhaseStatusPending)
		}
		result.Phases = append(result.Phases, pr)
	}

	if systemCommitTaskID != nil {
		a := cpd.assignMap["__system_commit"]
		result.Tasks = append(result.Tasks, SubmitTaskResult{
			Name:   "__system_commit",
			TaskID: *systemCommitTaskID,
			Worker: a.WorkerID,
			Model:  a.Model,
		})
	}

	return result
}

func defaultCompletionPolicy() model.CompletionPolicy {
	return model.CompletionPolicy{
		Mode:                    "all_required_completed",
		AllowDynamicTasks:       false,
		OnRequiredFailed:        "fail_command",
		OnRequiredCancelled:     "cancel_command",
		OnOptionalFailed:        "ignore",
		DependencyFailurePolicy: "cancel_dependents",
	}
}
