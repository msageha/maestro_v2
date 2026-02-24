package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// QueueHandler orchestrates fsnotify event routing and periodic scan execution.
type QueueHandler struct {
	maestroDir string
	config     model.Config
	logger     *log.Logger
	logLevel   LogLevel

	leaseManager        *LeaseManager
	dispatcher          *Dispatcher
	dependencyResolver  *DependencyResolver
	cancelHandler       *CancelHandler
	resultHandler       *ResultHandler
	reconciler          *Reconciler
	deadLetterProcessor *DeadLetterProcessor
	metricsHandler      *MetricsHandler
	lockMap             *lock.MutexMap

	// scanCounters accumulates counters during a single PeriodicScan cycle.
	scanCounters ScanCounters

	// Debounce state
	debounceMu    sync.Mutex
	debounceTimer *time.Timer

	// scanMu serializes PeriodicScan (exclusive) vs queue writes (shared RLock).
	// Spec §5.6: per-agent mutex — queue writes hold RLock + per-target lockMap key.
	scanMu sync.RWMutex

	// daemonPID for lease_owner format "daemon:{pid}" per spec §5.8.1.
	daemonPID int

	// busyChecker is called to probe agent busy state before lease expiry release.
	// Returns true if the agent is currently busy (lease should be extended).
	busyChecker func(agentID string) bool
}

// NewQueueHandler creates a new QueueHandler with all sub-modules.
func NewQueueHandler(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel) *QueueHandler {
	lm := NewLeaseManager(cfg.Watcher, logger, logLevel)
	dispatcher := NewDispatcher(maestroDir, cfg, lm, logger, logLevel)
	dr := NewDependencyResolver(nil, logger, logLevel) // StateReader wired in Phase 6
	ch := NewCancelHandler(maestroDir, cfg, logger, logLevel)
	rh := NewResultHandler(maestroDir, cfg, lockMap, logger, logLevel)
	rec := NewReconciler(maestroDir, cfg, lockMap, logger, logLevel, rh, rh.executorFactory)
	dlp := NewDeadLetterProcessor(maestroDir, cfg, lockMap, logger, logLevel)
	mh := NewMetricsHandler(maestroDir, cfg, logger, logLevel)

	return &QueueHandler{
		maestroDir:          maestroDir,
		config:              cfg,
		logger:              logger,
		logLevel:            logLevel,
		leaseManager:        lm,
		dispatcher:          dispatcher,
		dependencyResolver:  dr,
		cancelHandler:       ch,
		resultHandler:       rh,
		reconciler:          rec,
		deadLetterProcessor: dlp,
		metricsHandler:      mh,
		lockMap:             lockMap,
		daemonPID:           os.Getpid(),
	}
}

// leaseOwnerID returns the lease owner identifier in "daemon:{pid}" format per spec §5.8.1.
func (qh *QueueHandler) leaseOwnerID() string {
	return fmt.Sprintf("daemon:%d", qh.daemonPID)
}

// SetStateReader wires the state reader for dependency resolution (Phase 6).
func (qh *QueueHandler) SetStateReader(reader StateReader) {
	qh.dependencyResolver = NewDependencyResolver(reader, qh.logger, qh.logLevel)
	qh.cancelHandler.SetStateReader(reader)
}

// SetExecutorFactory overrides the executor factory for testing.
func (qh *QueueHandler) SetExecutorFactory(f ExecutorFactory) {
	qh.dispatcher.SetExecutorFactory(f)
	qh.cancelHandler.SetExecutorFactory(f)
	qh.resultHandler.SetExecutorFactory(f)
	qh.reconciler.SetExecutorFactory(f)
}

// SetCanComplete wires the CanComplete function for R4 reconciliation.
func (qh *QueueHandler) SetCanComplete(f CanCompleteFunc) {
	qh.reconciler.SetCanComplete(f)
}

// SetBusyChecker overrides the busy checker for testing.
func (qh *QueueHandler) SetBusyChecker(f func(agentID string) bool) {
	qh.busyChecker = f
}

