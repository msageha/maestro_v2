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

	scanStart := qh.clock.Now()
	qh.scanCounters = ScanCounters{}

	var work deferredWork

	// Load queue files
	commandQueue, commandPath := qh.loadCommandQueue()
	taskQueues := qh.loadAllTaskQueues()
	notificationQueue, notificationPath := qh.loadNotificationQueue()

	signalQueue, signalPath := qh.loadPlannerSignalQueue()
	signalsDirty := false
	signalIndex := buildSignalIndex(signalQueue.Signals)

	commandsDirty := false
	notificationsDirty := false
	taskDirty := make(map[string]bool)

	// Step 0: Dead letter processing — remove entries exceeding max retry attempts
	if qh.deadLetterProcessor != nil {
		dlResults := qh.deadLetterProcessor.ProcessCommandDeadLetters(&commandQueue, &commandsDirty)
		for queueFile, tq := range taskQueues {
			tqDirty := taskDirty[queueFile]
			dlResults = append(dlResults, qh.deadLetterProcessor.ProcessTaskDeadLetters(tq, &tqDirty)...)
			taskDirty[queueFile] = tqDirty
		}
		dlResults = append(dlResults, qh.deadLetterProcessor.ProcessNotificationDeadLetters(&notificationQueue, &notificationsDirty)...)
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
			notificationQueue.Notifications = append(notificationQueue.Notifications, pendingNtfs...)
			notificationsDirty = true
			if notificationPath == "" {
				notificationPath = filepath.Join(qh.maestroDir, "queue", "orchestrator.yaml")
			}
		}
	}

	// Step 0.4: Circuit breaker — check progress timeout and emit planner signals
	if qh.circuitBreaker != nil && qh.circuitBreaker.Enabled() {
		for i := range commandQueue.Commands {
			cmd := &commandQueue.Commands[i]
			if cmd.Status != model.StatusInProgress {
				continue
			}

			// Check progress timeout (consecutive failure trips happen in resultWritePhaseB)
			shouldTrip, reason := qh.circuitBreaker.CheckProgressTimeout(cmd.ID)
			if shouldTrip {
				timeoutMin := qh.circuitBreaker.ProgressTimeoutMinutes()
				if err := qh.circuitBreaker.StateReader().TripCircuitBreaker(cmd.ID, reason, timeoutMin); err != nil {
					qh.log(LogLevelError, "circuit_breaker_trip_timeout command=%s error=%v", cmd.ID, err)
				} else {
					qh.log(LogLevelWarn, "circuit_breaker_tripped_timeout command=%s reason=%s", cmd.ID, reason)
				}
			}

			// Emit planner signal for tripped commands (covers both failure-count and timeout trips)
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
				qh.upsertPlannerSignal(&signalQueue, &signalsDirty, model.PlannerSignal{
					Kind:      "circuit_breaker_tripped",
					CommandID: cmd.ID,
					Message:   msg,
					CreatedAt: now,
					UpdatedAt: now,
				}, signalIndex)
			}
		}
	}

	// Step 0.5: Cancel pending tasks for cancelled commands
	for i := range commandQueue.Commands {
		cmd := &commandQueue.Commands[i]
		if qh.cancelHandler.IsCommandCancelRequested(cmd) {
			for queueFile, tq := range taskQueues {
				results := qh.cancelHandler.CancelPendingTasks(tq.Queue.Tasks, cmd.ID)
				if len(results) > 0 {
					taskDirty[queueFile] = true
					qh.scanCounters.TasksCancelled += len(results)
					wID := workerIDFromPath(queueFile)
					if wID != "" {
						qh.cancelHandler.WriteSyntheticResults(results, wID)
					}
				}
			}
		}
	}

	// Step 0.6: Interrupt in_progress tasks for cancelled commands (defer tmux interrupt)
	for i := range commandQueue.Commands {
		cmd := &commandQueue.Commands[i]
		if qh.cancelHandler.IsCommandCancelRequested(cmd) {
			for queueFile, tq := range taskQueues {
				wID := workerIDFromPath(queueFile)
				results, interrupts := qh.cancelHandler.InterruptInProgressTasksDeferred(tq.Queue.Tasks, cmd.ID, wID)
				if len(results) > 0 {
					taskDirty[queueFile] = true
					qh.scanCounters.TasksCancelled += len(results)
					if wID != "" {
						qh.cancelHandler.WriteSyntheticResults(results, wID)
					}
				}
				work.interrupts = append(work.interrupts, interrupts...)
			}
		}
	}

	// Step 0.7: Phase transitions — detect and persist to state/commands/
	if qh.dependencyResolver.stateReader != nil {
		for i := range commandQueue.Commands {
			cmd := &commandQueue.Commands[i]
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
					qh.upsertPlannerSignal(&signalQueue, &signalsDirty, model.PlannerSignal{
						Kind:      "awaiting_fill",
						CommandID: cmd.ID,
						PhaseID:   tr.PhaseID,
						PhaseName: tr.PhaseName,
						Message:   msg,
						CreatedAt: now,
						UpdatedAt: now,
					}, signalIndex)
				case model.PhaseStatusTimedOut:
					msg := fmt.Sprintf("[maestro] kind:fill_timeout command_id:%s phase:%s\nfill deadline expired",
						cmd.ID, tr.PhaseName)
					qh.upsertPlannerSignal(&signalQueue, &signalsDirty, model.PlannerSignal{
						Kind:      "fill_timeout",
						CommandID: cmd.ID,
						PhaseID:   tr.PhaseID,
						PhaseName: tr.PhaseName,
						Message:   msg,
						CreatedAt: now,
						UpdatedAt: now,
					}, signalIndex)
				}
			}
		}
	}

	// Step 0.7.1: Worktree phase boundary merge — collect merge work items for Phase B
	if qh.worktreeManager != nil && qh.dependencyResolver.stateReader != nil {
		for i := range commandQueue.Commands {
			cmd := &commandQueue.Commands[i]
			if cmd.Status != model.StatusInProgress {
				continue
			}
			if !qh.worktreeManager.HasWorktrees(cmd.ID) {
				continue
			}
			mergeItems := qh.collectWorktreePhaseMerges(cmd.ID)
			work.worktreeMerges = append(work.worktreeMerges, mergeItems...)
		}
	}

	// Step 0.7.2: Worktree publish-to-base — detect command completion for publishing
	if qh.worktreeManager != nil && qh.dependencyResolver.stateReader != nil {
		for i := range commandQueue.Commands {
			cmd := &commandQueue.Commands[i]
			if cmd.Status != model.StatusInProgress {
				continue
			}
			if !qh.worktreeManager.HasWorktrees(cmd.ID) {
				continue
			}
			publishes, cleanups := qh.collectWorktreePublishAndCleanup(cmd.ID, taskQueues)
			work.worktreePublishes = append(work.worktreePublishes, publishes...)
			work.worktreeCleanups = append(work.worktreeCleanups, cleanups...)
		}
	}

	// Step 0.8: Process planner signals — evaluate backoff/staleness, defer delivery
	if len(signalQueue.Signals) > 0 {
		qh.processPlannerSignalsDeferred(&signalQueue, &signalsDirty, &work)
	}

	// Preemptive command lease renewal: renew before checking hasExpiredLeases
	// so that renewed commands don't trigger recovery mode unnecessarily.
	qh.preemptiveCommandRenewal(&commandQueue, &commandsDirty)

	// Steps 1 & 2: Dispatch or recovery (mutually exclusive per spec §5.8.1).
	expiredExists := qh.hasExpiredLeases(taskQueues, &commandQueue, &notificationQueue)

	if expiredExists {
		// Step 2: Collect busy check items for expired leases (defer tmux probes)
		for queueFile, tq := range taskQueues {
			agentID := workerIDFromPath(queueFile)
			d := taskDirty[queueFile]
			items := qh.collectExpiredTaskBusyChecks(tq, agentID, queueFile, &d)
			taskDirty[queueFile] = d
			work.busyChecks = append(work.busyChecks, items...)
		}
		work.busyChecks = append(work.busyChecks, qh.collectExpiredCommandBusyChecks(&commandQueue, &commandsDirty)...)
		// Notification expiry: always release (busy check doesn't affect outcome)
		qh.recoverExpiredNotificationLeases(&notificationQueue, &notificationsDirty)
		qh.log(LogLevelDebug, "expired_leases_detected busy_checks=%d skipping_dispatch", len(work.busyChecks))
	} else {
		// Step 1: Collect dispatch items (defer tmux dispatch)
		qh.collectPendingCommandDispatches(&commandQueue, &commandsDirty, &work)

		globalInFlight := qh.buildGlobalInFlightSet(taskQueues)
		for queueFile, tq := range taskQueues {
			workerID := workerIDFromPath(queueFile)
			if workerID == "" {
				qh.log(LogLevelWarn, "skip_dispatch cannot derive worker from %s", queueFile)
				continue
			}
			dirty := qh.collectPendingTaskDispatches(tq, workerID, globalInFlight, &work)
			if dirty {
				taskDirty[queueFile] = true
			}
		}
		qh.collectPendingNotificationDispatches(&notificationQueue, &notificationsDirty, &work)
	}

	// Step 1.5: Dependency failure check (always runs regardless of expired leases)
	for queueFile, tq := range taskQueues {
		dirty, interrupts := qh.checkPendingDependencyFailuresDeferred(tq, workerIDFromPath(queueFile))
		dirty2, interrupts2 := qh.checkInProgressDependencyFailuresDeferred(tq, workerIDFromPath(queueFile))
		if dirty || dirty2 {
			taskDirty[queueFile] = true
		}
		work.interrupts = append(work.interrupts, interrupts...)
		work.interrupts = append(work.interrupts, interrupts2...)
	}

	// Flush dirty queues to disk
	qh.flushQueues(commandQueue, commandPath, commandsDirty,
		taskQueues, taskDirty,
		notificationQueue, notificationPath, notificationsDirty,
		signalQueue, signalPath, signalsDirty)

	return phaseAResult{
		work:      work,
		scanStart: scanStart,
		counters:  qh.scanCounters,
	}
}

