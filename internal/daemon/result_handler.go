package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// ResultHandler monitors results/ and delivers notifications to agents.
// Worker results → Planner (side-channel via agent_executor).
// Planner results → Orchestrator (queue write).
type ResultHandler struct {
	maestroDir        string
	config            model.Config
	lockMap           *lock.MutexMap
	logger            *log.Logger
	logLevel          LogLevel
	executorFactory   ExecutorFactory
	continuousHandler *ContinuousHandler
}

// NewResultHandler creates a new ResultHandler.
func NewResultHandler(
	maestroDir string,
	cfg model.Config,
	lockMap *lock.MutexMap,
	logger *log.Logger,
	logLevel LogLevel,
) *ResultHandler {
	return &ResultHandler{
		maestroDir: maestroDir,
		config:     cfg,
		lockMap:    lockMap,
		logger:     logger,
		logLevel:   logLevel,
		executorFactory: func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
			return agent.NewExecutor(dir, wcfg, level)
		},
	}
}

// SetExecutorFactory overrides the executor factory for testing.
func (rh *ResultHandler) SetExecutorFactory(f ExecutorFactory) {
	rh.executorFactory = f
}

// SetContinuousHandler wires the continuous handler for iteration tracking.
func (rh *ResultHandler) SetContinuousHandler(ch *ContinuousHandler) {
	rh.continuousHandler = ch
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

	for {
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
			rh.log(LogLevelWarn, "notify_planner_failed worker=%s task=%s error=%v", workerID, taskID, notifyErr)
		} else {
			rh.markTaskNotifySuccess(entry)
			notified++
			rh.log(LogLevelInfo, "notify_planner_success worker=%s task=%s command=%s", workerID, taskID, commandID)
		}

		if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
			rh.log(LogLevelError, "write_result worker=%s error=%v", workerID, err)
		}
		rh.lockMap.Unlock(lockKey)
	}
}

// processCommandResultFile processes planner results using the notification lease pattern.
// Notifies Orchestrator via queue write + macOS notification.
func (rh *ResultHandler) processCommandResultFile() int {
	notified := 0
	resultPath := filepath.Join(rh.maestroDir, "results", "planner.yaml")
	attempted := make(map[string]bool)

	for {
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
			rh.log(LogLevelWarn, "notify_orchestrator_failed command=%s error=%v", commandID, notifyErr)
		} else {
			rh.markCommandNotifySuccess(entry)
			notified++
			rh.log(LogLevelInfo, "notify_orchestrator_success command=%s status=%s", commandID, resultStatus)

			// Continuous mode: advance iteration counter
			if rh.continuousHandler != nil {
				if err := rh.continuousHandler.CheckAndAdvance(commandID, resultStatus); err != nil {
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
	t, err := time.Parse(time.RFC3339, *expiresAt)
	if err != nil {
		return true
	}
	return time.Now().UTC().After(t)
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
	expiresAt := time.Now().UTC().Add(time.Duration(rh.notifyLeaseSec()) * time.Second).Format(time.RFC3339)
	r.NotifyLeaseOwner = &owner
	r.NotifyLeaseExpiresAt = &expiresAt
	r.NotifyAttempts++
}

func (rh *ResultHandler) acquireCommandNotifyLease(r *model.CommandResult) {
	owner := rh.leaseOwner()
	expiresAt := time.Now().UTC().Add(time.Duration(rh.notifyLeaseSec()) * time.Second).Format(time.RFC3339)
	r.NotifyLeaseOwner = &owner
	r.NotifyLeaseExpiresAt = &expiresAt
	r.NotifyAttempts++
}

func (rh *ResultHandler) markTaskNotifySuccess(r *model.TaskResult) {
	now := time.Now().UTC().Format(time.RFC3339)
	r.Notified = true
	r.NotifiedAt = &now
	r.NotifyLeaseOwner = nil
	r.NotifyLeaseExpiresAt = nil
}

func (rh *ResultHandler) markTaskNotifyFailure(r *model.TaskResult, errMsg string) {
	r.NotifyLastError = &errMsg
	r.NotifyLeaseOwner = nil
	r.NotifyLeaseExpiresAt = nil
}

func (rh *ResultHandler) markCommandNotifySuccess(r *model.CommandResult) {
	now := time.Now().UTC().Format(time.RFC3339)
	r.Notified = true
	r.NotifiedAt = &now
	r.NotifyLeaseOwner = nil
	r.NotifyLeaseExpiresAt = nil
}

func (rh *ResultHandler) markCommandNotifyFailure(r *model.CommandResult, errMsg string) {
	r.NotifyLastError = &errMsg
	r.NotifyLeaseOwner = nil
	r.NotifyLeaseExpiresAt = nil
}

// --- Notification delivery ---

// notifyPlannerOfWorkerResult sends a task_result notification to Planner via agent_executor.
func (rh *ResultHandler) notifyPlannerOfWorkerResult(commandID, taskID, workerID, taskStatus string) error {
	exec, err := rh.executorFactory(rh.maestroDir, rh.config.Watcher, rh.config.Logging.Level)
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}
	defer func() { _ = exec.Close() }()

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

	nq, _, err := loadNotificationQueueFile(queuePath)
	if err != nil {
		return err
	}

	// Idempotency: check if source_result_id already exists
	for _, ntf := range nq.Notifications {
		if ntf.SourceResultID == resultID {
			rh.log(LogLevelDebug, "orchestrator_notification_duplicate source_result_id=%s", resultID)
			return nil
		}
	}

	id, err := model.GenerateID(model.IDTypeNotification)
	if err != nil {
		return fmt.Errorf("generate notification ID: %w", err)
	}

	// Map result status to notification type
	notifType := "command_completed"
	switch status {
	case model.StatusFailed:
		notifType = "command_failed"
	case model.StatusCancelled:
		notifType = "command_cancelled"
	}

	now := time.Now().UTC().Format(time.RFC3339)
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

	return yamlutil.AtomicWrite(queuePath, nq)
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
	if level < rh.logLevel {
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
	rh.logger.Printf("%s %s result_handler: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}