// GetLeaseManager returns the internal lease manager (for testing).
func (qh *QueueHandler) GetLeaseManager() *LeaseManager {
	return qh.leaseManager
}

// GetDispatcher returns the internal dispatcher (for testing).
func (qh *QueueHandler) GetDispatcher() *Dispatcher {
	return qh.dispatcher
}

// HandleFileEvent routes an fsnotify event to the appropriate handler.
func (qh *QueueHandler) HandleFileEvent(filePath string) {
	base := filepath.Base(filePath)
	dir := filepath.Base(filepath.Dir(filePath))

	switch dir {
	case "queue":
		qh.debounceAndScan(base)
	case "results":
		qh.log(LogLevelDebug, "result_event file=%s", base)
		if qh.resultHandler != nil {
			qh.resultHandler.HandleResultFileEvent(filePath)
		}
	}
}

// debounceAndScan applies debounce logic before triggering a scan.
func (qh *QueueHandler) debounceAndScan(trigger string) {
	debounceSec := qh.config.Watcher.DebounceSec
	if debounceSec <= 0 {
		debounceSec = 0.5
	}

	qh.debounceMu.Lock()
	defer qh.debounceMu.Unlock()

	if qh.debounceTimer != nil {
		qh.debounceTimer.Stop()
	}

	qh.debounceTimer = time.AfterFunc(
		time.Duration(debounceSec*float64(time.Second)),
		func() {
			qh.log(LogLevelDebug, "debounced_scan trigger=%s", trigger)
			qh.PeriodicScan()
		},
	)
}

