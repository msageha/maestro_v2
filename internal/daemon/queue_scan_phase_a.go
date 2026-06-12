package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// --- Phase A: entry point and step functions ---
// periodicScanPhaseA and initScanState live in ScanPhaseExecutor
// (scan_phase_executor.go) which owns lock lifecycle and counters.
// QueueHandler provides the single entry point executePhaseASteps that
// aggregates all Phase A step calls; ScanPhaseExecutor invokes only this
// entry point, keeping QueueHandler as the coordinator for phase logic.

// executePhaseASteps runs all Phase A steps in the prescribed order.
// This is the single entry point called by ScanPhaseExecutor.periodicScanPhaseA.
func (qh *QueueHandler) executePhaseASteps(s *scanState) {
	qh.stepAdmissionSync(s)
	qh.stepDeadLetters(s)
	qh.stepCircuitBreaker(s)
	qh.stepCancelPending(s)
	qh.stepCancelInterrupt(s)
	qh.stepCancelAutoComplete(s)
	qh.stepCascadeRevivalSignal(s)
	qh.stepPhaseTransitions(s)
	qh.stepAwaitingFillWatchdog(s)
	qh.stepWorktreePhaseMerges(s)
	qh.stepRetryCommitFailedWorkers(s)
	qh.stepWorktreePublish(s)
	qh.stepFinalizeQuarantinedDeferredComplete(s)
	qh.stepWorktreeFastTrackCleanup(s)
	qh.stepWorktreeOrphanCleanup(s)
	qh.stepWorktreeStallDetection(s)
	qh.stepResolvingWorkerStallDetection(s)
	qh.stepCheckWorktreeConfigViolations(s)
	qh.stepPlannerSignals(s)
	qh.stepPreemptiveRenewal(s)
	// Run blocked-pane timeout BEFORE dispatch/recovery so the threshold
	// is enforced at scan-tick granularity (default 60s) rather than at
	// lease expiry (default 5 min). Failing here removes the task from
	// the in-progress set the downstream steps see, avoiding redundant
	// busy-check work on a pane the daemon has already given up on.
	qh.stepBlockedPaneTimeout(s)
	qh.stepDispatchOrRecovery(s)
	qh.stepDependencyFailures(s)
	qh.stepIdleStatusSync(s)
}

// stepAdmissionSync resets the admission controller and records in-flight tasks
// at the start of each scan cycle, providing accurate slot counts for dispatch.
func (qh *QueueHandler) stepAdmissionSync(s *scanState) {
	if qh.admissionCtrl == nil {
		return
	}
	var inProgress []*model.Task
	for _, tq := range s.tasks {
		for i := range tq.Queue.Tasks {
			if tq.Queue.Tasks[i].Status == model.StatusInProgress {
				inProgress = append(inProgress, &tq.Queue.Tasks[i])
			}
		}
	}
	qh.admissionCtrl.RecordInFlight(inProgress)
}

// stepDeadLetters — Step 0: Remove entries exceeding max retry attempts.
func (qh *QueueHandler) stepDeadLetters(s *scanState) {
	if qh.deadLetterProcessor == nil {
		return
	}

	dlResults := qh.deadLetterProcessor.ProcessCommandDeadLetters(&s.commands.Data, &s.commands.Dirty)
	for queueFile, tq := range s.tasks {
		tqDirty := s.taskDirty[queueFile]
		dlResults = append(dlResults, qh.deadLetterProcessor.ProcessTaskDeadLetters(tq, &tqDirty)...)
		s.taskDirty[queueFile] = tqDirty
	}
	dlResults = append(dlResults, qh.deadLetterProcessor.ProcessNotificationDeadLetters(&s.notifications.Data, &s.notifications.Dirty)...)
	qh.scanExecutor.scanCounters.DeadLetters += len(dlResults)
	for _, dl := range dlResults {
		if dl.TaskID != "" {
			qh.scanExecutor.scanCounters.TasksFailed++
		}
	}
	if len(dlResults) > 0 {
		qh.log(LogLevelInfo, "dead_letter_scan removed=%d", len(dlResults))
	}

	pendingNtfs := qh.deadLetterProcessor.DrainPendingNotifications()
	if len(pendingNtfs) > 0 {
		s.notifications.Data.Notifications = append(s.notifications.Data.Notifications, pendingNtfs...)
		s.notifications.Dirty = true
		if s.notifications.Path == "" {
			s.notifications.Path = notificationQueuePath(qh.maestroDir)
		}
	}
}

