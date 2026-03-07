package daemon

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// CancelHandler processes cancellation of commands and their tasks.
type CancelHandler struct {
	maestroDir      string
	config          model.Config
	dl              *DaemonLogger
	logger          *log.Logger
	logLevel        LogLevel
	clock           Clock
	executorFactory ExecutorFactory
	stateReader     StateReader
	lockMap         *lock.MutexMap
	worktreeManager *WorktreeManager

	execMu        sync.Mutex // protects cachedExec, cachedExecErr, execInit
	cachedExec    AgentExecutor
	cachedExecErr error
	execInit      bool

	fieldMu sync.RWMutex // protects stateReader and worktreeManager
}

// NewCancelHandler creates a new CancelHandler.
func NewCancelHandler(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel) *CancelHandler {
	return &CancelHandler{
		maestroDir: maestroDir,
		config:     cfg,
		dl:         NewDaemonLoggerFromLegacy("cancel_handler", logger, logLevel),
		lockMap:    lockMap,
		logger:     logger,
		logLevel:   logLevel,
		clock:      RealClock{},
		executorFactory: func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
			return agent.NewExecutor(dir, wcfg, level)
		},
	}
}

// SetExecutorFactory overrides the executor factory for testing.
// Resets the cached executor so the new factory is used on next call.
func (ch *CancelHandler) SetExecutorFactory(f ExecutorFactory) {
	ch.execMu.Lock()
	old := ch.cachedExec
	ch.executorFactory = f
	ch.cachedExec = nil
	ch.cachedExecErr = nil
	ch.execInit = false
	ch.execMu.Unlock()

	if old != nil {
		_ = old.Close()
	}
}

// getExecutor returns the shared executor instance, creating it lazily on first call.
// The Executor is safe for concurrent use (log.Logger uses internal mutex,
// os.File in append mode is POSIX-safe, all other fields are immutable).
func (ch *CancelHandler) getExecutor() (AgentExecutor, error) {
	ch.execMu.Lock()
	defer ch.execMu.Unlock()
	if !ch.execInit {
		ch.cachedExec, ch.cachedExecErr = ch.executorFactory(ch.maestroDir, ch.config.Watcher, ch.config.Logging.Level)
		ch.execInit = true
	}
	if ch.cachedExecErr != nil {
		return nil, fmt.Errorf("%w: %v", errExecutorInit, ch.cachedExecErr)
	}
	return ch.cachedExec, nil
}

// CloseExecutor releases the shared executor's resources.
// Safe to call multiple times; subsequent calls are no-ops.
func (ch *CancelHandler) CloseExecutor() {
	ch.execMu.Lock()
	exec := ch.cachedExec
	ch.cachedExec = nil
	ch.cachedExecErr = nil
	ch.execInit = false
	ch.execMu.Unlock()

	if exec != nil {
		_ = exec.Close()
	}
}

// SetStateReader wires the state reader for updating task states on cancellation.
func (ch *CancelHandler) SetStateReader(reader StateReader) {
	ch.fieldMu.Lock()
	defer ch.fieldMu.Unlock()
	ch.stateReader = reader
}

// SetWorktreeManager wires the worktree manager for cleanup on cancellation (H4).
func (ch *CancelHandler) SetWorktreeManager(wm *WorktreeManager) {
	ch.fieldMu.Lock()
	defer ch.fieldMu.Unlock()
	ch.worktreeManager = wm
}

// getStateReader returns the state reader with proper read-lock protection.
func (ch *CancelHandler) getStateReader() StateReader {
	ch.fieldMu.RLock()
	defer ch.fieldMu.RUnlock()
	return ch.stateReader
}

// getWorktreeManager returns the worktree manager with proper read-lock protection.
func (ch *CancelHandler) getWorktreeManager() *WorktreeManager {
	ch.fieldMu.RLock()
	defer ch.fieldMu.RUnlock()
	return ch.worktreeManager
}

