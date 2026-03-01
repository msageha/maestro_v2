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

	// scanMu serializes PeriodicScan phases (exclusive) vs queue writes (shared RLock).
	// Spec §5.6: per-agent mutex — queue writes hold RLock + per-target lockMap key.
	scanMu sync.RWMutex

	// scanRunMu serializes the full PeriodicScan cycle (Phase A → B → C).
	// Prevents overlapping scans since Phase B releases scanMu.
	scanRunMu sync.Mutex

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
	ch := NewCancelHandler(maestroDir, cfg, lockMap, logger, logLevel)
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

// Stop cancels any pending debounce timer.
func (qh *QueueHandler) Stop() {
	qh.debounceMu.Lock()
	defer qh.debounceMu.Unlock()
	if qh.debounceTimer != nil {
		qh.debounceTimer.Stop()
		qh.debounceTimer = nil
	}
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
			defer func() {
				if r := recover(); r != nil {
					qh.log(LogLevelError, "panic in debounceAndScan: %v", r)
				}
			}()
			qh.log(LogLevelDebug, "debounced_scan trigger=%s", trigger)
			qh.PeriodicScan()
		},
	)
}

// PeriodicScan executes all scan steps in a three-phase pattern to avoid
// holding scanMu during slow tmux I/O operations.
//
// Phase A (scanMu.Lock): Load queues, fast mutations, collect deferred work, flush.
// Phase B (no lock): Execute slow tmux I/O (interrupts, busy probes, dispatch, signals).
// Phase C (scanMu.Lock): Reload queues, apply Phase B results with fencing, flush, reconcile.
func (qh *QueueHandler) PeriodicScan() {
	// scanRunMu serializes the full A/B/C cycle so that concurrent scan triggers
	// wait for the current cycle to finish rather than overlapping with Phase B.
	qh.scanRunMu.Lock()
	defer qh.scanRunMu.Unlock()

	qh.log(LogLevelDebug, "periodic_scan start")

	pa := qh.periodicScanPhaseA()
	pb := qh.periodicScanPhaseB(pa)
	deferredNotifs := qh.periodicScanPhaseC(pa, pb)

	// Execute deferred reconciler notifications outside scanMu.Lock
	// to avoid blocking queue writes during slow tmux I/O.
	if qh.reconciler != nil && len(deferredNotifs) > 0 {
		qh.reconciler.ExecuteDeferredNotifications(deferredNotifs)
	}

	qh.log(LogLevelDebug, "periodic_scan complete")
}

