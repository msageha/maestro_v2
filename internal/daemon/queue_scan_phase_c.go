package daemon

import (
	"errors"
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

// executeScanPhaseCBody contains the periodic scan apply phase. This is the
// third scan phase (A=collect, B=execute, C=apply), unrelated to the Phase C
// adaptive-control feature bundle in phase_c_manager.go.
func (qh *QueueHandler) executeScanPhaseCBody(se *ScanPhaseExecutor, pa phaseAResult, pb phaseBResult) []DeferredNotification {
	// Load queues once for the entire phase (reused by metrics step below)
	commandQueue, commandPath, err := qh.queueStore.LoadCommandQueue()
	if err != nil {
		qh.log(LogLevelError, "phase_c_load_command_queue error=%v", err)
		return nil
	}
	taskQueues, err := qh.queueStore.LoadAllTaskQueues()
	if err != nil {
		qh.log(LogLevelError, "phase_c_load_task_queues error=%v", err)
		return nil
	}
	notificationQueue, notificationPath, err := qh.queueStore.LoadNotificationQueue()
	if err != nil {
		qh.log(LogLevelError, "phase_c_load_notification_queue error=%v", err)
		return nil
	}

	hasSignalWork := len(pb.worktreeMerges) > 0 || len(pb.worktreePublishes) > 0 || len(pb.signals) > 0
	var signalQueue model.PlannerSignalQueue
	var signalPath string
	if hasSignalWork {
		signalQueue, signalPath, err = qh.queueStore.LoadPlannerSignalQueue()
		if err != nil {
			qh.log(LogLevelError, "phase_c_load_signal_queue error=%v", err)
			return nil
		}
	}

	// --- Apply cancel marks + dispatch + busy check results (single load/flush) ---
	qh.applyCancelDispatchAndBusyChecks(se, pa, pb, commandQueue, commandPath, taskQueues, notificationQueue, notificationPath)

	// Sync agent idle status after applying dispatch results. Phase A's
	// stepIdleStatusSync runs before Phase B dispatches, so agents whose work
	// completed in this cycle still show @status="busy". This ensures
	// dashboard and `maestro status` are consistent within the same scan.
	qh.syncIdleAfterPhaseC(commandQueue, taskQueues, notificationQueue)

	// --- Apply worktree merge, publish, and signal delivery results (single load/flush) ---
	if hasSignalWork {
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
			target := findTaskInQueue(tq, m.TaskID)
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

func findTaskInQueue(tq *taskQueueEntry, taskID string) *model.Task {
	if tq == nil {
		return nil
	}
	for i := range tq.Queue.Tasks {
		if tq.Queue.Tasks[i].ID == taskID {
			return &tq.Queue.Tasks[i]
		}
	}
	return nil
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
			// Gate: decide whether this phase-merge attempt is safe to record
			// in MergedPhases. Two acceptable outcomes:
			//
			//   1. Integration reached IntegrationStatusMerged — the phase
			//      contributed new commits and the merge was fully successful.
			//
			//   2. Integration status did NOT reach Merged but there is also
			//      no worker in Conflict/Resolving. This is the "no-op merge"
			//      case: the phase had no uncommitted worker changes (e.g. a
			//      research-only phase whose tasks produced no code edits),
			//      so `determineMergeOutcome` reverted `state.Integration.Status`
			//      to its pre-merge value. Without case 2, an empty phase
			//      would stay in MergedPhases=absent forever and the merge
			//      collector would re-emit a merge item on every scan. That
			//      retry loop is directly responsible for the production
			//      regression where a foundation-only phase's re-attempted
			//      merge eventually landed during the *next* phase's
			//      in-flight worker edit window and silently absorbed the
			//      newer phase's dirty changes, corrupting the foundation
			//      commit and leaving the later phase with
			//      `no_commits_to_merge` forever.
			//
			// We still defer when a worker is in Conflict/Resolving: those
			// are owned by the resume-merge pipeline and MarkPhaseMerged
			// must wait until the resolution pipeline has re-merged them.
			cmdState, stateErr := qh.worktreeManager.GetCommandState(mr.Item.CommandID)
			if stateErr != nil {
				// State file may be absent because cleanup has already
				// removed it (e.g. publish-on-success path runs before
				// MarkPhaseMerged in the same scan). That is a benign
				// no-op for this gate — the merge was either already
				// recorded or rendered moot by cleanup. Demote to debug
				// instead of warning the operator.
				qh.log(LogLevelDebug,
					"mark_phase_merged_state_check_skipped command=%s phase=%s reason=state_unavailable error=%v",
					mr.Item.CommandID, mr.Item.PhaseID, stateErr)
				continue
			}
			if cmdState == nil {
				qh.log(LogLevelDebug,
					"mark_phase_merged_deferred command=%s phase=%s reason=state_nil",
					mr.Item.CommandID, mr.Item.PhaseID)
				continue
			}
			status := cmdState.Integration.Status
			if !isStatusSafeForMarkMerged(status) {
				qh.log(LogLevelDebug,
					"mark_phase_merged_deferred command=%s phase=%s integration_status=%s reason=status_not_markable",
					mr.Item.CommandID, mr.Item.PhaseID, status)
				continue
			}
			if status == model.IntegrationStatusCreated {
				qh.log(LogLevelInfo,
					"mark_phase_merged_no_op command=%s phase=%s integration_status=%s reason=nothing_to_merge",
					mr.Item.CommandID, mr.Item.PhaseID, status)
			}
			if err := qh.worktreeManager.MarkPhaseMerged(mr.Item.CommandID, mr.Item.PhaseID); err != nil {
				qh.log(LogLevelWarn, "mark_phase_merged_failed command=%s phase=%s error=%v",
					mr.Item.CommandID, mr.Item.PhaseID, err)
			}
		}
	}
}

func isStatusSafeForMarkMerged(status model.IntegrationStatus) bool {
	// Merged means the phase produced new commits, or a no-op phase ran after
	// an earlier phase had already merged. Created means the first phase was a
	// no-op. Other statuses require recovery before downstream phases proceed.
	return status == model.IntegrationStatusMerged || status == model.IntegrationStatusCreated
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
			if errors.Is(pr.Error, errIntegrationNotPublishable) {
				qh.log(LogLevelDebug, "worktree_publish_skipped command=%s error=%v",
					pr.Item.CommandID, pr.Error)
			} else {
				qh.log(LogLevelError, "worktree_publish_failed command=%s error=%v",
					pr.Item.CommandID, pr.Error)
			}
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
			deferredFinalized := false
			if qh.deferredPlanCompleter != nil {
				completed, err := qh.deferredPlanCompleter(pr.Item.CommandID)
				if err != nil {
					qh.log(LogLevelWarn, "deferred_complete_failed command=%s error=%v (falling back to signal)",
						pr.Item.CommandID, err)
				} else if completed {
					qh.log(LogLevelInfo, "deferred_complete_success command=%s", pr.Item.CommandID)
					deferredFinalized = true
				}
			}

			// publish_completed is emitted in two cases:
			//   (a) deferredFinalized == true — the Planner explicitly asked
			//       for deferred completion and the daemon just finalised it.
			//       Both branches are informational only (Planner already
			//       finished its turn via deferred_publish), so emitting is
			//       safe and symmetric with the non-deferred path.
			//   (b) command is not yet terminal in the queue — the standard
			//       "publish succeeded, Planner has not yet called complete"
			//       flow.
			//
			// We still skip the signal when the command is already terminal
			// AND there was no deferred intent — that case is the legacy
			// race where a concurrent plan complete during Phase B/C marks
			// the queue terminal without a deferred_complete file. Emitting
			// in that race would re-deliver to a Planner who already
			// finished outside the deferred path.
			if !deferredFinalized {
				terminalCQ := commandQueue
				if freshCQ, _, err := qh.queueStore.LoadCommandQueue(); err == nil {
					terminalCQ = freshCQ
				}
				if isCommandTerminalInQueue(terminalCQ, pr.Item.CommandID) {
					qh.log(LogLevelInfo, "publish_completed_signal_suppressed command=%s (command already terminal, no deferred intent)",
						pr.Item.CommandID)
					continue
				}
			}

			// Informational notification: publish has succeeded. This signal
			// does NOT instruct the Planner to call `plan complete`; that path
			// caused redundant double-fire previously.
			//
			// Responsibility split:
			//   - `plan complete` is the Planner's responsibility (envelope
			//     + planner.md instruct it after all tasks complete).
			//   - If the Planner called plan complete before publish finished,
			//     it received `deferred_publish` and `deferredPlanCompleter`
			//     above auto-finalises the deferred intent — the signal still
			//     fires for symmetry.
			//   - This signal only informs the Planner that publish succeeded
			//     (useful for post-publish verification like `--run-on-main`).
			msg := fmt.Sprintf("[maestro] kind:publish_completed command_id:%s\n"+
				"The integration branch has been successfully published to the base branch. "+
				"This is an informational notice — no action required in the default case. "+
				"If a prior `maestro plan complete` returned `deferred_publish`, the daemon "+
				"has already finalised it automatically. Do NOT add `--run-on-main` "+
				"verification tasks by default; only add them when the command's "+
				"original content explicitly requires post-publish verification on main.",
				pr.Item.CommandID)
			reason := ""
			if deferredFinalized {
				// Tag the signal so the dispatch-time stale filter
				// (stepPlannerSignalsDeferred) keeps it instead of dropping
				// it as a "terminal command" race artefact. The Planner is
				// expected to receive publish_completed in the deferred
				// path for parity with the non-deferred path.
				reason = "deferred_complete_finalized"
			}
			qh.upsertPlannerSignal(signalQueue, signalsDirty, model.PlannerSignal{
				Kind:      "publish_completed",
				CommandID: pr.Item.CommandID,
				Message:   msg,
				Reason:    reason,
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
	// The retry-publish step is normally driven by the daemon's
	// AutoRecoverAfterResolution hook once the worker reports the resolution
	// task as completed (see planner.md "Publish Conflict Recovery" → step 3).
	// Calling `maestro plan retry-publish` manually is an escape hatch for the
	// cases the auto-recover hook cannot cover (worker reported failed,
	// AutoRecover itself errored, etc.). The signal therefore only asks the
	// Planner to dispatch a resolution worker — it does NOT instruct an
	// unconditional retry-publish call (older copies of this message did, and
	// drifted out of sync with the AutoRecover behaviour).
	msg := fmt.Sprintf("[maestro] kind:publish_conflict command_id:%s\n"+
		"Forward-merge of base branch into integration failed due to content conflicts.\n"+
		"conflict_files: %s\n"+
		"The Planner should dispatch a worker (with --run-on-integration) to resolve the conflicts on the integration branch. "+
		"After the worker reports completed, the daemon's AutoRecoverAfterResolution hook will fire `retry-publish` automatically; "+
		"only invoke `maestro plan retry-publish --command-id %s` manually if the worker failed or AutoRecover errored.",
		commandID, strings.Join(files, ", "), commandID)

	qh.upsertPlannerSignal(signalQueue, signalsDirty, model.PlannerSignal{
		Kind:          "publish_conflict",
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

	// Result notification retry has been moved out of Phase C so the slow
	// per-result tmux delivery and inline retry budget no longer hold
	// scanMu.Lock for the full
	// (maxRetries+1) * delivery_timeout + maxRetries * retry_delay
	// window. With a Planner pane that returns submit-confirmation
	// uncertain (or any other slow agent), that wait was directly
	// blocking every UDS handler that takes scanMu.RLock —
	// queue_write / plan complete / verify write all surfaced as CLI
	// timeouts. ScanPhaseExecutor.Execute now invokes
	// resultHandler.ScanAllResults after Phase C releases scanMu (see
	// scan_phase_executor.go for the exact ordering).

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
