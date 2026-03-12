package daemon

import (
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

const (
	// maxNotifyAttempts is the maximum number of notification delivery attempts
	// before giving up. Prevents infinite retries when tmux is unavailable.
	maxNotifyAttempts = 5

	// notifyBackoffInitial is the initial backoff delay after a failed notification.
	// Exponential backoff: 1s → 2s → 4s → 8s → 16s (capped at notifyBackoffMax).
	notifyBackoffInitial = 1 * time.Second

	// notifyBackoffMax is the maximum backoff delay between retry attempts.
	notifyBackoffMax = 30 * time.Second

	// maxResultLoopIterations is the iteration count threshold at which a warning is
	// logged in processWorkerResultFile / processCommandResultFile. The loops are
	// unbounded (terminated by the attempted-set) so all pending results are drained.
	maxResultLoopIterations = 100
)

// ResultHandler monitors results/ and delivers notifications to agents.
// Worker results → Planner (side-channel via agent_executor).
// Planner results → Orchestrator (queue write).
type ResultHandler struct {
	maestroDir        string
	config            model.Config
	lockMap           *lock.MutexMap
	dl                *DaemonLogger
	logger            *log.Logger
	logLevel          LogLevel
	clock             Clock
	execProvider      *ExecutorProvider
	continuousHandler *ContinuousHandler
	eventBus          *events.Bus

	mu sync.RWMutex // protects continuousHandler, eventBus
}

// NewResultHandler creates a new ResultHandler.
func NewResultHandler(
	maestroDir string,
	cfg model.Config,
	lockMap *lock.MutexMap,
	logger *log.Logger,
	logLevel LogLevel,
) *ResultHandler {
	clock := RealClock{}
	factory := ExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return agent.NewExecutor(dir, wcfg, level)
	})
	return &ResultHandler{
		maestroDir: maestroDir,
		config:     cfg,
		lockMap:    lockMap,
		dl:         NewDaemonLoggerFromLegacy("result_handler", logger, logLevel),
		logger:     logger,
		logLevel:   logLevel,
		clock:      clock,
		execProvider: NewExecutorProvider(maestroDir, cfg.Watcher, cfg.Logging.Level, factory, clock, PolicyCacheError),
	}
}

// SetExecutorFactory overrides the executor factory for testing.
// Resets the cached executor so the new factory is used on next call.
func (rh *ResultHandler) SetExecutorFactory(f ExecutorFactory) {
	rh.execProvider.SetFactory(f)
}

// getExecutor returns the shared executor instance, creating it lazily on first call.
// The Executor is safe for concurrent use (log.Logger uses internal mutex,
// os.File in append mode is POSIX-safe, all other fields are immutable).
func (rh *ResultHandler) getExecutor() (AgentExecutor, error) {
	return rh.execProvider.GetExecutor()
}

// CloseExecutor releases the shared executor's resources.
// Safe to call multiple times; subsequent calls are no-ops.
func (rh *ResultHandler) CloseExecutor() {
	rh.execProvider.CloseExecutor()
}

// SetContinuousHandler wires the continuous handler for iteration tracking.
func (rh *ResultHandler) SetContinuousHandler(ch *ContinuousHandler) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.continuousHandler = ch
}

// SetEventBus sets the event bus for publishing events.
func (rh *ResultHandler) SetEventBus(bus *events.Bus) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.eventBus = bus
}

// getEventBus returns the event bus with proper synchronization.
func (rh *ResultHandler) getEventBus() *events.Bus {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	return rh.eventBus
}

// getContinuousHandler returns the continuous handler with proper synchronization.
func (rh *ResultHandler) getContinuousHandler() *ContinuousHandler {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	return rh.continuousHandler
}

// HandleResultFileEvent processes a single results/ file change from fsnotify.
// Called from QueueHandler.HandleFileEvent (NOT under fileMu).
func (rh *ResultHandler) HandleResultFileEvent(filePath string) {
	base := filepath.Base(filePath)
	if !strings.HasSuffix(base, ".yaml") {
		return
	}

	name := strings.TrimSuffix(base, ".yaml")
	if name == "planner" {
		n := rh.processCommandResultFile()
		if n > 0 {
			rh.log(LogLevelInfo, "result_event_notify file=%s notified=%d", base, n)
		}
	} else if strings.HasPrefix(name, "worker") {
		n := rh.processWorkerResultFile(name)
		if n > 0 {
			rh.log(LogLevelInfo, "result_event_notify file=%s notified=%d", base, n)
		}
	}
}

