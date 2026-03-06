package plan

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// PlanStateReader implements daemon.StateReader by reading state/commands/ YAML files.
type PlanStateReader struct {
	stateManager *StateManager
}

// NewPlanStateReader creates a PlanStateReader backed by the given StateManager.
func NewPlanStateReader(sm *StateManager) *PlanStateReader {
	return &PlanStateReader{stateManager: sm}
}

// GetTaskState returns the status of a task from the command state file.
func (r *PlanStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		// Use errors.Is() to detect not-found without masking other FS errors
		if errors.Is(err, os.ErrNotExist) {
			return "", core.ErrStateNotFound
		}
		return "", err
	}

	status, ok := state.TaskStates[taskID]
	if !ok {
		return "", fmt.Errorf("task %s not found in command %s", taskID, commandID)
	}
	return status, nil
}

// GetCommandPhases returns phase metadata for a command.
func (r *PlanStateReader) GetCommandPhases(commandID string) ([]core.PhaseInfo, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		// Use errors.Is() to detect not-found without masking other FS errors
		if errors.Is(err, os.ErrNotExist) {
			return nil, core.ErrStateNotFound
		}
		return nil, err
	}

	if len(state.Phases) == 0 {
		return nil, nil
	}

	// Build phase-name-to-ID lookup once (not per-phase)
	phaseNameToID := make(map[string]string, len(state.Phases))
	for _, sp := range state.Phases {
		phaseNameToID[sp.Name] = sp.PhaseID
	}

	var phases []core.PhaseInfo
	for _, p := range state.Phases {
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

		phases = append(phases, core.PhaseInfo{
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

// GetTaskDependencies returns the task IDs that the given task depends on.
func (r *PlanStateReader) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		// Use errors.Is() to detect not-found without masking other FS errors
		if errors.Is(err, os.ErrNotExist) {
			return nil, core.ErrStateNotFound
		}
		return nil, err
	}

	deps, ok := state.TaskDependencies[taskID]
	if !ok {
		return nil, nil
	}
	return deps, nil
}

// ApplyPhaseTransition persists a phase status change under the command lock.
func (r *PlanStateReader) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	r.stateManager.LockCommand(commandID)
	defer r.stateManager.UnlockCommand(commandID)

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	found := false
	for i := range state.Phases {
		if state.Phases[i].PhaseID == phaseID {
			found = true
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

	if !found {
		return fmt.Errorf("phase %s in command %s: %w", phaseID, commandID, core.ErrPhaseNotFound)
	}

	state.UpdatedAt = now
	return r.stateManager.SaveState(state)
}

// UpdateTaskState updates a single task's status under the command lock.
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

	// Verify taskID is a known task (exists in required, optional, or system commit)
	if !isKnownTaskID(state, taskID) {
		return fmt.Errorf("task %s not found in command %s: unknown task ID", taskID, commandID)
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

// IsCommandCancelRequested checks the cancel.requested flag in the state file.
func (r *PlanStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		// Use errors.Is() to detect not-found without masking other FS errors
		if errors.Is(err, os.ErrNotExist) {
			return false, core.ErrStateNotFound
		}
		return false, err
	}
	return state.Cancel.Requested, nil
}

// IsSystemCommitReady checks if a task is a system commit task and whether
// all user tasks/phases are terminal.
func (r *PlanStateReader) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		// Use errors.Is() to detect not-found without masking other FS errors
		if errors.Is(err, os.ErrNotExist) {
			return false, false, core.ErrStateNotFound
		}
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

// GetCircuitBreakerState returns the circuit breaker state for a command.
func (r *PlanStateReader) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, core.ErrStateNotFound
		}
		return nil, err
	}
	cb := state.CircuitBreaker
	return &cb, nil
}

// TripCircuitBreaker sets the circuit breaker to tripped state under the command lock.
func (r *PlanStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	r.stateManager.LockCommand(commandID)
	defer r.stateManager.UnlockCommand(commandID)

	state, err := r.stateManager.LoadState(commandID)
	if err != nil {
		return err
	}

	if state.CircuitBreaker.Tripped {
		return nil // already tripped, idempotent
	}

	// Re-validate progress timeout under lock to prevent TOCTOU race:
	// A concurrent success write may have updated LastProgressAt since the unlocked read.
	if progressTimeoutMinutes > 0 && state.CircuitBreaker.LastProgressAt != nil {
		lastProgress, parseErr := time.Parse(time.RFC3339, *state.CircuitBreaker.LastProgressAt)
		if parseErr == nil && time.Since(lastProgress) < time.Duration(progressTimeoutMinutes)*time.Minute {
			return nil // timeout no longer exceeded
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	state.CircuitBreaker.Tripped = true
	state.CircuitBreaker.TrippedAt = &now
	state.CircuitBreaker.TripReason = &reason

	// Set cancel request so existing cancel flow handles task cancellation
	if !state.Cancel.Requested {
		state.Cancel.Requested = true
		state.Cancel.RequestedAt = &now
		state.Cancel.RequestedBy = strPtr("circuit_breaker")
		state.Cancel.Reason = &reason
	}

	state.UpdatedAt = now
	return r.stateManager.SaveState(state)
}

func strPtr(s string) *string { return &s }

// isKnownTaskID checks whether taskID belongs to the command's known tasks
// (required, optional, or system commit).
func isKnownTaskID(state *model.CommandState, taskID string) bool {
	for _, id := range state.RequiredTaskIDs {
		if id == taskID {
			return true
		}
	}
	for _, id := range state.OptionalTaskIDs {
		if id == taskID {
			return true
		}
	}
	if state.SystemCommitTaskID != nil && *state.SystemCommitTaskID == taskID {
		return true
	}
	return false
}