// CleanupCommandWorktrees cleans up all worktrees for a cancelled command.
// No-op if worktreeManager is nil (worktree feature disabled).
func (ch *CancelHandler) CleanupCommandWorktrees(commandID string) {
	wm := ch.getWorktreeManager()
	if wm == nil {
		return
	}
	if !wm.HasWorktrees(commandID) {
		return
	}
	if err := wm.CleanupCommand(commandID); err != nil {
		ch.log(LogLevelWarn, "cancel_worktree_cleanup command=%s error=%v", commandID, err)
	} else {
		ch.log(LogLevelInfo, "cancel_worktree_cleanup_done command=%s", commandID)
	}
}

// IsCommandCancelRequested checks if a command has been marked for cancellation.
// For submitted commands, state/commands/ is the authoritative source (spec §4.3).
// Queue metadata (cancel_*) is only used for unsubmitted commands (no state file).
func (ch *CancelHandler) IsCommandCancelRequested(cmd *model.Command) bool {
	sr := ch.getStateReader()
	if sr != nil {
		requested, err := sr.IsCommandCancelRequested(cmd.ID)
		if err == nil {
			return requested
		}
		// State not found → unsubmitted command → use queue metadata (spec §4.3)
		if errors.Is(err, ErrStateNotFound) {
			return cmd.CancelRequestedAt != nil
		}
		// State exists but corrupted → log and return false (safe default)
		ch.log(LogLevelError, "cancel_check state_read_error command=%s error=%v", cmd.ID, err)
		return false
	}
	// No state reader configured → use queue metadata as best effort
	return cmd.CancelRequestedAt != nil
}

// CancelPendingTasks transitions pending tasks of a cancelled command to cancelled.
// Returns the number of tasks cancelled (periodic scan step 0.5).
func (ch *CancelHandler) CancelPendingTasks(tasks []model.Task, commandID string) []CancelledTaskResult {
	sr := ch.getStateReader()
	var results []CancelledTaskResult

	for i := range tasks {
		task := &tasks[i]
		if task.CommandID != commandID || task.Status != model.StatusPending {
			continue
		}

		if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusCancelled); err != nil {
			ch.log(LogLevelWarn, "cancel_pending_skip task=%s error=%v", task.ID, err)
			continue
		}

		task.Status = model.StatusCancelled
		task.UpdatedAt = ch.clock.Now().UTC().Format(time.RFC3339)

		ch.log(LogLevelInfo, "cancel_pending task=%s command=%s", task.ID, commandID)

		// Update state/commands/ with cancelled status + reason
		if sr != nil {
			if err := sr.UpdateTaskState(commandID, task.ID, model.StatusCancelled, "command_cancel_requested"); err != nil {
				ch.log(LogLevelWarn, "cancel_state_update task=%s error=%v", task.ID, err)
			}
		}

		results = append(results, CancelledTaskResult{
			TaskID:    task.ID,
			CommandID: commandID,
			Status:    "cancelled",
			Reason:    "command_cancel_requested",
		})
	}

	return results
}