// PeriodicScan executes all scan steps in order.
// Steps: 0 (dead_letter) → 0.5 → 0.6 → 0.7 → [if expired: 2 | else: 1] → 1.5 → flush → 2.5 → 3 → 4 (metrics)
func (qh *QueueHandler) PeriodicScan() {
	qh.scanMu.Lock()
	defer qh.scanMu.Unlock()

	scanStart := time.Now()
	qh.scanCounters = ScanCounters{}

	qh.log(LogLevelDebug, "periodic_scan start")

	// Load queue files
	commandQueue, commandPath := qh.loadCommandQueue()
	taskQueues := qh.loadAllTaskQueues()
	notificationQueue, notificationPath := qh.loadNotificationQueue()

	signalQueue, signalPath := qh.loadPlannerSignalQueue()
	signalsDirty := false

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

		// Drain buffered orchestrator notifications generated by command dead
		// letter post-processing and append to the in-memory notification queue.
		// This avoids a race where a direct disk write would be overwritten
		// when the in-memory notification queue is flushed later.
		pendingNtfs := qh.deadLetterProcessor.DrainPendingNotifications()
		if len(pendingNtfs) > 0 {
			notificationQueue.Notifications = append(notificationQueue.Notifications, pendingNtfs...)
			notificationsDirty = true
			if notificationPath == "" {
				notificationPath = filepath.Join(qh.maestroDir, "queue", "orchestrator.yaml")
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

	// Step 0.6: Interrupt in_progress tasks for cancelled commands
	for i := range commandQueue.Commands {
		cmd := &commandQueue.Commands[i]
		if qh.cancelHandler.IsCommandCancelRequested(cmd) {
			for queueFile, tq := range taskQueues {
				results := qh.cancelHandler.InterruptInProgressTasks(tq.Queue.Tasks, cmd.ID)
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

				// Persist phase transition to state/commands/
				if err := qh.dependencyResolver.stateReader.ApplyPhaseTransition(cmd.ID, tr.PhaseID, tr.NewStatus); err != nil {
					qh.log(LogLevelError, "phase_transition_apply command=%s phase=%s error=%v",
						cmd.ID, tr.PhaseID, err)
					continue
				}

				now := time.Now().UTC().Format(time.RFC3339)
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
					})
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
					})
				}
			}
		}
	}

	// Step 0.8: Process planner signals — retry delivery with short probe config
	if len(signalQueue.Signals) > 0 {
		qh.processPlannerSignals(&signalQueue, &signalsDirty)
	}

	// Steps 1 & 2: Dispatch or recovery (mutually exclusive per spec §5.8.1).
	// If expired leases exist, run recovery first and skip dispatch this cycle.
	expiredExists := qh.hasExpiredLeases(taskQueues, &commandQueue, &notificationQueue)

	if expiredExists {
		// Step 2 first: Lease expiry recovery (expired leases detected)
		for queueFile, tq := range taskQueues {
			agentID := workerIDFromPath(queueFile)
			dirty := qh.recoverExpiredTaskLeases(tq, agentID)
			if dirty {
				taskDirty[queueFile] = true
			}
		}
		qh.recoverExpiredCommandLeases(&commandQueue, &commandsDirty)
		qh.recoverExpiredNotificationLeases(&notificationQueue, &notificationsDirty)
		qh.log(LogLevelInfo, "expired_leases_recovered skipping_dispatch")
	} else {
		// Step 1: Dispatch pending entries (no expired leases)
		qh.dispatchPendingCommands(&commandQueue, &commandsDirty)

		globalInFlight := qh.buildGlobalInFlightSet(taskQueues)
		for queueFile, tq := range taskQueues {
			workerID := workerIDFromPath(queueFile)
			if workerID == "" {
				qh.log(LogLevelWarn, "skip_dispatch cannot derive worker from %s", queueFile)
				continue
			}
			dirty := qh.dispatchPendingTasks(tq, workerID, globalInFlight)
			if dirty {
				taskDirty[queueFile] = true
			}
		}
		qh.dispatchPendingNotifications(&notificationQueue, &notificationsDirty)
	}

	// Step 1.5: Dependency failure check (always runs regardless of expired leases)
	for queueFile, tq := range taskQueues {
		dirty := qh.checkPendingDependencyFailures(tq, workerIDFromPath(queueFile))
		dirty2 := qh.checkInProgressDependencyFailures(tq, workerIDFromPath(queueFile))
		if dirty || dirty2 {
			taskDirty[queueFile] = true
		}
	}

	// Flush dirty queues to disk BEFORE Step 2.5/3 to prevent stale in-memory
	// copies from overwriting disk changes made by ResultHandler/Reconciler.
	if commandsDirty && commandPath != "" {
		if err := yamlutil.AtomicWrite(commandPath, commandQueue); err != nil {
			qh.log(LogLevelError, "write_commands error=%v", err)
		}
	}
	for queueFile, tq := range taskQueues {
		if taskDirty[queueFile] {
			if err := yamlutil.AtomicWrite(queueFile, tq.Queue); err != nil {
				qh.log(LogLevelError, "write_tasks file=%s error=%v", queueFile, err)
			}
		}
	}
	if notificationsDirty && notificationPath != "" {
		qh.lockMap.Lock("queue:orchestrator")
		if err := yamlutil.AtomicWrite(notificationPath, notificationQueue); err != nil {
			qh.log(LogLevelError, "write_notifications error=%v", err)
		}
		qh.lockMap.Unlock("queue:orchestrator")
	}
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

	// Step 2.5: Result notification retry (scan all results/ for unnotified entries)
	// Runs AFTER flushing dirty queues so ResultHandler reads fresh disk state.
	if qh.resultHandler != nil {
		n := qh.resultHandler.ScanAllResults()
		qh.scanCounters.NotificationRetries += n
		if n > 0 {
			qh.log(LogLevelInfo, "result_notify_scan notified=%d", n)
		}
	}

	// Step 3: Reconciliation
	// Runs AFTER flushing dirty queues so Reconciler reads fresh disk state.
	if qh.reconciler != nil {
		repairs := qh.reconciler.Reconcile()
		qh.scanCounters.ReconciliationRepairs += len(repairs)
		for _, repair := range repairs {
			qh.log(LogLevelInfo, "reconciliation pattern=%s command=%s task=%s detail=%s",
				repair.Pattern, repair.CommandID, repair.TaskID, repair.Detail)
		}
	}

	// Step 4: Metrics and dashboard
	if qh.metricsHandler != nil {
		scanDuration := time.Since(scanStart)
		if err := qh.metricsHandler.UpdateMetrics(commandQueue, taskQueues, notificationQueue, scanStart, scanDuration, &qh.scanCounters); err != nil {
			qh.log(LogLevelError, "update_metrics error=%v", err)
		}
		if err := qh.metricsHandler.UpdateDashboard(commandQueue, taskQueues, notificationQueue); err != nil {
			qh.log(LogLevelError, "update_dashboard error=%v", err)
		}
	}

	qh.log(LogLevelDebug, "periodic_scan complete")
}

