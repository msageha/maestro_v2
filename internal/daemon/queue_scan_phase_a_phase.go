package daemon

import (
	"fmt"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// stepPhaseTransitions — Step 0.7: Detect and persist phase transitions.
func (qh *QueueHandler) stepPhaseTransitions(s *scanState) {
	if !qh.dependencyResolver.HasStateReader() {
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

			if err := qh.dependencyResolver.GetStateManager().ApplyPhaseTransition(cmd.ID, tr.PhaseID, tr.NewStatus); err != nil {
				qh.log(LogLevelError, "phase_transition_apply command=%s phase=%s error=%v",
					cmd.ID, tr.PhaseID, err)
				continue
			}

			now := qh.clock.Now().UTC().Format(time.RFC3339)
			switch tr.NewStatus {
			case model.PhaseStatusCompleted:
				// A-3: Self-diagnosis on phase completion
				diagPrompt := qh.diagnosePhaseTasks(cmd.ID, tr.PhaseID, tr.PhaseName, s.tasks)
				if diagPrompt != "" {
					msg := fmt.Sprintf("[maestro] kind:phase_diagnosis command_id:%s phase:%s\n%s",
						cmd.ID, tr.PhaseName, diagPrompt)
					qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
						Kind:      "phase_diagnosis",
						CommandID: cmd.ID,
						PhaseID:   tr.PhaseID,
						PhaseName: tr.PhaseName,
						Message:   msg,
						CreatedAt: now,
						UpdatedAt: now,
					}, s.signalIndex)
					qh.log(LogLevelInfo, "phase_diagnosis_emitted command=%s phase=%s",
						cmd.ID, tr.PhaseName)
				}
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

// stepPlannerSignals — Step 0.8: Evaluate backoff/staleness, defer delivery.
func (qh *QueueHandler) stepPlannerSignals(s *scanState) {
	if len(s.signals.Data.Signals) > 0 {
		qh.stepPlannerSignalsDeferred(&s.signals.Data, &s.signals.Dirty, &s.work)
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
		inFlightPaths := collectInFlightPaths(s.tasks, qh.leaseManager.IsLeaseExpired)
		for queueFile, tq := range s.tasks {
			workerID := workerIDFromPath(queueFile)
			if workerID == "" {
				qh.log(LogLevelWarn, "skip_dispatch cannot derive worker from %s", queueFile)
				continue
			}
			dirty := qh.collectPendingTaskDispatches(tq, workerID, globalInFlight, inFlightPaths, &s.work)
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
		dirty := qh.checkPendingDependencyFailuresDeferred(tq, workerIDFromPath(queueFile))
		dirty2, interrupts2 := qh.checkInProgressDependencyFailuresDeferred(tq, workerIDFromPath(queueFile))
		if dirty || dirty2 {
			s.taskDirty[queueFile] = true
		}
		s.work.interrupts = append(s.work.interrupts, interrupts2...)
	}
}

// diagnosePhaseTasks collects tasks belonging to a completed phase and produces
// a diagnosis prompt using the injected phaseDiagnoser. Returns the formatted
// prompt string, or "" if diagnosis yields no actionable information or if
// no diagnoser is configured.
func (qh *QueueHandler) diagnosePhaseTasks(commandID, phaseID, _ string, taskQueues map[string]*taskQueueEntry) string {
	if qh.scanExecutor.phaseDiagnoser == nil || !qh.dependencyResolver.HasStateReader() {
		return ""
	}

	phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(commandID)
	if err != nil {
		qh.log(LogLevelWarn, "phase_diagnosis_skip command=%s phase=%s reason=get_phases_error: %v",
			commandID, phaseID, err)
		return ""
	}

	// Find the phase and its task IDs
	var phaseTaskIDs []string
	var phase model.Phase
	for _, p := range phases {
		if p.ID == phaseID {
			phaseTaskIDs = p.RequiredTaskIDs
			phase = model.Phase{
				PhaseID: p.ID,
				Name:    p.Name,
				Status:  p.Status,
			}
			break
		}
	}

	if len(phaseTaskIDs) == 0 {
		return ""
	}

	// Build a set of task IDs for fast lookup
	taskIDSet := make(map[string]bool, len(phaseTaskIDs))
	for _, id := range phaseTaskIDs {
		taskIDSet[id] = true
	}

	// Collect matching tasks from all task queues
	var tasks []model.Task
	for _, tq := range taskQueues {
		for i := range tq.Queue.Tasks {
			if taskIDSet[tq.Queue.Tasks[i].ID] {
				tasks = append(tasks, tq.Queue.Tasks[i])
			}
		}
	}

	return qh.scanExecutor.phaseDiagnoser(phase, tasks, nil)
}
