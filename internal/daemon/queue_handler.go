package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// QueueHandler orchestrates fsnotify event routing and periodic scan execution.
type QueueHandler struct {
	maestroDir string
	config     model.Config
	logger     *log.Logger
	logLevel   LogLevel

	leaseManager       *LeaseManager
	dispatcher         *Dispatcher
	dependencyResolver *DependencyResolver
	cancelHandler      *CancelHandler

	// Debounce state
	debounceMu    sync.Mutex
	debounceTimer *time.Timer

	// Per-file mutex for concurrent safety
	fileMu sync.Mutex

	// busyChecker is called to probe agent busy state before lease expiry release.
	// Returns true if the agent is currently busy (lease should be extended).
	busyChecker func(agentID string) bool
}

// NewQueueHandler creates a new QueueHandler with all sub-modules.
func NewQueueHandler(maestroDir string, cfg model.Config, logger *log.Logger, logLevel LogLevel) *QueueHandler {
	lm := NewLeaseManager(cfg.Watcher, logger, logLevel)
	dispatcher := NewDispatcher(maestroDir, cfg, lm, logger, logLevel)
	dr := NewDependencyResolver(nil, logger, logLevel) // StateReader wired in Phase 6
	ch := NewCancelHandler(maestroDir, cfg, logger, logLevel)

	return &QueueHandler{
		maestroDir:         maestroDir,
		config:             cfg,
		logger:             logger,
		logLevel:           logLevel,
		leaseManager:       lm,
		dispatcher:         dispatcher,
		dependencyResolver: dr,
		cancelHandler:      ch,
	}
}

// SetStateReader wires the state reader for dependency resolution (Phase 6).
func (qh *QueueHandler) SetStateReader(reader StateReader) {
	qh.dependencyResolver = NewDependencyResolver(reader, qh.logger, qh.logLevel)
}

// SetExecutorFactory overrides the executor factory for testing.
func (qh *QueueHandler) SetExecutorFactory(f ExecutorFactory) {
	qh.dispatcher.SetExecutorFactory(f)
	qh.cancelHandler.SetExecutorFactory(f)
}

// SetBusyChecker overrides the busy checker for testing.
func (qh *QueueHandler) SetBusyChecker(f func(agentID string) bool) {
	qh.busyChecker = f
}

