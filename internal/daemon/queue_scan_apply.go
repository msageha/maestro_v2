package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// --- Phase C apply methods ---

// dispatchApplyOps abstracts type-specific dispatch result operations for the
// unified fence-check + success/failure logic. Follows the same callback pattern
// as busyCheckOps.
type dispatchApplyOps struct {
	kind           string
	id             string
	status         model.Status
	leaseEpoch     int
	leaseExpiresAt *string
	onFailure      func(dr dispatchResult) // type-specific failure handling
	onSuccess      func()                  // type-specific success handling
	markDirty      func()
}

// applyDispatchCore contains the shared fence-stale check, success/failure
// dispatch, and dirty-marking logic for command, task, and notification
// dispatch results.
func (qh *QueueHandler) applyDispatchCore(dr dispatchResult, ops dispatchApplyOps) {
	if rej := checkResultFencing(ops.status, ops.leaseEpoch, ops.leaseExpiresAt, dr.Item.Epoch, dr.Item.ExpiresAt); rej.Stale() {
		qh.log(LogLevelDebug, "dispatch_fence_stale kind=%s id=%s epoch=%d/%d reason=%s",
			ops.kind, ops.id, ops.leaseEpoch, dr.Item.Epoch, rej.Reason)
		return
	}
	if !dr.Success {
		ops.onFailure(dr)
	} else {
		ops.onSuccess()
	}
	ops.markDirty()
}

func (qh *QueueHandler) applyCommandDispatchResult(dr dispatchResult, cq *model.CommandQueue, dirty *bool) {
	for i := range cq.Commands {
		cmd := &cq.Commands[i]
		if cmd.ID != dr.Item.Command.ID {
			continue
		}
		qh.applyDispatchCore(dr, dispatchApplyOps{
			kind:           "command",
			id:             cmd.ID,
			status:         cmd.Status,
			leaseEpoch:     cmd.LeaseEpoch,
			leaseExpiresAt: cmd.LeaseExpiresAt,
			onFailure: func(dr dispatchResult) {
				// For transient busy detection errors, release lease to allow immediate retry
				if errors.Is(dr.Error, agent.ErrBusyUndecided) {
					qh.log(LogLevelWarn, "dispatch_failed_undecided_release type=command id=%s", cmd.ID)
					if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
						qh.log(LogLevelError, "release_command_lease_failed id=%s error=%v", cmd.ID, err)
					} else {
						qh.scanExecutor.scanCounters.LeaseReleases++
					}
					return
				}
				qh.log(LogLevelWarn, "dispatch_failed_lease_kept type=command id=%s error=%v", cmd.ID, dr.Error)
			},
			onSuccess: func() { qh.scanExecutor.scanCounters.CommandsDispatched++ },
			markDirty: func() { *dirty = true },
		})
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
			qh.applyDispatchCore(dr, dispatchApplyOps{
				kind:           "task",
				id:             task.ID,
				status:         task.Status,
				leaseEpoch:     task.LeaseEpoch,
				leaseExpiresAt: task.LeaseExpiresAt,
				onFailure: func(dr dispatchResult) {
					qh.log(LogLevelWarn, "dispatch_failed type=task id=%s error=%v", task.ID, dr.Error)
					if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
						qh.log(LogLevelError, "release_task_lease task=%s error=%v", task.ID, err)
					}
					qh.scanExecutor.scanCounters.LeaseReleases++
				},
				onSuccess: func() { qh.scanExecutor.scanCounters.TasksDispatched++ },
				markDirty: func() { taskDirty[queueFile] = true },
			})
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
		qh.applyDispatchCore(dr, dispatchApplyOps{
			kind:           "notification",
			id:             ntf.ID,
			status:         ntf.Status,
			leaseEpoch:     ntf.LeaseEpoch,
			leaseExpiresAt: ntf.LeaseExpiresAt,
			onFailure: func(dr dispatchResult) {
				qh.log(LogLevelWarn, "dispatch_failed type=notification id=%s error=%v", ntf.ID, dr.Error)
				if err := qh.leaseManager.ReleaseNotificationLease(ntf); err != nil {
					qh.log(LogLevelError, "release_notification_lease id=%s error=%v", ntf.ID, err)
				}
			},
			onSuccess: func() {
				ntf.Status = model.StatusCompleted
				ntf.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
			},
			markDirty: func() { *dirty = true },
		})
		return
	}
}

// busyCheckOps abstracts lease operations for the unified busy check logic.
// Each field corresponds to a type-specific lease operation (task vs command).
type busyCheckOps struct {
	kind         string // "task" or "command"
	ownerLabel   string // e.g., "worker=worker1" or "owner=planner"
	releaseLease func() error
	extendLease  func() error
	extendGrace  func(time.Duration) error
	markDirty    func()
}