// periodicScanPhaseB executes all slow tmux I/O operations without holding any lock.
// Order: interrupts → busy checks → dispatches → signals (per Codex review).
// SRE-002: accepts context for cancellation support during slow I/O.
func (qh *QueueHandler) periodicScanPhaseB(ctx context.Context, pa phaseAResult) phaseBResult {
	var result phaseBResult

	// 1. Execute interrupts first (before dispatches to avoid killing new tasks)
	for _, item := range pa.work.interrupts {
		if ctx.Err() != nil {
			break
		}
		if err := qh.cancelHandler.interruptAgent(item.WorkerID, item.TaskID, item.CommandID, item.Epoch); err != nil {
			qh.log(LogLevelWarn, "phase_b_interrupt worker=%s task=%s error=%v", item.WorkerID, item.TaskID, err)
		}
	}

	// 2. Execute busy probes for expired leases
	for _, item := range pa.work.busyChecks {
		if ctx.Err() != nil {
			break
		}
		busy := qh.isAgentBusy(ctx, item.AgentID)
		result.busyChecks = append(result.busyChecks, busyCheckResult{
			Item: item,
			Busy: busy,
		})
	}

	// 3. Execute dispatches
	for _, item := range pa.work.dispatches {
		if ctx.Err() != nil {
			break
		}
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
	}

	// 4. Execute signal deliveries
	for _, item := range pa.work.signals {
		if ctx.Err() != nil {
			break
		}
		err := qh.deliverPlannerSignal(ctx, item.CommandID, item.Message)
		result.signals = append(result.signals, signalDeliveryResult{
			Item:    item,
			Success: err == nil,
			Error:   err,
		})
	}

	// 5. Execute agent clears (fire-and-forget)
	for _, agentID := range pa.work.clears {
		if ctx.Err() != nil {
			break
		}
		qh.clearAgent(ctx, agentID)
	}

	// 6. Execute worktree merges (slow git I/O, outside scanMu.Lock)
	for _, item := range pa.work.worktreeMerges {
		if ctx.Err() != nil {
			break
		}
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

			// Sync integration → worker worktrees if merge succeeded with no conflicts
			// (done here in Phase B to avoid holding scanMu during slow git I/O)
			if len(conflicts) == 0 && err == nil {
				if syncErr := qh.worktreeManager.SyncFromIntegration(item.CommandID, item.WorkerIDs); syncErr != nil {
					qh.log(LogLevelWarn, "worktree_sync_failed command=%s error=%v", item.CommandID, syncErr)
				}
			}
		}

		result.worktreeMerges = append(result.worktreeMerges, mr)
	}

	// 7. Execute worktree publishes (slow git I/O, outside scanMu.Lock)
	// Re-verify integration status before publishing to guard against merge
	// conflicts from step 6 in the same scan cycle (codex review finding #2).
	var additionalCleanups []worktreeCleanupItem
	for _, item := range pa.work.worktreePublishes {
		if ctx.Err() != nil {
			break
		}
		pr := worktreePublishResult{Item: item}
		if qh.worktreeManager != nil {
			// Re-check integration status: a merge conflict in step 6 may have
			// changed it from "merged" to "conflict" since Phase A collected this item.
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

		// On success, collect cleanup if configured
		if pr.Error == nil && qh.config.Worktree.CleanupOnSuccess {
			additionalCleanups = append(additionalCleanups, worktreeCleanupItem{
				CommandID: item.CommandID,
				Reason:    "success",
			})
		}
	}

	// 8. Execute worktree cleanups (Phase A collected + post-publish)
	allCleanups := append(pa.work.worktreeCleanups, additionalCleanups...)
	for _, item := range allCleanups {
		if ctx.Err() != nil {
			break
		}
		cr := worktreeCleanupResult{Item: item}
		if qh.worktreeManager != nil {
			cr.Error = qh.worktreeManager.CleanupCommand(item.CommandID)
		}
		result.worktreeCleanups = append(result.worktreeCleanups, cr)
	}

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
	// Load queues once for both sections. Under scanMu.Lock() no external
	// changes occur, so in-memory mutations from dispatch are visible to
	// busy-check apply without a disk round-trip.
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
			// Mark phase as merged to prevent re-merging on next scan
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
		// Reload signal queue from disk (may have been modified during Phase B)
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
				// Hard timeout reached: release immediately instead of waiting
				// for expiry to enforce max_in_progress_min strictly.
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

// collectExpiredCommandBusyChecks auto-extends expired command leases in Phase A.
// Unlike tasks, commands are never released on lease expiry because:
//   - Planner is a singleton; releasing causes duplicate dispatch
//   - Busy-check false negatives are common (Planner has long API call intervals)
//   - Reconciler R0 handles truly stuck planning via max_in_progress_min timeout
//
// Malformed entries (lease_expires_at == nil) are repaired by setting a new lease.
func (qh *QueueHandler) collectExpiredCommandBusyChecks(cq *model.CommandQueue, dirty *bool) []busyCheckItem {
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
	// No busy check items for commands — auto-extended in Phase A
	return nil
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
				// VerdictUndecided = pane looks idle but had stale busy pattern
				// Safe to retry immediately
				qh.log(LogLevelWarn, "dispatch_failed_undecided_release type=command id=%s", cmd.ID)
				if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "release_command_lease_failed id=%s error=%v", cmd.ID, err)
				} else {
					qh.scanCounters.LeaseReleases++
				}
				*dirty = true
				return
			}

			// Keep lease for other errors (prevents duplicate dispatch)
			// The dispatch may have actually succeeded (tmux delivery OK but executor
			// reported error). Releasing would cause pending revert → re-dispatch.
			// Lease auto-extend will keep it in_progress; Reconciler R0 handles stuck state.
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
