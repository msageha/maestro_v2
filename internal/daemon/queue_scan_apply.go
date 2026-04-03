package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// --- Phase C apply methods ---

func (qh *QueueHandler) applyCommandDispatchResult(dr dispatchResult, cq *model.CommandQueue, dirty *bool) {
	for i := range cq.Commands {
		cmd := &cq.Commands[i]
		if cmd.ID != dr.Item.Command.ID {
			continue
		}
		// Epoch fencing: verify entry hasn't changed since Phase A
		if isFenceStale(cmd.Status, cmd.LeaseEpoch, cmd.LeaseExpiresAt, dr.Item.Epoch, dr.Item.ExpiresAt) {
			qh.log(LogLevelWarn, "dispatch_fence_stale kind=command id=%s epoch=%d/%d",
				cmd.ID, cmd.LeaseEpoch, dr.Item.Epoch)
			return
		}
		if !dr.Success {
			// For transient busy detection errors, release lease to allow immediate retry
			if errors.Is(dr.Error, agent.ErrBusyUndecided) {
				qh.log(LogLevelWarn, "dispatch_failed_undecided_release type=command id=%s", cmd.ID)
				if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "release_command_lease_failed id=%s error=%v", cmd.ID, err)
				} else {
					qh.scanCounters.LeaseReleases++
				}
				*dirty = true
				return
			}

			qh.log(LogLevelWarn, "dispatch_failed_lease_kept type=command id=%s error=%v", cmd.ID, dr.Error)
		} else {
			qh.scanCounters.CommandsDispatched++
		}
		*dirty = true
		return
	}
}

func (qh *QueueHandler) applyTaskDispatchResult(dr dispatchResult, taskQueues map[string]*taskQueueEntry, taskDirty map[string]bool) {
	for queueFile, tq := range taskQueues {
		for i := range tq.Queue.Tasks {
			task := &tq.Queue.Tasks[i]
			if task.ID != dr.Item.Task.ID {
				continue
			}
			if isFenceStale(task.Status, task.LeaseEpoch, task.LeaseExpiresAt, dr.Item.Epoch, dr.Item.ExpiresAt) {
				qh.log(LogLevelWarn, "dispatch_fence_stale kind=task id=%s epoch=%d/%d",
					task.ID, task.LeaseEpoch, dr.Item.Epoch)
				return
			}
			if !dr.Success {
				qh.log(LogLevelWarn, "dispatch_failed type=task id=%s error=%v", task.ID, dr.Error)
				if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
					qh.log(LogLevelError, "release_task_lease task=%s error=%v", task.ID, err)
				}
				qh.scanCounters.LeaseReleases++
			} else {
				qh.scanCounters.TasksDispatched++
			}
			taskDirty[queueFile] = true
			return
		}
	}
}

func (qh *QueueHandler) applyNotificationDispatchResult(dr dispatchResult, nq *model.NotificationQueue, dirty *bool) {
	for i := range nq.Notifications {
		ntf := &nq.Notifications[i]
		if ntf.ID != dr.Item.Notification.ID {
			continue
		}
		if isFenceStale(ntf.Status, ntf.LeaseEpoch, ntf.LeaseExpiresAt, dr.Item.Epoch, dr.Item.ExpiresAt) {
			qh.log(LogLevelWarn, "dispatch_fence_stale kind=notification id=%s epoch=%d/%d",
				ntf.ID, ntf.LeaseEpoch, dr.Item.Epoch)
			return
		}
		if !dr.Success {
			qh.log(LogLevelWarn, "dispatch_failed type=notification id=%s error=%v", ntf.ID, dr.Error)
			if err := qh.leaseManager.ReleaseNotificationLease(ntf); err != nil {
				qh.log(LogLevelError, "release_notification_lease id=%s error=%v", ntf.ID, err)
			}
		} else {
			ntf.Status = model.StatusCompleted
			ntf.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
		}
		*dirty = true
		return
	}
}

