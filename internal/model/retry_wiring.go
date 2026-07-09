package model

// WireRetryTaskIntoState mutates state so retryTaskID inherits
// predecessorTaskID's completion semantics:
//
//   - replaces the predecessor in required/optional/system-commit
//     membership (fallback: appends the retry to OptionalTaskIDs so it
//     remains a known task),
//   - joins the predecessor's phase, reopening a failed phase
//     (failed→active is the dedicated add-retry transition in
//     ValidatePhaseTransition),
//   - inherits the predecessor's dependencies so blocked_by checks resolve
//     against the same upstream tasks,
//   - records retry lineage so EffectiveStatus / cascade-recovery walks can
//     resolve the chain,
//   - enters the lifecycle at planned (§2.1).
//
// Pure state mutation — callers own locking and persistence. Both the
// daemon retry pipeline (TaskRetryHandler.RegisterRetryTaskInState) and the
// reconciler repair pipeline (R9) must use this single implementation:
// a partially wired retry (TaskStates only) leaves the predecessor in
// RequiredTaskIDs without lineage, so its cancelled marker after the retry
// completes reads as a hard cancel and fails the phase.
//
// Returns false when the predecessor could not be located in any
// membership list (callers typically log this).
func WireRetryTaskIntoState(state *CommandState, retryTaskID, predecessorTaskID, now string) bool {
	if state.TaskStates == nil {
		state.TaskStates = make(map[string]Status)
	}
	if state.TaskDependencies == nil {
		state.TaskDependencies = make(map[string][]string)
	}
	if state.RetryLineage == nil {
		state.RetryLineage = make(map[string]string)
	}

	membered := true
	if predecessorTaskID == "" || !replaceTaskMembership(state, predecessorTaskID, retryTaskID) {
		membered = false
		state.OptionalTaskIDs = append(state.OptionalTaskIDs, retryTaskID)
	}

	if predecessorTaskID != "" {
		for i := range state.Phases {
			phase := &state.Phases[i]
			if !phaseContainsTask(phase, predecessorTaskID) {
				continue
			}
			phase.TaskIDs = append(phase.TaskIDs, retryTaskID)
			if phase.Status == PhaseStatusFailed {
				phase.Status = PhaseStatusActive
				reopenedAt := now
				phase.ReopenedAt = &reopenedAt
				phase.CompletedAt = nil
			}
			break
		}

		if deps, ok := state.TaskDependencies[predecessorTaskID]; ok && len(deps) > 0 {
			cloned := make([]string, len(deps))
			copy(cloned, deps)
			state.TaskDependencies[retryTaskID] = cloned
		}

		state.RetryLineage[retryTaskID] = predecessorTaskID
	}

	state.SetTaskState(retryTaskID, StatusPlanned, now)
	state.UpdatedAt = now
	return membered
}

// UnwireRetryTaskFromState undoes WireRetryTaskIntoState when the queue add
// fails after state registration. The phase status is intentionally not
// reverted from active back to failed: the next CheckPhaseTransitions scan
// re-evaluates the phase from its current task statuses (active→failed is a
// permitted transition). ReopenedAt is cleared so audit reads don't see a
// phantom reopen.
func UnwireRetryTaskFromState(state *CommandState, retryTaskID, predecessorTaskID, now string) {
	delete(state.TaskStates, retryTaskID)
	delete(state.TaskStatusChangedAt, retryTaskID)
	delete(state.TaskDependencies, retryTaskID)
	delete(state.RetryLineage, retryTaskID)

	if predecessorTaskID != "" {
		if !replaceTaskMembership(state, retryTaskID, predecessorTaskID) {
			// Defensive fallback: the retry was appended to OptionalTaskIDs
			// (predecessor was not membered). Strip it so we don't leak an
			// orphan ID.
			state.OptionalTaskIDs = removeStringFromSlice(state.OptionalTaskIDs, retryTaskID)
		}
	} else {
		state.OptionalTaskIDs = removeStringFromSlice(state.OptionalTaskIDs, retryTaskID)
	}

	for i := range state.Phases {
		phase := &state.Phases[i]
		before := len(phase.TaskIDs)
		phase.TaskIDs = removeStringFromSlice(phase.TaskIDs, retryTaskID)
		if len(phase.TaskIDs) != before {
			phase.ReopenedAt = nil
		}
	}

	state.UpdatedAt = now
}

// replaceTaskMembership swaps oldID for newID in the first membership list
// that contains it (required, optional, system-commit). Returns false when
// oldID is not membered anywhere.
func replaceTaskMembership(state *CommandState, oldID, newID string) bool {
	for i, id := range state.RequiredTaskIDs {
		if id == oldID {
			state.RequiredTaskIDs[i] = newID
			return true
		}
	}
	for i, id := range state.OptionalTaskIDs {
		if id == oldID {
			state.OptionalTaskIDs[i] = newID
			return true
		}
	}
	if state.SystemCommitTaskID != nil && *state.SystemCommitTaskID == oldID {
		updated := newID
		state.SystemCommitTaskID = &updated
		return true
	}
	return false
}

// phaseContainsTask reports whether phase.TaskIDs already lists taskID.
func phaseContainsTask(phase *Phase, taskID string) bool {
	for _, tid := range phase.TaskIDs {
		if tid == taskID {
			return true
		}
	}
	return false
}

// removeStringFromSlice returns a new slice with the first occurrence of
// target removed. Returns the input unchanged when target is absent.
func removeStringFromSlice(ss []string, target string) []string {
	for i, s := range ss {
		if s == target {
			return append(ss[:i], ss[i+1:]...)
		}
	}
	return ss
}
