package daemon

import (
	"errors"
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
)

func (qh *QueueHandler) advanceTaskLifecycle(task *model.Task, targets ...model.Status) error {
	if qh == nil || qh.dependencyResolver == nil || !qh.dependencyResolver.HasStateReader() || task == nil {
		return nil
	}
	sm := qh.dependencyResolver.GetStateManager()
	if sm == nil {
		return nil
	}
	for _, target := range targets {
		current, err := sm.GetTaskState(task.CommandID, task.ID)
		if err != nil {
			return fmt.Errorf("load task state for %s/%s: %w", task.CommandID, task.ID, err)
		}
		if current == target {
			continue
		}
		if model.IsTerminal(current) {
			return fmt.Errorf("task %s/%s is terminal (%s), cannot advance to %s",
				task.CommandID, task.ID, current, target)
		}
		if err := sm.UpdateTaskState(task.CommandID, task.ID, target, ""); err != nil {
			return fmt.Errorf("advance task %s/%s %s -> %s: %w",
				task.CommandID, task.ID, current, target, err)
		}
	}
	return nil
}

func (qh *QueueHandler) markTaskReady(task *model.Task) error {
	if task == nil {
		return nil
	}
	if qh == nil || qh.dependencyResolver == nil || !qh.dependencyResolver.HasStateReader() {
		return nil
	}
	current, err := qh.dependencyResolver.GetStateManager().GetTaskState(task.CommandID, task.ID)
	if err != nil {
		if errors.Is(err, model.ErrTaskNotFound) || errors.Is(err, model.ErrStateNotFound) {
			return nil
		}
		return fmt.Errorf("load task state for %s/%s: %w", task.CommandID, task.ID, err)
	}
	switch current {
	case model.StatusPending:
		return qh.advanceTaskLifecycle(task, model.StatusPlanned, model.StatusReady)
	case model.StatusPlanned, model.StatusPausedForReplan, model.StatusPausedForHuman:
		return qh.advanceTaskLifecycle(task, model.StatusReady)
	case model.StatusReady, model.StatusDispatched, model.StatusRunning, model.StatusRepairPending:
		return nil
	default:
		if model.IsTerminal(current) {
			return nil
		}
		return fmt.Errorf("task %s/%s is %s, cannot mark ready", task.CommandID, task.ID, current)
	}
}

// markTaskDispatched advances the command-state task entry into the
// `dispatched` slot at the moment the queue lease is acquired, closing
// the synchronization gap between queue.task.Status (in_progress) and
// state.TaskStates[task] (which used to lag at `ready` until
// markTaskRunning fired in applyTaskDispatchResult.onSuccess).
//
// Without this hop the result_write audit logs spurious
// `invalid_state_transition from=ready to=completed` lines whenever the
// dispatch attempt reaches the worker before markTaskRunning runs (or
// fails entirely, in which case markTaskRunning never runs at all). The
// state machine's BFS still routes ready → completed via the
// intermediate states, so the prior behaviour was functionally correct
// — the desync was just visible in the audit feed.
//
// Idempotent for already-dispatched / running / repair_pending entries
// so callers can invoke it on every lease acquisition. Terminal entries
// are left alone (the lease should never have been acquired in that
// case; we log nothing here and rely on the caller's own guards).
func (qh *QueueHandler) markTaskDispatched(task *model.Task) error {
	if task == nil {
		return nil
	}
	if qh == nil || qh.dependencyResolver == nil || !qh.dependencyResolver.HasStateReader() {
		return nil
	}
	current, err := qh.dependencyResolver.GetStateManager().GetTaskState(task.CommandID, task.ID)
	if err != nil {
		if errors.Is(err, model.ErrTaskNotFound) || errors.Is(err, model.ErrStateNotFound) {
			return nil
		}
		return fmt.Errorf("load task state for %s/%s: %w", task.CommandID, task.ID, err)
	}
	switch current {
	case model.StatusPending:
		return qh.advanceTaskLifecycle(task, model.StatusPlanned, model.StatusReady, model.StatusDispatched)
	case model.StatusPlanned, model.StatusPausedForReplan, model.StatusPausedForHuman:
		return qh.advanceTaskLifecycle(task, model.StatusReady, model.StatusDispatched)
	case model.StatusReady:
		return qh.advanceTaskLifecycle(task, model.StatusDispatched)
	case model.StatusDispatched, model.StatusRunning, model.StatusRepairPending:
		return nil
	default:
		if model.IsTerminal(current) {
			return nil
		}
		return fmt.Errorf("task %s/%s is %s, cannot mark dispatched", task.CommandID, task.ID, current)
	}
}

func (qh *QueueHandler) markTaskRunning(task *model.Task) error {
	if task == nil {
		return nil
	}
	if qh == nil || qh.dependencyResolver == nil || !qh.dependencyResolver.HasStateReader() {
		return nil
	}
	current, err := qh.dependencyResolver.GetStateManager().GetTaskState(task.CommandID, task.ID)
	if err != nil {
		if errors.Is(err, model.ErrTaskNotFound) || errors.Is(err, model.ErrStateNotFound) {
			return nil
		}
		return fmt.Errorf("load task state for %s/%s: %w", task.CommandID, task.ID, err)
	}
	switch current {
	case model.StatusPending:
		return qh.advanceTaskLifecycle(task, model.StatusPlanned, model.StatusReady, model.StatusDispatched, model.StatusRunning)
	case model.StatusPlanned, model.StatusPausedForReplan, model.StatusPausedForHuman:
		return qh.advanceTaskLifecycle(task, model.StatusReady, model.StatusDispatched, model.StatusRunning)
	case model.StatusReady:
		return qh.advanceTaskLifecycle(task, model.StatusDispatched, model.StatusRunning)
	case model.StatusDispatched:
		return qh.advanceTaskLifecycle(task, model.StatusRunning)
	case model.StatusRepairPending:
		return qh.advanceTaskLifecycle(task, model.StatusRunning)
	case model.StatusRunning:
		return nil
	default:
		if model.IsTerminal(current) {
			return nil
		}
		return fmt.Errorf("task %s/%s is %s, cannot mark running", task.CommandID, task.ID, current)
	}
}
