package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// periodicScanPhaseC has been moved to ScanPhaseExecutor (scan_phase_executor.go).
// executeScanPhaseCBody contains Phase C's logic, called by ScanPhaseExecutor
// after lock acquisition and counter restoration.
func (qh *QueueHandler) executeScanPhaseCBody(se *ScanPhaseExecutor, pa phaseAResult, pb phaseBResult) []DeferredNotification {
	// Load queues once for the entire phase (reused by metrics step below)
	commandQueue, commandPath, err := qh.queueStore.LoadCommandQueue()
	if err != nil {
		qh.log(LogLevelError, "phase_c_load_command_queue error=%v", err)
	}
	taskQueues, err := qh.queueStore.LoadAllTaskQueues()
	if err != nil {
		qh.log(LogLevelError, "phase_c_load_task_queues error=%v", err)
	}
	notificationQueue, notificationPath, err := qh.queueStore.LoadNotificationQueue()
	if err != nil {
		qh.log(LogLevelError, "phase_c_load_notification_queue error=%v", err)
	}

	// --- Apply cancel marks + dispatch + busy check results (single load/flush) ---
	qh.applyCancelDispatchAndBusyChecks(se, pa, pb, commandQueue, commandPath, taskQueues, notificationQueue, notificationPath)

	// Sync agent idle status after applying dispatch results. Phase A's
	// stepIdleStatusSync runs before Phase B dispatches, so agents whose work
	// completed in this cycle still show @status="busy". This ensures
	// dashboard and `maestro status` are consistent within the same scan.
	qh.syncIdleAfterPhaseC(commandQueue, taskQueues, notificationQueue)

	// --- Apply worktree merge, publish, and signal delivery results (single load/flush) ---
	hasSignalWork := len(pb.worktreeMerges) > 0 || len(pb.worktreePublishes) > 0 || len(pb.signals) > 0
	if hasSignalWork {
		signalQueue, signalPath, err := qh.queueStore.LoadPlannerSignalQueue()
		if err != nil {
			qh.log(LogLevelError, "phase_c_load_signal_queue error=%v", err)
		}
		signalsDirty := false
		signalIndex := buildSignalIndex(signalQueue.Signals)
		now := qh.clock.Now().UTC().Format(time.RFC3339)

		qh.applyWorktreeResultSignals(pb, &signalQueue, &signalsDirty, signalIndex, now, commandQueue)
		qh.dispatchConflictsAndFlushSignals(pb, &signalQueue, signalPath, &signalsDirty, now)
	}

	// Post-scan maintenance: cleanup, reconciliation, metrics
	return qh.runPostScanMaintenance(se, pa, pb, commandQueue, taskQueues, notificationQueue)
}