// busyCheckOps abstracts lease operations for the unified busy check logic.
// Each field corresponds to a type-specific lease operation (task vs command).
type busyCheckOps struct {
	kind         string                      // "task" or "command"
	ownerLabel   string                      // e.g., "worker=worker1" or "owner=planner"
	releaseLease func() error
	extendLease  func() error
	extendGrace  func(time.Duration) error
	markDirty    func()
}

// applyBusyCheckCore contains the shared busy-check-result processing logic
// for both task and command entries. Callers provide type-specific operations
// via busyCheckOps callbacks.
func (qh *QueueHandler) applyBusyCheckCore(bc busyCheckResult, entryID string, status model.Status, leaseEpoch int, leaseExpiresAt *string, ops busyCheckOps) {
	if isFenceStale(status, leaseEpoch, leaseExpiresAt, bc.Item.Epoch, bc.Item.ExpiresAt) {
		qh.log(LogLevelWarn, "busy_check_fence_stale kind=%s id=%s epoch=%d/%d",
			ops.kind, entryID, leaseEpoch, bc.Item.Epoch)
		return
	}

	// Undecided: apply grace lease extension with shorter TTL to prevent
	// expired lease from triggering recovery mode and blocking new dispatches.
	// Still respect max_in_progress_min hard timeout to avoid infinite grace renewals.
	if bc.Undecided {
		maxMin := qh.config.Watcher.EffectiveMaxInProgressMin()
		if isMaxInProgressTimeout(qh.clock.Now(), bc.Item.UpdatedAt, maxMin) {
			qh.log(LogLevelWarn, "lease_undecided_max_timeout type=%s id=%s %s max=%dm, releasing",
				ops.kind, entryID, ops.ownerLabel, maxMin)
			if err := ops.releaseLease(); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=%s id=%s error=%v", ops.kind, entryID, err)
				return
			}
			qh.scanCounters.LeaseReleases++
			ops.markDirty()
			return
		}
		graceTTL := qh.leaseManager.GraceLeaseTTL(qh.config.Watcher.ScanIntervalSec)
		qh.log(LogLevelInfo, "lease_grace_extend type=%s id=%s %s epoch=%d grace_ttl=%s",
			ops.kind, entryID, ops.ownerLabel, leaseEpoch, graceTTL)
		if err := ops.extendGrace(graceTTL); err != nil {
			qh.log(LogLevelError, "lease_grace_extend_failed type=%s id=%s error=%v", ops.kind, entryID, err)
		}
		qh.scanCounters.LeaseExtensions++
		ops.markDirty()
		return
	}

	if bc.Busy {
		maxMin := qh.config.Watcher.EffectiveMaxInProgressMin()
		if !isMaxInProgressTimeout(qh.clock.Now(), bc.Item.UpdatedAt, maxMin) {
			qh.log(LogLevelInfo, "lease_extend_busy type=%s id=%s %s epoch=%d",
				ops.kind, entryID, ops.ownerLabel, leaseEpoch)
			if err := ops.extendLease(); err != nil {
				qh.log(LogLevelError, "lease_extend_failed type=%s id=%s error=%v", ops.kind, entryID, err)
			}
			qh.scanCounters.LeaseExtensions++
			ops.markDirty()
			return
		}
		qh.log(LogLevelWarn, "lease_max_in_progress_timeout type=%s id=%s %s max=%dm",
			ops.kind, entryID, ops.ownerLabel, maxMin)
	}

	if err := ops.releaseLease(); err != nil {
		qh.log(LogLevelError, "expire_release_failed type=%s id=%s error=%v", ops.kind, entryID, err)
		return
	}
	qh.scanCounters.LeaseReleases++
	ops.markDirty()
}

func (qh *QueueHandler) applyTaskBusyCheckResult(bc busyCheckResult, taskQueues map[string]*taskQueueEntry, taskDirty map[string]bool) {
	tq, ok := taskQueues[bc.Item.QueueFile]
	if !ok {
		return
	}
	for i := range tq.Queue.Tasks {
		task := &tq.Queue.Tasks[i]
		if task.ID != bc.Item.EntryID {
			continue
		}
		qh.applyBusyCheckCore(bc, task.ID, task.Status, task.LeaseEpoch, task.LeaseExpiresAt, busyCheckOps{
			kind:         "task",
			ownerLabel:   fmt.Sprintf("worker=%s", bc.Item.AgentID),
			releaseLease: func() error { return qh.leaseManager.ReleaseTaskLease(task) },
			extendLease:  func() error { return qh.leaseManager.ExtendTaskLease(task) },
			extendGrace:  func(ttl time.Duration) error { return qh.leaseManager.ExtendTaskLeaseGrace(task, ttl) },
			markDirty:    func() { taskDirty[bc.Item.QueueFile] = true },
		})
		return
	}
}

