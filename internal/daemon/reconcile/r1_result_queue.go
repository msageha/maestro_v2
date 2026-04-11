package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

const maxRetryEnqueueAttempts = 3

// R1ResultQueue detects results/ terminal + queue/ in_progress mismatch.
// Action: update queue to terminal, clear lease. Update last_reconciled_at on state file.
// Additionally, it consumes RetryEnqueueFailed entries from command state, re-enqueuing
// orphaned retry tasks or marking them as failed after max attempts.
type R1ResultQueue struct{}

// Apply detects result/queue terminal-in_progress mismatches and corrects queue state.
func (R1ResultQueue) Apply(run *Run) Outcome {
	repairedCommands := make(map[string]bool)

	// --- Phase 1: Original result/queue mismatch detection ---
	resultsDir := filepath.Join(run.Deps.MaestroDir, "results")
	entries, err := run.cachedReadDir(resultsDir)
	if err != nil {
		return Outcome{}
	}

	repairs := make([]Repair, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := strings.TrimSuffix(name, ".yaml")
		resultPath := filepath.Join(resultsDir, name)
		queuePath := filepath.Join(run.Deps.MaestroDir, "queue", name)

		resultData, err := os.ReadFile(resultPath) //nolint:gosec // resultPath is constructed from a controlled application results directory
		if err != nil {
			continue
		}
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(resultData, &rf); err != nil {
			continue
		}

		terminalResults := make(map[string]model.Status)
		for _, result := range rf.Results {
			if model.IsTerminal(result.Status) {
				terminalResults[result.TaskID] = result.Status
			}
		}
		if len(terminalResults) == 0 {
			continue
		}

		workerRepairs, workerRepairedCommands := reconcileTerminalQueue(
			run, PatternR1, workerID, queuePath, terminalResults,
			unmarshalTaskQueue, setTaskQueueItems, taskQueueAccessor(),
		)
		repairs = append(repairs, workerRepairs...)
		for cmdID := range workerRepairedCommands {
			repairedCommands[cmdID] = true
		}
	}

	for commandID := range repairedCommands {
		run.updateLastReconciledAt(commandID)
	}

	// --- Phase 2: RetryEnqueueFailed consumption ---
	retryRepairs := r1ConsumeRetryEnqueueFailed(run)
	repairs = append(repairs, retryRepairs...)

	// --- Phase 3: QueueWriteFailed (H2 sticky error) cleanup ---
	// Phase 1 above already repairs the result-terminal/queue-in_progress
	// mismatch by walking results files. This phase clears the durable
	// QueueWriteFailed marker once the queue task has reached a terminal state
	// (either by Phase 1 in this same Apply or by an earlier scan).
	queueWriteRepairs := r1ConsumeQueueWriteFailed(run)
	repairs = append(repairs, queueWriteRepairs...)

	return Outcome{Repairs: repairs}
}

