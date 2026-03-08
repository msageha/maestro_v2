package daemon

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// periodicScanPhaseA runs under scanMu.Lock. It loads queues, performs fast
// in-memory mutations (dead letter, cancel, phase transitions, dependency checks),
// collects deferred work items for slow I/O, and flushes queues to disk.
func (qh *QueueHandler) periodicScanPhaseA() phaseAResult {
	qh.scanMu.Lock()
	defer qh.scanMu.Unlock()

	s := qh.initScanState()

	// Execute steps in fixed order (matches original Step 0 through Step 1.5).
	qh.stepDeadLetters(&s)
	qh.stepCircuitBreaker(&s)
	qh.stepCancelPending(&s)
	qh.stepCancelInterrupt(&s)
	qh.stepPhaseTransitions(&s)
	qh.stepWorktreePhaseMerges(&s)
	qh.stepWorktreePublish(&s)
	qh.stepPlannerSignals(&s)
	qh.stepPreemptiveRenewal(&s)
	qh.stepDispatchOrRecovery(&s)
	qh.stepDependencyFailures(&s)

	// Flush dirty queues to disk
	qh.flushQueues(s.commands.Data, s.commands.Path, s.commands.Dirty,
		s.tasks, s.taskDirty,
		s.notifications.Data, s.notifications.Path, s.notifications.Dirty,
		s.signals.Data, s.signals.Path, s.signals.Dirty)

	return phaseAResult{
		work:      s.work,
		scanStart: s.scanStart,
		counters:  qh.scanCounters,
	}
}

// initScanState loads all queue files and initialises a scanState.
func (qh *QueueHandler) initScanState() scanState {
	scanStart := qh.clock.Now()
	qh.scanCounters = ScanCounters{}

	commandQueue, commandPath := qh.loadCommandQueue()
	taskQueues := qh.loadAllTaskQueues()
	notificationQueue, notificationPath := qh.loadNotificationQueue()
	signalQueue, signalPath := qh.loadPlannerSignalQueue()

	return scanState{
		commands:      fileState[model.CommandQueue]{Data: commandQueue, Path: commandPath},
		tasks:         taskQueues,
		taskDirty:     make(map[string]bool),
		notifications: fileState[model.NotificationQueue]{Data: notificationQueue, Path: notificationPath},
		signals:       fileState[model.PlannerSignalQueue]{Data: signalQueue, Path: signalPath},
		signalIndex:   buildSignalIndex(signalQueue.Signals),
		scanStart:     scanStart,
	}
}

// --- Phase A step functions (executed in order) ---

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
	qh.scanCounters.DeadLetters += len(dlResults)
	for _, dl := range dlResults {
		if dl.TaskID != "" {
			qh.scanCounters.TasksFailed++
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

		shouldTrip, reason := qh.circuitBreaker.CheckProgressTimeout(cmd.ID)
		if shouldTrip {
			timeoutMin := qh.circuitBreaker.ProgressTimeoutMinutes()
			if err := qh.circuitBreaker.StateReader().TripCircuitBreaker(cmd.ID, reason, timeoutMin); err != nil {
				qh.log(LogLevelError, "circuit_breaker_trip_timeout command=%s error=%v", cmd.ID, err)
			} else {
				qh.log(LogLevelWarn, "circuit_breaker_tripped_timeout command=%s reason=%s", cmd.ID, reason)
			}
		}

		if qh.circuitBreaker.StateReader() == nil {
			continue
		}
		cbState, err := qh.circuitBreaker.StateReader().GetCircuitBreakerState(cmd.ID)
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

// stepCancelPending — Step 0.5: Cancel pending tasks for cancelled commands.
func (qh *QueueHandler) stepCancelPending(s *scanState) {
	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if qh.cancelHandler.IsCommandCancelRequested(cmd) {
			for queueFile, tq := range s.tasks {
				results := qh.cancelHandler.CancelPendingTasks(tq.Queue.Tasks, cmd.ID)
				if len(results) > 0 {
					s.taskDirty[queueFile] = true
					qh.scanCounters.TasksCancelled += len(results)
					wID := workerIDFromPath(queueFile)
					if wID != "" {
						qh.cancelHandler.WriteSyntheticResults(results, wID)
					}
				}
			}
		}
	}
}

// stepCancelInterrupt — Step 0.6: Interrupt in_progress tasks for cancelled commands.
func (qh *QueueHandler) stepCancelInterrupt(s *scanState) {
	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if qh.cancelHandler.IsCommandCancelRequested(cmd) {
			for queueFile, tq := range s.tasks {
				wID := workerIDFromPath(queueFile)
				results, interrupts := qh.cancelHandler.InterruptInProgressTasksDeferred(tq.Queue.Tasks, cmd.ID, wID)
				if len(results) > 0 {
					s.taskDirty[queueFile] = true
					qh.scanCounters.TasksCancelled += len(results)
					if wID != "" {
						qh.cancelHandler.WriteSyntheticResults(results, wID)
					}
				}
				s.work.interrupts = append(s.work.interrupts, interrupts...)
			}
		}
	}
}

