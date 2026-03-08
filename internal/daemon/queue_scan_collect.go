package daemon

import (
	"fmt"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// --- Collect methods for Phase A ---

// collectPendingCommandDispatches acquires leases and records dispatch items (no tmux).
// Guard: any in_progress command blocks new dispatches regardless of lease validity.
// Planner processes one command at a time; expired leases are handled by busy-check
// recovery (auto-extend for commands) and Reconciler R0 for stuck planning.
func (qh *QueueHandler) collectPendingCommandDispatches(cq *model.CommandQueue, dirty *bool, work *deferredWork) {
	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusInProgress {
			qh.log(LogLevelDebug, "command_in_progress_guard id=%s epoch=%d blocking_dispatch", cmd.ID, cmd.LeaseEpoch)
			return
		}
	}

	sorted := qh.dispatcher.SortPendingCommands(cq.Commands)
	for _, idx := range sorted {
		cmd := &cq.Commands[idx]
		if err := qh.leaseManager.AcquireCommandLease(cmd, qh.leaseOwnerID()); err != nil {
			qh.log(LogLevelWarn, "lease_acquire_failed type=command id=%s error=%v", cmd.ID, err)
			continue
		}
		cmd.Attempts++
		*dirty = true

		cmdCopy := *cmd
		work.dispatches = append(work.dispatches, dispatchItem{
			Kind:      "command",
			Command:   &cmdCopy,
			Epoch:     cmd.LeaseEpoch,
			ExpiresAt: safeStr(cmd.LeaseExpiresAt),
		})
		break
	}
}

// collectPendingTaskDispatches acquires leases and records dispatch items (no tmux).
func (qh *QueueHandler) collectPendingTaskDispatches(tq *taskQueueEntry, workerID string, globalInFlight map[string]bool, work *deferredWork) bool {
	dirty := false
	sorted := qh.dispatcher.SortPendingTasks(tq.Queue.Tasks)

	for _, idx := range sorted {
		task := &tq.Queue.Tasks[idx]

		if globalInFlight[workerID] {
			qh.log(LogLevelDebug, "worker_busy worker=%s task=%s (global in-flight)", workerID, task.ID)
			break
		}

		// Check if task is in cooldown period
		if task.NotBefore != nil {
			notBefore, err := time.Parse(time.RFC3339, *task.NotBefore)
			if err == nil && qh.clock.Now().Before(notBefore) {
				qh.log(LogLevelDebug, "task_cooldown task=%s not_before=%s", task.ID, *task.NotBefore)
				continue
			}
		}

		blocked, err := qh.dependencyResolver.IsTaskBlocked(task)
		if err != nil {
			qh.log(LogLevelWarn, "dependency_check_error task=%s error=%v", task.ID, err)
			continue
		}
		if blocked {
			continue
		}

		isSysCommit, ready, sErr := qh.dependencyResolver.IsSystemCommitReady(task.CommandID, task.ID)
		if sErr != nil {
			qh.log(LogLevelWarn, "system_commit_check task=%s error=%v", task.ID, sErr)
			continue
		}
		if isSysCommit && !ready {
			qh.log(LogLevelDebug, "system_commit_not_ready task=%s command=%s", task.ID, task.CommandID)
			continue
		}

		if err := qh.leaseManager.AcquireTaskLease(task, qh.leaseOwnerID()); err != nil {
			qh.log(LogLevelWarn, "lease_acquire_failed type=task id=%s error=%v", task.ID, err)
			continue
		}
		task.Attempts++

		taskCopy := *task
		work.dispatches = append(work.dispatches, dispatchItem{
			Kind:      "task",
			Task:      &taskCopy,
			WorkerID:  workerID,
			Epoch:     task.LeaseEpoch,
			ExpiresAt: safeStr(task.LeaseExpiresAt),
		})
		globalInFlight[workerID] = true
		dirty = true
		break
	}
	return dirty
}

// collectPendingNotificationDispatches acquires leases and records dispatch items (no tmux).
func (qh *QueueHandler) collectPendingNotificationDispatches(nq *model.NotificationQueue, dirty *bool, work *deferredWork) {
	for _, ntf := range nq.Notifications {
		if ntf.Status == model.StatusInProgress && ntf.LeaseExpiresAt != nil {
			if t, err := time.Parse(time.RFC3339, *ntf.LeaseExpiresAt); err == nil && t.After(qh.clock.Now()) {
				return
			}
		}
	}

	sorted := qh.dispatcher.SortPendingNotifications(nq.Notifications)
	for _, idx := range sorted {
		ntf := &nq.Notifications[idx]
		if err := qh.leaseManager.AcquireNotificationLease(ntf, qh.leaseOwnerID()); err != nil {
			qh.log(LogLevelWarn, "lease_acquire_failed type=notification id=%s error=%v", ntf.ID, err)
			continue
		}
		ntf.Attempts++
		*dirty = true

		ntfCopy := *ntf
		work.dispatches = append(work.dispatches, dispatchItem{
			Kind:         "notification",
			Notification: &ntfCopy,
			Epoch:        ntf.LeaseEpoch,
			ExpiresAt:    safeStr(ntf.LeaseExpiresAt),
		})
		break
	}
}

