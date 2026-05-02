package daemon

import (
	"fmt"
	"slices"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/paneactivity"
	"github.com/msageha/maestro_v2/internal/model"
)

// detachTaskSlices replaces slice fields on a Task with independent copies,
// breaking shared backing arrays with the queue's in-memory data.
// The Task pointer itself is unchanged (batch 2 pointer reference preserved);
// only slice headers are swapped to new allocations.
func detachTaskSlices(t *model.Task) {
	t.DefinitionOfDone = slices.Clone(t.DefinitionOfDone)
	t.Constraints = slices.Clone(t.Constraints)
	t.BlockedBy = slices.Clone(t.BlockedBy)
	t.ToolsHint = slices.Clone(t.ToolsHint)
	t.SkillRefs = slices.Clone(t.SkillRefs)
	t.ExpectedPaths = slices.Clone(t.ExpectedPaths)
	if t.DefinitionOfAbort != nil && len(t.DefinitionOfAbort.ExplicitFailureConditions) > 0 {
		doa := *t.DefinitionOfAbort
		doa.ExplicitFailureConditions = slices.Clone(t.DefinitionOfAbort.ExplicitFailureConditions)
		t.DefinitionOfAbort = &doa
	}
}

// --- Collect methods for Phase A ---

// collectPendingCommandDispatches acquires leases and records dispatch items (no tmux).
//
// Concurrency guard: an in_progress command blocks new dispatches because the
// Planner pane processes one command's instructions at a time and a second
// dispatch would interrupt it.
//
// Planner-idle exception (added 2026-05-01): a command can be in_progress yet
// require zero Planner attention — for example, every required task has
// reached paused_for_replan and is waiting for the daemon's R10 deadletter
// path or for a downstream operator decision. In that case the Planner pane
// is genuinely idle and the in_progress hold pointlessly blocks every
// subsequent command for up to the deadletter window. The guard now skips
// commands whose state shows the Planner has run out of authored tasks,
// allowing fresh commands to dispatch while the deferred command waits its
// turn for the deadletter to escalate (or for a manual resume).
//
// Expired leases are handled by busy-check recovery (auto-extend for
// commands) and Reconciler R0 for stuck planning.
func (qh *QueueHandler) collectPendingCommandDispatches(cq *model.CommandQueue, dirty *bool, work *deferredWork) {
	// Skip dispatch when tmux session is lost — delivery would fail.
	if qh.sessionLost != nil && qh.sessionLost.Load() {
		qh.log(LogLevelDebug, "session_lost_guard: skipping command dispatch")
		return
	}

	for i := range cq.Commands {
		cmd := &cq.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		if qh.isCommandPlannerIdle(cmd.ID) {
			qh.log(LogLevelDebug,
				"command_in_progress_planner_idle id=%s epoch=%d (deferred to daemon recovery; not blocking dispatch)",
				cmd.ID, cmd.LeaseEpoch)
			continue
		}
		qh.log(LogLevelDebug, "command_in_progress_guard id=%s epoch=%d blocking_dispatch", cmd.ID, cmd.LeaseEpoch)
		return
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

		work.dispatches = append(work.dispatches, dispatchItem{
			Kind:      "command",
			Command:   cmd,
			Epoch:     cmd.LeaseEpoch,
			ExpiresAt: safeStr(cmd.LeaseExpiresAt),
		})
		break
	}
}