// LeaseManager returns the internal lease manager (for testing).
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

	if dir == "queue" {
		qh.debounceAndScan(base)
	} else if dir == "results" {
		qh.log(LogLevelDebug, "result_event file=%s", base)
		// Result processing is Phase 7 scope
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
// Steps: 0 (dead_letter, Phase 9) → 0.5 → 0.6 → 0.7 → 1 → 1.5 → 2
func (qh *QueueHandler) PeriodicScan() {
	qh.fileMu.Lock()
	defer qh.fileMu.Unlock()

	qh.log(LogLevelDebug, "periodic_scan start")

	// Load queue files
	commandQueue, commandPath := qh.loadCommandQueue()
	taskQueues := qh.loadAllTaskQueues()
	notificationQueue, notificationPath := qh.loadNotificationQueue()

	commandsDirty := false
	notificationsDirty := false
	taskDirty := make(map[string]bool)

	// Step 0: dead_letter (Phase 9 — skip for now)

	// Step 0.5: Cancel pending tasks for cancelled commands
	for i := range commandQueue.Commands {
		cmd := &commandQueue.Commands[i]
		if qh.cancelHandler.IsCommandCancelRequested(cmd) {
			for queueFile, tq := range taskQueues {
				results := qh.cancelHandler.CancelPendingTasks(tq.Queue.Tasks, cmd.ID)
				if len(results) > 0 {
					taskDirty[queueFile] = true
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
				}
			}
		}
	}

	// Step 0.7: Phase transitions
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
				// Phase state mutations are applied by Phase 6's StateWriter.
				// Here we only log transitions and send awaiting_fill notifications.
				if tr.NewStatus == model.PhaseStatusAwaitingFill {
					phase := PhaseInfo{ID: tr.PhaseID, Name: tr.PhaseName}
					msg := qh.dependencyResolver.BuildAwaitingFillNotification(cmd.ID, phase)
					qh.log(LogLevelInfo, "awaiting_fill_notify command=%s phase=%s msg=%s",
						cmd.ID, tr.PhaseName, msg)
				}
			}
		}
	}

	// Step 1: Dispatch pending entries
	qh.dispatchPendingCommands(&commandQueue, &commandsDirty)

	// Build global in-flight set across ALL task queue files for at-most-one-in-flight.
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

	// Step 1.5: In-progress task dependency failure check
	for queueFile, tq := range taskQueues {
		dirty := qh.checkInProgressDependencyFailures(tq)
		if dirty {
			taskDirty[queueFile] = true
		}
	}

	// Step 2: Lease expiry recovery
	for queueFile, tq := range taskQueues {
		dirty := qh.recoverExpiredTaskLeases(tq)
		if dirty {
			taskDirty[queueFile] = true
		}
	}
	qh.recoverExpiredCommandLeases(&commandQueue, &commandsDirty)
	qh.recoverExpiredNotificationLeases(&notificationQueue, &notificationsDirty)

	// Write dirty queues
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
		if err := yamlutil.AtomicWrite(notificationPath, notificationQueue); err != nil {
			qh.log(LogLevelError, "write_notifications error=%v", err)
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
	sorted := qh.dispatcher.SortPendingCommands(cq.Commands)
	for _, idx := range sorted {
		cmd := &cq.Commands[idx]
		if err := qh.leaseManager.AcquireCommandLease(cmd, "planner"); err != nil {
			qh.log(LogLevelWarn, "lease_acquire_failed type=command id=%s error=%v", cmd.ID, err)
			continue
		}
		cmd.Attempts++

		if err := qh.dispatcher.DispatchCommand(cmd); err != nil {
			qh.log(LogLevelWarn, "dispatch_rollback type=command id=%s error=%v", cmd.ID, err)
			cmd.Attempts--
			qh.leaseManager.ReleaseCommandLease(cmd)
		}
		*dirty = true
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

		if err := qh.leaseManager.AcquireTaskLease(task, workerID); err != nil {
			qh.log(LogLevelWarn, "lease_acquire_failed type=task id=%s error=%v", task.ID, err)
			continue
		}
		task.Attempts++

		if err := qh.dispatcher.DispatchTask(task, workerID); err != nil {
			qh.log(LogLevelWarn, "dispatch_rollback type=task id=%s error=%v", task.ID, err)
			task.Attempts--
			qh.leaseManager.ReleaseTaskLease(task)
		} else {
			// Mark worker as in-flight globally so subsequent files see it
			globalInFlight[workerID] = true
		}
		dirty = true
	}
	return dirty
}

func (qh *QueueHandler) dispatchPendingNotifications(nq *model.NotificationQueue, dirty *bool) {
	sorted := qh.dispatcher.SortPendingNotifications(nq.Notifications)
	for _, idx := range sorted {
		ntf := &nq.Notifications[idx]
		if err := qh.leaseManager.AcquireNotificationLease(ntf, "orchestrator"); err != nil {
			qh.log(LogLevelWarn, "lease_acquire_failed type=notification id=%s error=%v", ntf.ID, err)
			continue
		}
		ntf.Attempts++

		if err := qh.dispatcher.DispatchNotification(ntf); err != nil {
			qh.log(LogLevelWarn, "dispatch_rollback type=notification id=%s error=%v", ntf.ID, err)
			ntf.Attempts--
			qh.leaseManager.ReleaseNotificationLease(ntf)
		}
		*dirty = true
	}
}