// collectExpiredTaskBusyChecks records busy check items for expired task leases.
// Malformed entries (lease_expires_at == nil) are released immediately since
// Phase C fencing would always reject them as stale.
func (qh *QueueHandler) collectExpiredTaskBusyChecks(tq *taskQueueEntry, agentID, queueFile string, dirty *bool) []busyCheckItem {
	var items []busyCheckItem
	expired := qh.leaseManager.ExpireTasks(tq.Queue.Tasks)
	for _, idx := range expired {
		task := &tq.Queue.Tasks[idx]
		// Malformed entry: no lease_expires_at → release immediately.
		// Phase C fencing requires ExpiresAt match, which can never succeed for nil.
		if task.LeaseExpiresAt == nil {
			qh.log(LogLevelWarn, "expire_release_malformed type=task id=%s (nil lease_expires_at)", task.ID)
			if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=task id=%s error=%v", task.ID, err)
			}
			qh.scanCounters.LeaseReleases++
			*dirty = true
			continue
		}
		if agentID != "" {
			items = append(items, busyCheckItem{
				Kind:      "task",
				EntryID:   task.ID,
				AgentID:   agentID,
				Epoch:     task.LeaseEpoch,
				QueueFile: queueFile,
				UpdatedAt: task.UpdatedAt,
				ExpiresAt: *task.LeaseExpiresAt,
			})
		} else {
			// No agent ID: release immediately
			if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=task id=%s error=%v", task.ID, err)
			}
			qh.scanCounters.LeaseReleases++
			*dirty = true
		}
	}
	return items
}

// preemptiveCommandRenewal renews command leases approaching expiry to prevent
// the expire→detect→auto-extend cycle and avoid triggering recovery mode.
func (qh *QueueHandler) preemptiveCommandRenewal(cq *model.CommandQueue, dirty *bool) {
	bufferSec := qh.config.Watcher.ScanIntervalSec + 30
	if bufferSec <= 30 {
		bufferSec = 90
	}
	renewable := qh.leaseManager.RenewableCommands(cq.Commands, bufferSec)
	for _, idx := range renewable {
		cmd := &cq.Commands[idx]
		maxMin := qh.config.Watcher.MaxInProgressMin
		if maxMin <= 0 {
			maxMin = 60
		}
		if t, err := time.Parse(time.RFC3339, cmd.UpdatedAt); err == nil {
			if qh.clock.Now().Sub(t) >= time.Duration(maxMin)*time.Minute {
				qh.log(LogLevelWarn, "command_lease_max_timeout id=%s epoch=%d max=%dm releasing (preemptive)",
					cmd.ID, cmd.LeaseEpoch, maxMin)
				if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "expire_release_failed type=command id=%s error=%v", cmd.ID, err)
				}
				qh.scanCounters.LeaseReleases++
				*dirty = true
				continue
			}
		}
		if err := qh.leaseManager.ExtendCommandLease(cmd); err != nil {
			qh.log(LogLevelError, "command_lease_preemptive_renew_failed id=%s error=%v", cmd.ID, err)
			continue
		}
		qh.log(LogLevelDebug, "command_lease_renewed id=%s epoch=%d", cmd.ID, cmd.LeaseEpoch)
		qh.scanCounters.LeaseRenewals++
		*dirty = true
	}
}

// autoExtendExpiredCommandLeases auto-extends expired command leases in Phase A.
// Unlike tasks, commands are never released on lease expiry because:
//   - Planner is a singleton; releasing causes duplicate dispatch
//   - Busy-check false negatives are common (Planner has long API call intervals)
//   - Reconciler R0 handles truly stuck planning via max_in_progress_min timeout
//
// Malformed entries (lease_expires_at == nil) are repaired by setting a new lease.
func (qh *QueueHandler) autoExtendExpiredCommandLeases(cq *model.CommandQueue, dirty *bool) {
	expired := qh.leaseManager.ExpireCommands(cq.Commands)
	for _, idx := range expired {
		cmd := &cq.Commands[idx]

		// Check max_in_progress_min hard timeout — if exceeded, release to let
		// Reconciler R0 handle the stuck command on next scan.
		maxMin := qh.config.Watcher.MaxInProgressMin
		if maxMin <= 0 {
			maxMin = 60
		}
		if t, err := time.Parse(time.RFC3339, cmd.UpdatedAt); err == nil {
			if qh.clock.Now().Sub(t) >= time.Duration(maxMin)*time.Minute {
				qh.log(LogLevelWarn, "command_lease_max_timeout id=%s epoch=%d max=%dm releasing",
					cmd.ID, cmd.LeaseEpoch, maxMin)
				if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "expire_release_failed type=command id=%s error=%v", cmd.ID, err)
				}
				qh.scanCounters.LeaseReleases++
				*dirty = true
				continue
			}
		}

		// Auto-extend: keep command in_progress to prevent duplicate dispatch
		if cmd.LeaseExpiresAt == nil {
			qh.log(LogLevelWarn, "expire_repair_malformed type=command id=%s (nil lease_expires_at)", cmd.ID)
		}
		if err := qh.leaseManager.ExtendCommandLease(cmd); err != nil {
			qh.log(LogLevelError, "command_lease_auto_extend_failed id=%s error=%v", cmd.ID, err)
			continue
		}
		qh.log(LogLevelDebug, "command_lease_auto_extend id=%s epoch=%d", cmd.ID, cmd.LeaseEpoch)
		qh.scanCounters.LeaseExtensions++
		*dirty = true
	}
}