// InterruptInProgressTasks interrupts in_progress tasks of a cancelled command.
// Returns the number of tasks interrupted (periodic scan step 0.6).
func (ch *CancelHandler) InterruptInProgressTasks(tasks []model.Task, commandID string, workerID string) []CancelledTaskResult {
	sr := ch.getStateReader()
	wm := ch.getWorktreeManager()
	var results []CancelledTaskResult

	for i := range tasks {
		task := &tasks[i]
		if task.CommandID != commandID || task.Status != model.StatusInProgress {
			continue
		}

		// Validate transition BEFORE sending interrupt (CR-029 consistency):
		// skip targets must not be interrupted.
		if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusCancelled); err != nil {
			ch.log(LogLevelWarn, "cancel_inprogress_skip task=%s error=%v", task.ID, err)
			continue
		}

		// Send interrupt via agent executor using workerID (agent ID like "worker1"),
		// NOT task.LeaseOwner which is in "daemon:{pid}" format.
		// Only interrupt if task has an active lease (LeaseOwner != nil).
		if task.LeaseOwner != nil && workerID != "" {
			if err := ch.interruptAgent(workerID, task.ID, task.CommandID, task.LeaseEpoch); err != nil {
				ch.log(LogLevelError, "interrupt_failed task=%s worker=%s error=%v",
					task.ID, workerID, err)
				// Continue to cancel even if interrupt fails
			}
		}

		// Clear lease fields before setting status to ensure any concurrent
		// observer sees either (in_progress, lease) or (cancelled, no_lease),
		// never (cancelled, active_lease).
		task.LeaseOwner = nil
		task.LeaseExpiresAt = nil
		task.UpdatedAt = ch.clock.Now().UTC().Format(time.RFC3339)
		task.Status = model.StatusCancelled

		ch.log(LogLevelInfo, "cancel_inprogress task=%s command=%s", task.ID, commandID)

		// H4: Discard uncommitted changes in worker worktree
		if wm != nil && workerID != "" {
			if err := wm.DiscardWorkerChanges(commandID, workerID); err != nil {
				ch.log(LogLevelWarn, "cancel_worktree_discard task=%s worker=%s error=%v",
					task.ID, workerID, err)
			}
		}

		// Update state/commands/ with cancelled status + reason
		if sr != nil {
			if err := sr.UpdateTaskState(commandID, task.ID, model.StatusCancelled, "command_cancel_requested"); err != nil {
				ch.log(LogLevelWarn, "cancel_state_update task=%s error=%v", task.ID, err)
			}
		}

		results = append(results, CancelledTaskResult{
			TaskID:    task.ID,
			CommandID: commandID,
			Status:    "cancelled",
			Reason:    "command_cancel_requested",
		})
	}

	return results
}

// InterruptInProgressTasksDeferred performs the same in-memory state mutation as
// InterruptInProgressTasks but defers the actual tmux interrupt to Phase B.
// Returns cancelled results and interrupt items to execute later.
func (ch *CancelHandler) InterruptInProgressTasksDeferred(tasks []model.Task, commandID string, workerID string) ([]CancelledTaskResult, []interruptItem) {
	sr := ch.getStateReader()
	wm := ch.getWorktreeManager()
	var results []CancelledTaskResult
	var interrupts []interruptItem

	for i := range tasks {
		task := &tasks[i]
		if task.CommandID != commandID || task.Status != model.StatusInProgress {
			continue
		}

		// Validate transition BEFORE collecting interrupt item (CR-029):
		// skip targets must not be added to the interrupt list.
		if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusCancelled); err != nil {
			ch.log(LogLevelWarn, "cancel_inprogress_skip task=%s error=%v", task.ID, err)
			continue
		}

		// Collect interrupt item for Phase B execution using workerID (agent ID like "worker1"),
		// NOT task.LeaseOwner which is in "daemon:{pid}" format.
		// Only collect if task has an active lease (LeaseOwner != nil).
		if task.LeaseOwner != nil && workerID != "" {
			interrupts = append(interrupts, interruptItem{
				WorkerID:  workerID,
				TaskID:    task.ID,
				CommandID: task.CommandID,
				Epoch:     task.LeaseEpoch,
			})
		}

		// Clear lease fields before setting status (same ordering as InterruptInProgressTasks).
		task.LeaseOwner = nil
		task.LeaseExpiresAt = nil
		task.UpdatedAt = ch.clock.Now().UTC().Format(time.RFC3339)
		task.Status = model.StatusCancelled

		ch.log(LogLevelInfo, "cancel_inprogress_deferred task=%s command=%s", task.ID, commandID)

		// H4: Discard uncommitted changes in worker worktree
		if wm != nil && workerID != "" {
			if err := wm.DiscardWorkerChanges(commandID, workerID); err != nil {
				ch.log(LogLevelWarn, "cancel_worktree_discard task=%s worker=%s error=%v",
					task.ID, workerID, err)
			}
		}

		if sr != nil {
			if err := sr.UpdateTaskState(commandID, task.ID, model.StatusCancelled, "command_cancel_requested"); err != nil {
				ch.log(LogLevelWarn, "cancel_state_update task=%s error=%v", task.ID, err)
			}
		}

		results = append(results, CancelledTaskResult{
			TaskID:    task.ID,
			CommandID: commandID,
			Status:    "cancelled",
			Reason:    "command_cancel_requested",
		})
	}

	return results, interrupts
}

