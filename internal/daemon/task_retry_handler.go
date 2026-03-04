package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

const (
	retryMetaPrefix = "retry_attempt="
	maxRetryMeta    = 5
)

// TaskRetryHandler handles task retry logic.
type TaskRetryHandler struct {
	maestroDir string
	config     model.Config
	lockMap    *lock.MutexMap
	clock      Clock
	dl         *DaemonLogger
	logger     *log.Logger
	logLevel   LogLevel
}

// NewTaskRetryHandler creates a new task retry handler.
func NewTaskRetryHandler(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel) *TaskRetryHandler {
	return NewTaskRetryHandlerWithDeps(maestroDir, cfg, lockMap, logger, logLevel, RealClock{})
}

// NewTaskRetryHandlerWithDeps creates a TaskRetryHandler with explicit dependencies.
func NewTaskRetryHandlerWithDeps(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel, clock Clock) *TaskRetryHandler {
	return &TaskRetryHandler{
		maestroDir: maestroDir,
		config:     cfg,
		lockMap:    lockMap,
		clock:      clock,
		dl:         NewDaemonLoggerFromLegacy("task_retry", logger, logLevel),
		logger:     logger,
		logLevel:   logLevel,
	}
}

// ShouldRetryTask determines if a failed task should be retried.
// retrySafe indicates whether the worker marked the result as safe to retry.
func (h *TaskRetryHandler) ShouldRetryTask(task *model.Task, exitCode int, retrySafe bool) (bool, string) {
	retryConfig := h.config.Retry.TaskExecution

	// Check if retry is enabled
	if !retryConfig.Enabled {
		return false, "retry disabled"
	}

	// CR-030: Respect worker's RetrySafe flag
	if !retrySafe {
		return false, "worker marked not retry safe"
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
	// QA-009: Deep copy slice fields to avoid shared backing arrays
	retryTask.BlockedBy = slices.Clone(originalTask.BlockedBy)
	retryTask.ToolsHint = slices.Clone(originalTask.ToolsHint)
	retryTask.ID = retryTaskID
	retryTask.Attempts = 0                                         // Reset dispatch attempts for new task
	retryTask.ExecutionRetries = originalTask.ExecutionRetries + 1 // Increment retry count
	retryTask.OriginalTaskID = originalTask.OriginalTaskID
	if retryTask.OriginalTaskID == "" {
		retryTask.OriginalTaskID = originalTask.ID // First retry, track original
	}
	retryTask.Status = model.StatusPending
	retryTask.LeaseOwner = nil
	retryTask.LeaseExpiresAt = nil
	retryTask.LeaseEpoch = 0
	retryTask.InProgressAt = nil // Reset so new dispatch sets fresh timestamp

	now := h.clock.Now().UTC()
	retryTask.CreatedAt = now.Format(time.RFC3339)
	retryTask.UpdatedAt = now.Format(time.RFC3339)

	// Set cooldown with NotBefore timestamp
	if retryConfig.CooldownSec > 0 {
		cooldownTime := now.Add(time.Duration(retryConfig.CooldownSec) * time.Second)
		notBefore := cooldownTime.Format(time.RFC3339)
		retryTask.NotBefore = &notBefore
		// CR-040: Cap priority increase to avoid unbounded growth.
		// Only add CooldownSec bump when ExecutionRetries is below the cap.
		// After maxPrioritySteps retries, priority stays at the same level.
		const maxPrioritySteps = 3
		if retryTask.ExecutionRetries <= maxPrioritySteps {
			retryTask.Priority = originalTask.Priority + retryConfig.CooldownSec
		} else {
			retryTask.Priority = originalTask.Priority
		}
	}

	// CR-039: Cap retry metadata in constraints to maxRetryMeta entries.
	// Build a fresh slice to avoid mutating the original task's backing array.
	retryMeta := fmt.Sprintf("retry_attempt=%d,original_task=%s,exit_code=%d",
		retryTask.ExecutionRetries, originalTask.ID, exitCode)
	retryTask.Constraints = withCappedRetryMeta(retryTask.Constraints, retryMeta)

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
	state.UpdatedAt = h.clock.Now().UTC().Format(time.RFC3339)

	if err := yaml.AtomicWrite(statePath, state); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}

	h.log(LogLevelInfo, "retry_task_registered task=%s command=%s",
		task.ID, commandID)
	return nil
}

// AddRetryTaskToQueue acquires the queue lock for workerID and adds the retry task.
// It is safe to call without holding any queue lock.
func (h *TaskRetryHandler) AddRetryTaskToQueue(task *model.Task, workerID string) error {
	lockKey := fmt.Sprintf("queue:%s", workerID)
	h.lockMap.Lock(lockKey)
	defer h.lockMap.Unlock(lockKey)

	return h.addRetryTaskToQueueLocked(task, workerID)
}

// addRetryTaskToQueueLocked adds a retry task to the worker's queue.
// Caller must hold lockMap lock for key "queue:<workerID>".
func (h *TaskRetryHandler) addRetryTaskToQueueLocked(task *model.Task, workerID string) error {
	queuePath := filepath.Join(h.maestroDir, "queue", workerID+".yaml")

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

// withCappedRetryMeta returns a new constraints slice with retry metadata entries
// capped at maxRetryMeta (keeping the newest). Non-retry constraints are preserved.
func withCappedRetryMeta(constraints []string, newMeta string) []string {
	plain := make([]string, 0, len(constraints)+1)
	meta := make([]string, 0, maxRetryMeta)

	for _, c := range constraints {
		if strings.HasPrefix(c, retryMetaPrefix) {
			meta = append(meta, c)
		} else {
			plain = append(plain, c)
		}
	}

	// Keep only the most recent (maxRetryMeta-1) old entries
	if len(meta) > maxRetryMeta-1 {
		meta = meta[len(meta)-(maxRetryMeta-1):]
	}

	out := make([]string, 0, len(plain)+len(meta)+1)
	out = append(out, plain...)
	out = append(out, meta...)
	out = append(out, newMeta)
	return out
}

func (h *TaskRetryHandler) log(level LogLevel, format string, args ...interface{}) {
	h.dl.Logf(level, format, args...)
}