// stepCircuitBreaker — Step 0.4: Check progress timeout and emit planner signals.
func (qh *QueueHandler) stepCircuitBreaker(s *scanState) {
	if qh.circuitBreaker == nil || !qh.circuitBreaker.Enabled() {
		return
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}

		stateMgr := qh.circuitBreaker.StateManager()
		if stateMgr == nil {
			continue
		}

		shouldTrip, reason := qh.circuitBreaker.CheckProgressTimeout(cmd.ID)
		if shouldTrip {
			timeoutMin := qh.circuitBreaker.ProgressTimeoutMinutes()
			if err := stateMgr.TripCircuitBreaker(cmd.ID, reason, timeoutMin); err != nil {
				qh.log(LogLevelError, "circuit_breaker_trip_timeout command=%s error=%v", cmd.ID, err)
			} else {
				qh.log(LogLevelWarn, "circuit_breaker_tripped_timeout command=%s reason=%s", cmd.ID, reason)
				// Emit signal immediately using the reason we already have,
				// avoiding a TOCTOU race between trip and signal emission.
				now := qh.clock.Now().UTC().Format(time.RFC3339)
				msg := fmt.Sprintf("[maestro] kind:circuit_breaker_tripped command_id:%s\nreason: %s",
					cmd.ID, reason)
				qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
					Kind:      "circuit_breaker_tripped",
					CommandID: cmd.ID,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, s.signalIndex)
			}
		} else {
			// The breaker may have been tripped by another path (e.g. result-write
			// handler via consecutive failures). Read current state to detect this.
			cbState, err := stateMgr.GetCircuitBreakerState(cmd.ID)
			if err != nil {
				continue
			}
			if cbState.Tripped {
				now := qh.clock.Now().UTC().Format(time.RFC3339)
				tripReason := "unknown"
				if cbState.TripReason != nil {
					tripReason = *cbState.TripReason
				}
				msg := fmt.Sprintf("[maestro] kind:circuit_breaker_tripped command_id:%s\nreason: %s",
					cmd.ID, tripReason)
				qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
					Kind:      "circuit_breaker_tripped",
					CommandID: cmd.ID,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, s.signalIndex)
			}
		}
	}
}

// stepCancelPending — Step 0.5: Cancel pending tasks for cancelled commands.
func (qh *QueueHandler) stepCancelPending(s *scanState) {
	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if qh.cancelHandler.IsCommandCancelRequested(cmd) {
			for queueFile, tq := range s.tasks {
				results := qh.cancelHandler.CancelPendingTasks(tq.Queue.Tasks, cmd.ID)
				if len(results) > 0 {
					s.taskDirty[queueFile] = true
					qh.scanExecutor.scanCounters.TasksCancelled += len(results)
					wID := workerIDFromPath(queueFile)
					if wID != "" {
						qh.cancelHandler.WriteSyntheticResults(results, wID)
					}
				}
			}
		}
	}
}

// stepCancelInterrupt — Step 0.6: Collect interrupt + cancelMark items for
// in_progress tasks of cancelled commands. The actual queue mutation is
// deferred to Phase C (after Phase B interrupts the worker), so a worker
// racing to completion before the interrupt can still report its real
// result via the normal result_write path.
func (qh *QueueHandler) stepCancelInterrupt(s *scanState) {
	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if !qh.cancelHandler.IsCommandCancelRequested(cmd) {
			continue
		}
		if s.work.cancelledCommandIDs == nil {
			s.work.cancelledCommandIDs = make(map[string]struct{})
		}
		s.work.cancelledCommandIDs[cmd.ID] = struct{}{}
		for queueFile, tq := range s.tasks {
			wID := workerIDFromPath(queueFile)
			marks, interrupts := qh.cancelHandler.CollectCancelInterruptItems(tq.Queue.Tasks, cmd.ID, wID)
			for _, m := range marks {
				m.QueueFile = queueFile
				s.work.cancelMarks = append(s.work.cancelMarks, m)
			}
			s.work.interrupts = append(s.work.interrupts, interrupts...)
		}
	}
}