// applyCancelDispatchAndBusyChecks applies cancel marks, dispatch results, and
// busy check results from Phase A and Phase B, then flushes all queue mutations
// in a single write.
func (qh *QueueHandler) applyCancelDispatchAndBusyChecks(
	se *ScanPhaseExecutor,
	pa phaseAResult,
	pb phaseBResult,
	commandQueue model.CommandQueue,
	commandPath string,
	taskQueues map[string]*taskQueueEntry,
	notificationQueue model.NotificationQueue,
	notificationPath string,
) {
	if len(pb.dispatches) == 0 && len(pb.busyChecks) == 0 && len(pa.work.cancelMarks) == 0 {
		return
	}

	commandsDirty := false
	notificationsDirty := false
	taskDirty := make(map[string]bool)

	// M3+H4: apply deferred cancel marks first. Phase B has already
	// interrupted the worker and discarded its worktree changes, so
	// any task still observed as in_progress with the same lease_epoch
	// can be safely transitioned to cancelled. Tasks that the worker
	// raced to a terminal state are skipped — the real result wins.
	var syntheticByWorker map[string][]CancelledTaskResult
	if len(pa.work.cancelMarks) > 0 {
		syntheticByWorker = make(map[string][]CancelledTaskResult)
		for _, m := range pa.work.cancelMarks {
			tq, ok := taskQueues[m.QueueFile]
			if !ok {
				qh.log(LogLevelInfo, "cancel_mark_skip_missing_queue file=%s task=%s",
					m.QueueFile, m.TaskID)
				continue
			}
			var target *model.Task
			for i := range tq.Queue.Tasks {
				if tq.Queue.Tasks[i].ID == m.TaskID {
					target = &tq.Queue.Tasks[i]
					break
				}
			}
			if target == nil {
				qh.log(LogLevelInfo, "cancel_mark_skip_missing_task task=%s", m.TaskID)
				continue
			}
			res, applied := qh.cancelHandler.ApplyCancelMark(target, m.LeaseEpoch)
			if !applied {
				qh.log(LogLevelInfo, "cancel_mark_skip_raced task=%s status=%s epoch=%d expected_epoch=%d",
					m.TaskID, target.Status, target.LeaseEpoch, m.LeaseEpoch)
				continue
			}
			taskDirty[m.QueueFile] = true
			se.scanCounters.TasksCancelled++
			if m.WorkerID != "" {
				syntheticByWorker[m.WorkerID] = append(syntheticByWorker[m.WorkerID], res)
			}
		}
	}

	for _, dr := range pb.dispatches {
		switch dr.Item.Kind {
		case "command":
			qh.applyCommandDispatchResult(dr, &commandQueue, &commandsDirty)
		case "task":
			qh.applyTaskDispatchResult(dr, taskQueues, taskDirty)
		case "notification":
			qh.applyNotificationDispatchResult(dr, &notificationQueue, &notificationsDirty)
		}
	}

	for _, bc := range pb.busyChecks {
		switch bc.Item.Kind {
		case "task":
			qh.applyTaskBusyCheckResult(bc, taskQueues, taskDirty)
		case "command":
			qh.applyCommandBusyCheckResult(bc, &commandQueue, &commandsDirty)
		}
	}

	// Single flush for cancel marks, dispatch, and busy check results
	qh.queueStore.FlushQueues(commandQueue, commandPath, commandsDirty,
		taskQueues, taskDirty,
		notificationQueue, notificationPath, notificationsDirty,
		model.PlannerSignalQueue{}, "", false)

	// Write synthetic cancelled results after the queue flush so any
	// concurrent reader observes queue state matching the result file.
	for wID, results := range syntheticByWorker {
		qh.cancelHandler.WriteSyntheticResults(results, wID)
	}
}

// applyWorktreeResultSignals processes worktree merge results and publish
// results from Phase B, emitting commit failure signals, conflict signals, and
// publish outcome signals into the planner signal queue.
func (qh *QueueHandler) applyWorktreeResultSignals(
	pb phaseBResult,
	signalQueue *model.PlannerSignalQueue,
	signalsDirty *bool,
	signalIndex map[signalKey]struct{},
	now string,
	commandQueue model.CommandQueue,
) {
	qh.applyMergeResultSignals(pb.worktreeMerges, signalQueue, signalsDirty, signalIndex, now)
	qh.applyPublishResultSignals(pb.worktreePublishes, signalQueue, signalsDirty, signalIndex, now, commandQueue)
}