// taskQueueEntry wraps a loaded task queue with its file path.
type taskQueueEntry struct {
	Queue model.TaskQueue
	Path  string
}

func (qh *QueueHandler) dispatchPendingCommands(cq *model.CommandQueue, dirty *bool) {
	// at-most-one-in-flight: skip if any command has a valid in_progress lease
	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusInProgress && cmd.LeaseExpiresAt != nil {
			if t, err := time.Parse(time.RFC3339, *cmd.LeaseExpiresAt); err == nil && t.After(time.Now()) {
				return
			}
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

		if err := qh.dispatcher.DispatchCommand(cmd); err != nil {
			qh.log(LogLevelWarn, "dispatch_failed type=command id=%s error=%v", cmd.ID, err)
			_ = qh.leaseManager.ReleaseCommandLease(cmd)
		} else {
			qh.scanCounters.CommandsDispatched++
		}
		*dirty = true
		break // at-most-one-in-flight: dispatch only one command per cycle
	}
}

func (qh *QueueHandler) dispatchPendingTasks(tq *taskQueueEntry, workerID string, globalInFlight map[string]bool) bool {
	dirty := false
	sorted := qh.dispatcher.SortPendingTasks(tq.Queue.Tasks)

	for _, idx := range sorted {
		task := &tq.Queue.Tasks[idx]

		// At-most-one-in-flight: check global set (across all queue files)
		if globalInFlight[workerID] {
			qh.log(LogLevelDebug, "worker_busy worker=%s task=%s (global in-flight)", workerID, task.ID)
			break
		}

		// Check blocked_by
		blocked, err := qh.dependencyResolver.IsTaskBlocked(task)
		if err != nil {
			qh.log(LogLevelWarn, "dependency_check_error task=%s error=%v", task.ID, err)
			continue
		}
		if blocked {
			continue
		}

		// System commit task dispatch guard: require all user phases/tasks terminal
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

		if err := qh.dispatcher.DispatchTask(task, workerID); err != nil {
			qh.log(LogLevelWarn, "dispatch_failed type=task id=%s error=%v", task.ID, err)
			_ = qh.leaseManager.ReleaseTaskLease(task)
		} else {
			// Mark worker as in-flight globally so subsequent files see it
			globalInFlight[workerID] = true
			qh.scanCounters.TasksDispatched++
		}
		dirty = true
		break // at-most-one-in-flight: one dispatch attempt per worker per cycle
	}
	return dirty
}

