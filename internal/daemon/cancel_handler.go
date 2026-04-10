package daemon

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	execProvider    *ExecutorProvider
	stateManager    StateManager
	lockMap         *lock.MutexMap
	worktreeManager *WorktreeManager
}

// NewCancelHandler creates a new CancelHandler with a shared ExecutorProvider.
func NewCancelHandler(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel, ep *ExecutorProvider) *CancelHandler {
	return &CancelHandler{
		maestroDir:   maestroDir,
		config:       cfg,
		dl:           NewDaemonLoggerFromLegacy("cancel_handler", logger, logLevel),
		lockMap:      lockMap,
		logger:       logger,
		logLevel:     logLevel,
		clock:        ep.clock,
		execProvider: ep,
	}
}

// getExecutor returns the shared executor instance, creating it lazily on first call.
// The Executor is safe for concurrent use (log.Logger uses internal mutex,
// os.File in append mode is POSIX-safe, all other fields are immutable).
func (ch *CancelHandler) getExecutor() (AgentExecutor, error) {
	return ch.execProvider.GetExecutor()
}

// SetStateReader wires the state manager for reading and updating task states on cancellation.
func (ch *CancelHandler) SetStateReader(reader StateManager) {
	ch.stateManager = reader
}

// SetWorktreeManager wires the worktree manager for cleanup on cancellation (H4).
func (ch *CancelHandler) SetWorktreeManager(wm *WorktreeManager) {
	ch.worktreeManager = wm
}

// IsCommandCancelRequested checks if a command has been marked for cancellation.
// For submitted commands, state/commands/ is the authoritative source (spec §4.3).
// Queue metadata (cancel_*) is only used for unsubmitted commands (no state file).
func (ch *CancelHandler) IsCommandCancelRequested(cmd *model.Command) bool {
	if ch.stateManager != nil {
		requested, err := ch.stateManager.IsCommandCancelRequested(cmd.ID)
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
	results := make([]CancelledTaskResult, 0, len(tasks))

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
		if ch.stateManager != nil {
			if err := ch.stateManager.UpdateTaskState(commandID, task.ID, model.StatusCancelled, "command_cancel_requested"); err != nil {
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

// CollectCancelInterruptItems inspects in_progress tasks for the cancelled
// command and returns deferred work items WITHOUT mutating task state.
//
// This is the Phase A half of the M3+H4 race-free cancellation protocol:
//
//   - Phase A (this function): collect interrupt + cancelMark items only.
//     Task state stays in_progress so a worker that races to completion before
//     the interrupt takes effect can still submit its result via the normal
//     result_write path (no FENCING_REJECT / DUPLICATE).
//   - Phase B: send the tmux interrupt and discard uncommitted worktree
//     changes (H4) — both must complete before queue mutation.
//   - Phase C: re-validate each cancelMark against the freshly-loaded queue
//     and apply ApplyCancelMark, which is a no-op for tasks that have already
//     transitioned to a terminal state by the worker.
func (ch *CancelHandler) CollectCancelInterruptItems(tasks []model.Task, commandID string, workerID string) ([]cancelMarkItem, []interruptItem) { //nolint:revive // unexported return types are intentional; callers are within the same package
	marks := make([]cancelMarkItem, 0, len(tasks))
	interrupts := make([]interruptItem, 0, len(tasks))

	for i := range tasks {
		task := &tasks[i]
		if task.CommandID != commandID || task.Status != model.StatusInProgress {
			continue
		}

		// Validate transition BEFORE collecting items (CR-029): skip targets
		// must not be added to the interrupt or mark lists.
		if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusCancelled); err != nil {
			ch.log(LogLevelWarn, "cancel_inprogress_skip task=%s error=%v", task.ID, err)
			continue
		}

		// Use workerID (agent ID like "worker1"), NOT task.LeaseOwner
		// (which is in "daemon:{pid}" format). Only emit an interrupt
		// when the task has an active lease.
		if task.LeaseOwner != nil && workerID != "" {
			interrupts = append(interrupts, interruptItem{
				WorkerID:  workerID,
				TaskID:    task.ID,
				CommandID: task.CommandID,
				Epoch:     task.LeaseEpoch,
			})
		}

		marks = append(marks, cancelMarkItem{
			WorkerID:   workerID,
			TaskID:     task.ID,
			CommandID:  task.CommandID,
			LeaseEpoch: task.LeaseEpoch,
		})

		ch.log(LogLevelInfo, "cancel_inprogress_collected task=%s command=%s", task.ID, commandID)
	}

	return marks, interrupts
}

// ApplyCancelMark mutates a queue task from in_progress to cancelled, but
// only if the task is still in_progress with the same lease_epoch that was
// observed when the cancel was collected in Phase A. If the worker raced to
// completion between Phase A and Phase C, the task is now terminal and this
// function is a no-op (returns applied=false), preserving the worker's real
// result. Caller must hold scanMu.Lock so the read-modify-write is atomic
// against result_write_handler (which acquires scanMu.RLock).
func (ch *CancelHandler) ApplyCancelMark(task *model.Task, expectedEpoch int) (CancelledTaskResult, bool) {
	// Accept both in_progress (still running) and pending (released back to
	// the queue between Phase A collection and Phase C apply, e.g. by
	// stepDispatchOrRecovery's malformed-lease release path). Both transitions
	// are valid command-task-queue transitions to cancelled. Epoch must match
	// to ensure we are cancelling the same dispatch generation we observed.
	if task.Status != model.StatusInProgress && task.Status != model.StatusPending {
		return CancelledTaskResult{}, false
	}
	if task.LeaseEpoch != expectedEpoch {
		return CancelledTaskResult{}, false
	}
	if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusCancelled); err != nil {
		return CancelledTaskResult{}, false
	}
	task.LeaseOwner = nil
	task.LeaseExpiresAt = nil
	task.UpdatedAt = ch.clock.Now().UTC().Format(time.RFC3339)
	task.Status = model.StatusCancelled

	if ch.stateManager != nil {
		if err := ch.stateManager.UpdateTaskState(task.CommandID, task.ID, model.StatusCancelled, "command_cancel_requested"); err != nil {
			ch.log(LogLevelWarn, "cancel_state_update task=%s error=%v", task.ID, err)
		}
	}
	return CancelledTaskResult{
		TaskID:    task.ID,
		CommandID: task.CommandID,
		Status:    "cancelled",
		Reason:    "command_cancel_requested",
	}, true
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

	data, err := os.ReadFile(resultPath) //nolint:gosec // resultPath is constructed from a controlled application results directory
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