// collectPendingTaskDispatches acquires leases and records dispatch items (no tmux).
// inFlightPaths contains expected_paths of all currently in-progress tasks across
// all queues, used to prevent dispatching tasks with overlapping file paths.
func (qh *QueueHandler) collectPendingTaskDispatches(tq *taskQueueEntry, workerID string, globalInFlight map[string]bool, inFlightPaths []inFlightPathEntry, work *deferredWork) bool {
	// Skip dispatch when tmux session is lost — delivery would fail.
	if qh.sessionLost != nil && qh.sessionLost.Load() {
		qh.log(LogLevelDebug, "session_lost_guard: skipping task dispatch for worker=%s", workerID)
		return false
	}

	dirty := false
	sorted := qh.dispatcher.SortPendingTasks(tq.Queue.Tasks)

	for _, idx := range sorted {
		task := &tq.Queue.Tasks[idx]

		if globalInFlight[workerID] {
			qh.log(LogLevelDebug, "worker_busy worker=%s task=%s (global in-flight)", workerID, task.ID)
			break
		}

		// Cooldown period: skip tasks whose NotBefore time has not yet arrived.
		if task.NotBefore != nil {
			if notBefore, err := qh.timeCache.ParseRFC3339(*task.NotBefore); err != nil {
				qh.log(LogLevelWarn, "task_not_before_parse_failed task=%s not_before=%q error=%v (ignoring cooldown)", task.ID, *task.NotBefore, err)
			} else if qh.clock.Now().Before(notBefore) {
				qh.log(LogLevelDebug, "task_cooldown task=%s not_before=%s", task.ID, *task.NotBefore)
				continue
			}
		}

		// Fallback: check if this worker is allowed in current mode
		if qh.fallbackMgr != nil && !qh.fallbackMgr.IsWorkerAllowed(workerID) {
			qh.log(LogLevelDebug, "fallback_worker_blocked worker=%s task=%s mode=%s", workerID, task.ID, qh.fallbackMgr.Mode())
			break
		}

		// Gating order rationale:
		// Cheap, deterministic checks (dependency, system-commit readiness,
		// path-overlap) run BEFORE admission slot acquisition so that a
		// blocked/conflicting task does not transiently consume a slot for the
		// remainder of this scan, lowering effective parallelism. Admission is
		// the last gate before lease acquisition; if the lease acquire fails
		// after admission succeeded, the slot must be released to keep
		// counters in sync (see Release call below).

		if blocked, err := qh.dependencyResolver.IsTaskBlocked(task); err != nil {
			qh.log(LogLevelWarn, "dependency_check_error task=%s error=%v", task.ID, err)
			continue
		} else if blocked {
			continue
		}

		if isSysCommit, ready, sErr := qh.dependencyResolver.IsSystemCommitReady(task.CommandID, task.ID); sErr != nil {
			qh.log(LogLevelWarn, "system_commit_check_error task=%s error=%v", task.ID, sErr)
			continue
		} else if isSysCommit && !ready {
			qh.log(LogLevelDebug, "system_commit_not_ready task=%s command=%s", task.ID, task.CommandID)
			continue
		}

		if err := qh.markTaskReady(task); err != nil {
			qh.log(LogLevelWarn, "task_ready_state_update_failed task=%s command=%s error=%v",
				task.ID, task.CommandID, err)
			continue
		}

		// Path overlap check: skip tasks that would touch files already being worked on
		if conflictID, candPath, flightPath := findOverlappingTask(task, inFlightPaths); conflictID != "" {
			qh.log(LogLevelDebug, "path_overlap: delaying task %s (conflicts with in-flight task %s on path %s vs %s)",
				task.ID, conflictID, candPath, flightPath)
			continue
		}

		// Admission control: now the last gate. Captures the op so we can
		// release the slot if the subsequent lease acquire fails.
		var (
			admissionOp       admission.OpType
			admissionAcquired bool
		)
		if qh.admissionCtrl != nil {
			op := qh.admissionCtrl.ClassifyTask(task)
			if !qh.admissionCtrl.TryAcquire(op) {
				qh.log(LogLevelDebug, "admission_blocked worker=%s task=%s op=%s", workerID, task.ID, op)
				continue
			}
			admissionOp = op
			admissionAcquired = true
		}

		if err := qh.leaseManager.AcquireTaskLease(task, qh.leaseOwnerID()); err != nil {
			// Release the admission slot we just acquired — keeping it would
			// permanently leak capacity until daemon restart.
			if admissionAcquired {
				qh.admissionCtrl.Release(admissionOp)
			}
			qh.log(LogLevelWarn, "lease_acquire_failed type=task id=%s error=%v", task.ID, err)
			continue
		}
		task.Attempts++
		detachTaskSlices(task)

		// Sync command-state with the queue lease at acquisition time.
		// AcquireTaskLease has just flipped queue.task.Status to in_progress,
		// so leaving state.TaskStates[task] at `ready` would create the
		// "invalid_state_transition from=ready to=completed" audit log when
		// the worker's result_write arrives later (the §2.1 BFS handles the
		// progression internally, but the audit log is the user-visible
		// signal of the desync). markTaskDispatched is idempotent for
		// already-dispatched/running entries, so retry waves do not produce
		// repeated state writes. State-reader-less environments (legacy
		// tests) skip silently.
		if err := qh.markTaskDispatched(task); err != nil {
			qh.log(LogLevelWarn,
				"task_dispatched_state_update_failed task=%s command=%s error=%v "+
					"(queue lease acquired anyway; result_write will route via BFS but audit log will flag the lag)",
				task.ID, task.CommandID, err)
		}

		work.dispatches = append(work.dispatches, dispatchItem{
			Kind:      "task",
			Task:      task,
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
	// Skip dispatch when tmux session is lost — delivery would fail.
	if qh.sessionLost != nil && qh.sessionLost.Load() {
		qh.log(LogLevelDebug, "session_lost_guard: skipping notification dispatch")
		return
	}

	for i := range nq.Notifications {
		ntf := &nq.Notifications[i]
		if ntf.Status == model.StatusInProgress && ntf.LeaseExpiresAt != nil {
			if t, err := qh.timeCache.ParseRFC3339(*ntf.LeaseExpiresAt); err == nil && t.After(qh.clock.Now()) {
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

		work.dispatches = append(work.dispatches, dispatchItem{
			Kind:         "notification",
			Notification: ntf,
			Epoch:        ntf.LeaseEpoch,
			ExpiresAt:    safeStr(ntf.LeaseExpiresAt),
		})
		break
	}
}

// collectExpiredTaskBusyChecks records busy check items for expired task leases.
// Malformed entries (lease_expires_at == nil) are released immediately since
// Phase C fencing would always reject them as stale.
//
// Pane-activity fast path (2026-04-29 e2e refactor): before falling back to
// the heavy busy-check round trip (which sleeps 5s on its activity probe and
// is prone to false negatives during a worker's quiet "thinking" phase),
// consult paneActivity.Tracker to see whether the worker pane has changed
// across scans. If it has, the worker is alive and we extend the lease in
// place — eliminating the need for an operator-tuned dispatch_lease_sec
// that matches per-task wall-clock duration. The slow busy-check path is
// retained as the fallback for the very first lease expiry (no baseline
// snapshot yet) and for capture failures.
func (qh *QueueHandler) collectExpiredTaskBusyChecks(tq *taskQueueEntry, agentID, queueFile string, dirty *bool) []busyCheckItem {
	var items []busyCheckItem
	expired := qh.leaseManager.ExpireTasks(tq.Queue.Tasks)
	if len(expired) == 0 {
		return nil
	}

	// Compute pane activity at most once per agent per scan. The same
	// agentID feeds every entry in `expired` (one queue per worker), so
	// caching the result avoids redundant tmux capture-pane calls when a
	// worker happens to have multiple expired entries.
	var paneVerdictCached paneactivity.Verdict
	paneVerdictResolved := false
	resolvePaneVerdict := func() paneactivity.Verdict {
		if !paneVerdictResolved {
			paneVerdictCached = qh.observePaneVerdictForAgent(agentID)
			paneVerdictResolved = true
		}
		return paneVerdictCached
	}

	for _, idx := range expired {
		task := &tq.Queue.Tasks[idx]
		// Malformed entry: no lease_expires_at → release immediately.
		// Phase C fencing requires ExpiresAt match, which can never succeed for nil.
		if task.LeaseExpiresAt == nil {
			qh.log(LogLevelWarn, "expire_release_malformed type=task id=%s (nil lease_expires_at)", task.ID)
			if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=task id=%s error=%v", task.ID, err)
			}
			qh.scanExecutor.scanCounters.LeaseReleases++
			*dirty = true
			continue
		}
		if agentID == "" {
			// No agent ID: release immediately (no pane to observe).
			if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=task id=%s error=%v", task.ID, err)
			}
			qh.scanExecutor.scanCounters.LeaseReleases++
			*dirty = true
			continue
		}

		// Pane-activity fast path: extend the lease when the worker pane is
		// visibly alive. The hard cap stays in max_in_progress_min — an
		// agent that is genuinely runaway still trips the watchdog.
		//
		// 2026-04-29 follow-up: trichotomous verdict. The legacy boolean
		// returned `false` for both VerdictIdle and VerdictUncertain,
		// which collapsed two materially different cases into the same
		// busy-check fallback path. Workers reaching their first
		// lease-expiry before any baseline existed (VerdictUncertain)
		// were repeatedly re-dispatched mid-task because the busy-check
		// path then false-released them. Treating Uncertain as a
		// one-cycle grace extension lets the next scan record a real
		// baseline and judge correctly, without paying the busy-check
		// round-trip on a worker that is almost certainly alive.
		maxMin := qh.config.Watcher.EffectiveMaxInProgressMin()
		maxTimeout := isMaxInProgressTimeout(qh.clock.Now(), task.UpdatedAt, maxMin, qh.timeCache)
		// Compute elapsed-since-dispatch so the lease-extend logs surface how
		// long a worker has been holding the slot. Empty string when the
		// timestamp is missing or unparseable — a quiet degradation for
		// observability so a parse failure never blocks lease handling.
		elapsedSinceDispatch := taskElapsedSinceDispatch(task, qh.clock.Now(), qh.timeCache)
		if maxTimeout {
			// Hard cap reached: surface a single explicit log line so an
			// operator inspecting a hung worker can see why the lease is
			// being released even when the pane shows activity. Without
			// this, the busy-check fallback log obscures that the cap was
			// the trigger. The actual release happens further down via
			// the busy-check path; this is a diagnostic only.
			qh.log(LogLevelWarn,
				"lease_extend_capped_max_in_progress type=task id=%s worker=%s epoch=%d elapsed=%s max=%dm "+
					"(hard cap reached; busy-check fallback will release)",
				task.ID, agentID, task.LeaseEpoch, elapsedSinceDispatch, maxMin)
		}
		if !maxTimeout {
			switch resolvePaneVerdict() {
			case paneactivity.VerdictActive:
				err := qh.leaseManager.ExtendTaskLease(task)
				if err == nil {
					qh.log(LogLevelInfo,
						"lease_extend_pane_active type=task id=%s worker=%s epoch=%d elapsed=%s max=%dm "+
							"(busy-check skipped; pane shows cross-scan activity)",
						task.ID, agentID, task.LeaseEpoch, elapsedSinceDispatch, maxMin)
					qh.scanExecutor.scanCounters.LeaseExtensions++
					// Pane-active is an authoritative liveness signal — refresh
					// circuit_breaker.last_progress_at so the progress-timeout
					// path does not trip a command whose worker is visibly
					// working but whose tasks legitimately take longer than
					// progress_timeout_minutes (Bug-N: a single 30-min task
					// running e2e + build + lint + typecheck got cancelled
					// even though lease_extend_pane_active was firing every
					// scan).
					if qh.circuitBreaker != nil && task.CommandID != "" {
						if cbErr := qh.circuitBreaker.MarkProgress(task.CommandID); cbErr != nil {
							qh.log(LogLevelDebug,
								"circuit_breaker_mark_progress task=%s command=%s error=%v "+
									"(pane-active extension still applied; bookkeeping skipped)",
								task.ID, task.CommandID, cbErr)
						}
					}
					*dirty = true
					continue
				}
				qh.log(LogLevelWarn,
					"lease_extend_pane_active_failed type=task id=%s worker=%s error=%v "+
						"(falling back to busy-check path)",
					task.ID, agentID, err)
			case paneactivity.VerdictUncertain:
				err := qh.leaseManager.ExtendTaskLease(task)
				if err == nil {
					qh.log(LogLevelInfo,
						"lease_extend_pane_uncertain type=task id=%s worker=%s epoch=%d elapsed=%s max=%dm "+
							"(no baseline yet; grace-extending one cycle so next scan can judge)",
						task.ID, agentID, task.LeaseEpoch, elapsedSinceDispatch, maxMin)
					qh.scanExecutor.scanCounters.LeaseExtensions++
					*dirty = true
					continue
				}
				qh.log(LogLevelWarn,
					"lease_extend_pane_uncertain_failed type=task id=%s worker=%s error=%v "+
						"(falling back to busy-check path)",
					task.ID, agentID, err)
			case paneactivity.VerdictIdle:
				qh.log(LogLevelDebug,
					"pane_idle_busy_check type=task id=%s worker=%s epoch=%d elapsed=%s "+
						"(pane has not changed across scans; falling back to busy-check probe)",
					task.ID, agentID, task.LeaseEpoch, elapsedSinceDispatch)
			case paneactivity.VerdictUnknown:
				// Tracker not wired or pane lookup failed — keep legacy
				// busy-check semantics so non-tmux test environments stay
				// deterministic.
			}
		}

		items = append(items, busyCheckItem{
			Kind:      "task",
			EntryID:   task.ID,
			AgentID:   agentID,
			Epoch:     task.LeaseEpoch,
			QueueFile: queueFile,
			UpdatedAt: task.UpdatedAt,
			ExpiresAt: *task.LeaseExpiresAt,
		})
	}
	return items
}

// observePaneVerdictForAgent captures the worker pane content once and
// asks paneActivity.Tracker for the cross-scan activity verdict. Returns
// VerdictUnknown on any failure (no tracker wired, pane not found,
// capture error) so the caller proceeds with the conservative
// busy-check fallback. Callers MUST distinguish VerdictUncertain from
// VerdictIdle — see the trichotomy in paneactivity.Verdict and the
// 2026-04-29 grace-extension rationale in collectExpiredTaskBusyChecks.
func (qh *QueueHandler) observePaneVerdictForAgent(agentID string) paneactivity.Verdict {
	if qh.paneActivity == nil || qh.paneCapture == nil || agentID == "" {
		return paneactivity.VerdictUnknown
	}
	paneTarget, err := qh.findPaneTarget(agentID)
	if err != nil || paneTarget == "" {
		return paneactivity.VerdictUnknown
	}
	content, err := qh.paneCapture(paneTarget)
	if err != nil {
		return paneactivity.VerdictUnknown
	}
	// minPrevAge guards against treating a same-scan re-capture as a
	// cross-scan delta. Half the configured scan interval is a safe
	// floor: we never expect two RecordObservation calls within that
	// window for the same agent.
	scanInterval := time.Duration(qh.config.Watcher.ScanIntervalSec) * time.Second
	minPrevAge := scanInterval / 2
	if minPrevAge < time.Second {
		minPrevAge = time.Second
	}
	return qh.paneActivity.ObserveVerdict(agentID, content, minPrevAge, qh.clock.Now().UTC())
}

// preemptiveCommandRenewal renews command leases approaching expiry to prevent
// the expire→detect→auto-extend cycle and avoid triggering recovery mode.
func (qh *QueueHandler) preemptiveCommandRenewal(cq *model.CommandQueue, dirty *bool, taskQueues map[string]*taskQueueEntry) {
	bufferSec := qh.config.Watcher.ScanIntervalSec + 30
	if bufferSec <= 30 {
		bufferSec = 90
	}
	renewable := qh.leaseManager.RenewableCommands(cq.Commands, bufferSec)
	for _, idx := range renewable {
		cmd := &cq.Commands[idx]
		maxMin := qh.config.Watcher.EffectiveMaxInProgressMin()
		if isMaxInProgressTimeout(qh.clock.Now(), cmd.UpdatedAt, maxMin, qh.timeCache) {
			// 2026-04-30 e2e regression: cmd.UpdatedAt is not refreshed
			// when the Planner makes progress (filling tasks, dispatching
			// workers, dry-running plan_submit), so a long-running command
			// can cross the max_in_progress_min threshold while either
			// live workers are still chipping away OR the Planner is
			// actively filling/finalising a phase. Releasing the command
			// then re-dispatches it to Planner as a brand new epoch,
			// destroying the work in flight (the user reproduced this
			// with a 30+ minute fix phase whose Planner had passed
			// dry-run and was about to submit).
			//
			// R0b (filling_stuck) and R6 (fill_timeout) reconcilers
			// handle the truly-stuck cases on their own dedicated
			// timeouts, so the right behaviour here is "extend while
			// any sign of activity exists" rather than "release on the
			// first hard-timeout boundary".
			if qh.commandHasActivePlannerWork(taskQueues, cmd.ID) {
				if err := qh.leaseManager.ExtendCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "command_lease_extend_on_active_failed id=%s error=%v", cmd.ID, err)
					continue
				}
				qh.log(LogLevelDebug,
					"command_lease_max_timeout_extended id=%s epoch=%d max=%dm (Planner or workers still active; deferring release; normal for long-running commands)",
					cmd.ID, cmd.LeaseEpoch, maxMin)
				qh.scanExecutor.scanCounters.LeaseRenewals++
				*dirty = true
				continue
			}
			qh.log(LogLevelWarn, "command_lease_max_timeout id=%s epoch=%d max=%dm releasing (preemptive)",
				cmd.ID, cmd.LeaseEpoch, maxMin)
			if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=command id=%s error=%v", cmd.ID, err)
			}
			qh.scanExecutor.scanCounters.LeaseReleases++
			*dirty = true
			continue
		}
		if err := qh.leaseManager.ExtendCommandLease(cmd); err != nil {
			qh.log(LogLevelError, "command_lease_preemptive_renew_failed id=%s error=%v", cmd.ID, err)
			continue
		}
		qh.log(LogLevelDebug, "command_lease_renewed id=%s epoch=%d", cmd.ID, cmd.LeaseEpoch)
		qh.scanExecutor.scanCounters.LeaseRenewals++
		*dirty = true
	}
}