// CancelledTaskResult represents a synthetic cancelled result entry.
type CancelledTaskResult struct {
	TaskID    string
	CommandID string
	Status    string
	Reason    string
}

// WriteSyntheticResults writes synthetic cancelled results to the results/ directory
// so that downstream processing (result handler, reconciler) can pick them up.
// The read-modify-write cycle is protected by lockMap to prevent data races with
// result_handler.processWorkerResultFile which uses the same "result:{workerID}" key.
func (ch *CancelHandler) WriteSyntheticResults(results []CancelledTaskResult, workerID string) {
	if len(results) == 0 {
		return
	}

	lockKey := "result:" + workerID
	ch.lockMap.Lock(lockKey)
	defer ch.lockMap.Unlock(lockKey)

	resultPath := filepath.Join(ch.maestroDir, "results", workerID+".yaml")
	var rf model.TaskResultFile

	data, err := os.ReadFile(resultPath)
	if err == nil {
		if unmarshalErr := yamlv3.Unmarshal(data, &rf); unmarshalErr != nil {
			ch.log(LogLevelWarn, "unmarshal_result_file worker=%s error=%v", workerID, unmarshalErr)
		}
	}
	if rf.SchemaVersion == 0 {
		rf.SchemaVersion = 1
		rf.FileType = "result_task"
	}

	now := ch.clock.Now().UTC().Format(time.RFC3339)
	for _, r := range results {
		resultID, err := model.GenerateID(model.IDTypeResult)
		if err != nil {
			ch.log(LogLevelError, "synthetic_result_id task=%s error=%v", r.TaskID, err)
			continue
		}
		rf.Results = append(rf.Results, model.TaskResult{
			ID:                     resultID,
			TaskID:                 r.TaskID,
			CommandID:              r.CommandID,
			Status:                 model.StatusCancelled,
			Summary:                fmt.Sprintf("cancelled: %s", r.Reason),
			PartialChangesPossible: true,
			RetrySafe:              false,
			Notified:               false,
			CreatedAt:              now,
		})
	}

	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		ch.log(LogLevelError, "synthetic_result_write worker=%s error=%v", workerID, err)
	}
}

// BuildSyntheticResult creates a synthetic result for a cancelled task.
func (ch *CancelHandler) BuildSyntheticResult(r CancelledTaskResult) map[string]any {
	return map[string]any{
		"task_id":    r.TaskID,
		"command_id": r.CommandID,
		"status":     r.Status,
		"reason":     r.Reason,
		"synthetic":  true,
		"created_at": ch.clock.Now().UTC().Format(time.RFC3339),
	}
}

// interruptAgent sends an interrupt to the agent running the task.
func (ch *CancelHandler) interruptAgent(workerID, taskID, commandID string, leaseEpoch int) error {
	exec, err := ch.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	result := exec.Execute(agent.ExecRequest{
		AgentID:    workerID,
		Mode:       agent.ModeInterrupt,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
	})

	if result.Error != nil {
		return result.Error
	}
	return nil
}

func (ch *CancelHandler) log(level LogLevel, format string, args ...any) {
	ch.dl.Logf(level, format, args...)
}
