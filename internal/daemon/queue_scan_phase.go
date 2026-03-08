package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
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

// periodicScanPhaseB executes all slow tmux I/O operations without holding any lock.
// Order: interrupts → busy checks → dispatches → signals (per Codex review).
// SRE-002: accepts context for cancellation support during slow I/O.
func (qh *QueueHandler) periodicScanPhaseB(ctx context.Context, pa phaseAResult) phaseBResult {
	var result phaseBResult

	// 1. Execute interrupts first (before dispatches to avoid killing new tasks)
	forEachUntilCanceled(ctx, pa.work.interrupts, func(item interruptItem) {
		if err := qh.cancelHandler.interruptAgent(item.WorkerID, item.TaskID, item.CommandID, item.Epoch); err != nil {
			qh.log(LogLevelWarn, "phase_b_interrupt worker=%s task=%s error=%v", item.WorkerID, item.TaskID, err)
		}
	})

	// 2. Execute busy probes for expired leases
	forEachUntilCanceled(ctx, pa.work.busyChecks, func(item busyCheckItem) {
		busy, undecided := qh.isAgentBusy(ctx, item.AgentID)
		result.busyChecks = append(result.busyChecks, busyCheckResult{
			Item:      item,
			Busy:      busy,
			Undecided: undecided,
		})
	})

	// 3. Execute dispatches
	forEachUntilCanceled(ctx, pa.work.dispatches, func(item dispatchItem) {
		var err error
		switch item.Kind {
		case "command":
			err = qh.dispatcher.DispatchCommand(item.Command)
		case "task":
			err = qh.dispatcher.DispatchTask(item.Task, item.WorkerID)
		case "notification":
			err = qh.dispatcher.DispatchNotification(item.Notification)
		}
		result.dispatches = append(result.dispatches, dispatchResult{
			Item:    item,
			Success: err == nil,
			Error:   err,
		})
	})

	// 4. Execute signal deliveries
	forEachUntilCanceled(ctx, pa.work.signals, func(item signalDeliveryItem) {
		err := qh.deliverPlannerSignal(ctx, item.CommandID, item.Message)
		result.signals = append(result.signals, signalDeliveryResult{
			Item:    item,
			Success: err == nil,
			Error:   err,
		})
	})

	// 5. Execute agent clears (fire-and-forget)
	forEachUntilCanceled(ctx, pa.work.clears, func(agentID string) {
		qh.clearAgent(ctx, agentID)
	})

	// 6. Execute worktree merges (slow git I/O, outside scanMu.Lock)
	forEachUntilCanceled(ctx, pa.work.worktreeMerges, func(item worktreeMergeItem) {
		mr := worktreeMergeResult{Item: item}

		// First commit worker changes
		if qh.worktreeManager != nil && qh.worktreeManager.AutoCommit() {
			for _, workerID := range item.WorkerIDs {
				msg := fmt.Sprintf("[maestro] auto-commit phase %s worker %s for %s",
					item.PhaseID, workerID, item.CommandID)
				if err := qh.worktreeManager.CommitWorkerChanges(item.CommandID, workerID, msg); err != nil {
					qh.log(LogLevelWarn, "worktree_auto_commit command=%s worker=%s error=%v",
						item.CommandID, workerID, err)
				}
			}
		}

		// Then merge to integration
		if qh.worktreeManager != nil && qh.worktreeManager.AutoMerge() {
			conflicts, err := qh.worktreeManager.MergeToIntegration(item.CommandID, item.WorkerIDs)
			mr.Conflicts = conflicts
			mr.Error = err

			if len(conflicts) == 0 && err == nil {
				if syncErr := qh.worktreeManager.SyncFromIntegration(item.CommandID, item.WorkerIDs); syncErr != nil {
					qh.log(LogLevelWarn, "worktree_sync_failed command=%s error=%v", item.CommandID, syncErr)
				}
			}
		}

		result.worktreeMerges = append(result.worktreeMerges, mr)
	})

	// 7. Execute worktree publishes (slow git I/O, outside scanMu.Lock)
	var additionalCleanups []worktreeCleanupItem
	forEachUntilCanceled(ctx, pa.work.worktreePublishes, func(item worktreePublishItem) {
		pr := worktreePublishResult{Item: item}
		if qh.worktreeManager != nil {
			cmdState, err := qh.worktreeManager.GetCommandState(item.CommandID)
			if err != nil || cmdState.Integration.Status != model.IntegrationStatusMerged {
				qh.log(LogLevelWarn, "worktree_publish_skip_stale command=%s status=%v err=%v",
					item.CommandID, func() string {
						if cmdState != nil {
							return string(cmdState.Integration.Status)
						}
						return "unknown"
					}(), err)
				pr.Error = fmt.Errorf("integration status no longer merged")
			} else {
				pr.Error = qh.worktreeManager.PublishToBase(item.CommandID)
			}
		}
		result.worktreePublishes = append(result.worktreePublishes, pr)

		if pr.Error == nil && qh.config.Worktree.CleanupOnSuccess {
			additionalCleanups = append(additionalCleanups, worktreeCleanupItem{
				CommandID: item.CommandID,
				Reason:    "success",
			})
		}
	})

	// 8. Execute worktree cleanups (Phase A collected + post-publish)
	allCleanups := append(pa.work.worktreeCleanups, additionalCleanups...)
	forEachUntilCanceled(ctx, allCleanups, func(item worktreeCleanupItem) {
		cr := worktreeCleanupResult{Item: item}
		if qh.worktreeManager != nil {
			cr.Error = qh.worktreeManager.CleanupCommand(item.CommandID)
		}
		result.worktreeCleanups = append(result.worktreeCleanups, cr)
	})

	return result
}