// applyMergeResultSignals emits commit failure signals, conflict signals, and
// records merged phases for each worktree merge result from Phase B.
func (qh *QueueHandler) applyMergeResultSignals(
	merges []worktreeMergeResult,
	signalQueue *model.PlannerSignalQueue,
	signalsDirty *bool,
	signalIndex map[signalKey]struct{},
	now string,
) {
	for _, mr := range merges {
		for _, cf := range mr.CommitFailures {
			qh.log(LogLevelError, "worktree_commit_failed command=%s phase=%s worker=%s reason=%s error=%v",
				mr.Item.CommandID, mr.Item.PhaseID, cf.WorkerID, cf.Reason, cf.Error)
			msg := fmt.Sprintf("[maestro] kind:commit_failed command_id:%s phase:%s worker:%s reason:%s\nerror: %v",
				mr.Item.CommandID, mr.Item.PhaseID, cf.WorkerID, cf.Reason, cf.Error)
			qh.upsertPlannerSignal(signalQueue, signalsDirty, model.PlannerSignal{
				Kind:      "commit_failed",
				CommandID: mr.Item.CommandID,
				PhaseID:   mr.Item.PhaseID,
				WorkerID:  cf.WorkerID,
				Reason:    cf.Reason,
				Message:   msg,
				CreatedAt: now,
				UpdatedAt: now,
			}, signalIndex)
		}
		if mr.Error != nil {
			qh.log(LogLevelError, "worktree_merge_failed command=%s phase=%s error=%v",
				mr.Item.CommandID, mr.Item.PhaseID, mr.Error)
		}
		for _, conflict := range mr.Conflicts {
			// Append base/ours/theirs refs to the legacy message format so
			// existing CSV-style consumers continue to parse it. New
			// structured fields are also populated below for planners that
			// understand the MVP-1 schema.
			msg := fmt.Sprintf("[maestro] kind:merge_conflict command_id:%s phase:%s worker:%s base:%s ours:%s theirs:%s\nconflict_files: %s",
				mr.Item.CommandID, mr.Item.PhaseID, conflict.WorkerID,
				conflict.BaseRef, conflict.OursRef, conflict.TheirsRef,
				strings.Join(conflict.ConflictFiles, ", "))
			cg := worktree.ComputeConflictGeneration(
				mr.Item.CommandID, mr.Item.PhaseID, conflict.WorkerID,
				conflict.BaseRef, conflict.OursRef, conflict.TheirsRef,
			)
			qh.upsertPlannerSignal(signalQueue, signalsDirty, model.PlannerSignal{
				Kind:               "merge_conflict",
				CommandID:          mr.Item.CommandID,
				PhaseID:            mr.Item.PhaseID,
				WorkerID:           conflict.WorkerID,
				Message:            msg,
				ConflictBaseRef:    conflict.BaseRef,
				ConflictOursRef:    conflict.OursRef,
				ConflictTheirsRef:  conflict.TheirsRef,
				ConflictFiles:      append([]string(nil), conflict.ConflictFiles...),
				ConflictGeneration: cg,
				ConflictType:       "task_merge_conflict",
				CreatedAt:          now,
				UpdatedAt:          now,
			}, signalIndex)
		}
		if mr.Error == nil && len(mr.Conflicts) == 0 && len(mr.CommitFailures) == 0 && qh.worktreeManager != nil {
			if err := qh.worktreeManager.MarkPhaseMerged(mr.Item.CommandID, mr.Item.PhaseID); err != nil {
				qh.log(LogLevelWarn, "mark_phase_merged_failed command=%s phase=%s error=%v",
					mr.Item.CommandID, mr.Item.PhaseID, err)
			}
		}
	}
}