func (qh *QueueHandler) applyCommandBusyCheckResult(bc busyCheckResult, cq *model.CommandQueue, dirty *bool) {
	for i := range cq.Commands {
		cmd := &cq.Commands[i]
		if cmd.ID != bc.Item.EntryID {
			continue
		}
		qh.applyBusyCheckCore(bc, cmd.ID, cmd.Status, cmd.LeaseEpoch, cmd.LeaseExpiresAt, busyCheckOps{
			kind:         "command",
			ownerLabel:   "owner=planner",
			releaseLease: func() error { return qh.leaseManager.ReleaseCommandLease(cmd) },
			extendLease:  func() error { return qh.leaseManager.ExtendCommandLease(cmd) },
			extendGrace:  func(ttl time.Duration) error { return qh.leaseManager.ExtendCommandLeaseGrace(cmd, ttl) },
			markDirty:    func() { *dirty = true },
		})
		return
	}
}

func (qh *QueueHandler) applySignalResults(results []signalDeliveryResult, sq *model.PlannerSignalQueue, dirty *bool) {
	now := qh.clock.Now().UTC()
	var retained []model.PlannerSignal
	matched := make([]bool, len(results))

	for _, sig := range sq.Signals {
		var delivered bool
		var dlErr error
		for j, r := range results {
			if matched[j] {
				continue
			}
			if r.Item.CommandID == sig.CommandID &&
				r.Item.PhaseID == sig.PhaseID &&
				r.Item.Kind == sig.Kind {
				delivered = true
				matched[j] = true
				if !r.Success {
					dlErr = r.Error
				}
				break
			}
		}

		if !delivered {
			retained = append(retained, sig)
			continue
		}

		attemptTime := now.Format(time.RFC3339)
		sig.LastAttemptAt = &attemptTime
		sig.Attempts++
		sig.UpdatedAt = now.Format(time.RFC3339)

		if dlErr == nil {
			qh.log(LogLevelInfo, "signal_delivered kind=%s command=%s phase=%s attempts=%d",
				sig.Kind, sig.CommandID, sig.PhaseID, sig.Attempts)
			qh.scanCounters.SignalDeliveries++
			*dirty = true
			continue
		}

		errStr := dlErr.Error()
		sig.LastError = &errStr
		nextAttempt := qh.computeSignalBackoff(sig.Attempts)
		nextAttemptStr := now.Add(nextAttempt).Format(time.RFC3339)
		sig.NextAttemptAt = &nextAttemptStr
		*dirty = true

		qh.log(LogLevelWarn, "signal_delivery_failed kind=%s command=%s phase=%s attempts=%d next_retry=%s error=%v",
			sig.Kind, sig.CommandID, sig.PhaseID, sig.Attempts, nextAttemptStr, dlErr)
		qh.scanCounters.SignalRetries++

		retained = append(retained, sig)
	}

	sq.Signals = retained
}

func (qh *QueueHandler) recoverExpiredNotificationLeases(nq *model.NotificationQueue, dirty *bool) {
	expired := qh.leaseManager.ExpireNotifications(nq.Notifications)
	for _, idx := range expired {
		ntf := &nq.Notifications[idx]

		// Notifications don't have ExtendLease — always release and let retry.
		// No busy probe here: this runs under scanMu.Lock (Phase A) and must stay fast.
		if err := qh.leaseManager.ReleaseNotificationLease(ntf); err != nil {
			qh.log(LogLevelError, "expire_release_failed type=notification id=%s error=%v", ntf.ID, err)
			continue
		}
		*dirty = true
	}
}