// periodicScanPhaseC runs under scanMu.Lock. It reloads queues from disk,
// applies Phase B results with epoch fencing, flushes, and runs post-flush steps.
// Returns deferred notifications from reconciliation that must be executed outside the lock.
func (qh *QueueHandler) periodicScanPhaseC(pa phaseAResult, pb phaseBResult) []DeferredNotification {
	qh.scanMu.Lock()
	defer qh.scanMu.Unlock()

	// Restore counters accumulated during Phase A
	qh.scanCounters = pa.counters

	// --- Apply dispatch + busy check results (single load/flush) ---
	if len(pb.dispatches) > 0 || len(pb.busyChecks) > 0 {
		commandQueue, commandPath := qh.loadCommandQueue()
		taskQueues := qh.loadAllTaskQueues()
		notificationQueue, notificationPath := qh.loadNotificationQueue()
		commandsDirty := false
		notificationsDirty := false
		taskDirty := make(map[string]bool)

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

		// Single flush for both dispatch and busy check results
		qh.flushQueues(commandQueue, commandPath, commandsDirty,
			taskQueues, taskDirty,
			notificationQueue, notificationPath, notificationsDirty,
			model.PlannerSignalQueue{}, "", false)
	}

	// --- Apply worktree merge results: emit conflict signals, record merged phases ---
	if len(pb.worktreeMerges) > 0 {
		signalQueue, signalPath := qh.loadPlannerSignalQueue()
		signalsDirty := false
		signalIndex := buildSignalIndex(signalQueue.Signals)
		now := qh.clock.Now().UTC().Format(time.RFC3339)
		for _, mr := range pb.worktreeMerges {
			if mr.Error != nil {
				qh.log(LogLevelError, "worktree_merge_failed command=%s phase=%s error=%v",
					mr.Item.CommandID, mr.Item.PhaseID, mr.Error)
			}
			for _, conflict := range mr.Conflicts {
				msg := fmt.Sprintf("[maestro] kind:merge_conflict command_id:%s phase:%s worker:%s\nconflict_files: %s",
					mr.Item.CommandID, mr.Item.PhaseID, conflict.WorkerID,
					strings.Join(conflict.ConflictFiles, ", "))
				qh.upsertPlannerSignal(&signalQueue, &signalsDirty, model.PlannerSignal{
					Kind:      "merge_conflict",
					CommandID: mr.Item.CommandID,
					PhaseID:   mr.Item.PhaseID,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, signalIndex)
			}
			if mr.Error == nil && qh.worktreeManager != nil {
				if err := qh.worktreeManager.MarkPhaseMerged(mr.Item.CommandID, mr.Item.PhaseID); err != nil {
					qh.log(LogLevelWarn, "mark_phase_merged_failed command=%s phase=%s error=%v",
						mr.Item.CommandID, mr.Item.PhaseID, err)
				}
			}
		}
		if signalsDirty {
			p := signalPath
			if p == "" {
				p = filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
			}
			if err := yamlutil.AtomicWrite(p, signalQueue); err != nil {
				qh.log(LogLevelError, "write_planner_signals error=%v", err)
			}
		}
	}

	// --- Apply worktree publish results: emit signal on failure ---
	if len(pb.worktreePublishes) > 0 {
		signalQueue, signalPath := qh.loadPlannerSignalQueue()
		signalsDirty := false
		signalIndex := buildSignalIndex(signalQueue.Signals)
		now := qh.clock.Now().UTC().Format(time.RFC3339)
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
		if signalsDirty {
			p := signalPath
			if p == "" {
				p = filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
			}
			if err := yamlutil.AtomicWrite(p, signalQueue); err != nil {
				qh.log(LogLevelError, "write_planner_signals error=%v", err)
			}
		}
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

	// --- Apply signal delivery results ---
	if len(pb.signals) > 0 {
		signalQueue, signalPath := qh.loadPlannerSignalQueue()
		signalsDirty := false
		qh.applySignalResults(pb.signals, &signalQueue, &signalsDirty)
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

	// Step 2.5: Result notification retry
	if qh.resultHandler != nil {
		n := qh.resultHandler.ScanAllResults()
		qh.scanCounters.NotificationRetries += n
		if n > 0 {
			qh.log(LogLevelInfo, "result_notify_scan notified=%d", n)
		}
	}

	// Step 3: Reconciliation (state mutations under scanMu, notifications deferred)
	var deferredNotifs []DeferredNotification
	if qh.reconciler != nil {
		repairs, notifs := qh.reconciler.Reconcile()
		deferredNotifs = notifs
		qh.scanCounters.ReconciliationRepairs += len(repairs)
		for _, repair := range repairs {
			qh.log(LogLevelInfo, "reconciliation pattern=%s command=%s task=%s detail=%s",
				repair.Pattern, repair.CommandID, repair.TaskID, repair.Detail)
		}
	}

	// Step 4: Metrics and dashboard
	if qh.metricsHandler != nil {
		commandQueue, _ := qh.loadCommandQueue()
		taskQueues := qh.loadAllTaskQueues()
		notificationQueue, _ := qh.loadNotificationQueue()
		scanDuration := qh.clock.Now().Sub(pa.scanStart)
		if err := qh.metricsHandler.UpdateMetrics(commandQueue, taskQueues, notificationQueue, pa.scanStart, scanDuration, &qh.scanCounters); err != nil {
			qh.log(LogLevelError, "update_metrics error=%v", err)
		}
		if err := qh.metricsHandler.UpdateDashboard(commandQueue, taskQueues, notificationQueue); err != nil {
			qh.log(LogLevelError, "update_dashboard error=%v", err)
		}
	}

	return deferredNotifs
}

// --- Collect methods for Phase A ---

// collectPendingCommandDispatches acquires leases and records dispatch items (no tmux).
// Guard: any in_progress command blocks new dispatches regardless of lease validity.
// Planner processes one command at a time; expired leases are handled by busy-check
// recovery (auto-extend for commands) and Reconciler R0 for stuck planning.
func (qh *QueueHandler) collectPendingCommandDispatches(cq *model.CommandQueue, dirty *bool, work *deferredWork) {
	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusInProgress {
			qh.log(LogLevelDebug, "command_in_progress_guard id=%s epoch=%d blocking_dispatch", cmd.ID, cmd.LeaseEpoch)
			return
		}
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

		cmdCopy := *cmd
		work.dispatches = append(work.dispatches, dispatchItem{
			Kind:      "command",
			Command:   &cmdCopy,
			Epoch:     cmd.LeaseEpoch,
			ExpiresAt: safeStr(cmd.LeaseExpiresAt),
		})
		break
	}
}

// collectPendingTaskDispatches acquires leases and records dispatch items (no tmux).
func (qh *QueueHandler) collectPendingTaskDispatches(tq *taskQueueEntry, workerID string, globalInFlight map[string]bool, work *deferredWork) bool {
	dirty := false
	sorted := qh.dispatcher.SortPendingTasks(tq.Queue.Tasks)

	for _, idx := range sorted {
		task := &tq.Queue.Tasks[idx]

		if globalInFlight[workerID] {
			qh.log(LogLevelDebug, "worker_busy worker=%s task=%s (global in-flight)", workerID, task.ID)
			break
		}

		// Check if task is in cooldown period
		if task.NotBefore != nil {
			notBefore, err := time.Parse(time.RFC3339, *task.NotBefore)
			if err == nil && qh.clock.Now().Before(notBefore) {
				qh.log(LogLevelDebug, "task_cooldown task=%s not_before=%s", task.ID, *task.NotBefore)
				continue
			}
		}

		blocked, err := qh.dependencyResolver.IsTaskBlocked(task)
		if err != nil {
			qh.log(LogLevelWarn, "dependency_check_error task=%s error=%v", task.ID, err)
			continue
		}
		if blocked {
			continue
		}

		isSysCommit, ready, sErr := qh.dependencyResolver.IsSystemCommitReady(task.CommandID, task.ID)
		if sErr != nil {
			qh.log(LogLevelWarn, "system_commit_check task=%s error=%v", task.ID, sErr)
			continue
		}
		if isSysCommit && !ready {
			qh.log(LogLevelDebug, "system_commit_not_ready task=%s command=%s", task.ID, task.CommandID)
			continue
		}

		if err := qh.leaseManager.AcquireTaskLease(task, qh.leaseOwnerID()); err != nil {
			qh.log(LogLevelWarn, "lease_acquire_failed type=task id=%s error=%v", task.ID, err)
			continue
		}
		task.Attempts++

		taskCopy := *task
		work.dispatches = append(work.dispatches, dispatchItem{
			Kind:      "task",
			Task:      &taskCopy,
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
	for _, ntf := range nq.Notifications {
		if ntf.Status == model.StatusInProgress && ntf.LeaseExpiresAt != nil {
			if t, err := time.Parse(time.RFC3339, *ntf.LeaseExpiresAt); err == nil && t.After(qh.clock.Now()) {
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

		ntfCopy := *ntf
		work.dispatches = append(work.dispatches, dispatchItem{
			Kind:         "notification",
			Notification: &ntfCopy,
			Epoch:        ntf.LeaseEpoch,
			ExpiresAt:    safeStr(ntf.LeaseExpiresAt),
		})
		break
	}
}

// collectExpiredTaskBusyChecks records busy check items for expired task leases.
// Malformed entries (lease_expires_at == nil) are released immediately since
// Phase C fencing would always reject them as stale.
func (qh *QueueHandler) collectExpiredTaskBusyChecks(tq *taskQueueEntry, agentID, queueFile string, dirty *bool) []busyCheckItem {
	var items []busyCheckItem
	expired := qh.leaseManager.ExpireTasks(tq.Queue.Tasks)
	for _, idx := range expired {
		task := &tq.Queue.Tasks[idx]
		// Malformed entry: no lease_expires_at → release immediately.
		// Phase C fencing requires ExpiresAt match, which can never succeed for nil.
		if task.LeaseExpiresAt == nil {
			qh.log(LogLevelWarn, "expire_release_malformed type=task id=%s (nil lease_expires_at)", task.ID)
			if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=task id=%s error=%v", task.ID, err)
			}
			qh.scanCounters.LeaseReleases++
			*dirty = true
			continue
		}
		if agentID != "" {
			items = append(items, busyCheckItem{
				Kind:      "task",
				EntryID:   task.ID,
				AgentID:   agentID,
				Epoch:     task.LeaseEpoch,
				QueueFile: queueFile,
				UpdatedAt: task.UpdatedAt,
				ExpiresAt: *task.LeaseExpiresAt,
			})
		} else {
			// No agent ID: release immediately
			if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
				qh.log(LogLevelError, "expire_release_failed type=task id=%s error=%v", task.ID, err)
			}
			qh.scanCounters.LeaseReleases++
			*dirty = true
		}
	}
	return items
}

// preemptiveCommandRenewal renews command leases approaching expiry to prevent
// the expire→detect→auto-extend cycle and avoid triggering recovery mode.
func (qh *QueueHandler) preemptiveCommandRenewal(cq *model.CommandQueue, dirty *bool) {
	bufferSec := qh.config.Watcher.ScanIntervalSec + 30
	if bufferSec <= 30 {
		bufferSec = 90
	}
	renewable := qh.leaseManager.RenewableCommands(cq.Commands, bufferSec)
	for _, idx := range renewable {
		cmd := &cq.Commands[idx]
		maxMin := qh.config.Watcher.MaxInProgressMin
		if maxMin <= 0 {
			maxMin = 60
		}
		if t, err := time.Parse(time.RFC3339, cmd.UpdatedAt); err == nil {
			if qh.clock.Now().Sub(t) >= time.Duration(maxMin)*time.Minute {
				qh.log(LogLevelWarn, "command_lease_max_timeout id=%s epoch=%d max=%dm releasing (preemptive)",
					cmd.ID, cmd.LeaseEpoch, maxMin)
				if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "expire_release_failed type=command id=%s error=%v", cmd.ID, err)
				}
				qh.scanCounters.LeaseReleases++
				*dirty = true
				continue
			}
		}
		if err := qh.leaseManager.ExtendCommandLease(cmd); err != nil {
			qh.log(LogLevelError, "command_lease_preemptive_renew_failed id=%s error=%v", cmd.ID, err)
			continue
		}
		qh.log(LogLevelDebug, "command_lease_renewed id=%s epoch=%d", cmd.ID, cmd.LeaseEpoch)
		qh.scanCounters.LeaseRenewals++
		*dirty = true
	}
}

// autoExtendExpiredCommandLeases auto-extends expired command leases in Phase A.
// Unlike tasks, commands are never released on lease expiry because:
//   - Planner is a singleton; releasing causes duplicate dispatch
//   - Busy-check false negatives are common (Planner has long API call intervals)
//   - Reconciler R0 handles truly stuck planning via max_in_progress_min timeout
//
// Malformed entries (lease_expires_at == nil) are repaired by setting a new lease.
func (qh *QueueHandler) autoExtendExpiredCommandLeases(cq *model.CommandQueue, dirty *bool) {
	expired := qh.leaseManager.ExpireCommands(cq.Commands)
	for _, idx := range expired {
		cmd := &cq.Commands[idx]

		// Check max_in_progress_min hard timeout — if exceeded, release to let
		// Reconciler R0 handle the stuck command on next scan.
		maxMin := qh.config.Watcher.MaxInProgressMin
		if maxMin <= 0 {
			maxMin = 60
		}
		if t, err := time.Parse(time.RFC3339, cmd.UpdatedAt); err == nil {
			if qh.clock.Now().Sub(t) >= time.Duration(maxMin)*time.Minute {
				qh.log(LogLevelWarn, "command_lease_max_timeout id=%s epoch=%d max=%dm releasing",
					cmd.ID, cmd.LeaseEpoch, maxMin)
				if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "expire_release_failed type=command id=%s error=%v", cmd.ID, err)
				}
				qh.scanCounters.LeaseReleases++
				*dirty = true
				continue
			}
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
		qh.scanCounters.LeaseExtensions++
		*dirty = true
	}
}

// checkPendingDependencyFailuresDeferred checks pending tasks for dependency failures.
// Same as checkPendingDependencyFailures but compatible with deferred interrupt pattern.
func (qh *QueueHandler) checkPendingDependencyFailuresDeferred(tq *taskQueueEntry, workerID string) (bool, []interruptItem) {
	dirty := false
	var cancelledResults []CancelledTaskResult

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

		if qh.dependencyResolver.stateReader != nil {
			if err := qh.dependencyResolver.stateReader.UpdateTaskState(task.CommandID, task.ID, model.StatusCancelled, reason); err != nil {
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
		qh.scanCounters.TasksCancelled += len(cancelledResults)
	}
	return dirty, nil // pending tasks have no interrupt items
}

// checkInProgressDependencyFailuresDeferred checks in-progress tasks and defers interrupts.
func (qh *QueueHandler) checkInProgressDependencyFailuresDeferred(tq *taskQueueEntry, workerID string) (bool, []interruptItem) {
	dirty := false
	var cancelledResults []CancelledTaskResult
	var interrupts []interruptItem

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

		// Defer interrupt to Phase B
		if workerID != "" {
			interrupts = append(interrupts, interruptItem{
				WorkerID:  workerID,
				TaskID:    task.ID,
				CommandID: task.CommandID,
				Epoch:     task.LeaseEpoch,
			})
		}

		task.Status = model.StatusCancelled
		task.LeaseOwner = nil
		task.LeaseExpiresAt = nil
		task.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
		dirty = true

		if qh.dependencyResolver.stateReader != nil {
			if err := qh.dependencyResolver.stateReader.UpdateTaskState(task.CommandID, task.ID, model.StatusCancelled, reason); err != nil {
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
		qh.scanCounters.TasksCancelled += len(cancelledResults)
	}
	return dirty, interrupts
}

// --- Phase C apply methods ---

func (qh *QueueHandler) applyCommandDispatchResult(dr dispatchResult, cq *model.CommandQueue, dirty *bool) {
	for i := range cq.Commands {
		cmd := &cq.Commands[i]
		if cmd.ID != dr.Item.Command.ID {
			continue
		}
		// Epoch fencing: verify entry hasn't changed since Phase A
		if cmd.LeaseEpoch != dr.Item.Epoch || cmd.Status != model.StatusInProgress ||
			cmd.LeaseExpiresAt == nil || *cmd.LeaseExpiresAt != dr.Item.ExpiresAt {
			qh.log(LogLevelWarn, "dispatch_fence_stale kind=command id=%s epoch=%d/%d",
				cmd.ID, cmd.LeaseEpoch, dr.Item.Epoch)
			return
		}
		if !dr.Success {
			// For transient busy detection errors, release lease to allow immediate retry
			if errors.Is(dr.Error, agent.ErrBusyUndecided) {
				qh.log(LogLevelWarn, "dispatch_failed_undecided_release type=command id=%s", cmd.ID)
				if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "release_command_lease_failed id=%s error=%v", cmd.ID, err)
				} else {
					qh.scanCounters.LeaseReleases++
				}
				*dirty = true
				return
			}

			qh.log(LogLevelWarn, "dispatch_failed_lease_kept type=command id=%s error=%v", cmd.ID, dr.Error)
		} else {
			qh.scanCounters.CommandsDispatched++
		}
		*dirty = true
		return
	}
}

func (qh *QueueHandler) applyTaskDispatchResult(dr dispatchResult, taskQueues map[string]*taskQueueEntry, taskDirty map[string]bool) {
	for queueFile, tq := range taskQueues {
		for i := range tq.Queue.Tasks {
			task := &tq.Queue.Tasks[i]
			if task.ID != dr.Item.Task.ID {
				continue
			}
			if task.LeaseEpoch != dr.Item.Epoch || task.Status != model.StatusInProgress ||
				task.LeaseExpiresAt == nil || *task.LeaseExpiresAt != dr.Item.ExpiresAt {
				qh.log(LogLevelWarn, "dispatch_fence_stale kind=task id=%s epoch=%d/%d",
					task.ID, task.LeaseEpoch, dr.Item.Epoch)
				return
			}
			if !dr.Success {
				qh.log(LogLevelWarn, "dispatch_failed type=task id=%s error=%v", task.ID, dr.Error)
				if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
					qh.log(LogLevelError, "release_task_lease task=%s error=%v", task.ID, err)
				}
				qh.scanCounters.LeaseReleases++
			} else {
				qh.scanCounters.TasksDispatched++
			}
			taskDirty[queueFile] = true
			return
		}
	}
}

func (qh *QueueHandler) applyNotificationDispatchResult(dr dispatchResult, nq *model.NotificationQueue, dirty *bool) {
	for i := range nq.Notifications {
		ntf := &nq.Notifications[i]
		if ntf.ID != dr.Item.Notification.ID {
			continue
		}
		if ntf.LeaseEpoch != dr.Item.Epoch || ntf.Status != model.StatusInProgress ||
			ntf.LeaseExpiresAt == nil || *ntf.LeaseExpiresAt != dr.Item.ExpiresAt {
			qh.log(LogLevelWarn, "dispatch_fence_stale kind=notification id=%s epoch=%d/%d",
				ntf.ID, ntf.LeaseEpoch, dr.Item.Epoch)
			return
		}
		if !dr.Success {
			qh.log(LogLevelWarn, "dispatch_failed type=notification id=%s error=%v", ntf.ID, dr.Error)
			if err := qh.leaseManager.ReleaseNotificationLease(ntf); err != nil {
				qh.log(LogLevelError, "release_notification_lease id=%s error=%v", ntf.ID, err)
			}
		} else {
			ntf.Status = model.StatusCompleted
			ntf.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
		}
		*dirty = true
		return
	}
}

func (qh *QueueHandler) applyTaskBusyCheckResult(bc busyCheckResult, taskQueues map[string]*taskQueueEntry, taskDirty map[string]bool) {
	tq, ok := taskQueues[bc.Item.QueueFile]
	if !ok {
		return
	}
	for i := range tq.Queue.Tasks {
		task := &tq.Queue.Tasks[i]
		if task.ID != bc.Item.EntryID {
			continue
		}
		// Fencing: verify entry hasn't changed since Phase A
		if task.LeaseEpoch != bc.Item.Epoch || task.Status != model.StatusInProgress ||
			task.LeaseExpiresAt == nil || *task.LeaseExpiresAt != bc.Item.ExpiresAt {
			qh.log(LogLevelWarn, "busy_check_fence_stale kind=task id=%s epoch=%d/%d",
				task.ID, task.LeaseEpoch, bc.Item.Epoch)
			return
		}

		// Undecided: apply grace lease extension with shorter TTL to prevent
		// expired lease from triggering recovery mode and blocking new dispatches.
		// Still respect max_in_progress_min hard timeout to avoid infinite grace renewals.
		if bc.Undecided {
			maxMin := qh.config.Watcher.MaxInProgressMin
			if maxMin <= 0 {
				maxMin = 60
			}
			if t, err := time.Parse(time.RFC3339, bc.Item.UpdatedAt); err == nil {
				if qh.clock.Now().Sub(t) >= time.Duration(maxMin)*time.Minute {
					qh.log(LogLevelWarn, "lease_undecided_max_timeout type=task id=%s worker=%s max=%dm, releasing",
						task.ID, bc.Item.AgentID, maxMin)
					if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
						qh.log(LogLevelError, "expire_release_failed type=task id=%s error=%v", task.ID, err)
						return
					}
					qh.scanCounters.LeaseReleases++
					taskDirty[bc.Item.QueueFile] = true
					return
				}
			}
			graceTTL := qh.leaseManager.GraceLeaseTTL(qh.config.Watcher.ScanIntervalSec)
			qh.log(LogLevelInfo, "lease_grace_extend type=task id=%s worker=%s epoch=%d grace_ttl=%s",
				task.ID, bc.Item.AgentID, task.LeaseEpoch, graceTTL)
			if err := qh.leaseManager.ExtendTaskLeaseGrace(task, graceTTL); err != nil {
				qh.log(LogLevelError, "lease_grace_extend_failed type=task id=%s error=%v", task.ID, err)
			}
			qh.scanCounters.LeaseExtensions++
			taskDirty[bc.Item.QueueFile] = true
			return
		}

		if bc.Busy {
			maxMin := qh.config.Watcher.MaxInProgressMin
			if maxMin <= 0 {
				maxMin = 60
			}
			withinLimit := true
			if t, err := time.Parse(time.RFC3339, bc.Item.UpdatedAt); err == nil {
				if qh.clock.Now().Sub(t) >= time.Duration(maxMin)*time.Minute {
					withinLimit = false
				}
			}
			if withinLimit {
				qh.log(LogLevelInfo, "lease_extend_busy type=task id=%s worker=%s epoch=%d",
					task.ID, bc.Item.AgentID, task.LeaseEpoch)
				if err := qh.leaseManager.ExtendTaskLease(task); err != nil {
					qh.log(LogLevelError, "lease_extend_failed type=task id=%s error=%v", task.ID, err)
				}
				qh.scanCounters.LeaseExtensions++
				taskDirty[bc.Item.QueueFile] = true
				return
			}
			qh.log(LogLevelWarn, "lease_max_in_progress_timeout type=task id=%s worker=%s max=%dm",
				task.ID, bc.Item.AgentID, maxMin)
		}

		if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
			qh.log(LogLevelError, "expire_release_failed type=task id=%s error=%v", task.ID, err)
			return
		}
		qh.scanCounters.LeaseReleases++
		taskDirty[bc.Item.QueueFile] = true
		return
	}
}