// applyPublishResultSignals emits publish_completed or publish_failed signals
// for each worktree publish result from Phase B. The publish_completed signal
// is suppressed when the command is already in a terminal status (e.g. plan
// complete was already called before publish finished), preventing the Planner
// from issuing a redundant plan complete call.
func (qh *QueueHandler) applyPublishResultSignals(
	publishes []worktreePublishResult,
	signalQueue *model.PlannerSignalQueue,
	signalsDirty *bool,
	signalIndex map[signalKey]struct{},
	now string,
	commandQueue model.CommandQueue,
) {
	for _, pr := range publishes {
		if pr.Error != nil {
			qh.log(LogLevelError, "worktree_publish_failed command=%s error=%v",
				pr.Item.CommandID, pr.Error)
			// Do NOT emit a generic publish_failed signal to the Planner. The
			// Daemon handles publish failure retries automatically via
			// recordPublishFailure/exponential backoff. If retries are
			// exhausted the integration is quarantined and R8
			// (NotifyPublishQuarantined) escalates to the Planner. Sending
			// publish_failed would cause the Planner to attempt normal task
			// failure recovery (plan_submit / add_retry_task) which fails
			// because the Worker task already completed successfully.
			//
			// However, if the failure is a publish conflict (forward-merge of
			// base into integration failed), emit a publish_conflict signal so
			// the Planner can proactively dispatch a resolution worker instead
			// of waiting for quarantine.
			qh.emitPublishConflictSignalIfNeeded(pr.Item.CommandID, signalQueue, signalsDirty, signalIndex, now)
		} else {
			qh.log(LogLevelInfo, "worktree_published command=%s", pr.Item.CommandID)

			// Try deferred auto-completion: if the Planner already called
			// plan complete (which was deferred because publish hadn't
			// finished), the daemon can now finalize it without a round-trip.
			if qh.deferredPlanCompleter != nil {
				completed, err := qh.deferredPlanCompleter(pr.Item.CommandID)
				if err != nil {
					qh.log(LogLevelWarn, "deferred_complete_failed command=%s error=%v (falling back to signal)",
						pr.Item.CommandID, err)
				} else if completed {
					qh.log(LogLevelInfo, "deferred_complete_success command=%s", pr.Item.CommandID)
					continue
				}
			}

			// Skip the publish_completed signal if the command is already
			// terminal — the Planner has already called plan complete and
			// sending the signal would cause a redundant second invocation.
			//
			// Reload the command queue from disk to capture concurrent
			// plan complete calls that ran after Phase C's initial queue
			// load. Without this reload the in-memory snapshot is stale
			// and the terminal check misses commands that were completed
			// during Phase B / early Phase C.
			terminalCQ := commandQueue
			if freshCQ, _, err := qh.queueStore.LoadCommandQueue(); err == nil {
				terminalCQ = freshCQ
			}
			if isCommandTerminalInQueue(terminalCQ, pr.Item.CommandID) {
				qh.log(LogLevelInfo, "publish_completed_signal_suppressed command=%s (command already terminal)",
					pr.Item.CommandID)
				continue
			}
			// Notify Planner so it can call `plan complete` now that the
			// integration branch is published. Without this signal the
			// command stays at plan_status:sealed when publish happens
			// after the Planner's initial complete attempt (e.g. conflict
			// recovery path where publish is deferred until after
			// resume-merge succeeds).
			msg := fmt.Sprintf("[maestro] kind:publish_completed command_id:%s\n"+
				"The integration branch has been successfully published to the base branch. "+
				"Call `maestro plan complete` to finalise the command.",
				pr.Item.CommandID)
			qh.upsertPlannerSignal(signalQueue, signalsDirty, model.PlannerSignal{
				Kind:      "publish_completed",
				CommandID: pr.Item.CommandID,
				Message:   msg,
				CreatedAt: now,
				UpdatedAt: now,
			}, signalIndex)
		}
	}
}

// emitPublishConflictSignalIfNeeded checks whether a publish failure was caused
// by a forward-merge conflict (PublishConflictFiles is non-empty) and emits a
// one-shot publish_conflict PlannerSignal so the Planner can proactively
// dispatch resolution workers. The signal is guarded by
// PublishConflictSignaled to avoid re-emission on every scan cycle.
func (qh *QueueHandler) emitPublishConflictSignalIfNeeded(
	commandID string,
	signalQueue *model.PlannerSignalQueue,
	signalsDirty *bool,
	signalIndex map[signalKey]struct{},
	now string,
) {
	if qh.worktreeManager == nil {
		return
	}
	cmdState, err := qh.worktreeManager.GetCommandState(commandID)
	if err != nil || cmdState == nil {
		return
	}
	if len(cmdState.Integration.PublishConflictFiles) == 0 {
		return
	}
	if cmdState.Integration.PublishConflictSignaled {
		return
	}

	files := cmdState.Integration.PublishConflictFiles
	msg := fmt.Sprintf("[maestro] kind:publish_conflict command_id:%s\n"+
		"Forward-merge of base branch into integration failed due to content conflicts.\n"+
		"conflict_files: %s\n"+
		"The Planner should dispatch a worker to resolve the conflicts on the integration branch, "+
		"then call `maestro plan retry-publish --command-id %s` to re-attempt publish.",
		commandID, strings.Join(files, ", "), commandID)

	qh.upsertPlannerSignal(signalQueue, signalsDirty, model.PlannerSignal{
		Kind:          "merge_conflict",
		CommandID:     commandID,
		Message:       msg,
		ConflictFiles: append([]string(nil), files...),
		ConflictType:  "publish_conflict",
		CreatedAt:     now,
		UpdatedAt:     now,
	}, signalIndex)

	// Mark as signaled to avoid re-emission.
	if err := qh.worktreeManager.MarkPublishConflictSignaled(commandID); err != nil {
		qh.log(LogLevelWarn, "publish_conflict_signal_mark command=%s error=%v", commandID, err)
	}
}