func (qh *QueueHandler) dispatchPendingNotifications(nq *model.NotificationQueue, dirty *bool) {
	// at-most-one-in-flight: skip if any notification has a valid in_progress lease
	for _, ntf := range nq.Notifications {
		if ntf.Status == model.StatusInProgress && ntf.LeaseExpiresAt != nil {
			if t, err := time.Parse(time.RFC3339, *ntf.LeaseExpiresAt); err == nil && t.After(time.Now()) {
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

		if err := qh.dispatcher.DispatchNotification(ntf); err != nil {
			qh.log(LogLevelWarn, "dispatch_failed type=notification id=%s error=%v", ntf.ID, err)
			_ = qh.leaseManager.ReleaseNotificationLease(ntf)
		} else {
			ntf.Status = model.StatusCompleted
			ntf.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		*dirty = true
		break // at-most-one-in-flight: dispatch only one notification per cycle
	}
}

func (qh *QueueHandler) checkPendingDependencyFailures(tq *taskQueueEntry, workerID string) bool {
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
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		dirty = true

		// Update state/commands/ with cancelled status + reason
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
	return dirty
}

func (qh *QueueHandler) checkInProgressDependencyFailures(tq *taskQueueEntry, workerID string) bool {
	dirty := false
	var cancelledResults []CancelledTaskResult

	for i := range tq.Queue.Tasks {
		task := &tq.Queue.Tasks[i]
		if task.Status != model.StatusInProgress {
			continue
		}

		// Skip tasks with expired leases (spec step 1.5: only valid leases)
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

		// Cancel the in_progress task — derive agent ID from queue file for interrupt
		if workerID != "" {
			_ = qh.cancelHandler.interruptAgent(workerID, task.ID, task.CommandID, task.LeaseEpoch)
		}
		task.Status = model.StatusCancelled
		task.LeaseOwner = nil
		task.LeaseExpiresAt = nil
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		dirty = true

		// Update state/commands/ with cancelled status + reason
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
	return dirty
}

func (qh *QueueHandler) recoverExpiredTaskLeases(tq *taskQueueEntry, agentID string) bool {
	dirty := false
	expired := qh.leaseManager.ExpireTasks(tq.Queue.Tasks)
	for _, idx := range expired {
		task := &tq.Queue.Tasks[idx]

		// Busy probe: if agent is still working AND within max_in_progress_min, extend
		if agentID != "" && qh.isAgentBusy(agentID) {
			maxMin := qh.config.Watcher.MaxInProgressMin
			if maxMin <= 0 {
				maxMin = 60
			}
			withinLimit := true
			if t, err := time.Parse(time.RFC3339, task.UpdatedAt); err == nil {
				if time.Since(t) >= time.Duration(maxMin)*time.Minute {
					withinLimit = false
				}
			}
			if withinLimit {
				qh.log(LogLevelInfo, "lease_extend_busy type=task id=%s worker=%s epoch=%d",
					task.ID, agentID, task.LeaseEpoch)
				if err := qh.leaseManager.ExtendTaskLease(task); err != nil {
					qh.log(LogLevelError, "lease_extend_failed type=task id=%s error=%v", task.ID, err)
				}
				dirty = true
				continue
			}
			// max_in_progress_min exceeded: reset agent via /clear then release lease
			qh.log(LogLevelWarn, "lease_max_in_progress_timeout type=task id=%s worker=%s max=%dm",
				task.ID, agentID, maxMin)
			qh.clearAgent(agentID)
		}

		if err := qh.leaseManager.ReleaseTaskLease(task); err != nil {
			qh.log(LogLevelError, "expire_release_failed type=task id=%s error=%v", task.ID, err)
			continue
		}
		dirty = true
	}
	return dirty
}

func (qh *QueueHandler) recoverExpiredCommandLeases(cq *model.CommandQueue, dirty *bool) {
	expired := qh.leaseManager.ExpireCommands(cq.Commands)
	for _, idx := range expired {
		cmd := &cq.Commands[idx]

		// Busy probe: if planner is still working AND within max_in_progress_min, extend
		if qh.isAgentBusy("planner") {
			maxMin := qh.config.Watcher.MaxInProgressMin
			if maxMin <= 0 {
				maxMin = 60
			}
			withinLimit := true
			if t, err := time.Parse(time.RFC3339, cmd.UpdatedAt); err == nil {
				if time.Since(t) >= time.Duration(maxMin)*time.Minute {
					withinLimit = false
				}
			}
			if withinLimit {
				qh.log(LogLevelInfo, "lease_extend_busy type=command id=%s owner=planner epoch=%d",
					cmd.ID, cmd.LeaseEpoch)
				if err := qh.leaseManager.ExtendCommandLease(cmd); err != nil {
					qh.log(LogLevelError, "lease_extend_failed type=command id=%s error=%v", cmd.ID, err)
				}
				*dirty = true
				continue
			}
			// max_in_progress_min exceeded: reset agent via /clear then release lease
			qh.log(LogLevelWarn, "lease_max_in_progress_timeout type=command id=%s owner=planner max=%dm",
				cmd.ID, maxMin)
			qh.clearAgent("planner")
		}

		if err := qh.leaseManager.ReleaseCommandLease(cmd); err != nil {
			qh.log(LogLevelError, "expire_release_failed type=command id=%s error=%v", cmd.ID, err)
			continue
		}
		*dirty = true
	}
}

func (qh *QueueHandler) recoverExpiredNotificationLeases(nq *model.NotificationQueue, dirty *bool) {
	expired := qh.leaseManager.ExpireNotifications(nq.Notifications)
	for _, idx := range expired {
		ntf := &nq.Notifications[idx]

		// Busy probe for orchestrator
		if qh.isAgentBusy("orchestrator") {
			qh.log(LogLevelInfo, "lease_extend_busy type=notification id=%s owner=orchestrator epoch=%d",
				ntf.ID, ntf.LeaseEpoch)
			// Notifications don't have ExtendLease — release and let retry
		}

		if err := qh.leaseManager.ReleaseNotificationLease(ntf); err != nil {
			qh.log(LogLevelError, "expire_release_failed type=notification id=%s error=%v", ntf.ID, err)
			continue
		}
		*dirty = true
	}
}

// loadPlannerSignalQueue loads .maestro/queue/planner_signals.yaml.
func (qh *QueueHandler) loadPlannerSignalQueue() (model.PlannerSignalQueue, string) {
	path := filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
	var sq model.PlannerSignalQueue

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			qh.log(LogLevelWarn, "load_planner_signals error=%v", err)
		}
		return sq, ""
	}

	if err := yamlv3.Unmarshal(data, &sq); err != nil {
		qh.log(LogLevelError, "parse_planner_signals error=%v", err)
		return sq, ""
	}
	return sq, path
}

// upsertPlannerSignal adds a signal or skips if one already exists for the same key.
func (qh *QueueHandler) upsertPlannerSignal(sq *model.PlannerSignalQueue, dirty *bool, sig model.PlannerSignal) {
	for _, existing := range sq.Signals {
		if existing.CommandID == sig.CommandID &&
			existing.PhaseID == sig.PhaseID &&
			existing.Kind == sig.Kind {
			qh.log(LogLevelDebug, "planner_signal_dedup kind=%s command=%s phase=%s",
				sig.Kind, sig.CommandID, sig.PhaseID)
			return
		}
	}
	if sq.SchemaVersion == 0 {
		sq.SchemaVersion = 1
		sq.FileType = "planner_signal_queue"
	}
	sq.Signals = append(sq.Signals, sig)
	*dirty = true
}

// processPlannerSignals attempts delivery for due signals, removes stale ones,
// and updates backoff state for failed attempts.
func (qh *QueueHandler) processPlannerSignals(sq *model.PlannerSignalQueue, dirty *bool) {
	now := time.Now().UTC()
	var retained []model.PlannerSignal

	for i := range sq.Signals {
		sig := &sq.Signals[i]

		// Skip signals whose next_attempt_at is in the future
		if sig.NextAttemptAt != nil {
			nextAt, err := time.Parse(time.RFC3339, *sig.NextAttemptAt)
			if err == nil && nextAt.After(now) {
				retained = append(retained, *sig)
				continue
			}
		}

		// Check if the phase is still in an actionable state
		if qh.dependencyResolver.stateReader != nil {
			phaseStatus, err := qh.dependencyResolver.GetPhaseStatus(sig.CommandID, sig.PhaseID)
			if err != nil {
				// If command/phase no longer exists, drop the stale signal
				errMsg := err.Error()
				if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "not exist") || os.IsNotExist(err) {
					qh.log(LogLevelInfo, "signal_orphaned_removed kind=%s command=%s phase=%s error=%v",
						sig.Kind, sig.CommandID, sig.PhaseID, err)
					*dirty = true
					continue
				}
				// Transient error: keep signal but don't attempt delivery
				qh.log(LogLevelWarn, "signal_phase_check command=%s phase=%s error=%v",
					sig.CommandID, sig.PhaseID, err)
				retained = append(retained, *sig)
				continue
			}

			if sig.Kind == "awaiting_fill" && phaseStatus != model.PhaseStatusAwaitingFill {
				qh.log(LogLevelInfo, "signal_stale_removed kind=%s command=%s phase=%s current_status=%s",
					sig.Kind, sig.CommandID, sig.PhaseID, phaseStatus)
				*dirty = true
				continue
			}

			if sig.Kind == "fill_timeout" && phaseStatus != model.PhaseStatusTimedOut {
				qh.log(LogLevelInfo, "signal_stale_removed kind=%s command=%s phase=%s current_status=%s",
					sig.Kind, sig.CommandID, sig.PhaseID, phaseStatus)
				*dirty = true
				continue
			}
		}

		// Attempt delivery with short config override
		err := qh.deliverPlannerSignal(sig.CommandID, sig.Message)
		attemptTime := now.Format(time.RFC3339)
		sig.LastAttemptAt = &attemptTime
		sig.Attempts++
		sig.UpdatedAt = now.Format(time.RFC3339)

		if err == nil {
			qh.log(LogLevelInfo, "signal_delivered kind=%s command=%s phase=%s attempts=%d",
				sig.Kind, sig.CommandID, sig.PhaseID, sig.Attempts)
			qh.scanCounters.SignalDeliveries++
			*dirty = true
			continue
		}

		// Failure: set backoff and retain
		errStr := err.Error()
		sig.LastError = &errStr
		nextAttempt := qh.computeSignalBackoff(sig.Attempts)
		nextAttemptStr := now.Add(nextAttempt).Format(time.RFC3339)
		sig.NextAttemptAt = &nextAttemptStr
		*dirty = true

		qh.log(LogLevelWarn, "signal_delivery_failed kind=%s command=%s phase=%s attempts=%d next_retry=%s error=%v",
			sig.Kind, sig.CommandID, sig.PhaseID, sig.Attempts, nextAttemptStr, err)
		qh.scanCounters.SignalRetries++

		retained = append(retained, *sig)
	}

	sq.Signals = retained
}

// deliverPlannerSignal attempts delivery to the planner with a short-probe config.
func (qh *QueueHandler) deliverPlannerSignal(commandID, message string) error {
	shortCfg := qh.config.Watcher
	shortCfg.BusyCheckMaxRetries = 1
	shortCfg.BusyCheckInterval = 1
	shortCfg.IdleStableSec = 1

	exec, err := qh.dispatcher.executorFactory(qh.maestroDir, shortCfg, qh.config.Logging.Level)
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}
	defer func() { _ = exec.Close() }()

	result := exec.Execute(agent.ExecRequest{
		AgentID:   "planner",
		Message:   message,
		Mode:      agent.ModeDeliver,
		CommandID: commandID,
	})
	if result.Error != nil {
		return result.Error
	}
	return nil
}