func (qh *QueueHandler) applyCommandBusyCheckResult(bc busyCheckResult, cq *model.CommandQueue, dirty *bool) {
	for i := range cq.Commands {
		cmd := &cq.Commands[i]
		if cmd.ID != bc.Item.EntryID {
			continue
		}
		if cmd.LeaseEpoch != bc.Item.Epoch || cmd.Status != model.StatusInProgress ||
			cmd.LeaseExpiresAt == nil || *cmd.LeaseExpiresAt != bc.Item.ExpiresAt {
			qh.log(LogLevelWarn, "busy_check_fence_stale kind=command id=%s epoch=%d/%d",
				cmd.ID, cmd.LeaseEpoch, bc.Item.Epoch)
			return
		}

		// Undecided: apply grace lease extension with shorter TTL to prevent
		// expired lease from triggering recovery mode and blocking new dispatches.
		// Still respect max_in_progress_min hard timeout to avoid infinite grace renewals.
		if bc.Undecided {
			maxMin := qh.config.Watcher.MaxInProgressMin
			if maxMin <= 0 {
				maxMin = 60
			}
			if t, err := time.Parse(time.RFC3339, bc.Item.UpdatedAt); err == nil {
				if qh.clock.Now().Sub(t) >= time.Duration(maxMin)*time.Minute {
					qh.log(LogLevelWarn, "lease_undecided_max_timeout type=command id=%s owner=planner max=%dm, releasing",
						cmd.ID, maxMin)
					if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
						qh.log(LogLevelError, "expire_release_failed type=command id=%s error=%v", cmd.ID, err)
						return
					}
					qh.scanCounters.LeaseReleases++
					*dirty = true
					return
				}
			}
			graceTTL := qh.leaseManager.GraceLeaseTTL(qh.config.Watcher.ScanIntervalSec)
			qh.log(LogLevelInfo, "lease_grace_extend type=command id=%s owner=planner epoch=%d grace_ttl=%s",
				cmd.ID, cmd.LeaseEpoch, graceTTL)
			if err := qh.leaseManager.ExtendCommandLeaseGrace(cmd, graceTTL); err != nil {
				qh.log(LogLevelError, "lease_grace_extend_failed type=command id=%s error=%v", cmd.ID, err)
			}
			qh.scanCounters.LeaseExtensions++
			*dirty = true
			return
		}

		if bc.Busy {
			maxMin := qh.config.Watcher.MaxInProgressMin
			if maxMin <= 0 {
				maxMin = 60
			}
			withinLimit := true
			if t, err := time.Parse(time.RFC3339, bc.Item.UpdatedAt); err == nil {
				if qh.clock.Now().Sub(t) >= time.Duration(maxMin)*time.Minute {
					withinLimit = false
				}
			}
			if withinLimit {
				qh.log(LogLevelInfo, "lease_extend_busy type=command id=%s owner=planner epoch=%d",
					cmd.ID, cmd.LeaseEpoch)
				if err := qh.leaseManager.ExtendCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "lease_extend_failed type=command id=%s error=%v", cmd.ID, err)
				}
				qh.scanCounters.LeaseExtensions++
				*dirty = true
				return
			}
			qh.log(LogLevelWarn, "lease_max_in_progress_timeout type=command id=%s owner=planner max=%dm",
				cmd.ID, maxMin)
		}

		if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
			qh.log(LogLevelError, "expire_release_failed type=command id=%s error=%v", cmd.ID, err)
			return
		}
		qh.scanCounters.LeaseReleases++
		*dirty = true
		return
	}
}