// stepRetryCommitFailedWorkers — auto-recover commit_failed_workers entries.
//
// When a worker's auto-commit fails (transient git lock, ENOSPC during a
// snapshot, etc.) the daemon records the worker in CommitFailedWorkers and
// the publish gate blocks. handleWorkerCommit only runs during merge
// orchestration and once a phase has merged the daemon never revisits the
// per-worker commit, so without a dedicated retry path the flag would
// become a permanent publish block.
//
// Every scan, for every command that still has CommitFailedWorkers, retry
// the worker commit through the same idempotent CommitWorkerChanges entry
// point. A successful retry clears the flag via RemoveCommitFailedWorker
// and unblocks publish on the next pass. A continuing failure simply
// leaves the flag in place (publish stays blocked, log emits at debug
// level so the operator-visible signal is the existing publish-block
// warning rather than a new flood of failures).
//
// Phase A is normally state-YAML-only, but commit retry is idempotent and
// fast — the alternative (a dedicated Phase B step) would not gain any
// safety because Phase B's existing merge orchestration cannot revisit
// already-merged phases. Keeping the retry in Phase A ensures the recovery
// path runs on every scan cycle.
func (qh *QueueHandler) stepRetryCommitFailedWorkers(s *scanState) {
	if qh.worktreeManager == nil {
		return
	}
	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil || cmdState == nil {
			continue
		}
		if len(cmdState.CommitFailedWorkers) == 0 {
			continue
		}
		// Snapshot to avoid mutating the slice we're iterating after the
		// successful Remove writes a fresh state.
		workers := make([]string, len(cmdState.CommitFailedWorkers))
		copy(workers, cmdState.CommitFailedWorkers)
		for _, workerID := range workers {
			retryMsg := fmt.Sprintf("auto-retry: %s", autoCommitFallbackMessage)
			if err := qh.worktreeManager.CommitWorkerChanges(cmd.ID, workerID, retryMsg); err != nil {
				qh.log(LogLevelDebug,
					"commit_failed_retry_failed command=%s worker=%s error=%v "+
						"(commit_failed_workers retained; publish stays blocked)",
					cmd.ID, workerID, err)
				continue
			}
			if clrErr := qh.worktreeManager.RemoveCommitFailedWorker(cmd.ID, workerID); clrErr != nil {
				qh.log(LogLevelWarn,
					"commit_failed_retry_clear_failed command=%s worker=%s error=%v",
					cmd.ID, workerID, clrErr)
				continue
			}
			qh.log(LogLevelInfo,
				"commit_failed_retry_succeeded command=%s worker=%s "+
					"(retry commit succeeded; commit_failed_workers cleared, publish unblocked)",
				cmd.ID, workerID)
		}
	}
}

// stepCascadeRevivalSignal — cascade-revival detector / signal emitter.
//
// When a verify_failed task A is successfully repaired (repair task R
// completed, A's effective status becomes Completed), a downstream task C
// that had been cascade-cancelled with reason
// "blocked_dependency_terminal:A" stays at StatusCancelled forever.
// cascadeRecover only fires inside AddRetryTask (the Planner-driven retry
// path); daemon-side verify-repair injects the repair task without going
// through AddRetryTask, so cascade-recovery never runs and the dependency
// chain remains broken even though the lineage is effectively healthy.
//
// This step doesn't yet reopen the cancelled task automatically (that
// would require either a worker queue mutation or a fresh inject path
// alongside the existing reconcilers — both bigger than fits in this
// pass). What it DOES do is detect the pattern, log it loudly, and emit
// a "cascade_revival_pending" planner signal so the Planner sees the
// situation and can issue an add_retry_task that carries the existing
// cascadeRecover machinery into the right shape. Without the signal the
// Planner has no observable cue that a healthy lineage is still being
// blocked by a stale cascade-cancellation.
//
// Scope: only commands at PlanStatusSealed/PlanStatusPlanning are inspected;
// terminal commands cannot benefit from a signal anyway. Phantom (no
// state file) commands are silently skipped.
func (qh *QueueHandler) stepCascadeRevivalSignal(s *scanState) {
	if !qh.dependencyResolver.HasStateReader() {
		return
	}
	stateReader := qh.dependencyResolver.GetStateReader()

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}

		state, ok := qh.loadCommandStateForDerivation(cmd.ID)
		if !ok {
			continue
		}
		if len(state.CancelledReasons) == 0 {
			continue
		}

		cascadeBlockPrefix := "blocked_dependency_terminal:"
		var revivable []string
		for taskID, reason := range state.CancelledReasons {
			if !strings.HasPrefix(reason, cascadeBlockPrefix) {
				continue
			}
			predID := reason[len(cascadeBlockPrefix):]
			if predID == "" {
				continue
			}
			predStatus, err := stateReader.GetEffectiveTaskStatus(cmd.ID, predID)
			if err != nil {
				continue
			}
			if predStatus == model.StatusCompleted {
				revivable = append(revivable, taskID+"<-"+predID)
			}
		}
		if len(revivable) == 0 {
			continue
		}

		now := qh.clock.Now().UTC().Format(time.RFC3339)
		msg := fmt.Sprintf(
			"[maestro] kind:cascade_revival_pending command_id:%s\n"+
				"the following cascade-cancelled tasks are blocked by predecessors that are now effectively completed:\n  %s\n"+
				"emit add_retry_task for the predecessor lineage so cascadeRecover can revive them.",
			cmd.ID, strings.Join(revivable, ", "))
		qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
			Kind:      "cascade_revival_pending",
			CommandID: cmd.ID,
			Message:   msg,
			CreatedAt: now,
			UpdatedAt: now,
		}, s.signalIndex)
		qh.log(LogLevelWarn,
			"cascade_revival_pending command=%s revivable=%v "+
				"(predecessor effectively completed; planner needs to reissue add_retry_task)",
			cmd.ID, revivable)
	}
}

