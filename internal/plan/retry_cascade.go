package plan

import (
	"fmt"
	"log"

	"github.com/msageha/maestro_v2/internal/model"
)

// maxCascadeRecoverDepth is the maximum recursion depth for cascade recovery.
// This prevents stack overflow when corrupted state files contain circular references.
const maxCascadeRecoverDepth = 10

func findPhaseForTask(state *model.CommandState, taskID string) (*model.Phase, int) {
	for i := range state.Phases {
		for _, tid := range state.Phases[i].TaskIDs {
			if tid == taskID {
				return &state.Phases[i], i
			}
		}
	}
	return nil, -1
}

func replaceInRequiredOrOptional(state *model.CommandState, oldID, newID string) error {
	found := false
	for i, id := range state.RequiredTaskIDs {
		if id == oldID {
			state.RequiredTaskIDs[i] = newID
			found = true
			break
		}
	}
	if !found {
		for i, id := range state.OptionalTaskIDs {
			if id == oldID {
				state.OptionalTaskIDs[i] = newID
				found = true
				break
			}
		}
	}
	// Update SystemCommitTaskID if it matches the old ID.
	if state.SystemCommitTaskID != nil && *state.SystemCommitTaskID == oldID {
		state.SystemCommitTaskID = &newID
		found = true
	}
	if !found {
		return fmt.Errorf("task %s not found in required, optional, or system_commit_task_id", oldID)
	}
	return nil
}

func rewriteDependencies(state *model.CommandState, oldID, newID string) {
	for taskID, deps := range state.TaskDependencies {
		for i, dep := range deps {
			if dep == oldID {
				state.TaskDependencies[taskID][i] = newID
			}
		}
	}
}

func cascadeRecover(
	state *model.CommandState,
	failedTaskID, newRetryTaskID string,
	workerConfig model.WorkerConfig,
	limits model.LimitsConfig,
	workerStates []WorkerState,
	origTaskCache map[string]model.Task,
) ([]CascadeRecoveredTask, error) {
	// CR-020: Work on a copy of workerStates so that on failure the caller's
	// slice is not left in a partially-mutated state.
	ws := make([]WorkerState, len(workerStates))
	copy(ws, workerStates)
	var recovered []CascadeRecoveredTask
	return cascadeRecoverRecursive(state, failedTaskID, newRetryTaskID, workerConfig, limits, ws, recovered, origTaskCache, 0)
}

func cascadeRecoverRecursive(
	state *model.CommandState,
	failedTaskID, newRetryTaskID string,
	workerConfig model.WorkerConfig,
	limits model.LimitsConfig,
	workerStates []WorkerState,
	recovered []CascadeRecoveredTask,
	origTaskCache map[string]model.Task,
	depth int,
) ([]CascadeRecoveredTask, error) {
	if depth >= maxCascadeRecoverDepth {
		return recovered, fmt.Errorf("cascade recovery exceeded maximum depth %d (at task %s)", maxCascadeRecoverDepth, failedTaskID)
	}
	candidates := findCascadeCandidates(state, failedTaskID)

	for _, cancelledTaskID := range candidates {
		// Skip command-cancel-requested tasks
		reason := state.CancelledReasons[cancelledTaskID]
		if reason == "command_cancel_requested" {
			continue
		}

		// Generate new task ID
		newTaskID, err := model.NewTaskID(model.TaskIDCallerPlannerRetry)
		if err != nil {
			return recovered, fmt.Errorf("generate cascade recovery ID: %w", err)
		}

		// Inherit original task's blocked_by, mapped through lineage
		origDeps := state.TaskDependencies[cancelledTaskID]
		newDeps, err := resolveBlockedByViaLineage(origDeps, state.RetryLineage)
		if err != nil {
			return recovered, fmt.Errorf("resolve blocked_by via lineage for cascade %s: %w", cancelledTaskID, err)
		}

		// Replace the failed dependency with the new retry task
		for i, dep := range newDeps {
			if dep == failedTaskID {
				newDeps[i] = newRetryTaskID
			}
		}

		// Worker assignment for recovered task — inherit bloom_level from original task
		bloomLevel := 3 // default fallback
		if origTask, ok := origTaskCache[cancelledTaskID]; ok {
			bloomLevel = origTask.BloomLevel
		}
		assignReqs := []TaskAssignmentRequest{{Name: "__cascade_recovery", BloomLevel: bloomLevel}}
		assignments, err := AssignWorkers(workerConfig, limits, workerStates, assignReqs)
		if err != nil {
			return recovered, fmt.Errorf("worker assignment for cascade %s: %w", cancelledTaskID, err)
		}
		assignment := assignments[0]

		// Update state
		if err := replaceInRequiredOrOptional(state, cancelledTaskID, newTaskID); err != nil {
			return recovered, fmt.Errorf("cascade replace in required/optional: %w", err)
		}
		state.RetryLineage[newTaskID] = cancelledTaskID
		rewriteDependencies(state, cancelledTaskID, newTaskID)
		state.TaskStates[newTaskID] = model.StatusPending
		state.TaskDependencies[newTaskID] = newDeps

		// Add to phase
		if phase, phaseIdx := findPhaseForTask(state, cancelledTaskID); phase != nil {
			state.Phases[phaseIdx].TaskIDs = append(state.Phases[phaseIdx].TaskIDs, newTaskID)
			if phase.Status == model.PhaseStatusFailed {
				now := nowUTC()
				if err := reopenPhase(state, phaseIdx, now); err != nil {
					return recovered, fmt.Errorf("reopen phase for cascade: %w", err)
				}
			}
		}

		recovered = append(recovered, CascadeRecoveredTask{
			TaskID:   newTaskID,
			Worker:   assignment.WorkerID,
			Model:    assignment.Model,
			Replaced: cancelledTaskID,
		})

		// Update worker states for further assignments
		for i := range workerStates {
			if workerStates[i].WorkerID == assignment.WorkerID {
				workerStates[i].PendingCount++
				break
			}
		}

		// Recurse for downstream cancelled tasks
		var err2 error
		recovered, err2 = cascadeRecoverRecursive(
			state, cancelledTaskID, newTaskID,
			workerConfig, limits, workerStates, recovered, origTaskCache, depth+1,
		)
		if err2 != nil {
			return recovered, err2
		}
	}

	return recovered, nil
}