// checkPendingDependencyFailuresDeferred checks pending tasks for dependency failures.
// Same as checkPendingDependencyFailures but compatible with deferred interrupt pattern.
func (qh *QueueHandler) checkPendingDependencyFailuresDeferred(tq *taskQueueEntry, workerID string) (bool, []interruptItem) {
	dirty := false
	var cancelledResults []CancelledTaskResult

	for i := range tq.Queue.Tasks {
		task := &tq.Queue.Tasks[i]
		if task.Status != model.StatusPending {
			continue
		}

		failedDep, failedStatus, err := qh.dependencyResolver.CheckDependencyFailure(task)
		if err != nil || failedDep == "" {
			continue
		}

		reason := fmt.Sprintf("blocked_dependency_terminal:%s", failedDep)
		qh.log(LogLevelWarn, "dependency_failure_pending task=%s dep=%s dep_status=%s",
			task.ID, failedDep, failedStatus)

		task.Status = model.StatusCancelled
		task.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
		dirty = true

		if qh.dependencyResolver.stateReader != nil {
			if err := qh.dependencyResolver.stateReader.UpdateTaskState(task.CommandID, task.ID, model.StatusCancelled, reason); err != nil {
				qh.log(LogLevelWarn, "dep_failure_state_update task=%s error=%v", task.ID, err)
			}
		}

		cancelledResults = append(cancelledResults, CancelledTaskResult{
			TaskID:    task.ID,
			CommandID: task.CommandID,
			Status:    "cancelled",
			Reason:    reason,
		})
	}

	if len(cancelledResults) > 0 && workerID != "" {
		qh.cancelHandler.WriteSyntheticResults(cancelledResults, workerID)
		qh.scanCounters.TasksCancelled += len(cancelledResults)
	}
	return dirty, nil // pending tasks have no interrupt items
}

// checkInProgressDependencyFailuresDeferred checks in-progress tasks and defers interrupts.
func (qh *QueueHandler) checkInProgressDependencyFailuresDeferred(tq *taskQueueEntry, workerID string) (bool, []interruptItem) {
	dirty := false
	var cancelledResults []CancelledTaskResult
	var interrupts []interruptItem

	for i := range tq.Queue.Tasks {
		task := &tq.Queue.Tasks[i]
		if task.Status != model.StatusInProgress {
			continue
		}

		if qh.leaseManager.IsLeaseExpired(task.LeaseExpiresAt) {
			continue
		}

		failedDep, failedStatus, err := qh.dependencyResolver.CheckDependencyFailure(task)
		if err != nil || failedDep == "" {
			continue
		}

		reason := fmt.Sprintf("blocked_dependency_terminal:%s", failedDep)
		qh.log(LogLevelWarn, "dependency_failure task=%s dep=%s dep_status=%s",
			task.ID, failedDep, failedStatus)

		// Defer interrupt to Phase B
		if workerID != "" {
			interrupts = append(interrupts, interruptItem{
				WorkerID:  workerID,
				TaskID:    task.ID,
				CommandID: task.CommandID,
				Epoch:     task.LeaseEpoch,
			})
		}

		task.Status = model.StatusCancelled
		task.LeaseOwner = nil
		task.LeaseExpiresAt = nil
		task.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
		dirty = true

		if qh.dependencyResolver.stateReader != nil {
			if err := qh.dependencyResolver.stateReader.UpdateTaskState(task.CommandID, task.ID, model.StatusCancelled, reason); err != nil {
				qh.log(LogLevelWarn, "dep_failure_state_update task=%s error=%v", task.ID, err)
			}
		}

		cancelledResults = append(cancelledResults, CancelledTaskResult{
			TaskID:    task.ID,
			CommandID: task.CommandID,
			Status:    "cancelled",
			Reason:    reason,
		})
	}

	if len(cancelledResults) > 0 && workerID != "" {
		qh.cancelHandler.WriteSyntheticResults(cancelledResults, workerID)
		qh.scanCounters.TasksCancelled += len(cancelledResults)
	}
	return dirty, interrupts
}