// computeSignalBackoff returns the backoff duration for the given attempt count.
func (qh *QueueHandler) computeSignalBackoff(attempts int) time.Duration {
	baseSec := 5
	maxSec := qh.config.Watcher.ScanIntervalSec
	if maxSec <= 0 {
		maxSec = 10
	}

	backoffSec := baseSec * (1 << (attempts - 1))
	if backoffSec > maxSec {
		backoffSec = maxSec
	}
	if backoffSec < baseSec {
		backoffSec = baseSec
	}
	return time.Duration(backoffSec) * time.Second
}

// isAgentBusy probes agent busy state via executor. Returns false if no checker is set.
func (qh *QueueHandler) isAgentBusy(agentID string) bool {
	if qh.busyChecker != nil {
		return qh.busyChecker(agentID)
	}

	// Default: use agent executor to probe busy state
	exec, err := qh.dispatcher.executorFactory(qh.maestroDir, qh.config.Watcher, qh.config.Logging.Level)
	if err != nil {
		qh.log(LogLevelWarn, "busy_probe_failed agent=%s error=%v", agentID, err)
		return false
	}
	defer func() { _ = exec.Close() }()

	result := exec.Execute(agent.ExecRequest{
		AgentID: agentID,
		Mode:    agent.ModeIsBusy,
	})

	return result.Success // Success=true means busy
}