// r1ConsumeQueueWriteFailed walks command state files and clears
// QueueWriteFailed entries whose corresponding worker queue task has reached
// a terminal state. The marker is purely a durability hint; Phase 1 performs
// the actual queue repair, so this phase only needs to handle cleanup.
//
// Lock ordering: snapshot markers under state lock, release, then acquire the
// queue lock to inspect, then reacquire the state lock to clear (queue → state
// canonical order is preserved on the clear path because we drop the state
// lock before touching queue).
func r1ConsumeQueueWriteFailed(run *Run) []Repair {
	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.cachedReadDir(stateDir)
	if err != nil {
		return nil
	}

	repairs := make([]Repair, 0, len(entries))

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		statePath := filepath.Join(stateDir, entry.Name())

		// Snapshot under state lock.
		type pending struct {
			taskID   string
			workerID string
			resultID string
		}
		var snapshot []pending
		func() {
			lockKey := "state:" + commandID
			run.Deps.LockMap.Lock(lockKey)
			defer run.Deps.LockMap.Unlock(lockKey)

			state, err := run.loadState(statePath)
			if err != nil {
				return
			}
			for taskID, value := range state.QueueWriteFailed {
				workerID, resultID := parseQueueWriteFailedValue(value)
				if workerID == "" {
					continue
				}
				snapshot = append(snapshot, pending{taskID: taskID, workerID: workerID, resultID: resultID})
			}
		}()

		if len(snapshot) == 0 {
			continue
		}

		// Inspect queue without state lock.
		clearable := make(map[string]bool)
		for _, p := range snapshot {
			if r1QueueTaskTerminal(run, p.workerID, p.taskID) {
				clearable[p.taskID] = true
			}
		}

		if len(clearable) == 0 {
			continue
		}

		// Reacquire state lock and clear entries that are still present.
		func() {
			lockKey := "state:" + commandID
			run.Deps.LockMap.Lock(lockKey)
			defer run.Deps.LockMap.Unlock(lockKey)

			state, err := run.loadState(statePath)
			if err != nil {
				return
			}
			if len(state.QueueWriteFailed) == 0 {
				return
			}
			modified := false
			for taskID := range clearable {
				if _, ok := state.QueueWriteFailed[taskID]; ok {
					delete(state.QueueWriteFailed, taskID)
					modified = true
					run.Log(core.LogLevelInfo, "R1 queue_write_failed_cleared task=%s command=%s",
						taskID, commandID)
					repairs = append(repairs, Repair{
						Pattern:   PatternR1,
						CommandID: commandID,
						TaskID:    taskID,
						Detail:    "queue_write_failed marker cleared (queue terminal)",
					})
				}
			}
			if !modified {
				return
			}
			now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
			state.LastReconciledAt = &now
			state.UpdatedAt = now
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				run.Log(core.LogLevelError, "R1 write_state_queue_write_failed command=%s error=%v", commandID, err)
			}
		}()
	}

	return repairs
}

// parseQueueWriteFailedValue parses the "workerID:resultID" encoding written
// by result_write_handler.go. Returns empty workerID if the value is malformed.
func parseQueueWriteFailedValue(value string) (workerID, resultID string) {
	idx := strings.Index(value, ":")
	if idx <= 0 || idx == len(value)-1 {
		return "", ""
	}
	return value[:idx], value[idx+1:]
}

// r1QueueTaskTerminal reports whether the named task is currently terminal in
// the worker's queue. Acquires queue lock internally. Returns false on any
// I/O or parse error (caller will retry on the next scan).
func r1QueueTaskTerminal(run *Run, workerID, taskID string) bool {
	queuePath := filepath.Join(run.Deps.MaestroDir, "queue", workerID+".yaml")

	run.Deps.LockMap.Lock("queue:" + workerID)
	defer run.Deps.LockMap.Unlock("queue:" + workerID)

	data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		return false
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		return false
	}
	for _, task := range tq.Tasks {
		if task.ID == taskID {
			return model.IsTerminal(task.Status)
		}
	}
	return false
}

// r1ConsumeRetryEnqueueFailed scans command state files for RetryEnqueueFailed entries
// and attempts to re-enqueue orphaned retry tasks.
func r1ConsumeRetryEnqueueFailed(run *Run) []Repair {
	stateDir := filepath.Join(run.Deps.MaestroDir, "state", "commands")
	entries, err := run.cachedReadDir(stateDir)
	if err != nil {
		return nil
	}

	repairs := make([]Repair, 0, len(entries))

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		commandID := strings.TrimSuffix(entry.Name(), ".yaml")
		statePath := filepath.Join(stateDir, entry.Name())

		reps := r1ProcessRetryEnqueueForCommand(run, commandID, statePath)
		repairs = append(repairs, reps...)
	}

	return repairs
}