// applyBusyCheckCore contains the shared busy-check-result processing logic
// for both task and command entries. Callers provide type-specific operations
// via busyCheckOps callbacks.
func (qh *QueueHandler) applyBusyCheckCore(bc busyCheckResult, entryID string, status model.Status, leaseEpoch int, leaseExpiresAt *string, ops busyCheckOps) {
	if rej := checkResultFencing(status, leaseEpoch, leaseExpiresAt, bc.Item.Epoch, bc.Item.ExpiresAt); rej.Stale() {
		qh.log(LogLevelDebug, "busy_check_fence_stale kind=%s id=%s epoch=%d/%d reason=%s",
			ops.kind, entryID, leaseEpoch, bc.Item.Epoch, rej.Reason)
		return
	}

	// Undecided: apply grace lease extension with shorter TTL to prevent
	// expired lease from triggering recovery mode and blocking new dispatches.
	// Still respect max_in_progress_min hard timeout to avoid infinite grace renewals.
	if bc.Undecided {
		maxMin := qh.config.Watcher.EffectiveMaxInProgressMin()
		if isMaxInProgressTimeout(qh.clock.Now(), bc.Item.UpdatedAt, maxMin, qh.timeCache) {
			qh.log(LogLevelWarn, "lease_undecided_max_timeout type=%s id=%s %s max=%dm, releasing",
				ops.kind, entryID, ops.ownerLabel, maxMin)
			if err := ops.releaseLease(); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=%s id=%s error=%v", ops.kind, entryID, err)
				return
			}
			qh.scanExecutor.scanCounters.LeaseReleases++
			ops.markDirty()
			return
		}
		// Grace lease limit: cumulative grace extensions must not exceed a fraction of max_in_progress_min
		graceLimit := maxGraceLeaseDuration(maxMin, qh.config.Watcher.ScanIntervalSec)
		dispatchDuration := time.Duration(qh.config.Watcher.DispatchLeaseSec) * time.Second
		if isGraceLeaseExceeded(qh.clock.Now(), bc.Item.UpdatedAt, dispatchDuration, graceLimit, qh.timeCache) {
			qh.log(LogLevelWarn, "lease_grace_limit_exceeded type=%s id=%s %s grace_limit=%s, releasing as stale",
				ops.kind, entryID, ops.ownerLabel, graceLimit)
			if err := ops.releaseLease(); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=%s id=%s error=%v", ops.kind, entryID, err)
				return
			}
			qh.scanExecutor.scanCounters.LeaseReleases++
			ops.markDirty()
			return
		}
		graceTTL := qh.leaseManager.GraceLeaseTTL(qh.config.Watcher.ScanIntervalSec)
		qh.log(LogLevelInfo, "lease_grace_extend type=%s id=%s %s epoch=%d grace_ttl=%s",
			ops.kind, entryID, ops.ownerLabel, leaseEpoch, graceTTL)
		if err := ops.extendGrace(graceTTL); err != nil {
			qh.log(LogLevelError, "lease_grace_extend_failed type=%s id=%s error=%v", ops.kind, entryID, err)
		}
		qh.scanExecutor.scanCounters.LeaseExtensions++
		ops.markDirty()
		return
	}

	if bc.Busy {
		maxMin := qh.config.Watcher.EffectiveMaxInProgressMin()
		if !isMaxInProgressTimeout(qh.clock.Now(), bc.Item.UpdatedAt, maxMin, qh.timeCache) {
			qh.log(LogLevelInfo, "lease_extend_busy type=%s id=%s %s epoch=%d",
				ops.kind, entryID, ops.ownerLabel, leaseEpoch)
			if err := ops.extendLease(); err != nil {
				qh.log(LogLevelError, "lease_extend_failed type=%s id=%s error=%v", ops.kind, entryID, err)
			}
			qh.scanExecutor.scanCounters.LeaseExtensions++
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
	qh.scanExecutor.scanCounters.LeaseReleases++
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
	retained := make([]model.PlannerSignal, 0, len(sq.Signals))

	// Build O(1) lookup map from (CommandID, PhaseID, Kind) → result,
	// replacing the previous O(n*m) nested loop with O(n+m) lookups.
	type signalMatchKey struct {
		CommandID string
		PhaseID   string
		Kind      string
	}
	resultMap := make(map[signalMatchKey]signalDeliveryResult, len(results))
	for _, r := range results {
		key := signalMatchKey{r.Item.CommandID, r.Item.PhaseID, r.Item.Kind}
		resultMap[key] = r
	}

	for _, sig := range sq.Signals {
		var delivered bool
		var dlErr error
		key := signalMatchKey{sig.CommandID, sig.PhaseID, sig.Kind}
		if r, ok := resultMap[key]; ok {
			delivered = true
			delete(resultMap, key)
			if !r.Success {
				dlErr = r.Error
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
			qh.scanExecutor.scanCounters.SignalDeliveries++
			*dirty = true
			continue
		}

		errStr := dlErr.Error()
		sig.LastError = &errStr

		// Dead letter: drop signal after max retry attempts
		maxAttempts := qh.config.Retry.SignalDispatch
		if maxAttempts > 0 && sig.Attempts >= maxAttempts {
			qh.log(LogLevelWarn, "signal_dead_letter kind=%s command=%s phase=%s attempts=%d max=%d error=%v",
				sig.Kind, sig.CommandID, sig.PhaseID, sig.Attempts, maxAttempts, dlErr)
			qh.scanExecutor.scanCounters.SignalDeadLetters++
			*dirty = true
			continue
		}

		nextAttempt := qh.computeSignalBackoff(sig.Attempts)
		nextAttemptStr := now.Add(nextAttempt).Format(time.RFC3339)
		sig.NextAttemptAt = &nextAttemptStr
		*dirty = true

		qh.log(LogLevelWarn, "signal_delivery_failed kind=%s command=%s phase=%s attempts=%d next_retry=%s error=%v",
			sig.Kind, sig.CommandID, sig.PhaseID, sig.Attempts, nextAttemptStr, dlErr)
		qh.scanExecutor.scanCounters.SignalRetries++

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
