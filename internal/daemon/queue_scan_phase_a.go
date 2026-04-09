package daemon

import (
	"errors"
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

	// Reset admission control and record current in-flight tasks.
	qh.stepAdmissionSync(&s)

	// Execute steps in fixed order (matches original Step 0 through Step 1.5).
	qh.stepDeadLetters(&s)
	qh.stepCircuitBreaker(&s)
	qh.stepCancelPending(&s)
	qh.stepCancelInterrupt(&s)
	qh.stepPhaseTransitions(&s)
	qh.stepWorktreePhaseMerges(&s)
	qh.stepWorktreePublish(&s)
	qh.stepWorktreeFastTrackCleanup(&s)
	qh.stepWorktreeOrphanCleanup(&s)
	qh.stepWorktreeStallDetection(&s)
	qh.stepCheckWorktreeConfigViolations(&s)
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

// initScanState loads all queue files and initializes a scanState.
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

		stateReader := qh.circuitBreaker.StateReader()
		if stateReader == nil {
			continue
		}

		shouldTrip, reason := qh.circuitBreaker.CheckProgressTimeout(cmd.ID)
		if shouldTrip {
			timeoutMin := qh.circuitBreaker.ProgressTimeoutMinutes()
			if err := stateReader.TripCircuitBreaker(cmd.ID, reason, timeoutMin); err != nil {
				qh.log(LogLevelError, "circuit_breaker_trip_timeout command=%s error=%v", cmd.ID, err)
			} else {
				qh.log(LogLevelWarn, "circuit_breaker_tripped_timeout command=%s reason=%s", cmd.ID, reason)
			}
		}

		cbState, err := stateReader.GetCircuitBreakerState(cmd.ID)
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

			if err := qh.dependencyResolver.GetStateReader().ApplyPhaseTransition(cmd.ID, tr.PhaseID, tr.NewStatus); err != nil {
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
	if qh.worktreeManager == nil || !qh.dependencyResolver.HasStateReader() {
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
		mergeItems := qh.collectWorktreePhaseMerges(cmd.ID, s.tasks)
		s.work.worktreeMerges = append(s.work.worktreeMerges, mergeItems...)
	}
}

// stepWorktreePublish — Step 0.7.2: Detect command completion for worktree publishing.
//
// Originally gated on cmd.Status == in_progress, which leaked
// .maestro/worktrees/<cmd>/ directories whenever a command terminated (failed,
// cancelled, or completed via a path that did not transit publish) before the
// integration branch was merged or cleaned. The guard is now relaxed: terminal
// commands are also considered as long as their integration branch is still in
// a state collectWorktreePublishAndCleanup can act on (created → publish/no-op,
// merged → publish, published → cleanup-on-success). Other states (publishing,
// quarantined, …) are skipped to avoid stomping on in-flight Phase B work or
// operator-managed quarantines. Pending commands are still skipped — they have
// no worktrees yet.
func (qh *QueueHandler) stepWorktreePublish(s *scanState) {
	if qh.worktreeManager == nil || !qh.dependencyResolver.HasStateReader() {
		return
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status == model.StatusPending {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		if cmd.Status != model.StatusInProgress {
			// Terminal command — only proceed if the integration branch is in
			// a state the publish/cleanup pathway can still act on.
			cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
			if err != nil || cmdState == nil {
				continue
			}
			switch cmdState.Integration.Status {
			case model.IntegrationStatusCreated,
				model.IntegrationStatusMerged,
				model.IntegrationStatusPublished:
				// fallthrough to collect
			default:
				continue
			}
		}
		publishes, cleanups := qh.collectWorktreePublishAndCleanup(cmd.ID, cmd.Content, s.tasks)
		s.work.worktreePublishes = append(s.work.worktreePublishes, publishes...)
		s.work.worktreeCleanups = append(s.work.worktreeCleanups, cleanups...)
	}
}

// stepWorktreeOrphanCleanup — Cleanup for non-phased terminal commands whose
// worktrees would otherwise leak.
//
// stepWorktreePublish reuses collectWorktreePublishAndCleanup, which only
// emits a cleanup item when integration.status is published (cleanup-on-success)
// or when there is a failed task with cleanup-on-failure enabled. That leaves
// a gap: a non-phased command can terminate (failed/cancelled) with the
// integration branch still in {created, failed, conflict, partial_merge} —
// nothing in publish/fast-track collects a cleanup, and the worktree directory
// + state yaml linger until the 168h GC reaper.
//
// This step targets exactly that gap. It is intentionally narrow:
//
//   - cmd terminal (any IsTerminal status)
//   - HasWorktrees
//   - integration ∈ {created, failed, conflict, partial_merge}
//     (merged/published handled by stepWorktreePublish, publishing is
//     in-flight Phase B, quarantined requires manual operator action)
//   - elapsed since cmd.UpdatedAt ≥ worktree.stall_cleanup_after
//
// Idempotency vs stepWorktreePublish: the integration-status sets are
// disjoint, so the same command never produces a cleanup item from both
// steps in the same scan. Across scans, once Phase B runs the cleanup the
// worktree state file disappears and HasWorktrees returns false on the next
// scan, so this step naturally stops firing.
//
// Skipped when:
//   - worktree isolation is disabled
//   - the configured duration is 0
//   - the command is not terminal
//   - the command has no worktrees
//   - integration status is not in the orphan set above
//   - the command's UpdatedAt is within the threshold
func (qh *QueueHandler) stepWorktreeOrphanCleanup(s *scanState) {
	if qh.worktreeManager == nil {
		return
	}
	if !qh.config.Worktree.Enabled {
		return
	}
	threshold := qh.config.Worktree.EffectiveStallCleanupAfter()
	if threshold <= 0 {
		return
	}
	now := qh.clock.Now()

	// Build a set of commands that earlier steps in this scan already
	// enqueued a cleanup for, so we never emit two cleanup items for the
	// same command in one scan (Phase B does not dedupe).
	alreadyCleaning := make(map[string]struct{}, len(s.work.worktreeCleanups))
	for _, c := range s.work.worktreeCleanups {
		alreadyCleaning[c.CommandID] = struct{}{}
	}

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if !model.IsTerminal(cmd.Status) {
			continue
		}
		if _, dup := alreadyCleaning[cmd.ID]; dup {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil || cmdState == nil {
			continue
		}
		switch cmdState.Integration.Status {
		case model.IntegrationStatusCreated,
			model.IntegrationStatusFailed,
			model.IntegrationStatusConflict,
			model.IntegrationStatusPartialMerge:
			// orphan candidate
		default:
			continue
		}

		// Use the latest of cmd.UpdatedAt and integration.UpdatedAt as the
		// staleness reference. Integration states like failed/conflict/
		// partial_merge can be written long after the command itself
		// terminated (e.g. a fresh publish conflict on a long-completed
		// command), so keying off cmd.UpdatedAt alone would clean up
		// immediately with no grace period. Taking the max gives the
		// orphan at least `threshold` of quiet time before cleanup.
		ref := cmd.UpdatedAt
		if ref == "" {
			ref = cmd.CreatedAt
		}
		refTime, err := time.Parse(time.RFC3339, ref)
		if err != nil {
			continue
		}
		if intTime, ierr := time.Parse(time.RFC3339, cmdState.Integration.UpdatedAt); ierr == nil && intTime.After(refTime) {
			refTime = intTime
		}
		elapsed := now.Sub(refTime)
		if elapsed < threshold {
			continue
		}

		alreadyCleaning[cmd.ID] = struct{}{}
		s.work.worktreeCleanups = append(s.work.worktreeCleanups, worktreeCleanupItem{
			CommandID: cmd.ID,
			Reason:    "orphan_terminal",
		})
		qh.log(LogLevelWarn,
			"orphan_worktree_cleanup_triggered command=%s cmd_status=%s integration_status=%s elapsed=%s threshold=%s",
			cmd.ID, cmd.Status, cmdState.Integration.Status, elapsed.Round(time.Second), threshold)
	}
}

// stepWorktreeFastTrackCleanup — Fast-track cleanup for stalled phases.
//
// When all of a command's tasks have reached terminal state but at least one
// phase remains non-terminal (pending / awaiting_fill / filling / active) and
// the command has not been updated for longer than
// `worktree.stall_cleanup_after`, force-fail the stuck phases and queue a
// worktree cleanup item that bypasses the merge path. This prevents
// .maestro/worktrees/cmd_*/ directories from leaking when daemon or worker
// sessions are interrupted mid-phase, while reusing the existing
// `worktreeCleanupItem` Phase B pathway (no new cleanup channel introduced).
//
// Skipped when:
//   - worktree isolation is disabled
//   - the configured duration is 0 (explicit disable)
//   - the command is not in_progress / has no worktrees
//   - any task is still non-terminal
//   - all phases are already terminal (handled by stepWorktreePublish)
//   - the integration is already quarantined (manual operator state)
//   - the command's UpdatedAt is within the threshold
func (qh *QueueHandler) stepWorktreeFastTrackCleanup(s *scanState) {
	if qh.worktreeManager == nil || !qh.dependencyResolver.HasStateReader() {
		return
	}
	if !qh.config.Worktree.Enabled {
		return
	}
	threshold := qh.config.Worktree.EffectiveStallCleanupAfter()
	if threshold <= 0 {
		return
	}
	now := qh.clock.Now()

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		allTerm, _ := qh.checkCommandTasksTerminal(cmd.ID, s.tasks)
		if !allTerm {
			continue
		}
		phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(cmd.ID)
		if err != nil {
			// Non-phased commands are handled by collectWorktreePublishAndCleanup;
			// other errors fail closed (do not force cleanup).
			continue
		}
		var stuck []PhaseInfo
		for _, p := range phases {
			if !model.IsPhaseTerminal(p.Status) &&
				p.Status != model.PhaseStatusAwaitingFill {
				// awaiting_fill is a legitimate waiting state: the planner has
				// been notified and is expected to submit tasks.  It has its own
				// fill-deadline timeout (checkAwaitingFillTimeout) and must not
				// be killed here.  Only truly stalled phases (pending, filling,
				// active with no running tasks) should be force-failed.
				stuck = append(stuck, p)
			}
		}
		if len(stuck) == 0 {
			continue
		}

		ref := cmd.UpdatedAt
		if ref == "" {
			ref = cmd.CreatedAt
		}
		refTime, err := time.Parse(time.RFC3339, ref)
		if err != nil {
			continue
		}
		elapsed := now.Sub(refTime)
		if elapsed < threshold {
			continue
		}

		cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil {
			continue
		}
		if cmdState.Integration.Status == model.IntegrationStatusQuarantined {
			continue
		}

		applied := qh.forceFailStuckPhases(cmd.ID, stuck, elapsed)
		if applied == 0 {
			continue
		}

		// Best-effort: align integration status so post-cleanup state is
		// consistent. Errors are logged but do not block the cleanup item.
		if err := qh.worktreeManager.MarkIntegrationFailed(cmd.ID); err != nil {
			qh.log(LogLevelWarn,
				"fast_track_cleanup_mark_integration_failed command=%s error=%v",
				cmd.ID, err)
		}

		s.work.worktreeCleanups = append(s.work.worktreeCleanups, worktreeCleanupItem{
			CommandID: cmd.ID,
			Reason:    "fast_track_stall",
		})
		qh.log(LogLevelWarn,
			"fast_track_cleanup_triggered command=%s stuck_phases=%d elapsed=%s threshold=%s",
			cmd.ID, applied, elapsed.Round(time.Second), threshold)
	}
}

// forceFailStuckPhases transitions each stuck phase to failed, logging each
// transition. Returns the number of phases successfully transitioned.
func (qh *QueueHandler) forceFailStuckPhases(commandID string, stuck []PhaseInfo, elapsed time.Duration) int {
	applied := 0
	for _, p := range stuck {
		if err := qh.dependencyResolver.GetStateReader().ApplyPhaseTransition(
			commandID, p.ID, model.PhaseStatusFailed,
		); err != nil {
			qh.log(LogLevelWarn,
				"fast_track_cleanup_phase_apply_failed command=%s phase=%s old_status=%s error=%v",
				commandID, p.ID, p.Status, err)
			continue
		}
		qh.log(LogLevelWarn,
			"fast_track_cleanup_phase_failed command=%s phase=%s old_status=%s elapsed=%s",
			commandID, p.ID, p.Status, elapsed.Round(time.Second))
		applied++
	}
	return applied
}

// stepWorktreeStallDetection — Step 0.7.3: Detect commands whose tasks and
// phases are all terminal but whose integration branch remains stuck in
// {created, merged} for longer than the configured stall timeout. Emits a
// worktree_stalled planner signal once per command (deduped via the
// integration state's StallSignaled flag). If signal persistence fails, the
// integration is transitioned to Failed so the command is not re-detected
// indefinitely.
func (qh *QueueHandler) stepWorktreeStallDetection(s *scanState) {
	if qh.worktreeManager == nil || !qh.dependencyResolver.HasStateReader() {
		return
	}
	if !qh.config.Worktree.Enabled {
		return
	}
	timeoutMin := qh.config.Worktree.EffectiveStallTimeoutMinutes()
	if timeoutMin <= 0 {
		return
	}

	now := qh.clock.Now()
	threshold := now.Add(-time.Duration(timeoutMin) * time.Minute)

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil {
			continue
		}
		if cmdState.Integration.Status != model.IntegrationStatusCreated &&
			cmdState.Integration.Status != model.IntegrationStatusMerged {
			continue
		}
		if cmdState.Integration.StallSignaled {
			continue
		}
		if !qh.allPhasesAndTasksTerminal(cmd.ID, s.tasks) {
			continue
		}

		// Phase 0 件 + Integration.Status==created の早期 stall fast-path:
		// phases が一切定義されていない command が created のまま放置されている
		// ケースは timeoutMin を待つ必要がない (case 5)。即時 stall シグナルを発火する。
		if cmdState.Integration.Status == model.IntegrationStatusCreated {
			phases, perr := qh.dependencyResolver.GetStateReader().GetCommandPhases(cmd.ID)
			noPhases := false
			if perr != nil {
				if errors.Is(perr, ErrStateNotFound) {
					noPhases = true
				}
			} else if len(phases) == 0 {
				noPhases = true
			}
			if noPhases {
				reason := "integration_stalled_no_phases:created"
				nowStr := now.UTC().Format(time.RFC3339)
				msg := fmt.Sprintf("[maestro] kind:worktree_stalled command_id:%s\nreason: %s\nstalled_since: %s",
					cmd.ID, reason, nowStr)
				qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
					Kind:      "worktree_stalled",
					CommandID: cmd.ID,
					Reason:    reason,
					Message:   msg,
					CreatedAt: nowStr,
					UpdatedAt: nowStr,
				}, s.signalIndex)

				markFn := qh.worktreeStallMarkFn
				if markFn == nil {
					markFn = qh.worktreeManager.MarkIntegrationStallSignaled
				}
				if err := markFn(cmd.ID); err != nil {
					qh.log(LogLevelWarn, "worktree_stall_mark_failed command=%s error=%v", cmd.ID, err)
					if mfErr := qh.worktreeManager.MarkIntegrationFailed(cmd.ID); mfErr != nil {
						qh.log(LogLevelError, "worktree_stall_integration_failed_transition command=%s error=%v",
							cmd.ID, mfErr)
					} else {
						qh.log(LogLevelWarn, "worktree_stall_integration_marked_failed command=%s", cmd.ID)
					}
					continue
				}
				qh.log(LogLevelWarn, "worktree_stall_signal_emitted command=%s reason=%s stalled_since=%s",
					cmd.ID, reason, nowStr)
				continue
			}
		}

		ref := cmd.UpdatedAt
		if ref == "" {
			ref = cmd.CreatedAt
		}
		refTime, err := time.Parse(time.RFC3339, ref)
		if err != nil {
			continue
		}
		if !refTime.Before(threshold) {
			continue
		}

		reason := fmt.Sprintf("integration_stalled:%s", cmdState.Integration.Status)
		stalledSince := refTime.UTC().Format(time.RFC3339)
		nowStr := now.UTC().Format(time.RFC3339)
		msg := fmt.Sprintf("[maestro] kind:worktree_stalled command_id:%s\nreason: %s\nstalled_since: %s",
			cmd.ID, reason, stalledSince)

		qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
			Kind:      "worktree_stalled",
			CommandID: cmd.ID,
			Reason:    reason,
			Message:   msg,
			CreatedAt: nowStr,
			UpdatedAt: nowStr,
		}, s.signalIndex)

		markFn := qh.worktreeStallMarkFn
		if markFn == nil {
			markFn = qh.worktreeManager.MarkIntegrationStallSignaled
		}
		if err := markFn(cmd.ID); err != nil {
			qh.log(LogLevelWarn, "worktree_stall_mark_failed command=%s error=%v", cmd.ID, err)
			if mfErr := qh.worktreeManager.MarkIntegrationFailed(cmd.ID); mfErr != nil {
				qh.log(LogLevelError, "worktree_stall_integration_failed_transition command=%s error=%v",
					cmd.ID, mfErr)
			} else {
				qh.log(LogLevelWarn, "worktree_stall_integration_marked_failed command=%s", cmd.ID)
			}
			continue
		}
		qh.log(LogLevelWarn, "worktree_stall_signal_emitted command=%s reason=%s stalled_since=%s",
			cmd.ID, reason, stalledSince)
	}
}

