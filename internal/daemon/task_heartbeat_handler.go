package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// TaskHeartbeatHandler handles task heartbeat requests.
type TaskHeartbeatHandler struct {
	maestroDir   string
	config       model.Config
	leaseManager *LeaseManager
	logger       *log.Logger
	logLevel     LogLevel
	scanMu       *sync.RWMutex
	lockMap      *lock.MutexMap
}

// NewTaskHeartbeatHandler creates a new task heartbeat handler.
func NewTaskHeartbeatHandler(maestroDir string, cfg model.Config, lm *LeaseManager,
	logger *log.Logger, logLevel LogLevel, scanMu *sync.RWMutex, lockMap *lock.MutexMap) *TaskHeartbeatHandler {
	return &TaskHeartbeatHandler{
		maestroDir:   maestroDir,
		config:       cfg,
		leaseManager: lm,
		logger:       logger,
		logLevel:     logLevel,
		scanMu:       scanMu,
		lockMap:      lockMap,
	}
}

// TaskHeartbeatParams represents the parameters for a task heartbeat request.
type TaskHeartbeatParams struct {
	TaskID   string `json:"task_id"`
	WorkerID string `json:"worker_id"`
	Epoch    int    `json:"epoch"`
}

// TaskHeartbeatResult represents the result of a task heartbeat.
type TaskHeartbeatResult struct {
	Extended bool   `json:"extended"`
	Message  string `json:"message,omitempty"`
}

// Handle processes a task heartbeat request.
func (h *TaskHeartbeatHandler) Handle(params json.RawMessage) *uds.Response {
	var p TaskHeartbeatParams
	if err := json.Unmarshal(params, &p); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("unmarshal params: %v", err))
	}

	// Validate parameters
	if p.TaskID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "task_id is required")
	}
	if p.WorkerID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "worker_id is required")
	}
	if p.Epoch < 0 {
		return uds.ErrorResponse(uds.ErrCodeValidation, "epoch must be non-negative")
	}
	// Validate worker ID to prevent path traversal
	if filepath.Base(p.WorkerID) != p.WorkerID || p.WorkerID == "." || p.WorkerID == ".." {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid worker ID: %q", p.WorkerID))
	}

	// Find the queue file for this worker
	queueFile := fmt.Sprintf("queue/%s.yaml", p.WorkerID)
	queuePath := filepath.Join(h.maestroDir, queueFile)

	// Acquire locks in the same order as result_write
	h.scanMu.RLock()
	defer h.scanMu.RUnlock()

	// Acquire file lock for the queue file
	unlock, err := h.acquireFileLock(queueFile)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("acquire file lock: %v", err))
	}
	defer unlock()

	// Read queue file
	queueData, err := os.ReadFile(queuePath)
	if err != nil {
		if os.IsNotExist(err) {
			return uds.ErrorResponse(uds.ErrCodeNotFound, fmt.Sprintf("queue file not found: %s", queueFile))
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("read queue file: %v", err))
	}

	var queue model.TaskQueue
	if err := yamlv3.Unmarshal(queueData, &queue); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("unmarshal queue: %v", err))
	}

	// Find the task in the queue
	var task *model.Task
	for i, t := range queue.Tasks {
		if t.ID == p.TaskID {
			task = &queue.Tasks[i]
			break
		}
	}

	if task == nil {
		return uds.ErrorResponse(uds.ErrCodeNotFound, fmt.Sprintf("task %s not found in queue %s", p.TaskID, queueFile))
	}

	// Validate task status
	if task.Status != model.StatusInProgress {
		h.log(LogLevelWarn, "heartbeat_rejected task=%s status=%s (not in_progress)", p.TaskID, task.Status)
		return uds.ErrorResponse(uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s is %s, not in_progress", p.TaskID, task.Status))
	}

	// Validate epoch to prevent stale heartbeats
	if task.LeaseEpoch != p.Epoch {
		h.log(LogLevelWarn, "heartbeat_rejected task=%s epoch_mismatch queue=%d request=%d",
			p.TaskID, task.LeaseEpoch, p.Epoch)
		return uds.ErrorResponse(uds.ErrCodeFencingReject,
			fmt.Sprintf("task %s epoch mismatch: queue=%d, request=%d", p.TaskID, task.LeaseEpoch, p.Epoch))
	}

	// Check max_in_progress_min limit
	maxMin := h.config.Watcher.MaxInProgressMin
	if maxMin <= 0 {
		maxMin = 60
	}

	updatedAt, err := time.Parse(time.RFC3339, task.UpdatedAt)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("parse updated_at: %v", err))
	}

	elapsed := time.Since(updatedAt)
	if elapsed >= time.Duration(maxMin)*time.Minute {
		h.log(LogLevelWarn, "heartbeat_rejected task=%s max_runtime_exceeded elapsed=%v max=%dm",
			p.TaskID, elapsed, maxMin)
		return uds.ErrorResponse(uds.ErrCodeMaxRuntimeExceeded,
			fmt.Sprintf("task %s exceeded max runtime of %d minutes", p.TaskID, maxMin))
	}

	// Extend the lease
	if err := h.leaseManager.ExtendTaskLease(task); err != nil {
		h.log(LogLevelError, "lease_extend_failed task=%s error=%v", p.TaskID, err)
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("extend lease: %v", err))
	}

	// Write updated queue back
	if err := yaml.AtomicWrite(queuePath, &queue); err != nil {
		h.log(LogLevelError, "queue_write_failed file=%s error=%v", queueFile, err)
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("write queue: %v", err))
	}

	h.log(LogLevelDebug, "heartbeat_processed task=%s worker=%s epoch=%d lease_extended",
		p.TaskID, p.WorkerID, p.Epoch)

	return uds.SuccessResponse(&TaskHeartbeatResult{
		Extended: true,
		Message:  fmt.Sprintf("Lease extended for task %s", p.TaskID),
	})
}

// acquireFileLock acquires a file-scoped lock and returns an unlock function.
func (h *TaskHeartbeatHandler) acquireFileLock(file string) (func(), error) {
	// Extract worker ID from file path (queue/worker.yaml -> worker)
	workerID := filepath.Base(file)
	if idx := strings.LastIndex(workerID, ".yaml"); idx > 0 {
		workerID = workerID[:idx]
	}
	// Use consistent lock key format: "queue:{workerID}"
	key := "queue:" + workerID
	h.lockMap.Lock(key)
	return func() { h.lockMap.Unlock(key) }, nil
}

func (h *TaskHeartbeatHandler) log(level LogLevel, format string, args ...interface{}) {
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
		prefix := fmt.Sprintf("[%s] ", levelStr)
		h.logger.Printf(prefix+format, args...)
	}
}