// commandHasActivePlannerWork reports whether the Planner or worker queues
// are still actively processing the command. Used by the command-lease
// max-timeout guard to distinguish "command genuinely stuck" (no live
// signal anywhere → safe to release and re-dispatch) from "command in
// flight" (release would destroy ongoing Planner deliberation or worker
// progress).
//
// Activity signals, in order of decreasing locality:
//
//  1. Any worker queue holds a non-terminal task whose CommandID matches.
//  2. Any phase belonging to the command is in a Planner-owned non-terminal
//     status (pending / awaiting_fill / filling / active). awaiting_fill and
//     filling specifically catch the "Planner is mid-thought" cases the
//     worker-queue check cannot see — there are no worker tasks yet because
//     the Planner has not finished decomposing the phase.
//
// Read failures on GetCommandPhases conservatively report "active" so a
// transient state-file read error never triggers a destructive re-dispatch.
//
// The R0b (filling_stuck) and R6 (fill_timeout) reconcilers own the
// truly-stuck detection on their own longer windows, so this helper is
// intentionally permissive.
func (qh *QueueHandler) commandHasActivePlannerWork(taskQueues map[string]*taskQueueEntry, commandID string) bool {
	if commandHasActiveWorkerTask(taskQueues, commandID) {
		return true
	}
	if !qh.dependencyResolver.HasStateReader() {
		return false
	}
	phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(commandID)
	if err != nil {
		// Read failure: extend rather than release. A re-dispatch on a
		// transient state-read error would be silently destructive.
		qh.log(LogLevelDebug,
			"command_active_check_state_read_failed command=%s error=%v (treating as active)",
			commandID, err)
		return true
	}
	for _, p := range phases {
		switch p.Status {
		case model.PhaseStatusPending, model.PhaseStatusAwaitingFill, model.PhaseStatusFilling, model.PhaseStatusActive:
			return true
		}
	}
	return false
}

