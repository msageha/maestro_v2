package daemon

import (
	"fmt"
	"path/filepath"
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
	qh.stepPhaseTransitions(s)
	qh.stepWorktreePhaseMerges(s)
	qh.stepWorktreePublish(s)
	qh.stepWorktreeFastTrackCleanup(s)
	qh.stepWorktreeOrphanCleanup(s)
	qh.stepWorktreeStallDetection(s)
	qh.stepCheckWorktreeConfigViolations(s)
	qh.stepPlannerSignals(s)
	qh.stepPreemptiveRenewal(s)
	qh.stepDispatchOrRecovery(s)
	qh.stepDependencyFailures(s)
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
			s.notifications.Path = filepath.Join(qh.maestroDir, "queue", "orchestrator.yaml")
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
				s.notifications.Path = filepath.Join(qh.maestroDir, "queue", "orchestrator.yaml")
			}
			qh.log(LogLevelInfo, "cancel_auto_complete_notification command=%s notif_id=%s", item.CommandID, notifID)
		}
	}
}