// ScanAllResults scans all results/ files for unnotified entries.
// Called from QueueHandler.PeriodicScan (step 2.5).
func (rh *ResultHandler) ScanAllResults() int {
	resultsDir := filepath.Join(rh.maestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			rh.log(LogLevelWarn, "scan_results read_dir error=%v", err)
		}
		return 0
	}

	total := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		baseName := strings.TrimSuffix(name, ".yaml")
		if baseName == "planner" {
			total += rh.processCommandResultFile()
		} else if strings.HasPrefix(baseName, "worker") {
			total += rh.processWorkerResultFile(baseName)
		}
	}
	return total
}

// processWorkerResultFile processes worker results using the notification lease pattern.
// Notifies Planner of each unnotified result. Each result is attempted at most once per call.
func (rh *ResultHandler) processWorkerResultFile(workerID string) int {
	notified := 0
	resultPath := filepath.Join(rh.maestroDir, "results", workerID+".yaml")
	attempted := make(map[string]bool)

	for iter := 0; ; iter++ {
		if iter == maxResultLoopIterations {
			rh.log(LogLevelWarn, "process_worker_result high_volume worker=%s processed=%d continuing", workerID, notified)
		}
		// Phase 1: Acquire lease under lock
		lockKey := "result:" + workerID
		rh.lockMap.Lock(lockKey)

		rf, err := rh.loadTaskResultFile(resultPath)
		if err != nil {
			rh.lockMap.Unlock(lockKey)
			rh.log(LogLevelWarn, "load_worker_results worker=%s error=%v", workerID, err)
			return notified
		}

		// Find first eligible result not already attempted in this call
		idx := rh.findUnnotifiedTaskResultExcluding(rf, attempted)
		if idx < 0 {
			rh.lockMap.Unlock(lockKey)
			return notified
		}

		result := &rf.Results[idx]
		resultID := result.ID
		taskID := result.TaskID
		commandID := result.CommandID
		taskStatus := string(result.Status)
		attempted[resultID] = true

		// Acquire notification lease
		rh.acquireTaskNotifyLease(result)
		if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
			rh.lockMap.Unlock(lockKey)
			rh.log(LogLevelError, "write_lease worker=%s result=%s error=%v", workerID, resultID, err)
			return notified
		}
		rh.lockMap.Unlock(lockKey)

		// Phase 2: Execute notification (outside lock)
		notifyErr := rh.notifyPlannerOfWorkerResult(commandID, taskID, workerID, taskStatus)

		// Phase 3: Update result under lock
		rh.lockMap.Lock(lockKey)
		rf, err = rh.loadTaskResultFile(resultPath)
		if err != nil {
			rh.lockMap.Unlock(lockKey)
			rh.log(LogLevelError, "reload_worker_results worker=%s error=%v", workerID, err)
			return notified
		}

		entry := rh.findTaskResultByID(rf, resultID)
		if entry == nil {
			rh.lockMap.Unlock(lockKey)
			rh.log(LogLevelWarn, "result_disappeared worker=%s result=%s", workerID, resultID)
			continue
		}

		if notifyErr != nil {
			rh.markTaskNotifyFailure(entry, notifyErr.Error())
			if entry.NotifyAttempts >= maxNotifyAttempts {
				rh.log(LogLevelError, "notify_exhausted worker=%s task=%s command=%s attempts=%d last_error=%v",
					workerID, taskID, commandID, entry.NotifyAttempts, notifyErr)
			} else {
				rh.log(LogLevelWarn, "notify_planner_failed worker=%s task=%s error=%v attempts=%d/%d next_retry_in=%s",
					workerID, taskID, notifyErr, entry.NotifyAttempts, maxNotifyAttempts, rh.notifyBackoff(entry.NotifyAttempts))
			}
		} else {
			rh.markTaskNotifySuccess(entry)
			notified++
			rh.log(LogLevelInfo, "notify_planner_success worker=%s task=%s command=%s", workerID, taskID, commandID)

			// Publish task_completed event (non-blocking, best-effort)
			if bus := rh.getEventBus(); bus != nil {
				bus.Publish(events.EventTaskCompleted, map[string]interface{}{
					"task_id":    taskID,
					"command_id": commandID,
					"worker_id":  workerID,
					"status":     taskStatus,
				})
			}
		}

		if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
			rh.log(LogLevelError, "write_result worker=%s error=%v", workerID, err)
		}
		rh.lockMap.Unlock(lockKey)
	}
}