// stepCancelAutoComplete — Step 0.6.1: Auto-complete cancel-requested commands
// when all tasks are already terminal. This closes the gap where stepCancelPending
// and stepCancelInterrupt are both no-ops because no pending/in_progress tasks remain.
// Also buffers a command_cancelled notification for the Orchestrator so it learns
// about the cancellation (the normal planner-result path is bypassed here).
func (qh *QueueHandler) stepCancelAutoComplete(s *scanState) {
	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if !qh.cancelHandler.IsCommandCancelRequested(cmd) {
			continue
		}
		item := qh.cancelHandler.AutoCompleteCancelledCommands(cmd, s.tasks)
		if item != nil {
			s.commands.Dirty = true

			// Buffer command_cancelled notification for Orchestrator.
			// The normal path (planner result → result_handler → orchestrator
			// queue) is bypassed when auto-completing, so we emit the
			// notification directly into the orchestrator queue during Phase A.
			notifID, err := model.GenerateID(model.IDTypeNotification)
			if err != nil {
				qh.log(LogLevelError, "cancel_auto_complete_notif_id command=%s error=%v", item.CommandID, err)
				continue
			}
			syntheticResultID, err := model.GenerateID(model.IDTypeResult)
			if err != nil {
				qh.log(LogLevelError, "cancel_auto_complete_result_id command=%s error=%v", item.CommandID, err)
				continue
			}
			now := qh.clock.Now().UTC().Format(time.RFC3339)
			if s.notifications.Data.SchemaVersion == 0 {
				s.notifications.Data.SchemaVersion = 1
				s.notifications.Data.FileType = "queue_notification"
			}
			s.notifications.Data.Notifications = append(s.notifications.Data.Notifications, model.Notification{
				ID:             notifID,
				CommandID:      item.CommandID,
				Type:           model.NotificationTypeCommandCancelled,
				SourceResultID: syntheticResultID,
				Content:        fmt.Sprintf("command %s cancelled", item.CommandID),
				Priority:       defaultNotificationPriority,
				Status:         model.StatusPending,
				CreatedAt:      now,
				UpdatedAt:      now,
			})
			s.notifications.Dirty = true
			if s.notifications.Path == "" {
				s.notifications.Path = notificationQueuePath(qh.maestroDir)
			}
			// WARN level so operators grepping daemon.log for cancelled
			// outcomes (the canonical workflow established by the
			// notify_log_status_keys memory) see this auto-cancel path
			// even when the canonical result_handler path
			// (notify_orchestrator_cancelled) does not fire — auto-
			// complete bypasses planner.yaml writes, so the
			// status-aware key key only emits via this synthesis
			// (Report 2026-05-06 P3 issue-6).
			qh.log(LogLevelWarn,
				"notify_orchestrator_cancelled_auto_complete command=%s notif_id=%s "+
					"(daemon synthesised a cancelled notification; investigate the cancellation reason in state.cancelled_reasons)",
				item.CommandID, notifID)
		}
	}
}
