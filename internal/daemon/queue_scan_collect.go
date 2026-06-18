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
// Planner-idle exception: a command can be in_progress yet require zero
// Planner attention — for example, every required task has reached
// paused_for_replan and is waiting for the daemon's R10 deadletter path
// or for a downstream operator decision. The guard skips commands whose
// state shows the Planner has run out of authored tasks, allowing fresh
// commands to dispatch while the deferred command waits its turn for the
// deadletter to escalate (or for a manual resume).
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
// allTaskQueues is the full task queue map (used by the RunOnIntegration
// pre-merge gate to look up dep tasks' owning workers).
func (qh *QueueHandler) collectPendingTaskDispatches(tq *taskQueueEntry, workerID string, globalInFlight map[string]bool, inFlightPaths []inFlightPathEntry, allTaskQueues map[string]*taskQueueEntry, work *deferredWork) bool {
	// Skip dispatch when tmux session is lost — delivery would fail.
	if qh.sessionLost != nil && qh.sessionLost.Load() {
		qh.log(LogLevelDebug, "session_lost_guard: skipping task dispatch for worker=%s", workerID)
		return false
	}

	dirty := false
	sorted := qh.dispatcher.SortPendingTasks(tq.Queue.Tasks)

	// Per-call caches that amortise per-task queue walks across the loop.
	// Both maps are local to this invocation, so no cross-goroutine sync
	// is required.
	//   - commandOwnersCache: {commandID -> {taskID -> workerID}}, lazily
	//     populated by depWorkersAwaitingIntegrationMerge so each command
	//     walks the queues once instead of per-task.
	//   - workerPurposesCache: {workerID -> purpose}, lazy single build
	//     for the entire dispatch loop. The first arg of buildWorkerPurposes
	//     is unused, so the result is the same regardless of which task
	//     triggered the build.
	commandOwnersCache := make(map[string]map[string]string)
	var workerPurposesCache map[string]string
	workerPurposesBuilt := false

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

		// (Removed) Fallback degraded-mode gate: blacklisting workers on
		// consecutive failures created a deadlock — a blacklisted worker
		// can never reach `healthy` because its dispatch is suppressed,
		// so recovery never fires. Autonomous LLM Orchestration must
		// surface failures via per-task retry / repair / circuit
		// breakers, never by silencing whole workers.

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

		// Cross-worker dependency pre-merge gate. A dep produced by worker B
		// is only visible to worker A (A != B) after B's commits reach
		// integration AND A's worktree is refreshed. depWorkersAwaitingIntegrationMerge
		// returns the unmerged deps; same-worker deps and already-integrated
		// deps are skipped. Same-phase cross-worker deps cannot wait for the
		// phase merge (the phase merge requires every required task terminal),
		// so we proactively merge the dep workers and defer dispatch to a later
		// scan once the merge result is observable. Restricting this to the
		// RunOnIntegration scope previously caused silent staleness for regular
		// cross-worker deps (Report 2026-05-03 issue-2).
		if qh.worktreeManager != nil && qh.worktreeManager.HasWorktrees(task.CommandID) {
			// RunOnIntegration / RunOnMain tasks dispatch against the
			// integration worktree (or main directly), so every dep —
			// including same-worker ones — must have already landed on
			// integration. includeSameWorker=true flips the gate to enforce
			// that. Report 2026-05-04 pinned a leak where integration
			// verify ran against a stale tree because a same-worker dep had
			// committed in the worker worktree but not yet merged to
			// integration.
			includeSameWorker := task.RunOnIntegration || task.RunOnMain
			pending := qh.depWorkersAwaitingIntegrationMerge(task, allTaskQueues, workerID, includeSameWorker, commandOwnersCache)
			if len(pending) > 0 {
				qh.log(LogLevelInfo,
					"task_pre_merge_gate task=%s worker=%s run_on_integration=%v run_on_main=%v include_same_worker=%v pending_dep_workers=%v "+
						"(queuing focused commit+merge before dispatch — cross-worker dep gate)",
					task.ID, workerID, task.RunOnIntegration, task.RunOnMain, includeSameWorker, pending)
				if !workerPurposesBuilt {
					workerPurposesCache = buildWorkerPurposes(task.CommandID, allTaskQueues)
					workerPurposesBuilt = true
				}
				work.runOnIntegrationPreMerges = append(work.runOnIntegrationPreMerges, runOnIntegrationPreMergeItem{
					CommandID:      task.CommandID,
					BlockedTaskID:  task.ID,
					DepWorkerIDs:   pending,
					WorkerPurposes: workerPurposesCache,
				})
				continue
			}
		}

		if isSysCommit, ready, sErr := qh.dependencyResolver.IsSystemCommitReady(task.CommandID, task.ID); sErr != nil {
			qh.log(LogLevelWarn, "system_commit_check_error task=%s error=%v", task.ID, sErr)
			continue
		} else if isSysCommit && !ready {
			qh.log(LogLevelDebug, "system_commit_not_ready task=%s command=%s", task.ID, task.CommandID)
			continue
		}

		if err := qh.markTaskReady(task); err != nil {
			level := LogLevelWarn
			if isStateTaskNotFoundError(err) {
				// Already demoted to nil inside advanceTaskLifecycle, but
				// keep the level guard here in case a future caller
				// surfaces the same shape via a different path.
				level = LogLevelDebug
			}
			qh.log(level, "task_ready_state_update_failed task=%s command=%s error=%v",
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

// depWorkersAwaitingIntegrationMerge returns the unique set of dep workers
// whose worktree changes have not yet been merged into integration. Used
// by the cross-worker / RunOnIntegration / RunOnMain pre-merge gate so
// the orchestrator can proactively merge them before dispatching a task
// that reads from the integration tree.
//
// includeSameWorker controls the same-worker shortcut:
//   - false: deps owned by dispatchingWorkerID are skipped, because the
//     dispatching task runs in its own worker worktree where same-worker
//     prior commits are already visible without an integration round-trip.
//   - true: deps owned by dispatchingWorkerID are also probed and queued
//     when behind integration. The dispatching task runs against the
//     integration tree (RunOnIntegration / RunOnMain), so even
//     "same-worker" deps must have landed on integration. Report of
//     2026-05-04 pinned the silent-staleness regression where worker1's
//     own dependency repair was committed in the worker worktree but
//     not merged to integration before worker1's own integration verify
//     dispatched.
//
// The truth source is the live git tree, not the cached worker.Status:
// Status is set to `integrated` exactly once (after a successful phase
// merge) and is NOT reverted when the same worker subsequently produces
// new uncommitted edits in a later phase. A status-only check therefore
// silently misses the most common stale-integration scenario. We instead
// ask IsWorkerAheadOrDirty, which inspects `git status --porcelain` and
// HEAD/merge-base directly.
//
// Returns nil when:
//   - the task has no BlockedBy entries
//   - every applicable dep worker is in lockstep with integration (no
//     dirt, HEAD matches), or in conflict/resolving (resume_merge owns
//     recovery)
//
// commandOwnersCache may be passed by callers that invoke this function in
// a loop over many tasks of the same command. When non-nil, the cache is
// populated lazily with `commandID → (taskID → ownerWorkerID)`, so the
// per-command queue-walk in buildAllCommandTaskOwners is amortised across
// all tasks instead of being repeated per-task. Pass nil to fall back to
// the simple per-call buildDepTaskWorkerMap path.
func (qh *QueueHandler) depWorkersAwaitingIntegrationMerge(task *model.Task, allTaskQueues map[string]*taskQueueEntry, dispatchingWorkerID string, includeSameWorker bool, commandOwnersCache map[string]map[string]string) []string {
	if len(task.BlockedBy) == 0 || qh.worktreeManager == nil {
		return nil
	}
	var depTaskOwners map[string]string
	if commandOwnersCache != nil {
		allOwners, hit := commandOwnersCache[task.CommandID]
		if !hit {
			allOwners = buildAllCommandTaskOwners(task.CommandID, allTaskQueues)
			commandOwnersCache[task.CommandID] = allOwners
		}
		depTaskOwners = make(map[string]string, len(task.BlockedBy))
		for _, id := range task.BlockedBy {
			if w, ok := allOwners[id]; ok {
				depTaskOwners[id] = w
			}
		}
	} else {
		depTaskOwners = buildDepTaskWorkerMap(task.CommandID, task.BlockedBy, allTaskQueues)
	}
	if len(depTaskOwners) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var pending []string
	for _, depWorker := range depTaskOwners {
		if depWorker == "" {
			continue
		}
		if depWorker == dispatchingWorkerID && !includeSameWorker {
			continue
		}
		if _, dup := seen[depWorker]; dup {
			continue
		}
		seen[depWorker] = struct{}{}
		needsMerge, err := qh.worktreeManager.IsWorkerAheadOrDirty(task.CommandID, depWorker)
		if err != nil {
			// IsWorkerAheadOrDirty returns true on probe failure so the
			// gate fails closed — log but treat as pending.
			qh.log(LogLevelWarn,
				"pre_merge_gate_probe_failed task=%s dep_worker=%s error=%v",
				task.ID, depWorker, err)
		}
		if needsMerge {
			pending = append(pending, depWorker)
		}
	}
	return pending
}

// buildAllCommandTaskOwners returns {taskID -> ownerWorkerID} for every
// task with the given commandID across all queue files. Suitable for
// caching across multiple BlockedBy lookups within the same command —
// callers in a per-task loop reuse this map instead of re-walking the
// queues for every task.
func buildAllCommandTaskOwners(commandID string, taskQueues map[string]*taskQueueEntry) map[string]string {
	owners := make(map[string]string)
	for queueFile, tq := range taskQueues {
		if tq == nil {
			continue
		}
		workerID := workerIDFromPath(queueFile)
		for i := range tq.Queue.Tasks {
			t := &tq.Queue.Tasks[i]
			if t.CommandID != commandID {
				continue
			}
			owners[t.ID] = workerID
		}
	}
	return owners
}

// buildDepTaskWorkerMap returns a {depTaskID -> ownerWorkerID} map by
// scanning the queue files for each dep task ID. Tasks live in worker-owned
// queue files (`queue/worker1.yaml`, etc.); this scan is the canonical way
// to learn which worker produced a dep task.
func buildDepTaskWorkerMap(commandID string, depTaskIDs []string, taskQueues map[string]*taskQueueEntry) map[string]string {
	if len(depTaskIDs) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(depTaskIDs))
	for _, id := range depTaskIDs {
		want[id] = struct{}{}
	}
	owners := make(map[string]string, len(depTaskIDs))
	for queueFile, tq := range taskQueues {
		if tq == nil {
			continue
		}
		workerID := workerIDFromPath(queueFile)
		for i := range tq.Queue.Tasks {
			t := &tq.Queue.Tasks[i]
			if t.CommandID != commandID {
				continue
			}
			if _, ok := want[t.ID]; ok {
				owners[t.ID] = workerID
			}
		}
	}
	return owners
}

// collectExpiredTaskBusyChecks records busy check items for expired task leases.
// Malformed entries (lease_expires_at == nil) are released immediately since
// Phase C fencing would always reject them as stale.
//
// Pane-activity fast path: before falling back to the heavy busy-check
// round trip (which sleeps 5s on its activity probe and is prone to
// false negatives during a worker's quiet "thinking" phase), consult
// paneActivity.Tracker to see whether the worker pane has changed
// across scans. If it has, the worker is alive and the lease is
// extended in place — eliminating the need for an operator-tuned
// dispatch_lease_sec that matches per-task wall-clock duration. The
// slow busy-check path is retained as the fallback for the very first
// lease expiry (no baseline snapshot yet) and for capture failures.
func (qh *QueueHandler) collectExpiredTaskBusyChecks(tq *taskQueueEntry, agentID, queueFile string, dirty *bool, scanVerdicts map[string]paneactivity.Verdict) []busyCheckItem {
	var items []busyCheckItem
	expired := qh.leaseManager.ExpireTasks(tq.Queue.Tasks)
	if len(expired) == 0 {
		return nil
	}

	// Compute pane activity at most once per agent per scan. The same
	// agentID feeds every entry in `expired` (one queue per worker), so
	// caching the result avoids redundant tmux capture-pane calls when a
	// worker happens to have multiple expired entries.
	//
	// Prefer the verdict stepBlockedPaneTimeout observed earlier in this
	// scan tick: re-observing here lands within minPrevAge of that
	// observation and degrades to a same-scan VerdictUncertain, which the
	// grace-extension below then extends on every lease expiry up to the
	// 30-minute hard cap (E2E 2026-06-11: a pane whose claude process had
	// died was repeatedly "grace-extended one cycle" for 30 minutes).
	var paneVerdictCached paneactivity.Verdict
	paneVerdictResolved := false
	if v, ok := scanVerdicts[agentID]; ok {
		paneVerdictCached = v
		paneVerdictResolved = true
	}
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
		// Trichotomous verdict: collapsing VerdictIdle and VerdictUncertain
		// into the same busy-check fallback would re-dispatch workers
		// reaching their first lease-expiry before any baseline existed.
		// Treating Uncertain as a one-cycle grace extension lets the next
		// scan record a real baseline and judge correctly, without paying
		// the busy-check round-trip on a worker that is almost certainly
		// alive.
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
			case paneactivity.VerdictTerminalError:
				// Pane is showing a runtime terminal error frame
				// (Claude API 4xx, content filter, invalid_request_error,
				// …). The LLM cannot self-recover, so extending the lease
				// is wasted wall-clock — fail immediately and respawn the
				// worker pane to clear the stale TUI. Without this fast
				// path the pane stayed "active" for max_in_progress_min
				// (~30 min) before the same task was re-dispatched onto
				// the same stale TUI (Report 2026-05-06 P0-2).
				qh.log(LogLevelWarn,
					"task_failed_terminal_error id=%s worker=%s epoch=%d elapsed=%s "+
						"(pane shows runtime terminal error; failing task immediately and respawning pane)",
					task.ID, agentID, task.LeaseEpoch, elapsedSinceDispatch)
				if qh.failTaskTerminalError(task, queueFile, agentID) {
					if recErr := qh.recoverWorkerPaneAfterBlocked(workerIDFromPath(queueFile)); recErr != nil {
						qh.log(LogLevelWarn,
							"terminal_error_recovery_respawn_failed worker=%s error=%v "+
								"(task already failed; next dispatch will still launch via standard pane init)",
							workerIDFromPath(queueFile), recErr)
					}
					*dirty = true
					continue
				}
				// failTaskTerminalError returned false → state machine
				// drift; fall through to the usual lease-handling path
				// instead of stalling on this entry.
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
			case paneactivity.VerdictBlocked:
				// Pane is alive (rendering an approval/confirmation prompt)
				// but NOT making forward progress. The interactive prompt
				// requires operator action; an autonomous LLM Orchestration
				// pipeline cannot proceed without it. Two-stage handling:
				//
				//   1. Within blockedPaneFailAfter the lease is extended so
				//      the in-flight Bash invocation does not race with a
				//      new dispatch epoch (would surface as FENCING_REJECT
				//      once the operator finally approves). MarkProgress is
				//      NOT called — progress_timeout keeps counting.
				//   2. Once the consecutive-blocked run exceeds the
				//      threshold the task is force-failed and Planner is
				//      told via a synthetic failed result. This is much
				//      tighter than circuit_breaker.progress_timeout (30 min
				//      default) so a Worker that hits a runtime confirmation
				//      prompt on a runtime-protected path edit does not
				//      deadlock the whole iteration.
				//
				// The proper recovery for a blocked prompt is to configure
				// auto-approval in the operator's ~/.claude / codex /
				// gemini settings; this timeout is a back-stop, not the
				// primary mitigation.
				blockedFor := time.Duration(0)
				if qh.paneActivity != nil {
					if since, ok := qh.paneActivity.BlockedSince(agentID); ok {
						blockedFor = qh.clock.Now().Sub(since)
					}
				}
				if threshold := blockedPaneFailAfter(); threshold > 0 && blockedFor >= threshold {
					qh.log(LogLevelWarn,
						"task_failed_blocked_pane_timeout id=%s worker=%s epoch=%d blocked_for=%s threshold=%s "+
							"(pane wedged on a confirmation prompt past blocked-pane timeout; force-failing task to unblock Planner)",
						task.ID, agentID, task.LeaseEpoch, blockedFor.Round(time.Second), threshold)
					if qh.failTaskBlockedPane(task, queueFile, agentID, blockedFor) {
						*dirty = true
						continue
					}
					// failTaskBlockedPane returned false → invalid transition or
					// state machine drift. Fall through to lease extension so
					// the scanner does not get stuck on the entry.
				}
				err := qh.leaseManager.ExtendTaskLease(task)
				if err == nil {
					qh.log(LogLevelWarn,
						"lease_extend_pane_blocked type=task id=%s worker=%s epoch=%d elapsed=%s blocked_for=%s threshold=%s "+
							"(pane wedged on confirmation prompt; lease extended; will force-fail at threshold)",
						task.ID, agentID, task.LeaseEpoch, elapsedSinceDispatch,
						blockedFor.Round(time.Second), blockedPaneFailAfter())
					qh.scanExecutor.scanCounters.LeaseExtensions++
					*dirty = true
					continue
				}
				qh.log(LogLevelWarn,
					"lease_extend_pane_blocked_failed type=task id=%s worker=%s error=%v "+
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
// grace-extension rationale in collectExpiredTaskBusyChecks.
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
			// cmd.UpdatedAt is not refreshed when the Planner makes
			// progress (filling tasks, dispatching workers, dry-running
			// plan_submit), so a long-running command can cross the
			// max_in_progress_min threshold while either live workers are
			// still chipping away OR the Planner is actively filling/
			// finalising a phase. Releasing the command would re-dispatch
			// it to Planner as a brand new epoch, destroying work in
			// flight.
			//
			// R0b (filling_stuck) and R6 (fill_timeout) reconcilers handle
			// the truly-stuck cases on their own dedicated timeouts, so
			// the right behaviour here is "extend while any sign of
			// activity exists" rather than "release on the first hard-
			// timeout boundary".
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
		qh.log(LogLevelDebug,
			"command_lease_max_timeout_extended id=%s reason=worker_task_active",
			commandID)
		return true
	}
	// Authoritative: as long as state.plan_status is non-terminal, the
	// Planner has not yet called `plan complete`, so the command is
	// genuinely "in flight" regardless of whether the worktree state
	// file still exists. Reported 2026-05-04 as a finalize race: at
	// 21:06:33 the integration was published and the worktree was
	// cleaned up, at 21:06:41 (8 seconds later, max=1m) the lease scanner
	// observed zero workers + zero phases-with-state-file (cleanup wiped
	// it) → released as max_timeout → 21:06:42 epoch=2 dispatch fired
	// against a command that was sitting in the seconds before
	// plan_complete arrived from the Planner. Anchoring on
	// state.PlanStatus closes the race because Plan complete is the
	// terminal write Planner makes for the command; if PlanStatus is
	// still pending/sealed/etc, the Planner is by definition still
	// owed plan_complete and re-dispatch would be premature.
	if cs, ok := qh.loadCommandStateForDerivation(commandID); ok && cs != nil {
		if cs.PlanStatus != "" && !model.IsPlanTerminal(cs.PlanStatus) {
			qh.log(LogLevelDebug,
				"command_lease_max_timeout_extended id=%s reason=plan_status_non_terminal plan_status=%s",
				commandID, cs.PlanStatus)
			return true
		}
	}
	// Worktree finalization is real activity even after every phase has
	// reached a terminal Planner status. The state.PlanStatus check above
	// catches the common case (Planner hasn't called plan_complete yet);
	// this branch covers the inverse race where state.PlanStatus has
	// already been flipped to a terminal value but the integration
	// pipeline (merge → publish → cleanup) is still running its
	// fire-and-forget tail.
	if qh.worktreeManager != nil && qh.worktreeManager.HasWorktrees(commandID) {
		if cmdState, err := qh.worktreeManager.GetCommandState(commandID); err == nil && cmdState != nil {
			switch cmdState.Integration.Status {
			case model.IntegrationStatusMerging,
				model.IntegrationStatusMerged,
				model.IntegrationStatusPublishing,
				model.IntegrationStatusPartialMerge:
				qh.log(LogLevelDebug,
					"command_lease_max_timeout_extended id=%s reason=integration_finalizing integration_status=%s",
					commandID, cmdState.Integration.Status)
				return true
			}
		}
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
			qh.log(LogLevelDebug,
				"command_lease_max_timeout_extended id=%s reason=phase_active phase=%s status=%s",
				commandID, p.ID, p.Status)
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

		// Validate the in_progress→cancelled transition before mutating
		// the queue entry. The scanMu is not held here, so a parallel
		// goroutine may have already transitioned this task; skipping
		// invalid transitions prevents this code path from clobbering a
		// freshly-terminal entry with cancelled.
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

		// This path intentionally bypasses lease.Manager.ReleaseTaskLease.
		// The canonical release path transitions in_progress→pending, but
		// a dependency-cancelled task is committing a TERMINAL status, so
		// the lease lifecycle is collapsed in-place — same pattern as
		// Phase A updateQueueState. LeaseEpoch is retained so any late
		// heartbeat from the prior holder still fences correctly via the
		// canonical mismatch path.
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
