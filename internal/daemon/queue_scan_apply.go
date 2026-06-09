package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/dispatch"
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
				// ErrSubmitConfirmUncertain on a command dispatch: the
				// Planner pane probe couldn't confirm the paste landed.
				// Tasks treat this as "assume running" because the worker's
				// next action is a `result write` we can detect, but for
				// commands the only "I'm running" signal is the Planner
				// itself calling `plan submit`, which writes the state
				// file. R0-dispatch eventually reverts to pending after
				// 600s (default), and that 10-minute round-trip per
				// attempt makes the retry.command_dispatch=5 budget take
				// ~50 min to dead-letter — long enough that operators
				// observe a "stuck in dispatch loop" symptom (Report
				// 2026-05-05 P1). Release the lease here so the next
				// scan retries within scan_interval (default 60s); after
				// retry.command_dispatch attempts the dead-letter
				// processor retires the command and notifies the
				// Orchestrator. The pane state may still be wedged so
				// retries probably keep failing — but they fail fast
				// instead of holding the queue slot.
				if errors.Is(dr.Error, agent.ErrSubmitConfirmUncertain) {
					qh.log(LogLevelWarn,
						"dispatch_failed_uncertain_release type=command id=%s attempts=%d "+
							"(planner-pane probe inconclusive; releasing lease so dead-letter can retire after retry.command_dispatch attempts)",
						cmd.ID, cmd.Attempts)
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
					// Destructive run_on_main/run_on_integration content is a
					// non-retryable policy violation: re-dispatching after a lease
					// release would hit the same validate failure on every scan
					// cycle, causing an infinite retry loop. Terminate the queue
					// entry directly so it stays out of subsequent scans, and log
					// at ERROR for operator review (the matched pattern is already
					// embedded in dr.Error).
					if errors.Is(dr.Error, dispatch.ErrDestructiveContentRejected) {
						qh.log(LogLevelError,
							"dispatch_blocked_destructive_content type=task id=%s command=%s reason=%v",
							task.ID, task.CommandID, dr.Error)
						err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusFailed)
						if err == nil {
							task.Status = model.StatusFailed
							task.LeaseOwner = nil
							task.LeaseExpiresAt = nil
							task.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
							qh.scanExecutor.scanCounters.LeaseReleases++
							// Write a synthetic failed result so the R1/R2
							// reconcilers can propagate the terminal status
							// to results/<worker>.yaml and state/commands/
							// <commandID>.yaml. Without this entry, the queue
							// is marked failed but no Worker ever wrote a
							// result file (the worker was never started),
							// leaving TaskStates stuck on the previous
							// in_progress/pending value because R2 only syncs
							// from result files. The synthetic write closes
							// the loop using the same downstream pipeline as
							// real worker results.
							workerID := strings.TrimSuffix(filepath.Base(queueFile), ".yaml")
							qh.writeSyntheticDestructiveResult(workerID, task.ID, task.CommandID, dr.Error.Error())
							return
						}
						// Defensive: in_progress → failed is allowed by the queue
						// graph. This branch only fires if the state machine drifts;
						// fall through to lease release so the scanner does not get
						// permanently stuck on the entry.
						qh.log(LogLevelError,
							"destructive_content_terminate_invalid task=%s from=%s to=failed reason=%v",
							task.ID, task.Status, err)
					}

					// ErrSubmitConfirmUncertain: the deliverer's submit-probe
					// budget exhausted without seeing a Claude UI marker. In
					// practice the worker has typically already received the
					// task prompt and is actively writing output by the time
					// this failure surfaces — the probe is over-cautious, not
					// the runtime. Releasing the lease in that state would
					// cause the next scan to re-dispatch the same task and the
					// worker's eventual result_write would hit FENCING_REJECT
					// because the queue entry bounced back to "pending" during
					// the lease_release window. Treat this symmetrically to
					// dispatch success: keep the lease, mark the task running,
					// count it as an "assumed" dispatch via
					// TasksDispatchedUncertain so operators can monitor probe
					// false-negative rates separately from confirmed dispatches.
					// If the worker truly didn't receive (rare), the lease will
					// expire after the dispatch lease TTL and hasExpiredLeases
					// picks it up via the standard expired-in_progress recovery
					// path.
					if errors.Is(dr.Error, agent.ErrSubmitConfirmUncertain) {
						// INFO severity: the upstream paste landed; the lease
						// stays open and the worker proceeds. The pane probe
						// was over-cautious (reported 2026-05-04 — workers
						// reliably completed the task afterwards), so emitting
						// at WARN was producing dashboard noise without
						// distinguishing real submit failures from probe
						// false-negatives.
						qh.log(LogLevelInfo,
							"dispatch_uncertain_assume_running type=task id=%s command=%s lease_epoch=%d error=%v "+
								"(lease retained; worker likely received the prompt — re-dispatch deferred until lease TTL expires)",
							task.ID, task.CommandID, task.LeaseEpoch, dr.Error)
						if err := qh.markTaskRunning(task); err != nil {
							qh.log(LogLevelError, "task_running_state_update_failed task=%s command=%s error=%v",
								task.ID, task.CommandID, err)
						}
						qh.scanExecutor.scanCounters.TasksDispatchedUncertain++
						return
					}

					qh.log(LogLevelWarn, "dispatch_failed type=task id=%s error=%v", task.ID, dr.Error)
					if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
						qh.log(LogLevelError, "release_task_lease task=%s error=%v", task.ID, err)
					}
					qh.scanExecutor.scanCounters.LeaseReleases++
				},
				onSuccess: func() {
					if err := qh.markTaskRunning(task); err != nil {
						qh.log(LogLevelError, "task_running_state_update_failed task=%s command=%s error=%v",
							task.ID, task.CommandID, err)
					}
					qh.scanExecutor.scanCounters.TasksDispatched++
				},
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
		// Snapshot status BEFORE applyBusyCheckCore so we can detect a
		// busy-check-driven release (in_progress → pending) afterwards
		// and stamp a cooldown to break the immediate re-dispatch loop.
		statusBefore := task.Status
		qh.applyBusyCheckCore(bc, task.ID, task.Status, task.LeaseEpoch, task.LeaseExpiresAt, busyCheckOps{
			kind:         "task",
			ownerLabel:   fmt.Sprintf("worker=%s", bc.Item.AgentID),
			releaseLease: func() error { return qh.leaseManager.ReleaseTaskLease(task) },
			extendLease:  func() error { return qh.leaseManager.ExtendTaskLease(task) },
			extendGrace:  func(ttl time.Duration) error { return qh.leaseManager.ExtendTaskLeaseGrace(task, ttl) },
			markDirty:    func() { taskDirty[bc.Item.QueueFile] = true },
		})

		// Hang-release cooldown: when busy-check flips a task back to
		// StatusPending, stamping NotBefore enforces a minimum quiet
		// period before another scan can re-acquire, breaking the tight
		// idle→release→re-dispatch→idle loop. Attempts is bumped so the
		// dead-letter processor terminates the entry after
		// retry.task_dispatch_attempts cycles.
		//
		// hangAttemptCost is intentionally 1 (not >1): an over-eager
		// amplifier was tested in 7th e2e (2026-05-03) and surfaced a
		// false-positive risk for long-running LLM thinking tasks where
		// the pane emits output infrequently — two such "quiet" windows
		// would push attempts past max_attempts and dead-letter a task
		// that is genuinely making progress. Keeping cost=1 means the
		// dead-letter threshold is reached only after `task_dispatch`
		// (default 5) consecutive hang releases, by which point the
		// Worker really is wedged. The cooldown still breaks the tight
		// loop, so the Worker pane gets a clean pause between attempts.
		if statusBefore == model.StatusInProgress && task.Status == model.StatusPending && !bc.Busy && !bc.Undecided {
			const (
				hangReleaseCooldown = 2 * time.Minute
				hangAttemptCost     = 1
			)
			notBefore := qh.clock.Now().Add(hangReleaseCooldown).UTC().Format(time.RFC3339)
			task.NotBefore = &notBefore
			task.Attempts += hangAttemptCost
			qh.log(LogLevelInfo,
				"task_hang_release_cooldown task=%s worker=%s attempts=%d cost=%d not_before=%s "+
					"(pane idle while in_progress; cooldown applied, dead-letter at attempts>=max_attempts)",
				task.ID, bc.Item.AgentID, task.Attempts, hangAttemptCost, notBefore)
		}
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

	// Build O(1) lookup map keyed by the FULL signal identity, including
	// WorkerID + ConflictGeneration. Per-worker signals (merge_conflict,
	// commit_failed, conflict_resolution_*) share (CommandID, PhaseID, Kind)
	// and only differ by WorkerID; keying on the triple alone collapsed two
	// per-worker results into one map entry, so the first matching signal
	// consumed it and the second was wrongly retained as "not delivered"
	// (duplicate re-delivery + misattributed retry/backoff). The key must
	// mirror signalDedupKey.
	type signalMatchKey struct {
		CommandID          string
		PhaseID            string
		Kind               string
		WorkerID           string
		ConflictGeneration string
	}
	resultMap := make(map[signalMatchKey]signalDeliveryResult, len(results))
	for _, r := range results {
		key := signalMatchKey{r.Item.CommandID, r.Item.PhaseID, r.Item.Kind, r.Item.WorkerID, r.Item.ConflictGeneration}
		resultMap[key] = r
	}

	for _, sig := range sq.Signals {
		var delivered bool
		var dlErr error
		key := signalMatchKey{sig.CommandID, sig.PhaseID, sig.Kind, sig.WorkerID, sig.ConflictGeneration}
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

		// Submit confirmation uncertain: tmux submitted the message but the
		// planner pane's read-back did not confirm landing within the
		// observation window. Allow one bounded retry: if the message
		// actually landed the first time, the Planner's reaction will
		// surface in the next scan and the second delivery turns into a
		// duplicate the Planner already absorbed. If it did not land, the
		// retry has the chance to deliver. Only after the *second*
		// uncertain result do we dead-letter — two attempts is the smallest
		// bound that still discriminates "tmux/pane glitch" (resolves next
		// tick) from "structural breakage" (every tick suspect).
		if errors.Is(dlErr, agent.ErrSubmitConfirmUncertain) {
			if sig.Attempts < 2 {
				nextAttempt := qh.computeSignalBackoff(sig.Attempts)
				nextAttemptStr := now.Add(nextAttempt).Format(time.RFC3339)
				sig.NextAttemptAt = &nextAttemptStr
				*dirty = true
				qh.log(LogLevelInfo,
					"signal_submit_confirm_uncertain_retry kind=%s command=%s phase=%s attempts=%d next_retry=%s "+
						"(submit confirmation uncertain; one bounded retry before dead-letter)",
					sig.Kind, sig.CommandID, sig.PhaseID, sig.Attempts, nextAttemptStr)
				qh.scanExecutor.scanCounters.SignalRetries++
				retained = append(retained, sig)
				continue
			}
			qh.log(LogLevelWarn,
				"signal_dead_letter_non_retryable kind=%s command=%s phase=%s attempts=%d reason=submit_confirm_uncertain (non-retryable after bounded retry; manual planner inspection required)",
				sig.Kind, sig.CommandID, sig.PhaseID, sig.Attempts)
			qh.scanExecutor.scanCounters.SignalDeadLetters++
			*dirty = true
			continue
		}

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

// writeSyntheticDestructiveResult appends a synthetic failed result entry to
// the worker's result file when a task is rejected by the run_on_main /
// run_on_integration destructive-content pre-flight (see
// dispatch.ErrDestructiveContentRejected). The Worker is never started for
// such tasks, so without a synthetic entry the result file stays empty and
// the R2ResultState reconciler — which keys off result files alone — cannot
// move TaskStates[<task>] off its prior pending/in_progress value, leaving
// the command state file out of sync with the queue.
//
// Lock ordering: takes only "result:<workerID>". The caller has already
// mutated the in-memory task queue and will flush via FlushQueues; the queue
// lock is acquired by FlushQueues, never simultaneously with the result lock,
// matching the pattern in CancelHandler.WriteSyntheticResults.
func (qh *QueueHandler) writeSyntheticDestructiveResult(workerID, taskID, commandID, reason string) {
	if workerID == "" {
		qh.log(LogLevelError,
			"synthetic_destructive_result_skipped task=%s command=%s reason=missing_worker_id",
			taskID, commandID)
		return
	}

	lockKey := "result:" + workerID
	qh.lockMap.Lock(lockKey)
	defer qh.lockMap.Unlock(lockKey)

	resultPath := resultFilePath(qh.maestroDir, workerID)
	if err := updateYAMLFile(resultPath, func(rf *model.TaskResultFile) error {
		if rf.SchemaVersion == 0 {
			rf.SchemaVersion = 1
			rf.FileType = "result_task"
		}
		resultID, err := model.GenerateID(model.IDTypeResult)
		if err != nil {
			return fmt.Errorf("generate synthetic result id: %w", err)
		}
		rf.Results = append(rf.Results, model.TaskResult{
			ID:                     resultID,
			TaskID:                 taskID,
			CommandID:              commandID,
			Status:                 model.StatusFailed,
			Summary:                fmt.Sprintf("dispatch_blocked_destructive_content: %s", reason),
			PartialChangesPossible: false,
			RetrySafe:              false,
			CreatedAt:              qh.clock.Now().UTC().Format(time.RFC3339),
		})
		return nil
	}); err != nil {
		qh.log(LogLevelError,
			"synthetic_destructive_result_write task=%s command=%s worker=%s error=%v "+
				"(queue terminal but state will lag until reconciler retries)",
			taskID, commandID, workerID, err)
	}
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