func (qh *QueueHandler) applySignalResults(results []signalDeliveryResult, sq *model.PlannerSignalQueue, dirty *bool) {
	now := qh.clock.Now().UTC()
	var retained []model.PlannerSignal
	matched := make([]bool, len(results))

	for _, sig := range sq.Signals {
		var delivered bool
		var dlErr error
		for j, r := range results {
			if matched[j] {
				continue
			}
			if r.Item.CommandID == sig.CommandID &&
				r.Item.PhaseID == sig.PhaseID &&
				r.Item.Kind == sig.Kind {
				delivered = true
				matched[j] = true
				if !r.Success {
					dlErr = r.Error
				}
				break
			}
		}

		if !delivered {
			retained = append(retained, sig)
			continue
		}

		attemptTime := now.Format(time.RFC3339)
		sig.LastAttemptAt = &attemptTime
		sig.Attempts++
		sig.UpdatedAt = now.Format(time.RFC3339)

		if dlErr == nil {
			qh.log(LogLevelInfo, "signal_delivered kind=%s command=%s phase=%s attempts=%d",
				sig.Kind, sig.CommandID, sig.PhaseID, sig.Attempts)
			qh.scanCounters.SignalDeliveries++
			*dirty = true
			continue
		}

		errStr := dlErr.Error()
		sig.LastError = &errStr
		nextAttempt := qh.computeSignalBackoff(sig.Attempts)
		nextAttemptStr := now.Add(nextAttempt).Format(time.RFC3339)
		sig.NextAttemptAt = &nextAttemptStr
		*dirty = true

		qh.log(LogLevelWarn, "signal_delivery_failed kind=%s command=%s phase=%s attempts=%d next_retry=%s error=%v",
			sig.Kind, sig.CommandID, sig.PhaseID, sig.Attempts, nextAttemptStr, dlErr)
		qh.scanCounters.SignalRetries++

		retained = append(retained, sig)
	}

	sq.Signals = retained
}

