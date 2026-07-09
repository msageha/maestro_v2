package reconcile

import (
	"os"
	"path/filepath"
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
		queuePath := filepath.Join(queueDirPath(run.Deps.MaestroDir), name)

		resultData, err := os.ReadFile(resultPath) //nolint:gosec // resultPath is constructed from a controlled application results directory
		if err != nil {
			continue
		}
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(resultData, &rf); err != nil {
			continue
		}

		terminalResults := make(map[string]terminalResultInfo)
		for _, result := range rf.Results {
			if !model.IsTerminal(result.Status) {
				continue
			}
			// Keep the newest terminal result per task so the stale-result
			// fence in taskQueueAccessor compares against the latest attempt.
			if existing, ok := terminalResults[result.TaskID]; ok && existing.CreatedAt >= result.CreatedAt {
				continue
			}
			terminalResults[result.TaskID] = terminalResultInfo{Status: result.Status, CreatedAt: result.CreatedAt}
		}
		if len(terminalResults) == 0 {
			continue
		}

		workerRepairs, workerRepairedCommands, err := reconcileTerminalQueue(
			run, PatternR1, workerID, queuePath, terminalResults,
			unmarshalTaskQueue, setTaskQueueItems, taskQueueAccessor(),
		)
		if err != nil {
			run.Log(core.LogLevelError, "R1 reconcile_terminal_queue worker=%s error=%v", workerID, err)
			continue
		}
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
// Lock ordering: state and queue locks are never held simultaneously.
// Snapshot markers under state lock → release → acquire queue lock to
// inspect → release → reacquire state lock to clear. This three-phase
// pattern is the canonical approach throughout the reconcile package.
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
		run.Deps.LockMap.WithLock("state:"+commandID, func() {
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
		})

		if len(snapshot) == 0 {
			continue
		}

		// Inspect queue without state lock.
		// Group pending items by workerID to read each queue file at most once,
		// replacing per-item O(n) linear scans with a single map lookup per task.
		byWorker := make(map[string][]string) // workerID → []taskID
		for _, p := range snapshot {
			byWorker[p.workerID] = append(byWorker[p.workerID], p.taskID)
		}
		clearable := make(map[string]bool)
		for wID, taskIDs := range byWorker {
			terminalTasks := r1LoadQueueTerminalTasks(run, wID)
			for _, tid := range taskIDs {
				if terminalTasks[tid] {
					clearable[tid] = true
				}
			}
		}

		if len(clearable) == 0 {
			continue
		}

		// Reacquire state lock and clear entries that are still present.
		run.Deps.LockMap.WithLock("state:"+commandID, func() {
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
		})
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

// r1LoadQueueTerminalTasks reads a worker's queue file once and returns a set
// of task IDs that are in terminal state. Acquires queue lock internally.
// Returns nil on any I/O or parse error (caller will retry on the next scan).
// Used to batch-check multiple tasks against a single queue read.
func r1LoadQueueTerminalTasks(run *Run, workerID string) map[string]bool {
	queuePath := taskQueuePath(run.Deps.MaestroDir, workerID)

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
	result := make(map[string]bool, len(tq.Tasks))
	for _, task := range tq.Tasks {
		if model.IsTerminal(task.Status) {
			result[task.ID] = true
		}
	}
	return result
}
