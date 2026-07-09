package plan

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
)

// maxCascadeRecoverDepth is the maximum recursion depth for cascade recovery.
// This prevents stack overflow when corrupted state files contain circular references.
const maxCascadeRecoverDepth = 10

// maxLineageDepth is the upper bound on the number of entries the visited map
// in getLatestDescendant may accumulate. It prevents unbounded memory growth
// when lineage chains become excessively long (e.g. due to repeated retries).
const maxLineageDepth = 1000

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

func replaceInRequiredOrOptional(state *model.CommandState, oldID, newID string) {
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
		// A cancelled task that was *already* replaced (e.g. by an earlier
		// cascade pass that walked the same dependency tree, or by a
		// previous retry) no longer appears in required/optional/
		// system_commit because the slot has been rewritten to the
		// successor's ID. Treat this as a benign no-op so the cascade
		// transaction does not abort and roll back. The lineage entry that
		// drives EffectiveStatus is what downstream callers consult; missing
		// slot membership is a purely structural artefact of the prior
		// replace.
		slogc().Debug("cascade replace skipped: task already replaced or absent from membership slots",
			"task_id", oldID, "new_id", newID)
	}
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
	selector ModelSelector,
) ([]CascadeRecoveredTask, error) {
	// All-or-nothing: snapshot CommandState before mutations so we can
	// restore it entirely if cascadeRecoverRecursive fails partway through.
	stateSnapshot, err := copyState(state)
	if err != nil {
		return nil, fmt.Errorf("cascade recovery state snapshot: %w", err)
	}

	// CR-020: Work on an immutable snapshot of workerStates so that on failure
	// the caller's slice is not left in a partially-mutated state.
	ws := SnapshotWorkerStates(workerStates)
	var recovered []CascadeRecoveredTask
	recovered, err = cascadeRecoverRecursive(state, failedTaskID, newRetryTaskID, workerConfig, limits, ws, recovered, origTaskCache, 0, selector)
	if err != nil {
		// Restore state to pre-cascade snapshot (all-or-nothing).
		if rsErr := restoreState(state, stateSnapshot); rsErr != nil {
			slogc().Error("cascade recovery rollback failed", "error", rsErr)
		}
		return nil, err
	}
	return recovered, nil
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
	selector ModelSelector,
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
		assignments, err := AssignWorkers(workerConfig, limits, workerStates, assignReqs, WithModelSelector(selector))
		if err != nil {
			return recovered, fmt.Errorf("worker assignment for cascade %s: %w", cancelledTaskID, err)
		}
		if len(assignments) == 0 {
			return recovered, fmt.Errorf("worker assignment for cascade %s: no assignments returned", cancelledTaskID)
		}
		assignment := assignments[0]

		// Apply state mutations for the recovered task
		if err := updateTaskStateForCascade(state, cancelledTaskID, newTaskID, newDeps); err != nil {
			return recovered, err
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
			selector,
		)
		if err2 != nil {
			return recovered, err2
		}
	}

	return recovered, nil
}

// updateTaskStateForCascade applies the state mutations needed when a cancelled
// task is replaced by a new cascade-recovered task.
func updateTaskStateForCascade(state *model.CommandState, cancelledTaskID, newTaskID string, newDeps []string) error {
	replaceInRequiredOrOptional(state, cancelledTaskID, newTaskID)
	state.RetryLineage[newTaskID] = cancelledTaskID
	rewriteDependencies(state, cancelledTaskID, newTaskID)
	// §2.1: cascade-retry tasks enter the lifecycle at `planned`.
	state.TaskStates[newTaskID] = model.StatusPlanned
	state.TaskDependencies[newTaskID] = newDeps

	// Add to phase
	if phase, phaseIdx := findPhaseForTask(state, cancelledTaskID); phase != nil {
		if phaseIdx < 0 || phaseIdx >= len(state.Phases) {
			return fmt.Errorf("cascade state mutation: phaseIdx %d out of range [0, %d)", phaseIdx, len(state.Phases))
		}
		state.Phases[phaseIdx].TaskIDs = append(state.Phases[phaseIdx].TaskIDs, newTaskID)
		if phase.Status == model.PhaseStatusFailed {
			now := nowUTC()
			if err := reopenPhase(state, phaseIdx, now); err != nil {
				return fmt.Errorf("reopen phase for cascade: %w", err)
			}
		}
	}

	// Re-validate DAG after dependency rewriting to catch cycles or
	// cross-phase violations introduced by the mutation.
	if err := ValidateTaskDAGAfterMutation(state); err != nil {
		return fmt.Errorf("cascade state mutation validation: %w", err)
	}
	return nil
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
	// lineage maps new -> old; buildSuccessorMap inverts it deterministically
	// (latest successor wins) so resolution does not depend on map iteration order.
	reverseLineage := buildSuccessorMap(lineage)

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
			slogc().Warn("lineage cycle detected", "start_task_id", taskID, "revisited_task_id", current)
			return "", fmt.Errorf("lineage cycle detected: task %s revisited while resolving lineage from %s", current, taskID)
		}
		if len(visited) >= maxLineageDepth {
			slogc().Warn("lineage depth exceeded", "max_depth", maxLineageDepth, "start_task_id", taskID)
			return "", fmt.Errorf("lineage depth exceeded maximum %d starting from task %s", maxLineageDepth, taskID)
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