// r1ProcessRetryEnqueueForCommand processes RetryEnqueueFailed entries for a single command.
func r1ProcessRetryEnqueueForCommand(run *Run, commandID, statePath string) []Repair {
	lockKey := "state:" + commandID
	run.Deps.LockMap.Lock(lockKey)
	defer run.Deps.LockMap.Unlock(lockKey)

	state, err := run.loadState(statePath)
	if err != nil {
		return nil
	}

	if len(state.RetryEnqueueFailed) == 0 {
		return nil
	}

	repairs := make([]Repair, 0, len(state.RetryEnqueueFailed))
	modified := false

	for taskID, value := range state.RetryEnqueueFailed {
		workerID, retryCount := parseRetryEnqueueValue(value)

		// Idempotency: check if task already exists in queue
		if r1TaskExistsInQueue(run, workerID, taskID) {
			delete(state.RetryEnqueueFailed, taskID)
			modified = true
			run.Log(core.LogLevelInfo, "R1 retry_enqueue_already_in_queue task=%s worker=%s command=%s",
				taskID, workerID, commandID)
			repairs = append(repairs, Repair{
				Pattern:   PatternR1,
				CommandID: commandID,
				TaskID:    taskID,
				Detail:    "retry_enqueue_failed cleared (task already in queue)",
			})
			continue
		}

		// Max retries exceeded → mark task as failed
		if retryCount >= maxRetryEnqueueAttempts {
			if state.TaskStates == nil {
				state.TaskStates = make(map[string]model.Status)
			}
			state.TaskStates[taskID] = model.StatusFailed
			delete(state.RetryEnqueueFailed, taskID)
			modified = true
			run.Log(core.LogLevelError, "R1 retry_enqueue_max_retries task=%s worker=%s command=%s attempts=%d",
				taskID, workerID, commandID, retryCount)
			repairs = append(repairs, Repair{
				Pattern:   PatternR1,
				CommandID: commandID,
				TaskID:    taskID,
				Detail:    fmt.Sprintf("retry_enqueue_failed max attempts (%d) exceeded, marked failed", retryCount),
			})
			continue
		}

		// Find the original task in the worker's queue to reconstruct retry metadata
		originalTask := r1FindOriginalTask(run, workerID, commandID)
		if originalTask == nil {
			// Cannot reconstruct task → mark as failed
			if state.TaskStates == nil {
				state.TaskStates = make(map[string]model.Status)
			}
			state.TaskStates[taskID] = model.StatusFailed
			delete(state.RetryEnqueueFailed, taskID)
			modified = true
			run.Log(core.LogLevelError, "R1 retry_enqueue_no_original task=%s worker=%s command=%s (original task not found, marked failed)",
				taskID, workerID, commandID)
			repairs = append(repairs, Repair{
				Pattern:   PatternR1,
				CommandID: commandID,
				TaskID:    taskID,
				Detail:    "retry_enqueue_failed original task not found, marked failed",
			})
			continue
		}

		// Create retry task from original
		retryTask := r1BuildRetryTask(originalTask, taskID, run.Deps.Clock)

		// Attempt to add to queue
		if err := r1AddTaskToQueue(run, workerID, &retryTask); err != nil {
			// Increment retry count
			state.RetryEnqueueFailed[taskID] = formatRetryEnqueueValue(workerID, retryCount+1)
			modified = true
			run.Log(core.LogLevelWarn, "R1 retry_enqueue_failed task=%s worker=%s command=%s attempt=%d error=%v",
				taskID, workerID, commandID, retryCount+1, err)
			continue
		}

		// Success: clear entry
		delete(state.RetryEnqueueFailed, taskID)
		modified = true
		run.Log(core.LogLevelInfo, "R1 retry_enqueue_success task=%s worker=%s command=%s",
			taskID, workerID, commandID)
		repairs = append(repairs, Repair{
			Pattern:   PatternR1,
			CommandID: commandID,
			TaskID:    taskID,
			Detail:    fmt.Sprintf("retry_enqueue_failed re-enqueued to %s", workerID),
		})
	}

	if modified {
		now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
		state.LastReconciledAt = &now
		state.UpdatedAt = now
		if err := yamlutil.AtomicWrite(statePath, state); err != nil {
			run.Log(core.LogLevelError, "R1 write_state_retry_enqueue command=%s error=%v", commandID, err)
			return nil
		}
	}

	return repairs
}