// periodicScanPhaseA runs under scanMu.Lock. It loads queues, performs fast
// in-memory mutations (dead letter, cancel, phase transitions, dependency checks),
// collects deferred work items for slow I/O, and flushes queues to disk.
func (qh *QueueHandler) periodicScanPhaseA() phaseAResult {
	qh.scanMu.Lock()
	defer qh.scanMu.Unlock()

	scanStart := time.Now()
	qh.scanCounters = ScanCounters{}

	var work deferredWork

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
func (qh *QueueHandler) periodicScanPhaseB(pa phaseAResult) phaseBResult {
	var result phaseBResult

	// 1. Execute interrupts first (before dispatches to avoid killing new tasks)
	for _, item := range pa.work.interrupts {
		if err := qh.cancelHandler.interruptAgent(item.WorkerID, item.TaskID, item.CommandID, item.Epoch); err != nil {
			qh.log(LogLevelWarn, "phase_b_interrupt worker=%s task=%s error=%v", item.WorkerID, item.TaskID, err)
		}
	}

	// 2. Execute busy probes for expired leases
	for _, item := range pa.work.busyChecks {
		busy := qh.isAgentBusy(item.AgentID)
		result.busyChecks = append(result.busyChecks, busyCheckResult{
			Item: item,
			Busy: busy,
		})
	}

	// 3. Execute dispatches
	for _, item := range pa.work.dispatches {
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
		err := qh.deliverPlannerSignal(item.CommandID, item.Message)
		result.signals = append(result.signals, signalDeliveryResult{
			Item:    item,
			Success: err == nil,
			Error:   err,
		})
	}

	// 5. Execute agent clears (fire-and-forget)
	for _, agentID := range pa.work.clears {
		qh.clearAgent(agentID)
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

	// --- Apply dispatch results ---
	if len(pb.dispatches) > 0 {
		// Reload queues to get any changes made during Phase B
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

		// Flush dispatch result changes
		qh.flushQueues(commandQueue, commandPath, commandsDirty,
			taskQueues, taskDirty,
			notificationQueue, notificationPath, notificationsDirty,
			model.PlannerSignalQueue{}, "", false)
	}

	// --- Apply busy check results (lease recovery) ---
	if len(pb.busyChecks) > 0 {
		commandQueue, commandPath := qh.loadCommandQueue()
		taskQueues := qh.loadAllTaskQueues()
		commandsDirty := false
		taskDirty := make(map[string]bool)

		for _, bc := range pb.busyChecks {
			switch bc.Item.Kind {
			case "task":
				qh.applyTaskBusyCheckResult(bc, taskQueues, taskDirty)
			case "command":
				qh.applyCommandBusyCheckResult(bc, &commandQueue, &commandsDirty)
			}
		}

		qh.flushQueues(commandQueue, commandPath, commandsDirty,
			taskQueues, taskDirty,
			model.NotificationQueue{}, "", false,
			model.PlannerSignalQueue{}, "", false)
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
		scanDuration := time.Since(pa.scanStart)
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
			if err == nil && time.Now().Before(notBefore) {
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
			if time.Since(t) >= time.Duration(maxMin)*time.Minute {
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
			if time.Since(t) >= time.Duration(maxMin)*time.Minute {
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

// processPlannerSignalsDeferred evaluates signals but defers tmux delivery to Phase B.
func (qh *QueueHandler) processPlannerSignalsDeferred(sq *model.PlannerSignalQueue, dirty *bool, work *deferredWork) {
	now := time.Now().UTC()
	var retained []model.PlannerSignal

	for i := range sq.Signals {
		sig := &sq.Signals[i]

		if sig.NextAttemptAt != nil {
			nextAt, err := time.Parse(time.RFC3339, *sig.NextAttemptAt)
			if err == nil && nextAt.After(now) {
				retained = append(retained, *sig)
				continue
			}
		}

		if qh.dependencyResolver.stateReader != nil {
			phaseStatus, err := qh.dependencyResolver.GetPhaseStatus(sig.CommandID, sig.PhaseID)
			if err != nil {
				errMsg := err.Error()
				if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "not exist") || os.IsNotExist(err) {
					qh.log(LogLevelInfo, "signal_orphaned_removed kind=%s command=%s phase=%s error=%v",
						sig.Kind, sig.CommandID, sig.PhaseID, err)
					*dirty = true
					continue
				}
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

		// Defer delivery to Phase B
		work.signals = append(work.signals, signalDeliveryItem{
			CommandID: sig.CommandID,
			PhaseID:   sig.PhaseID,
			Kind:      sig.Kind,
			Message:   sig.Message,
		})
		retained = append(retained, *sig)
	}

	sq.Signals = retained
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
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
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
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
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
			// Keep lease on dispatch error to prevent duplicate dispatch.
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
				_ = qh.leaseManager.ReleaseTaskLease(task)
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
			_ = qh.leaseManager.ReleaseNotificationLease(ntf)
		} else {
			ntf.Status = model.StatusCompleted
			ntf.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
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
				if time.Since(t) >= time.Duration(maxMin)*time.Minute {
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
	now := time.Now().UTC()
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

// flushQueues writes dirty queues to disk atomically.
func (qh *QueueHandler) flushQueues(
	commandQueue model.CommandQueue, commandPath string, commandsDirty bool,
	taskQueues map[string]*taskQueueEntry, taskDirty map[string]bool,
	notificationQueue model.NotificationQueue, notificationPath string, notificationsDirty bool,
	signalQueue model.PlannerSignalQueue, signalPath string, signalsDirty bool,
) {
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
}

func safeStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// taskQueueEntry wraps a loaded task queue with its file path.
type taskQueueEntry struct {
	Queue model.TaskQueue
	Path  string
}

// --- Deferred work types for three-phase PeriodicScan ---

// dispatchItem captures a dispatch decision made in Phase A for execution in Phase B.
type dispatchItem struct {
	Kind         string              // "command", "task", "notification"
	Command      *model.Command      // snapshot for command dispatch
	Task         *model.Task         // snapshot for task dispatch
	Notification *model.Notification // snapshot for notification dispatch
	WorkerID     string              // worker ID (tasks only)
	Epoch        int                 // lease_epoch at time of decision
	ExpiresAt    string              // lease_expires_at snapshot for fencing
}

// busyCheckItem captures an expired-lease busy probe to execute in Phase B.
type busyCheckItem struct {
	Kind      string // "task", "command"
	EntryID   string
	AgentID   string
	Epoch     int
	QueueFile string // task queue file path
	UpdatedAt string // for max_in_progress_min check
	ExpiresAt string // fencing snapshot
}

// interruptItem captures a tmux interrupt to execute in Phase B.
type interruptItem struct {
	WorkerID  string
	TaskID    string
	CommandID string
	Epoch     int
}

// signalDeliveryItem captures a planner signal delivery for Phase B.
type signalDeliveryItem struct {
	CommandID string
	PhaseID   string
	Kind      string
	Message   string
}

// deferredWork collects all slow I/O operations for Phase B execution.
type deferredWork struct {
	dispatches []dispatchItem
	interrupts []interruptItem
	busyChecks []busyCheckItem
	signals    []signalDeliveryItem
	clears     []string // agent IDs to /clear
}

// dispatchResult captures the outcome of a Phase B dispatch.
type dispatchResult struct {
	Item    dispatchItem
	Success bool
	Error   error
}

// busyCheckResult captures the outcome of a Phase B busy probe.
type busyCheckResult struct {
	Item busyCheckItem
	Busy bool
}

// signalDeliveryResult captures the outcome of a Phase B signal delivery.
type signalDeliveryResult struct {
	Item    signalDeliveryItem
	Success bool
	Error   error
}

// phaseAResult holds all data Phase A passes to Phase B and Phase C.
type phaseAResult struct {
	work      deferredWork
	scanStart time.Time
	counters  ScanCounters
}

// phaseBResult holds all results from Phase B for Phase C to apply.
type phaseBResult struct {
	dispatches []dispatchResult
	busyChecks []busyCheckResult
	signals    []signalDeliveryResult
}

// NOTE: The following legacy dispatch/recovery methods have been replaced by
// collect* methods (Phase A) and apply* methods (Phase C) in the three-phase scan.

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

	if attempts < 1 {
		attempts = 1
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
