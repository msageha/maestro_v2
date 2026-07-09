package model

import "fmt"

// AdvanceTaskState applies the §2.1 task lifecycle progression that takes
// t.TaskStates[taskID] to target. When the direct transition is permitted by
// ValidateTaskStateTransition, it is applied as-is; otherwise a shortest valid
// path is computed via BFS over validTaskStateTransitions and each intermediate
// step is applied in order. Every applied step goes through SetTaskState so
// TaskStatusChangedAt stays in lockstep with TaskStates (nowRFC3339 stamps
// the transition time).
//
// The function is purely in-memory: callers are responsible for persisting
// the mutated tracking struct and for providing whatever locking is needed.
// Universal transitions (`* → paused_for_human`, `* → aborted`) are handled by
// their direct ValidateTaskStateTransition shortcut and never re-routed through
// the graph.
//
// This helper is the single entry point that lets callers express logical
// intent (e.g. "this task is now completed") without having to spell out
// every intermediate state transition (running → verify_pending → completed).
func AdvanceTaskState(t *TaskTracking, taskID string, target Status, nowRFC3339 string) error {
	if t == nil || t.TaskStates == nil {
		return fmt.Errorf("AdvanceTaskState: states map is nil")
	}
	current, ok := t.TaskStates[taskID]
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
		t.SetTaskState(taskID, target, nowRFC3339)
		return nil
	}

	path, err := shortestTaskStatePath(current, target)
	if err != nil {
		return fmt.Errorf("AdvanceTaskState: %w", err)
	}
	for _, next := range path {
		if err := ValidateTaskStateTransition(t.TaskStates[taskID], next); err != nil {
			// Defensive: BFS only enqueues valid edges, but re-validate so the
			// universal-transition map shortcut and the graph stay consistent.
			return fmt.Errorf("AdvanceTaskState: applying %s → %s: %w",
				t.TaskStates[taskID], next, err)
		}
		t.SetTaskState(taskID, next, nowRFC3339)
	}
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