// clearAgent sends /clear to the specified agent pane to reset a stuck session.
func (qh *QueueHandler) clearAgent(agentID string) {
	exec, err := qh.dispatcher.executorFactory(qh.maestroDir, qh.config.Watcher, qh.config.Logging.Level)
	if err != nil {
		qh.log(LogLevelWarn, "clear_agent create_executor error=%v", err)
		return
	}
	defer func() { _ = exec.Close() }()

	result := exec.Execute(agent.ExecRequest{
		AgentID: agentID,
		Mode:    agent.ModeClear,
	})
	if result.Error != nil {
		qh.log(LogLevelWarn, "clear_agent agent=%s error=%v", agentID, result.Error)
	} else {
		qh.log(LogLevelInfo, "clear_agent agent=%s success", agentID)
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

// workerIDFromPath extracts the worker ID from a queue file path.
// e.g., "/path/to/queue/worker1.yaml" → "worker1"
func workerIDFromPath(path string) string {
	base := filepath.Base(path) // "worker1.yaml"
	if !strings.HasPrefix(base, "worker") || !strings.HasSuffix(base, ".yaml") {
		return ""
	}
	return strings.TrimSuffix(base, ".yaml") // "worker1"
}

// --- File I/O helpers ---

func (qh *QueueHandler) loadCommandQueue() (model.CommandQueue, string) {
	path := filepath.Join(qh.maestroDir, "queue", "planner.yaml")
	var cq model.CommandQueue

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			qh.log(LogLevelWarn, "load_commands error=%v", err)
		}
		return cq, ""
	}

	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		qh.log(LogLevelError, "parse_commands error=%v", err)
		return cq, ""
	}
	return cq, path
}