// isCommandTerminalInQueue returns true if the command identified by commandID
// has a terminal status in the given command queue.
func isCommandTerminalInQueue(cq model.CommandQueue, commandID string) bool {
	for i := range cq.Commands {
		if cq.Commands[i].ID == commandID {
			return model.IsTerminal(cq.Commands[i].Status)
		}
	}
	return false
}

// dispatchConflictsAndFlushSignals performs the pre-flush for C1 conflict
// dispatch, runs conflict resolution dispatch, applies signal delivery results,
// and performs the final signal queue flush.
func (qh *QueueHandler) dispatchConflictsAndFlushSignals(
	pb phaseBResult,
	signalQueue *model.PlannerSignalQueue,
	signalPath string,
	signalsDirty *bool,
	now string,
) {
	// Pre-flush: write new signals to disk before C1 so that
	// DispatchConflictResolution (which reads via YAMLSignalStore) can
	// find freshly-created merge_conflict signals in this scan cycle.
	// Without this, the first C1 dispatch always fails because the signal
	// exists only in memory until the final flush at the end of Phase C.
	if *signalsDirty && qh.worktreeManager != nil {
		p := signalPath
		if p == "" {
			p = filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
		}
		if err := yamlutil.AtomicWrite(p, *signalQueue); err != nil {
			qh.log(LogLevelError, "write_planner_signals_pre_c1 error=%v", err)
		}
	}

	// C1: opportunistically dispatch the conflict resolver pipeline for any
	// merge_conflict signal that is still in its initial (empty) state.
	// This is the minimal wiring that hands a freshly-detected conflict to
	// the resolver pipeline; the resolver agent itself runs out-of-band
	// and the operator-driven resolve_conflict CLI op (→
	// recover.ResolveConflict) closes the loop by clearing
	// CommitFailedWorkers, resetting MergeFailureCount, and clearing the
	// merge_conflict signal.
	if qh.worktreeManager != nil {
		for i := range signalQueue.Signals {
			sig := &signalQueue.Signals[i]
			if sig.Kind != "merge_conflict" || sig.ResolutionState != "" || sig.ConflictGeneration == "" {
				continue
			}
			if err := qh.worktreeManager.DispatchConflictResolution(
				sig.CommandID, sig.PhaseID, sig.WorkerID, sig.ConflictGeneration,
			); err != nil {
				qh.log(LogLevelWarn, "conflict_dispatch_failed command=%s phase=%s worker=%s error=%v",
					sig.CommandID, sig.PhaseID, sig.WorkerID, err)
			} else {
				// The store mutated the on-disk signal already; mirror the
				// state on our in-memory copy so the next iteration sees it.
				sig.ResolutionState = "dispatched"
				sig.UpdatedAt = now
			}
		}
	}

	// Signal delivery results: remove delivered signals
	qh.applySignalResults(pb.signals, signalQueue, signalsDirty)

	// Single flush for all signal queue mutations
	if *signalsDirty {
		p := signalPath
		if p == "" {
			p = filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
		}
		if len(signalQueue.Signals) == 0 {
			_ = os.Remove(p)
		} else {
			if err := yamlutil.AtomicWrite(p, *signalQueue); err != nil {
				qh.log(LogLevelError, "write_planner_signals error=%v", err)
			}
		}
	}
}