// allPhasesAndTasksTerminal returns true iff every task that belongs to the
// given command is terminal AND every phase known to the state reader is
// terminal. Used by stall detection.
func (qh *QueueHandler) allPhasesAndTasksTerminal(commandID string, taskQueues map[string]*taskQueueEntry) bool {
	allTerm, _ := qh.checkCommandTasksTerminal(commandID, taskQueues)
	if !allTerm {
		return false
	}
	phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(commandID)
	if err != nil {
		// Non-phased commands surface ErrStateNotFound here; we still want
		// stall detection in that case, so a not-found error is treated as
		// "no phases" rather than a hard failure. Other errors fail closed.
		if !errors.Is(err, ErrStateNotFound) {
			return false
		}
		return true
	}
	for _, p := range phases {
		if !model.IsPhaseTerminal(p.Status) {
			return false
		}
	}
	return true
}

// stepCheckWorktreeConfigViolations — Step 0.7.4: When AutoCommit/AutoMerge are
// disabled, warn the operator (via WARN log + planner signal) for any in-progress
// command whose integration branch has stayed unmerged longer than the configured
// fallback timeout. The daemon does NOT force a merge; this step only surfaces
// the situation so the operator can act. Independent of stepWorktreeStallDetection.
func (qh *QueueHandler) stepCheckWorktreeConfigViolations(s *scanState) {
	if qh.worktreeManager == nil {
		return
	}
	// Both flags enabled → normal mode, nothing to warn about.
	if qh.worktreeManager.AutoCommit() && qh.worktreeManager.AutoMerge() {
		return
	}
	timeoutMin := qh.config.Worktree.EffectiveFallbackMergeTimeoutMinutes()
	if timeoutMin <= 0 {
		return
	}
	timeout := time.Duration(timeoutMin) * time.Minute

	for i := range s.commands.Data.Commands {
		cmd := &s.commands.Data.Commands[i]
		if cmd.Status != model.StatusInProgress {
			continue
		}
		if !qh.worktreeManager.HasWorktrees(cmd.ID) {
			continue
		}
		cmdState, err := qh.worktreeManager.GetCommandState(cmd.ID)
		if err != nil || cmdState == nil {
			continue
		}
		switch cmdState.Integration.Status {
		case model.IntegrationStatusMerged,
			model.IntegrationStatusPublishing,
			model.IntegrationStatusPublished:
			continue
		}
		createdAt, err := time.Parse(time.RFC3339, cmdState.Integration.CreatedAt)
		if err != nil {
			continue
		}
		elapsed := qh.clock.Now().Sub(createdAt)
		if elapsed < timeout {
			continue
		}

		elapsedMin := int(elapsed.Minutes())
		autoCommit := qh.worktreeManager.AutoCommit()
		autoMerge := qh.worktreeManager.AutoMerge()
		qh.log(LogLevelWarn,
			"worktree_config_violation command=%s auto_commit=%v auto_merge=%v elapsed_min=%d timeout_min=%d",
			cmd.ID, autoCommit, autoMerge, elapsedMin, timeoutMin)

		now := qh.clock.Now().UTC().Format(time.RFC3339)
		msg := fmt.Sprintf(
			"[maestro] kind:worktree_config_violation command_id:%s\n"+
				"auto_commit=%v auto_merge=%v\n"+
				"integration branch unmerged for %d minutes (timeout: %d)\n"+
				"operator action required: review worktree config or merge manually",
			cmd.ID, autoCommit, autoMerge, elapsedMin, timeoutMin)
		qh.upsertPlannerSignal(&s.signals.Data, &s.signals.Dirty, model.PlannerSignal{
			Kind:      "worktree_config_violation",
			CommandID: cmd.ID,
			Message:   msg,
			CreatedAt: now,
			UpdatedAt: now,
		}, s.signalIndex)
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