// processCommandResultFile processes planner results using the notification lease pattern.
// Notifies Orchestrator via queue write.
func (rh *ResultHandler) processCommandResultFile() int {
	notified := 0
	resultPath := filepath.Join(rh.maestroDir, "results", "planner.yaml")
	attempted := make(map[string]bool)

	for iter := 0; ; iter++ {
		if iter == maxResultLoopIterations {
			rh.log(LogLevelWarn, "process_command_result high_volume processed=%d continuing", notified)
		}
		lockKey := "result:planner"
		rh.lockMap.Lock(lockKey)

		rf, err := rh.loadCommandResultFile(resultPath)
		if err != nil {
			rh.lockMap.Unlock(lockKey)
			rh.log(LogLevelWarn, "load_command_results error=%v", err)
			return notified
		}

		idx := rh.findUnnotifiedCommandResultExcluding(rf, attempted)
		if idx < 0 {
			rh.lockMap.Unlock(lockKey)
			return notified
		}

		result := &rf.Results[idx]
		resultID := result.ID
		commandID := result.CommandID
		resultStatus := result.Status
		attempted[resultID] = true

		rh.acquireCommandNotifyLease(result)
		if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
			rh.lockMap.Unlock(lockKey)
			rh.log(LogLevelError, "write_lease planner result=%s error=%v", resultID, err)
			return notified
		}
		rh.lockMap.Unlock(lockKey)

		// Execute notification (outside lock)
		notifyErr := rh.notifyOrchestratorOfCommandResult(resultID, commandID, resultStatus)

		// Update result under lock
		rh.lockMap.Lock(lockKey)
		rf, err = rh.loadCommandResultFile(resultPath)
		if err != nil {
			rh.lockMap.Unlock(lockKey)
			rh.log(LogLevelError, "reload_command_results error=%v", err)
			return notified
		}

		entry := rh.findCommandResultByID(rf, resultID)
		if entry == nil {
			rh.lockMap.Unlock(lockKey)
			rh.log(LogLevelWarn, "command_result_disappeared result=%s", resultID)
			continue
		}

		if notifyErr != nil {
			rh.markCommandNotifyFailure(entry, notifyErr.Error())
			if entry.NotifyAttempts >= maxNotifyAttempts {
				rh.log(LogLevelError, "notify_exhausted command=%s attempts=%d last_error=%v",
					commandID, entry.NotifyAttempts, notifyErr)
			} else {
				rh.log(LogLevelWarn, "notify_orchestrator_failed command=%s error=%v attempts=%d/%d next_retry_in=%s",
					commandID, notifyErr, entry.NotifyAttempts, maxNotifyAttempts, rh.notifyBackoff(entry.NotifyAttempts))
			}
		} else {
			rh.markCommandNotifySuccess(entry)
			notified++
			rh.log(LogLevelInfo, "notify_orchestrator_success command=%s status=%s", commandID, resultStatus)

			// Continuous mode: advance iteration counter
			if ch := rh.getContinuousHandler(); ch != nil {
				if err := ch.CheckAndAdvance(commandID, resultStatus); err != nil {
					rh.log(LogLevelWarn, "continuous_advance command=%s error=%v", commandID, err)
				}
			}
		}

		if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
			rh.log(LogLevelError, "write_result planner error=%v", err)
		}
		rh.lockMap.Unlock(lockKey)
	}
}

// --- Notification lease helpers ---

// notifyBackoff computes the exponential backoff delay for the given attempt count
// with ±25% uniform jitter to prevent thundering herd on recovery.
// Formula: min(notifyBackoffInitial * 2^(attempts-1), notifyBackoffMax) * (0.75 + rand*0.5).
func (rh *ResultHandler) notifyBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := notifyBackoffInitial * time.Duration(1<<(attempts-1))
	if delay > notifyBackoffMax {
		delay = notifyBackoffMax
	}
	jittered := time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5))
	return jittered
}

func (rh *ResultHandler) notifyLeaseSec() int {
	if rh.config.Watcher.NotifyLeaseSec > 0 {
		return rh.config.Watcher.NotifyLeaseSec
	}
	return 120
}

func (rh *ResultHandler) leaseOwner() string {
	return fmt.Sprintf("daemon:%d", os.Getpid())
}