// runPostScanMaintenance handles recovery hint logging, worktree cleanup
// logging, result notification retry, reconciliation, metrics, and dashboard
// updates.
func (qh *QueueHandler) runPostScanMaintenance(
	se *ScanPhaseExecutor,
	pa phaseAResult,
	pb phaseBResult,
	commandQueue model.CommandQueue,
	taskQueues map[string]*taskQueueEntry,
	notificationQueue model.NotificationQueue,
) []DeferredNotification {
	// --- Log recovery hints from Phase B partial failures ---
	for _, hint := range pb.recoveryHints {
		qh.log(LogLevelWarn, "phase_b_recovery_hint %s", hint)
	}

	// --- Apply worktree cleanup results: log only ---
	for _, cr := range pb.worktreeCleanups {
		if cr.Error != nil {
			qh.log(LogLevelWarn, "worktree_cleanup_failed command=%s reason=%s error=%v",
				cr.Item.CommandID, cr.Item.Reason, cr.Error)
		} else {
			qh.log(LogLevelInfo, "worktree_cleanup_complete command=%s reason=%s",
				cr.Item.CommandID, cr.Item.Reason)
		}
	}

	// Step 2.5: Result notification retry
	if qh.resultHandler != nil {
		n := qh.resultHandler.ScanAllResults()
		se.scanCounters.NotificationRetries += n
		if n > 0 {
			qh.log(LogLevelInfo, "result_notify_scan notified=%d", n)
		}
	}

	// Step 3: Reconciliation (state mutations under scanMu, notifications deferred)
	var deferredNotifs []DeferredNotification
	if qh.reconciler != nil {
		repairs, notifs := qh.reconciler.Reconcile()
		deferredNotifs = notifs
		se.scanCounters.ReconciliationRepairs += len(repairs)
		for _, repair := range repairs {
			qh.log(LogLevelInfo, "reconciliation pattern=%s command=%s task=%s detail=%s",
				repair.Pattern, repair.CommandID, repair.TaskID, repair.Detail)
		}
	}

	// Step 4: Metrics and dashboard (reuses queues loaded at phase start)
	if qh.metricsHandler != nil {
		gauges := metrics.Gauges{
			WorktreeCommandsStalled: qh.countWorktreeCommandsStalled(commandQueue),
			BakFilesCount:           countBakFiles(qh.maestroDir),
		}
		snapshots := taskQueuesToSnapshots(taskQueues)
		if err := qh.metricsHandler.UpdateMetrics(commandQueue, snapshots, notificationQueue, pa.scanStart, &se.scanCounters, gauges); err != nil {
			qh.log(LogLevelError, "update_metrics error=%v", err)
		}
	}
	if err := qh.updateDashboard(commandQueue, taskQueues, notificationQueue); err != nil {
		qh.log(LogLevelError, "update_dashboard error=%v", err)
	}

	return deferredNotifs
}

// countWorktreeCommandsStalled returns the number of commands in cq whose
// worktree integration state has the StallSignaled flag set. Commands with no
// worktree state, or whose state cannot be loaded, are skipped silently —
// stall detection itself logs the relevant errors during Phase A.
func (qh *QueueHandler) countWorktreeCommandsStalled(cq model.CommandQueue) int {
	if qh.worktreeManager == nil {
		return 0
	}
	count := 0
	for _, cmd := range cq.Commands {
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		state, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil || state == nil {
			continue
		}
		if state.Integration.StallSignaled {
			count++
		}
	}
	return count
}

// countBakFiles returns the total number of files ending in ".bak" anywhere
// under the maestro directory. Walk errors are tolerated by skipping the
// offending entry; this is a best-effort gauge.
func countBakFiles(maestroDir string) int {
	if maestroDir == "" {
		return 0
	}
	count := 0
	_ = filepath.Walk(maestroDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".bak") {
			count++
		}
		return nil
	})
	return count
}