// stepPhaseTransitions — Step 0.7: Detect and persist phase transitions.
func (qh *QueueHandler) stepPhaseTransitions(s *scanState) {
	if qh.dependencyResolver.stateReader == nil {
		return
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		transitions, err := qh.dependencyResolver.CheckPhaseTransitions(cmd.ID)
		if err != nil {
			qh.log(LogLevelWarn, "phase_transition_check command=%s error=%v", cmd.ID, err)
			continue
		}

		for _, tr := range transitions {
			qh.log(LogLevelInfo, "phase_transition command=%s phase=%s %s→%s reason=%s",
				cmd.ID, tr.PhaseName, tr.OldStatus, tr.NewStatus, tr.Reason)

			if err := qh.dependencyResolver.stateReader.ApplyPhaseTransition(cmd.ID, tr.PhaseID, tr.NewStatus); err != nil {
				qh.log(LogLevelError, "phase_transition_apply command=%s phase=%s error=%v",
					cmd.ID, tr.PhaseID, err)
				continue
			}

			now := qh.clock.Now().UTC().Format(time.RFC3339)
			switch tr.NewStatus {
			case model.PhaseStatusAwaitingFill:
				phase := PhaseInfo{ID: tr.PhaseID, Name: tr.PhaseName}
				msg := qh.dependencyResolver.BuildAwaitingFillNotification(cmd.ID, phase)
				qh.log(LogLevelInfo, "awaiting_fill_signal command=%s phase=%s",
					cmd.ID, tr.PhaseName)
				qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
					Kind:      "awaiting_fill",
					CommandID: cmd.ID,
					PhaseID:   tr.PhaseID,
					PhaseName: tr.PhaseName,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, s.signalIndex)
			case model.PhaseStatusTimedOut:
				msg := fmt.Sprintf("[maestro] kind:fill_timeout command_id:%s phase:%s\nfill deadline expired",
					cmd.ID, tr.PhaseName)
				qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
					Kind:      "fill_timeout",
					CommandID: cmd.ID,
					PhaseID:   tr.PhaseID,
					PhaseName: tr.PhaseName,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, s.signalIndex)
			}
		}
	}
}

// stepWorktreePhaseMerges — Step 0.7.1: Collect merge work items for Phase B.
func (qh *QueueHandler) stepWorktreePhaseMerges(s *scanState) {
	if qh.worktreeManager == nil || qh.dependencyResolver.stateReader == nil {
		return
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		mergeItems := qh.collectWorktreePhaseMerges(cmd.ID)
		s.work.worktreeMerges = append(s.work.worktreeMerges, mergeItems...)
	}
}

// stepWorktreePublish — Step 0.7.2: Detect command completion for worktree publishing.
func (qh *QueueHandler) stepWorktreePublish(s *scanState) {
	if qh.worktreeManager == nil || qh.dependencyResolver.stateReader == nil {
		return
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		publishes, cleanups := qh.collectWorktreePublishAndCleanup(cmd.ID, s.tasks)
		s.work.worktreePublishes = append(s.work.worktreePublishes, publishes...)
		s.work.worktreeCleanups = append(s.work.worktreeCleanups, cleanups...)
	}
}

// stepPlannerSignals — Step 0.8: Evaluate backoff/staleness, defer delivery.
func (qh *QueueHandler) stepPlannerSignals(s *scanState) {
	if len(s.signals.Data.Signals) > 0 {
		qh.processPlannerSignalsDeferred(&s.signals.Data, &s.signals.Dirty, &s.work)
	}
}

// stepPreemptiveRenewal — Renew command leases before checking expired leases.
func (qh *QueueHandler) stepPreemptiveRenewal(s *scanState) {
	qh.preemptiveCommandRenewal(&s.commands.Data, &s.commands.Dirty)
}

// stepDispatchOrRecovery — Steps 1 & 2: Dispatch or recovery (mutually exclusive).
func (qh *QueueHandler) stepDispatchOrRecovery(s *scanState) {
	expiredExists := qh.hasExpiredLeases(s.tasks, &s.commands.Data, &s.notifications.Data)

	if expiredExists {
		// Step 2: Collect busy check items for expired leases
		for queueFile, tq := range s.tasks {
			agentID := workerIDFromPath(queueFile)
			d := s.taskDirty[queueFile]
			items := qh.collectExpiredTaskBusyChecks(tq, agentID, queueFile, &d)
			s.taskDirty[queueFile] = d
			s.work.busyChecks = append(s.work.busyChecks, items...)
		}
		qh.autoExtendExpiredCommandLeases(&s.commands.Data, &s.commands.Dirty)
		qh.recoverExpiredNotificationLeases(&s.notifications.Data, &s.notifications.Dirty)
		qh.log(LogLevelDebug, "expired_leases_detected busy_checks=%d skipping_dispatch", len(s.work.busyChecks))
	} else {
		// Step 1: Collect dispatch items
		qh.collectPendingCommandDispatches(&s.commands.Data, &s.commands.Dirty, &s.work)

		globalInFlight := qh.buildGlobalInFlightSet(s.tasks)
		for queueFile, tq := range s.tasks {
			workerID := workerIDFromPath(queueFile)
			if workerID == "" {
				qh.log(LogLevelWarn, "skip_dispatch cannot derive worker from %s", queueFile)
				continue
			}
			dirty := qh.collectPendingTaskDispatches(tq, workerID, globalInFlight, &s.work)
			if dirty {
				s.taskDirty[queueFile] = true
			}
		}
		qh.collectPendingNotificationDispatches(&s.notifications.Data, &s.notifications.Dirty, &s.work)
	}
}

// stepDependencyFailures — Step 1.5: Check pending/in-progress tasks for dependency failures.
func (qh *QueueHandler) stepDependencyFailures(s *scanState) {
	for queueFile, tq := range s.tasks {
		dirty, interrupts := qh.checkPendingDependencyFailuresDeferred(tq, workerIDFromPath(queueFile))
		dirty2, interrupts2 := qh.checkInProgressDependencyFailuresDeferred(tq, workerIDFromPath(queueFile))
		if dirty || dirty2 {
			s.taskDirty[queueFile] = true
		}
		s.work.interrupts = append(s.work.interrupts, interrupts...)
		s.work.interrupts = append(s.work.interrupts, interrupts2...)
	}
}