func (rh *ResultHandler) isLeaseExpired(expiresAt *string) bool {
	if expiresAt == nil {
		return true
	}
	// Try RFC3339Nano first (used by backoff), then RFC3339 (used by leases).
	t, err := time.Parse(time.RFC3339Nano, *expiresAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339, *expiresAt)
		if err != nil {
			return true
		}
	}
	return rh.clock.Now().UTC().After(t)
}

func (rh *ResultHandler) findUnnotifiedTaskResultExcluding(rf *model.TaskResultFile, exclude map[string]bool) int {
	for i := range rf.Results {
		r := &rf.Results[i]
		if r.Notified {
			continue
		}
		if exclude[r.ID] {
			continue
		}
		if r.NotifyAttempts >= maxNotifyAttempts {
			continue
		}
		if r.NotifyLeaseOwner == nil || rh.isLeaseExpired(r.NotifyLeaseExpiresAt) {
			return i
		}
	}
	return -1
}

func (rh *ResultHandler) findUnnotifiedCommandResultExcluding(rf *model.CommandResultFile, exclude map[string]bool) int {
	for i := range rf.Results {
		r := &rf.Results[i]
		if r.Notified {
			continue
		}
		if exclude[r.ID] {
			continue
		}
		if r.NotifyAttempts >= maxNotifyAttempts {
			continue
		}
		if r.NotifyLeaseOwner == nil || rh.isLeaseExpired(r.NotifyLeaseExpiresAt) {
			return i
		}
	}
	return -1
}

func (rh *ResultHandler) findTaskResultByID(rf *model.TaskResultFile, id string) *model.TaskResult {
	for i := range rf.Results {
		if rf.Results[i].ID == id {
			return &rf.Results[i]
		}
	}
	return nil
}

func (rh *ResultHandler) findCommandResultByID(rf *model.CommandResultFile, id string) *model.CommandResult {
	for i := range rf.Results {
		if rf.Results[i].ID == id {
			return &rf.Results[i]
		}
	}
	return nil
}

func (rh *ResultHandler) acquireTaskNotifyLease(r *model.TaskResult) {
	owner := rh.leaseOwner()
	expiresAt := rh.clock.Now().UTC().Add(time.Duration(rh.notifyLeaseSec()) * time.Second).Format(time.RFC3339)
	r.NotifyLeaseOwner = &owner
	r.NotifyLeaseExpiresAt = &expiresAt
	r.NotifyAttempts++
}

func (rh *ResultHandler) acquireCommandNotifyLease(r *model.CommandResult) {
	owner := rh.leaseOwner()
	expiresAt := rh.clock.Now().UTC().Add(time.Duration(rh.notifyLeaseSec()) * time.Second).Format(time.RFC3339)
	r.NotifyLeaseOwner = &owner
	r.NotifyLeaseExpiresAt = &expiresAt
	r.NotifyAttempts++
}

func (rh *ResultHandler) markTaskNotifySuccess(r *model.TaskResult) {
	now := rh.clock.Now().UTC().Format(time.RFC3339)
	r.Notified = true
	r.NotifiedAt = &now
	r.NotifyLeaseOwner = nil
	r.NotifyLeaseExpiresAt = nil
}

func (rh *ResultHandler) markTaskNotifyFailure(r *model.TaskResult, errMsg string) {
	r.NotifyLastError = &errMsg
	// Set backoff lease to prevent immediate retry.
	// The entry will be skipped until the backoff period expires.
	// Use RFC3339Nano to preserve sub-second precision (backoff jitter can be <1s).
	backoff := rh.notifyBackoff(r.NotifyAttempts)
	owner := "backoff"
	expiresAt := rh.clock.Now().UTC().Add(backoff).Format(time.RFC3339Nano)
	r.NotifyLeaseOwner = &owner
	r.NotifyLeaseExpiresAt = &expiresAt
}

func (rh *ResultHandler) markCommandNotifySuccess(r *model.CommandResult) {
	now := rh.clock.Now().UTC().Format(time.RFC3339)
	r.Notified = true
	r.NotifiedAt = &now
	r.NotifyLeaseOwner = nil
	r.NotifyLeaseExpiresAt = nil
}

func (rh *ResultHandler) markCommandNotifyFailure(r *model.CommandResult, errMsg string) {
	r.NotifyLastError = &errMsg
	// Set backoff lease to prevent immediate retry.
	// Use RFC3339Nano to preserve sub-second precision (backoff jitter can be <1s).
	backoff := rh.notifyBackoff(r.NotifyAttempts)
	owner := "backoff"
	expiresAt := rh.clock.Now().UTC().Add(backoff).Format(time.RFC3339Nano)
	r.NotifyLeaseOwner = &owner
	r.NotifyLeaseExpiresAt = &expiresAt
}