// commandHasActiveWorkerTask reports whether any worker queue currently holds
// a non-terminal task whose CommandID matches commandID. Used as a fast-path
// check inside commandHasActivePlannerWork before falling back to the
// state-file read for phase status.
func commandHasActiveWorkerTask(taskQueues map[string]*taskQueueEntry, commandID string) bool {
	for _, tq := range taskQueues {
		for _, t := range tq.Queue.Tasks {
			if t.CommandID != commandID {
				continue
			}
			if !model.IsTerminal(t.Status) {
				return true
			}
		}
	}
	return false
}

// autoExtendExpiredCommandLeases auto-extends expired command leases in Phase A.
// Unlike tasks, commands are never released on lease expiry because:
//   - Planner is a singleton; releasing causes duplicate dispatch
//   - Busy-check false negatives are common (Planner has long API call intervals)
//   - Reconciler R0 handles truly stuck planning via max_in_progress_min timeout
//
// Malformed entries (lease_expires_at == nil) are repaired by setting a new lease.
func (qh *QueueHandler) autoExtendExpiredCommandLeases(cq *model.CommandQueue, dirty *bool, taskQueues map[string]*taskQueueEntry) {
	expired := qh.leaseManager.ExpireCommands(cq.Commands)
	for _, idx := range expired {
		cmd := &cq.Commands[idx]

		// Check max_in_progress_min hard timeout — if exceeded AND no
		// Planner / worker activity is detectable, release so the
		// dedicated reconcilers (R0b/R6/R10) can pick the command up
		// on their own longer windows. While any activity signal is
		// live we extend instead — see commandHasActivePlannerWork
		// for the full rationale.
		maxMin := qh.config.Watcher.EffectiveMaxInProgressMin()
		if isMaxInProgressTimeout(qh.clock.Now(), cmd.UpdatedAt, maxMin, qh.timeCache) {
			if qh.commandHasActivePlannerWork(taskQueues, cmd.ID) {
				if err := qh.leaseManager.ExtendCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "command_lease_extend_on_active_failed id=%s error=%v", cmd.ID, err)
					continue
				}
				qh.log(LogLevelDebug,
					"command_lease_max_timeout_extended id=%s epoch=%d max=%dm (Planner or workers still active; deferring release; normal for long-running commands)",
					cmd.ID, cmd.LeaseEpoch, maxMin)
				qh.scanExecutor.scanCounters.LeaseRenewals++
				*dirty = true
				continue
			}
			qh.log(LogLevelWarn, "command_lease_max_timeout id=%s epoch=%d max=%dm releasing",
				cmd.ID, cmd.LeaseEpoch, maxMin)
			if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=command id=%s error=%v", cmd.ID, err)
			}
			qh.scanExecutor.scanCounters.LeaseReleases++
			*dirty = true
			continue
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
		qh.scanExecutor.scanCounters.LeaseExtensions++
		*dirty = true
	}
}