func (qh *QueueHandler) recoverExpiredNotificationLeases(nq *model.NotificationQueue, dirty *bool) {
	expired := qh.leaseManager.ExpireNotifications(nq.Notifications)
	for _, idx := range expired {
		ntf := &nq.Notifications[idx]

		// Notifications don't have ExtendLease — always release and let retry.
		// No busy probe here: this runs under scanMu.Lock (Phase A) and must stay fast.
		if err := qh.leaseManager.ReleaseNotificationLease(ntf); err != nil {
			qh.log(LogLevelError, "expire_release_failed type=notification id=%s error=%v", ntf.ID, err)
			continue
		}
		*dirty = true
	}
}

// buildGlobalInFlightSet scans ALL task queues to find workers with in_progress tasks
// that have valid (non-expired) leases. Keyed by worker ID derived from queue file path.
func (qh *QueueHandler) buildGlobalInFlightSet(taskQueues map[string]*taskQueueEntry) map[string]bool {
	inFlight := make(map[string]bool)
	for queueFile, tq := range taskQueues {
		workerID := workerIDFromPath(queueFile)
		if workerID == "" {
			continue
		}
		for _, task := range tq.Queue.Tasks {
			if task.Status == model.StatusInProgress && !qh.leaseManager.IsLeaseExpired(task.LeaseExpiresAt) {
				inFlight[workerID] = true
				break
			}
		}
	}
	return inFlight
}