// purgeSupersededRetryEntries removes stale map entries for tasks that have
// been superseded by retries. This prevents RetryLineage, TaskDependencies,
// and CancelledReasons from growing without bound in long-running commands.
// Only terminal tasks that appear as values in RetryLineage (i.e., have been
// replaced) are cleaned up; cascade recovery has already processed them.
func purgeSupersededRetryEntries(state *model.CommandState) {
	if len(state.RetryLineage) == 0 {
		return
	}

	// Collect superseded task IDs (old tasks replaced by new ones).
	superseded := make(map[string]bool, len(state.RetryLineage))
	for _, oldID := range state.RetryLineage {
		superseded[oldID] = true
	}

	for oldID := range superseded {
		status, exists := state.TaskStates[oldID]
		if !exists || !model.IsTerminal(status) {
			continue
		}
		delete(state.TaskDependencies, oldID)
		delete(state.CancelledReasons, oldID)
	}
}

func findCascadeCandidates(state *model.CommandState, failedTaskID string) []string {
	expectedReason := fmt.Sprintf("blocked_dependency_terminal:%s", failedTaskID)
	var candidates []string
	for taskID, reason := range state.CancelledReasons {
		if reason == expectedReason {
			candidates = append(candidates, taskID)
		}
	}
	return candidates
}

func resolveBlockedByViaLineage(blockedBy []string, lineage map[string]string) ([]string, error) {
	// Build reverse lineage map once (O(m)) instead of per-element (was O(n*m)).
	// lineage maps new -> old, reverse maps old -> new.
	reverseLineage := make(map[string]string, len(lineage))
	for newID, oldID := range lineage {
		reverseLineage[oldID] = newID
	}

	resolved := make([]string, len(blockedBy))
	for i, dep := range blockedBy {
		desc, err := getLatestDescendant(dep, reverseLineage)
		if err != nil {
			return nil, err
		}
		resolved[i] = desc
	}
	return resolved, nil
}

func getLatestDescendant(taskID string, reverseLineage map[string]string) (string, error) {
	visited := make(map[string]bool)
	current := taskID
	for {
		if visited[current] {
			log.Printf("[WARN] lineage cycle detected starting from task %s, revisited %s", taskID, current)
			return "", fmt.Errorf("lineage cycle detected: task %s revisited while resolving lineage from %s", current, taskID)
		}
		visited[current] = true
		next, ok := reverseLineage[current]
		if !ok {
			return current, nil
		}
		current = next
	}
}

func reopenPhase(state *model.CommandState, phaseIdx int, now string) error {
	phase := &state.Phases[phaseIdx]
	if phase.Status != model.PhaseStatusFailed {
		return fmt.Errorf("cannot reopen phase %q from status %s (only failed→active allowed)",
			phase.Name, phase.Status)
	}
	phase.Status = model.PhaseStatusActive
	phase.ReopenedAt = &now
	phase.CompletedAt = nil
	return nil
}
