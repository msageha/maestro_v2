package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// periodicScanPhaseC has been moved to ScanPhaseExecutor (scan_phase_executor.go).
// executeScanPhaseCBody contains Phase C's logic, called by ScanPhaseExecutor
// after lock acquisition and counter restoration.
func (qh *QueueHandler) executeScanPhaseCBody(se *ScanPhaseExecutor, pa phaseAResult, pb phaseBResult) []DeferredNotification {
	// Load queues once for the entire phase (reused by metrics step below)
	commandQueue, commandPath := qh.loadCommandQueue()
	taskQueues := qh.loadAllTaskQueues()
	notificationQueue, notificationPath := qh.loadNotificationQueue()

	// --- Apply cancel marks + dispatch + busy check results (single load/flush) ---
	if len(pb.dispatches) > 0 || len(pb.busyChecks) > 0 || len(pa.work.cancelMarks) > 0 {
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
		qh.flushQueues(commandQueue, commandPath, commandsDirty,
			taskQueues, taskDirty,
			notificationQueue, notificationPath, notificationsDirty,
			model.PlannerSignalQueue{}, "", false)

		// Write synthetic cancelled results after the queue flush so any
		// concurrent reader observes queue state matching the result file.
		for wID, results := range syntheticByWorker {
			qh.cancelHandler.WriteSyntheticResults(results, wID)
		}
	}

	// --- Apply worktree merge, publish, and signal delivery results (single load/flush) ---
	hasSignalWork := len(pb.worktreeMerges) > 0 || len(pb.worktreePublishes) > 0 || len(pb.signals) > 0
	if hasSignalWork {
		signalQueue, signalPath := qh.loadPlannerSignalQueue()
		signalsDirty := false
		signalIndex := buildSignalIndex(signalQueue.Signals)
		now := qh.clock.Now().UTC().Format(time.RFC3339)

		// Worktree merge results: emit commit failure signals, conflict signals, record merged phases
		for _, mr := range pb.worktreeMerges {
			for _, cf := range mr.CommitFailures {
				qh.log(LogLevelError, "worktree_commit_failed command=%s phase=%s worker=%s reason=%s error=%v",
					mr.Item.CommandID, mr.Item.PhaseID, cf.WorkerID, cf.Reason, cf.Error)
				msg := fmt.Sprintf("[maestro] kind:commit_failed command_id:%s phase:%s worker:%s reason:%s\nerror: %v",
					mr.Item.CommandID, mr.Item.PhaseID, cf.WorkerID, cf.Reason, cf.Error)
				qh.upsertPlannerSignal(&signalQueue, &signalsDirty, model.PlannerSignal{
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
				qh.upsertPlannerSignal(&signalQueue, &signalsDirty, model.PlannerSignal{
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

		// Worktree publish results: emit signal on failure
		for _, pr := range pb.worktreePublishes {
			if pr.Error != nil {
				qh.log(LogLevelError, "worktree_publish_failed command=%s error=%v",
					pr.Item.CommandID, pr.Error)
				msg := fmt.Sprintf("[maestro] kind:publish_failed command_id:%s\nerror: %v",
					pr.Item.CommandID, pr.Error)
				qh.upsertPlannerSignal(&signalQueue, &signalsDirty, model.PlannerSignal{
					Kind:      "publish_failed",
					CommandID: pr.Item.CommandID,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, signalIndex)
			} else {
				qh.log(LogLevelInfo, "worktree_published command=%s", pr.Item.CommandID)
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
		qh.applySignalResults(pb.signals, &signalQueue, &signalsDirty)

		// Single flush for all signal queue mutations
		if signalsDirty {
			p := signalPath
			if p == "" {
				p = filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
			}
			if len(signalQueue.Signals) == 0 {
				_ = os.Remove(p)
			} else {
				if err := yamlutil.AtomicWrite(p, signalQueue); err != nil {
					qh.log(LogLevelError, "write_planner_signals error=%v", err)
				}
			}
		}
	}

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
		scanDuration := qh.clock.Now().Sub(pa.scanStart)
		gauges := MetricsGauges{
			WorktreeCommandsStalled: qh.countWorktreeCommandsStalled(commandQueue),
			BakFilesCount:           countBakFiles(qh.maestroDir),
		}
		if err := qh.metricsHandler.UpdateMetrics(commandQueue, taskQueues, notificationQueue, pa.scanStart, scanDuration, &se.scanCounters, gauges); err != nil {
			qh.log(LogLevelError, "update_metrics error=%v", err)
		}
		if err := qh.metricsHandler.UpdateDashboard(commandQueue, taskQueues, notificationQueue); err != nil {
			qh.log(LogLevelError, "update_dashboard error=%v", err)
		}
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