// collectWorktreePhaseMerges detects phases that just completed and collects
// merge work items for Phase B execution. Runs in Phase A under scanMu.Lock.
// Only performs fast in-memory checks — all git I/O is deferred to Phase B.
// Skips phases that have already been merged (tracked in worktree command state).
func (qh *QueueHandler) collectWorktreePhaseMerges(commandID string) []worktreeMergeItem {
	if qh.dependencyResolver.stateReader == nil || qh.worktreeManager == nil {
		return nil
	}

	phases, err := qh.dependencyResolver.stateReader.GetCommandPhases(commandID)
	if err != nil {
		return nil
	}

	// Load worktree state to check already-merged phases
	cmdState, err := qh.worktreeManager.GetCommandState(commandID)
	if err != nil {
		return nil
	}

	var items []worktreeMergeItem
	for _, phase := range phases {
		if string(phase.Status) != "completed" {
			continue
		}
		// Skip phases already merged
		if cmdState.MergedPhases != nil {
			if _, merged := cmdState.MergedPhases[phase.ID]; merged {
				continue
			}
		}
		// Only merge if this phase has tasks
		if len(phase.RequiredTaskIDs) == 0 {
			continue
		}

		// Use only workers that actually have worktrees
		var workerIDs []string
		for _, ws := range cmdState.Workers {
			workerIDs = append(workerIDs, ws.WorkerID)
		}
		if len(workerIDs) == 0 {
			continue
		}

		items = append(items, worktreeMergeItem{
			CommandID: commandID,
			PhaseID:   phase.ID,
			WorkerIDs: workerIDs,
		})
	}

	return items
}

