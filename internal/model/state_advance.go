package model

import "fmt"

// AdvanceTaskState applies the §2.1 task lifecycle progression that takes
// state[taskID] to target. When the direct transition is permitted by
// ValidateTaskStateTransition, it is applied as-is; otherwise a shortest valid
// path is computed via BFS over validTaskStateTransitions and each intermediate
// step is applied in order.
//
// The function is purely in-memory: callers are responsible for persisting
// the mutated map and for providing whatever locking is needed. Universal
// transitions (`* → paused_for_human`, `* → aborted`) are handled by their
// direct ValidateTaskStateTransition shortcut and never re-routed through the
// graph.
//
// REQUIREMENTS.md §2.1: this helper is the single entry point that lets
// callers express logical intent (e.g. "this task is now completed") without
// having to spell out every intermediate state transition the requirements
// mandate (running → verify_pending → completed).
func AdvanceTaskState(states map[string]Status, taskID string, target Status) error {
	if states == nil {
		return fmt.Errorf("AdvanceTaskState: states map is nil")
	}
	current, ok := states[taskID]
	if !ok {
		return fmt.Errorf("AdvanceTaskState: task %q not registered in states", taskID)
	}
	if current == target {
		return nil
	}
	if IsTerminal(current) {
		return fmt.Errorf("AdvanceTaskState: task %q is already terminal (%s); cannot advance to %s",
			taskID, current, target)
	}

	// Direct transition (covers universal `* → paused_for_human` / `* → aborted`).
	if err := ValidateTaskStateTransition(current, target); err == nil {
		states[taskID] = target
		return nil
	}

	path, err := shortestTaskStatePath(current, target)
	if err != nil {
		return fmt.Errorf("AdvanceTaskState: %w", err)
	}
	for _, next := range path {
		if err := ValidateTaskStateTransition(states[taskID], next); err != nil {
			// Defensive: BFS only enqueues valid edges, but re-validate so the
			// universal-transition map shortcut and the graph stay consistent.
			return fmt.Errorf("AdvanceTaskState: applying %s → %s: %w",
				states[taskID], next, err)
		}
		states[taskID] = next
	}
	return nil
}

// TerminateForAbort applies the §2.1 universal `* → aborted` transition for
// taskID. Returns nil if the task is already aborted (idempotent) and an error
// if the task is missing or in a non-aborted terminal state.
func TerminateForAbort(states map[string]Status, taskID string) error {
	if states == nil {
		return fmt.Errorf("TerminateForAbort: states map is nil")
	}
	current, ok := states[taskID]
	if !ok {
		return fmt.Errorf("TerminateForAbort: task %q not registered in states", taskID)
	}
	if current == StatusAborted {
		return nil
	}
	if IsTerminal(current) {
		return fmt.Errorf("TerminateForAbort: task %q is already terminal (%s); cannot abort",
			taskID, current)
	}
	states[taskID] = StatusAborted
	return nil
}

// shortestTaskStatePath returns the sequence of intermediate states (excluding
// from, including to) that forms the shortest valid path from `from` to `to`
// through validTaskStateTransitions. Universal-transition shortcuts
// (paused_for_human, aborted) are not used as intermediate hops because they
// represent abnormal exits, not advancement steps.
func shortestTaskStatePath(from, to Status) ([]Status, error) {
	type node struct {
		state Status
		path  []Status
	}
	visited := map[Status]bool{from: true}
	queue := []node{{state: from, path: nil}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		neighbors, ok := validTaskStateTransitions[cur.state]
		if !ok {
			continue
		}
		for next := range neighbors {
			if visited[next] {
				continue
			}
			// Skip universal exits as intermediate hops; they're terminal
			// abnormal exits, not progression milestones.
			if next == StatusPausedForHuman && to != StatusPausedForHuman {
				continue
			}
			if next == StatusAborted && to != StatusAborted {
				continue
			}
			newPath := make([]Status, len(cur.path)+1)
			copy(newPath, cur.path)
			newPath[len(cur.path)] = next
			if next == to {
				return newPath, nil
			}
			// Don't traverse through terminal states (other than the target).
			if IsTerminal(next) {
				continue
			}
			visited[next] = true
			queue = append(queue, node{state: next, path: newPath})
		}
	}
	return nil, fmt.Errorf("no valid path from %s to %s", from, to)
}