func (qh *QueueHandler) checkInProgressDependencyFailures(tq *taskQueueEntry) bool {
	dirty := false
	for i := range tq.Queue.Tasks {
		task := &tq.Queue.Tasks[i]
		if task.Status != model.StatusInProgress {
			continue
		}

		failedDep, failedStatus, err := qh.dependencyResolver.CheckDependencyFailure(task)
		if err != nil || failedDep == "" {
			continue
		}

		qh.log(LogLevelWarn, "dependency_failure task=%s dep=%s dep_status=%s",
			task.ID, failedDep, failedStatus)

		// Cancel the in_progress task
		if task.LeaseOwner != nil {
			qh.cancelHandler.interruptAgent(*task.LeaseOwner, task.ID, task.CommandID, task.LeaseEpoch)
		}
		task.Status = model.StatusCancelled
		task.LeaseOwner = nil
		task.LeaseExpiresAt = nil
		task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		dirty = true
	}
	return dirty
}

func (qh *QueueHandler) recoverExpiredTaskLeases(tq *taskQueueEntry) bool {
	dirty := false
	expired := qh.leaseManager.ExpireTasks(tq.Queue.Tasks)
	for _, idx := range expired {
		task := &tq.Queue.Tasks[idx]

		// Busy probe: if agent is still working, extend instead of releasing
		if task.LeaseOwner != nil && qh.isAgentBusy(*task.LeaseOwner) {
			qh.log(LogLevelInfo, "lease_extend_busy type=task id=%s worker=%s epoch=%d",
				task.ID, *task.LeaseOwner, task.LeaseEpoch)
			if err := qh.leaseManager.ExtendTaskLease(task); err != nil {
				qh.log(LogLevelError, "lease_extend_failed type=task id=%s error=%v", task.ID, err)
			}
			dirty = true
			continue
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

		// Busy probe: if planner is still working, extend instead of releasing
		if cmd.LeaseOwner != nil && qh.isAgentBusy(*cmd.LeaseOwner) {
			qh.log(LogLevelInfo, "lease_extend_busy type=command id=%s owner=%s epoch=%d",
				cmd.ID, *cmd.LeaseOwner, cmd.LeaseEpoch)
			if err := qh.leaseManager.ExtendCommandLease(cmd); err != nil {
				qh.log(LogLevelError, "lease_extend_failed type=command id=%s error=%v", cmd.ID, err)
			}
			*dirty = true
			continue
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
		if ntf.LeaseOwner != nil && qh.isAgentBusy(*ntf.LeaseOwner) {
			qh.log(LogLevelInfo, "lease_extend_busy type=notification id=%s owner=%s epoch=%d",
				ntf.ID, *ntf.LeaseOwner, ntf.LeaseEpoch)
			// Notifications don't have ExtendLease — release and let retry
		}

		if err := qh.leaseManager.ReleaseNotificationLease(ntf); err != nil {
			qh.log(LogLevelError, "expire_release_failed type=notification id=%s error=%v", ntf.ID, err)
			continue
		}
		*dirty = true
	}
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
	defer exec.Close()

	result := exec.Execute(agent.ExecRequest{
		AgentID: agentID,
		Mode:    agent.ModeIsBusy,
	})

	return result.Success // Success=true means busy
}

// buildGlobalInFlightSet scans ALL task queues to find workers with in_progress tasks.
func (qh *QueueHandler) buildGlobalInFlightSet(taskQueues map[string]*taskQueueEntry) map[string]bool {
	inFlight := make(map[string]bool)
	for _, tq := range taskQueues {
		for _, task := range tq.Queue.Tasks {
			if task.Status == model.StatusInProgress && task.LeaseOwner != nil {
				inFlight[*task.LeaseOwner] = true
			}
		}
	}
	return inFlight
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

// LockFiles acquires the shared file mutex for write handlers.
func (qh *QueueHandler) LockFiles() {
	qh.fileMu.Lock()
}

// UnlockFiles releases the shared file mutex for write handlers.
func (qh *QueueHandler) UnlockFiles() {
	qh.fileMu.Unlock()
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