// hasExpiredLeases checks whether any queue entry has an expired lease.
// Used to decide whether to prioritize recovery over dispatch (spec §5.8.1).
func (qh *QueueHandler) hasExpiredLeases(
	taskQueues map[string]*taskQueueEntry,
	cq *model.CommandQueue,
	nq *model.NotificationQueue,
) bool {
	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusInProgress && qh.leaseManager.IsLeaseExpired(cmd.LeaseExpiresAt) {
			return true
		}
	}
	for _, tq := range taskQueues {
		for _, task := range tq.Queue.Tasks {
			if task.Status == model.StatusInProgress && qh.leaseManager.IsLeaseExpired(task.LeaseExpiresAt) {
				return true
			}
		}
	}
	for _, ntf := range nq.Notifications {
		if ntf.Status == model.StatusInProgress && qh.leaseManager.IsLeaseExpired(ntf.LeaseExpiresAt) {
			return true
		}
	}
	return false
}

// collectWorktreePublishAndCleanup checks if a command is ready for worktree
// publish-to-base or cleanup. Returns publish and cleanup items for Phase B.
// Runs in Phase A under scanMu.Lock — only fast checks and YAML reads.
func (qh *QueueHandler) collectWorktreePublishAndCleanup(
	commandID string,
	taskQueues map[string]*taskQueueEntry,
) ([]worktreePublishItem, []worktreeCleanupItem) {
	// Load worktree state
	cmdState, err := qh.worktreeManager.GetCommandState(commandID)
	if err != nil {
		return nil, nil
	}

	// Check if all tasks for this command are terminal
	allTerminal, hasFailed := qh.checkCommandTasksTerminal(commandID, taskQueues)
	if !allTerminal {
		return nil, nil
	}

	// For phased commands, also verify all phases are terminal.
	// Errors fail closed (skip publish) to avoid premature publishing.
	phases, err := qh.dependencyResolver.stateReader.GetCommandPhases(commandID)
	if err != nil {
		if !errors.Is(err, ErrStateNotFound) {
			qh.log(LogLevelWarn, "worktree_publish_phase_check_failed command=%s error=%v", commandID, err)
		}
		return nil, nil
	}
	for _, phase := range phases {
		if !model.IsPhaseTerminal(phase.Status) {
			return nil, nil
		}
	}

	var publishes []worktreePublishItem
	var cleanups []worktreeCleanupItem

	if hasFailed {
		// Don't publish if any task failed — partial results stay on integration branch
		qh.log(LogLevelInfo, "worktree_publish_skip_failed command=%s", commandID)
		if qh.config.Worktree.CleanupOnFailure {
			cleanups = append(cleanups, worktreeCleanupItem{
				CommandID: commandID,
				Reason:    "failure",
			})
		}
		return publishes, cleanups
	}

	// No failures — check integration status to decide action
	switch cmdState.Integration.Status {
	case model.IntegrationStatusMerged:
		// Ready to publish
		publishes = append(publishes, worktreePublishItem{
			CommandID: commandID,
		})
		qh.log(LogLevelInfo, "worktree_publish_collected command=%s", commandID)
	case model.IntegrationStatusPublished:
		// Already published — collect cleanup if configured and not yet cleaned
		if qh.config.Worktree.CleanupOnSuccess {
			cleanups = append(cleanups, worktreeCleanupItem{
				CommandID: commandID,
				Reason:    "success",
			})
		}
	default:
		// Not ready (created, merging, conflict, publishing, failed)
		qh.log(LogLevelDebug, "worktree_publish_not_ready command=%s integration_status=%s",
			commandID, cmdState.Integration.Status)
	}

	return publishes, cleanups
}

// checkCommandTasksTerminal checks if all tasks for a command across all task
// queues are in terminal state. Returns (allTerminal, hasFailed).
// Runs in Phase A under scanMu.Lock — iterates already-loaded in-memory queues.
func (qh *QueueHandler) checkCommandTasksTerminal(
	commandID string,
	taskQueues map[string]*taskQueueEntry,
) (bool, bool) {
	taskCount := 0
	hasFailed := false

	for _, tq := range taskQueues {
		for _, task := range tq.Queue.Tasks {
			if task.CommandID != commandID {
				continue
			}
			taskCount++
			if !model.IsTerminal(task.Status) {
				return false, false
			}
			if task.Status == model.StatusFailed || task.Status == model.StatusDeadLetter {
				hasFailed = true
			}
		}
	}

	if taskCount == 0 {
		return false, false // No tasks found — command not ready
	}
	return true, hasFailed
}