// parseRetryEnqueueValue parses a RetryEnqueueFailed map value.
// Format: "workerID" (count=0) or "workerID:count".
func parseRetryEnqueueValue(value string) (workerID string, retryCount int) {
	idx := strings.LastIndex(value, ":")
	if idx < 0 {
		return value, 0
	}
	// Attempt to parse suffix as int; if it fails, treat entire value as workerID
	countStr := value[idx+1:]
	count, err := strconv.Atoi(countStr)
	if err != nil {
		return value, 0
	}
	return value[:idx], count
}

// formatRetryEnqueueValue formats a RetryEnqueueFailed map value with retry count.
func formatRetryEnqueueValue(workerID string, retryCount int) string {
	if retryCount == 0 {
		return workerID
	}
	return fmt.Sprintf("%s:%d", workerID, retryCount)
}

// r1TaskExistsInQueue checks if a task with the given ID exists in the worker's queue.
// Acquires queue lock internally.
func r1TaskExistsInQueue(run *Run, workerID, taskID string) bool {
	queuePath := filepath.Join(run.Deps.MaestroDir, "queue", workerID+".yaml")

	run.Deps.LockMap.Lock("queue:" + workerID)
	defer run.Deps.LockMap.Unlock("queue:" + workerID)

	data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		return false
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		return false
	}
	for _, task := range tq.Tasks {
		if task.ID == taskID {
			return true
		}
	}
	return false
}

// r1FindOriginalTask finds a terminal task in the worker's queue that belongs to the
// same command. This is used as a template to reconstruct the retry task.
// Acquires queue lock internally.
func r1FindOriginalTask(run *Run, workerID, commandID string) *model.Task {
	queuePath := filepath.Join(run.Deps.MaestroDir, "queue", workerID+".yaml")

	run.Deps.LockMap.Lock("queue:" + workerID)
	defer run.Deps.LockMap.Unlock("queue:" + workerID)

	data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		return nil
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		return nil
	}

	// Find the most recent terminal task for this command
	var best *model.Task
	for i := range tq.Tasks {
		task := &tq.Tasks[i]
		if task.CommandID == commandID && model.IsTerminal(task.Status) {
			if best == nil || task.UpdatedAt > best.UpdatedAt {
				cp := *task
				best = &cp
			}
		}
	}
	return best
}

// r1BuildRetryTask creates a pending task from an original task template with the given retry task ID.
func r1BuildRetryTask(original *model.Task, retryTaskID string, clock core.Clock) model.Task {
	now := clock.Now().UTC().Format(time.RFC3339)
	retryTask := *original
	retryTask.ID = retryTaskID
	retryTask.Status = model.StatusPending
	retryTask.LeaseOwner = nil
	retryTask.LeaseExpiresAt = nil
	retryTask.LeaseEpoch = 0
	retryTask.Attempts = 0
	retryTask.InProgressAt = nil
	retryTask.LastError = nil
	retryTask.DeadLetteredAt = nil
	retryTask.DeadLetterReason = nil
	retryTask.ExecutionRetries = original.ExecutionRetries + 1
	if original.OriginalTaskID != "" {
		retryTask.OriginalTaskID = original.OriginalTaskID
	} else {
		retryTask.OriginalTaskID = original.ID
	}
	retryTask.CreatedAt = now
	retryTask.UpdatedAt = now
	return retryTask
}

// r1AddTaskToQueue appends a task to the worker's queue file.
// Acquires queue lock internally.
func r1AddTaskToQueue(run *Run, workerID string, task *model.Task) error {
	queuePath := filepath.Join(run.Deps.MaestroDir, "queue", workerID+".yaml")

	run.Deps.LockMap.Lock("queue:" + workerID)
	defer run.Deps.LockMap.Unlock("queue:" + workerID)

	data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read queue: %w", err)
	}

	var tq model.TaskQueue
	if len(data) > 0 {
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			return fmt.Errorf("parse queue: %w", err)
		}
	} else {
		tq.SchemaVersion = 1
		tq.FileType = "queue_task"
	}

	tq.Tasks = append(tq.Tasks, *task)

	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		return fmt.Errorf("write queue: %w", err)
	}

	return nil
}