// checkPendingDependencyFailuresDeferred checks pending tasks for dependency failures.
// Same as checkPendingDependencyFailures but compatible with deferred interrupt pattern.
func (qh *QueueHandler) checkPendingDependencyFailuresDeferred(tq *taskQueueEntry, workerID string) bool {
	dirty := false
	cancelledResults := make([]CancelledTaskResult, 0, len(tq.Queue.Tasks))

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

		if qh.dependencyResolver.HasStateReader() {
			if stateManager := qh.dependencyResolver.GetStateManager(); stateManager == nil {
				qh.log(LogLevelWarn, "dep_failure_state_manager_missing task=%s", task.ID)
			} else if err := stateManager.UpdateTaskState(task.CommandID, task.ID, model.StatusCancelled, reason); err != nil {
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
		qh.scanExecutor.scanCounters.TasksCancelled += len(cancelledResults)
	}
	return dirty // pending tasks have no interrupt items
}

// checkInProgressDependencyFailuresDeferred checks in-progress tasks and defers interrupts.
func (qh *QueueHandler) checkInProgressDependencyFailuresDeferred(tq *taskQueueEntry, workerID string) (bool, []interruptItem) {
	dirty := false
	cancelledResults := make([]CancelledTaskResult, 0, len(tq.Queue.Tasks))
	interrupts := make([]interruptItem, 0, len(tq.Queue.Tasks))

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

		// F-033: validate the in_progress→cancelled transition before mutating
		// the queue entry. The scanMu is not held here, so a parallel goroutine
		// may have already transitioned this task (e.g. R1 reconciler clearing
		// queue_write_failed). Skipping invalid transitions prevents this code
		// path from clobbering a freshly-terminal entry with cancelled.
		if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusCancelled); err != nil {
			qh.log(LogLevelWarn,
				"dependency_failure_invalid_transition task=%s from=%s to=cancelled error=%v",
				task.ID, task.Status, err)
			continue
		}

		// Defer interrupt to Phase B
		if workerID != "" {
			interrupts = append(interrupts, interruptItem{
				WorkerID:  workerID,
				TaskID:    task.ID,
				CommandID: task.CommandID,
				Epoch:     task.LeaseEpoch,
			})
		}

		// F-032: this code path intentionally bypasses lease.Manager.ReleaseTaskLease.
		// The canonical release path transitions in_progress→pending, but a
		// dependency-cancelled task is committing a TERMINAL status, so the
		// lease lifecycle is collapsed in-place — same pattern as Phase A
		// updateQueueState (see F-035 godoc). LeaseEpoch is retained so any
		// late heartbeat from the prior holder still fences correctly via the
		// canonical mismatch path. Routing through ReleaseTaskLease would
		// require an extra intermediate write (in_progress→pending→cancelled)
		// for no observable gain — `lease.Manager` tracks no per-release
		// metrics today (no counters / no audit ledger entry on release).
		task.Status = model.StatusCancelled
		task.LeaseOwner = nil
		task.LeaseExpiresAt = nil
		task.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
		dirty = true

		if qh.dependencyResolver.HasStateReader() {
			if stateManager := qh.dependencyResolver.GetStateManager(); stateManager == nil {
				qh.log(LogLevelWarn, "dep_failure_state_manager_missing task=%s", task.ID)
			} else if err := stateManager.UpdateTaskState(task.CommandID, task.ID, model.StatusCancelled, reason); err != nil {
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
		qh.scanExecutor.scanCounters.TasksCancelled += len(cancelledResults)
	}
	return dirty, interrupts
}