// --- Notification delivery ---

// notifyPlannerOfWorkerResult sends a task_result notification to Planner via agent_executor.
func (rh *ResultHandler) notifyPlannerOfWorkerResult(commandID, taskID, workerID, taskStatus string) error {
	exec, err := rh.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	message := agent.BuildTaskResultNotification(commandID, taskID, workerID, taskStatus)

	result := exec.Execute(agent.ExecRequest{
		AgentID:   "planner",
		Message:   message,
		Mode:      agent.ModeDeliver,
		TaskID:    taskID,
		CommandID: commandID,
	})

	if result.Error != nil {
		return result.Error
	}
	return nil
}

// notifyOrchestratorOfCommandResult writes a notification to queue/orchestrator.yaml.
func (rh *ResultHandler) notifyOrchestratorOfCommandResult(resultID, commandID string, status model.Status) error {
	if err := rh.writeNotificationToOrchestratorQueue(resultID, commandID, status); err != nil {
		return fmt.Errorf("write orchestrator notification: %w", err)
	}
	return nil
}

// writeNotificationToOrchestratorQueue directly writes a notification to queue/orchestrator.yaml.
// Uses source_result_id idempotency to prevent duplicate notifications.
func (rh *ResultHandler) writeNotificationToOrchestratorQueue(resultID, commandID string, status model.Status) error {
	queuePath := filepath.Join(rh.maestroDir, "queue", "orchestrator.yaml")

	// Serialize access to orchestrator queue file to prevent lost-update races
	// between fsnotify event path (no fileMu) and PeriodicScan path.
	rh.lockMap.Lock("queue:orchestrator")
	defer rh.lockMap.Unlock("queue:orchestrator")

	return updateYAMLFile(queuePath, func(nq *model.NotificationQueue) error {
		if nq.SchemaVersion == 0 {
			nq.SchemaVersion = 1
			nq.FileType = "queue_notification"
		}

		// Idempotency: check if source_result_id already exists
		for _, ntf := range nq.Notifications {
			if ntf.SourceResultID == resultID {
				rh.log(LogLevelDebug, "orchestrator_notification_duplicate source_result_id=%s", resultID)
				return errNoUpdate
			}
		}

		id, err := model.GenerateID(model.IDTypeNotification)
		if err != nil {
			return fmt.Errorf("generate notification ID: %w", err)
		}

		// Map result status to notification type
		notifType := model.NotificationTypeCommandCompleted
		switch status {
		case model.StatusFailed:
			notifType = model.NotificationTypeCommandFailed
		case model.StatusCancelled:
			notifType = model.NotificationTypeCommandCancelled
		}

		now := rh.clock.Now().UTC().Format(time.RFC3339)
		content := fmt.Sprintf("command %s %s", commandID, status)

		nq.Notifications = append(nq.Notifications, model.Notification{
			ID:             id,
			CommandID:      commandID,
			Type:           notifType,
			SourceResultID: resultID,
			Content:        content,
			Priority:       100,
			Status:         model.StatusPending,
			CreatedAt:      now,
			UpdatedAt:      now,
		})

		return nil
	})
}

// WriteNotificationToOrchestratorQueue is the exported wrapper for writeNotificationToOrchestratorQueue.
// Used by the reconcile package via the ResultNotifier interface.
func (rh *ResultHandler) WriteNotificationToOrchestratorQueue(resultID, commandID string, status model.Status) error {
	return rh.writeNotificationToOrchestratorQueue(resultID, commandID, status)
}

// --- File I/O helpers ---

func (rh *ResultHandler) loadTaskResultFile(path string) (*model.TaskResultFile, error) {
	var rf model.TaskResultFile
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			rf.SchemaVersion = 1
			rf.FileType = "result_task"
			return &rf, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &rf, nil
}

func (rh *ResultHandler) loadCommandResultFile(path string) (*model.CommandResultFile, error) {
	var rf model.CommandResultFile
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			rf.SchemaVersion = 1
			rf.FileType = "result_command"
			return &rf, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &rf, nil
}

// --- Logging ---

func (rh *ResultHandler) log(level LogLevel, format string, args ...any) {
	rh.dl.Logf(level, format, args...)
}
