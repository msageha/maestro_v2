package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// TaskRetryHandler handles task retry logic.
type TaskRetryHandler struct {
	maestroDir string
	config     model.Config
	lockMap    *lock.MutexMap
	logger     *log.Logger
	logLevel   LogLevel
}

// NewTaskRetryHandler creates a new task retry handler.
func NewTaskRetryHandler(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel) *TaskRetryHandler {
	return &TaskRetryHandler{
		maestroDir: maestroDir,
		config:     cfg,
		lockMap:    lockMap,
		logger:     logger,
		logLevel:   logLevel,
	}
}

// ShouldRetryTask determines if a failed task should be retried.
func (h *TaskRetryHandler) ShouldRetryTask(task *model.Task, exitCode int) (bool, string) {
	retryConfig := h.config.Retry.TaskExecution

	// Check if retry is enabled
	if !retryConfig.Enabled {
		return false, "retry disabled"
	}

	// Check if max retries exceeded (use ExecutionRetries for actual retry count)
	if retryConfig.MaxRetries > 0 && task.ExecutionRetries >= retryConfig.MaxRetries {
		return false, fmt.Sprintf("max retries exceeded (%d/%d)", task.ExecutionRetries, retryConfig.MaxRetries)
	}

	// Check if exit code is retryable
	isRetryable := false
	for _, code := range retryConfig.RetryableExitCodes {
		if code == exitCode {
			isRetryable = true
			break
		}
	}

	if !isRetryable {
		return false, fmt.Sprintf("exit code %d not retryable", exitCode)
	}

	return true, ""
}

// CreateRetryTask creates a new task for retry with cooldown.
func (h *TaskRetryHandler) CreateRetryTask(originalTask *model.Task, workerID string, exitCode int) (*model.Task, error) {
	retryConfig := h.config.Retry.TaskExecution

	// Generate a proper task ID for the retry task
	retryTaskID, err := model.GenerateID(model.IDTypeTask)
	if err != nil {
		return nil, fmt.Errorf("generate retry task ID: %w", err)
	}

	// Create retry task with same content but new ID and increased retry count
	retryTask := *originalTask
	retryTask.ID = retryTaskID
	retryTask.Attempts = 0 // Reset dispatch attempts for new task
	retryTask.ExecutionRetries = originalTask.ExecutionRetries + 1 // Increment retry count
	retryTask.OriginalTaskID = originalTask.OriginalTaskID
	if retryTask.OriginalTaskID == "" {
		retryTask.OriginalTaskID = originalTask.ID // First retry, track original
	}
	retryTask.Status = model.StatusPending
	retryTask.LeaseOwner = nil
	retryTask.LeaseExpiresAt = nil
	retryTask.LeaseEpoch = 0

	now := time.Now().UTC()
	retryTask.CreatedAt = now.Format(time.RFC3339)
	retryTask.UpdatedAt = now.Format(time.RFC3339)

	// Set cooldown with NotBefore timestamp
	if retryConfig.CooldownSec > 0 {
		cooldownTime := now.Add(time.Duration(retryConfig.CooldownSec) * time.Second)
		notBefore := cooldownTime.Format(time.RFC3339)
		retryTask.NotBefore = &notBefore
		// Also adjust priority as a hint
		retryTask.Priority = originalTask.Priority + retryConfig.CooldownSec
	}

	// Add retry metadata to constraints
	retryMeta := fmt.Sprintf("retry_attempt=%d,original_task=%s,exit_code=%d",
		retryTask.ExecutionRetries, originalTask.ID, exitCode)
	retryTask.Constraints = append(retryTask.Constraints, retryMeta)

	return &retryTask, nil
}

// RegisterRetryTaskInState registers a retry task in the command state.
func (h *TaskRetryHandler) RegisterRetryTaskInState(task *model.Task, commandID string) error {
	stateLockKey := fmt.Sprintf("state:%s", commandID)
	h.lockMap.Lock(stateLockKey)
	defer h.lockMap.Unlock(stateLockKey)

	statePath := filepath.Join(h.maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		return fmt.Errorf("read state file: %w", err)
	}

	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		return fmt.Errorf("parse state file: %w", err)
	}

	// Register retry task in state
	if state.TaskStates == nil {
		state.TaskStates = make(map[string]model.Status)
	}
	state.TaskStates[task.ID] = model.StatusPending
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if err := yaml.AtomicWrite(statePath, state); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}

	h.log(LogLevelInfo, "retry_task_registered task=%s command=%s",
		task.ID, commandID)
	return nil
}

// AddRetryTaskToQueue adds a retry task to the worker's queue.
func (h *TaskRetryHandler) AddRetryTaskToQueue(task *model.Task, workerID string) error {
	queueFile := fmt.Sprintf("queue/%s.yaml", workerID)
	queuePath := filepath.Join(h.maestroDir, queueFile)

	// Fix: Use worker ID for lock key, not queueFile path
	lockKey := fmt.Sprintf("queue:%s", workerID)
	h.lockMap.Lock(lockKey)
	defer h.lockMap.Unlock(lockKey)

	// Read existing queue
	queueData, err := os.ReadFile(queuePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read queue: %w", err)
	}

	var queue model.TaskQueue
	if len(queueData) > 0 {
		if err := yamlv3.Unmarshal(queueData, &queue); err != nil {
			return fmt.Errorf("parse queue: %w", err)
		}
	} else {
		queue.SchemaVersion = 1
		queue.FileType = "queue_task"
	}

	// Add retry task
	queue.Tasks = append(queue.Tasks, *task)

	// Write queue back
	if err := yaml.AtomicWrite(queuePath, queue); err != nil {
		return fmt.Errorf("write queue: %w", err)
	}

	h.log(LogLevelInfo, "retry_task_added task=%s worker=%s attempt=%d",
		task.ID, workerID, task.Attempts)

	return nil
}

func (h *TaskRetryHandler) log(level LogLevel, format string, args ...interface{}) {
	if h.logLevel <= level {
		levelStr := "INFO"
		switch level {
		case LogLevelDebug:
			levelStr = "DEBUG"
		case LogLevelInfo:
			levelStr = "INFO"
		case LogLevelWarn:
			levelStr = "WARN"
		case LogLevelError:
			levelStr = "ERROR"
		}
		h.logger.Printf("[%s] "+format, append([]interface{}{levelStr}, args...)...)
	}
}