func (qh *QueueHandler) loadAllTaskQueues() map[string]*taskQueueEntry {
	queueDir := filepath.Join(qh.maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		qh.log(LogLevelWarn, "read_queue_dir error=%v", err)
		return nil
	}

	result := make(map[string]*taskQueueEntry)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		path := filepath.Join(queueDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			qh.log(LogLevelWarn, "load_task_queue file=%s error=%v", name, err)
			continue
		}

		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			qh.log(LogLevelError, "parse_task_queue file=%s error=%v", name, err)
			continue
		}

		result[path] = &taskQueueEntry{Queue: tq, Path: path}
	}
	return result
}

func (qh *QueueHandler) loadNotificationQueue() (model.NotificationQueue, string) {
	path := filepath.Join(qh.maestroDir, "queue", "orchestrator.yaml")
	var nq model.NotificationQueue

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			qh.log(LogLevelWarn, "load_notifications error=%v", err)
		}
		return nq, ""
	}

	if err := yamlv3.Unmarshal(data, &nq); err != nil {
		qh.log(LogLevelError, "parse_notifications error=%v", err)
		return nq, ""
	}
	return nq, path
}

// LockFiles acquires a shared (read) lock for queue write handlers.
// Multiple queue writes can proceed in parallel; PeriodicScan holds exclusive lock.
func (qh *QueueHandler) LockFiles() {
	qh.scanMu.RLock()
}

// UnlockFiles releases the shared (read) lock for queue write handlers.
func (qh *QueueHandler) UnlockFiles() {
	qh.scanMu.RUnlock()
}

func (qh *QueueHandler) log(level LogLevel, format string, args ...any) {
	if level < qh.logLevel {
		return
	}
	levelStr := "INFO"
	switch level {
	case LogLevelDebug:
		levelStr = "DEBUG"
	case LogLevelWarn:
		levelStr = "WARN"
	case LogLevelError:
		levelStr = "ERROR"
	}
	msg := fmt.Sprintf(format, args...)
	qh.logger.Printf("%s %s queue_handler: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}
