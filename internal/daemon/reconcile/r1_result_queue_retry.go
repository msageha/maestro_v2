package reconcile

import (
	"fmt"
	"log/slog"
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
//
// Lock ordering: state lock and queue lock are never held simultaneously.
// Phase 1 snapshots entries under state lock, Phase 2 performs queue I/O
// without the state lock, and Phase 3 reacquires the state lock to apply
// results. This matches the convention used by r1ConsumeQueueWriteFailed
// and R0PlanningStuck.
func r1ProcessRetryEnqueueForCommand(run *Run, commandID, statePath string) []Repair {
	lockKey := "state:" + commandID

	// Phase 1: Snapshot RetryEnqueueFailed entries under state lock.
	type retryEntry struct {
		taskID     string
		workerID   string
		retryCount int
		// predecessorID is the orphaned retry's lineage predecessor (the
		// task it was retrying). Captured so Phase 2 can rebuild the retry
		// from the CORRECT template instead of guessing.
		predecessorID string
	}
	var snapshot []retryEntry

	run.Deps.LockMap.WithLock(lockKey, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			return
		}
		for taskID, value := range state.RetryEnqueueFailed {
			wid, rc := parseRetryEnqueueValue(value)
			snapshot = append(snapshot, retryEntry{
				taskID:        taskID,
				workerID:      wid,
				retryCount:    rc,
				predecessorID: state.RetryLineage[taskID],
			})
		}
	})

	if len(snapshot) == 0 {
		return nil
	}

	// Phase 2: Queue operations without state lock.
	// Each result records the action to apply in Phase 3.
	const (
		actionClearedExists    = "cleared_exists"
		actionMaxRetriesFailed = "max_retries_failed"
		actionNoOriginalFailed = "no_original_failed"
		actionEnqueued         = "enqueued"
		actionRetryIncremented = "retry_incremented"
	)
	type entryResult struct {
		entry  retryEntry
		action string
	}
	results := make([]entryResult, 0, len(snapshot))

	for _, entry := range snapshot {
		if r1TaskExistsInQueue(run, entry.workerID, entry.taskID) {
			run.Log(core.LogLevelInfo, "R1 retry_enqueue_already_in_queue task=%s worker=%s command=%s",
				entry.taskID, entry.workerID, commandID)
			results = append(results, entryResult{entry: entry, action: actionClearedExists})
			continue
		}

		if entry.retryCount >= maxRetryEnqueueAttempts {
			run.Log(core.LogLevelError, "R1 retry_enqueue_max_retries task=%s worker=%s command=%s attempts=%d",
				entry.taskID, entry.workerID, commandID, entry.retryCount)
			results = append(results, entryResult{entry: entry, action: actionMaxRetriesFailed})
			continue
		}

		originalTask := r1FindOriginalTask(run, entry.workerID, commandID, entry.predecessorID)
		if originalTask == nil {
			run.Log(core.LogLevelError, "R1 retry_enqueue_no_original task=%s worker=%s command=%s predecessor=%q (original task not found, marked failed)",
				entry.taskID, entry.workerID, commandID, entry.predecessorID)
			results = append(results, entryResult{entry: entry, action: actionNoOriginalFailed})
			continue
		}

		retryTask := r1BuildRetryTask(originalTask, entry.taskID, run.Deps.Clock)
		if err := r1AddTaskToQueue(run, entry.workerID, &retryTask); err != nil {
			run.Log(core.LogLevelWarn, "R1 retry_enqueue_failed task=%s worker=%s command=%s attempt=%d error=%v",
				entry.taskID, entry.workerID, commandID, entry.retryCount+1, err)
			results = append(results, entryResult{entry: entry, action: actionRetryIncremented})
			continue
		}

		run.Log(core.LogLevelInfo, "R1 retry_enqueue_success task=%s worker=%s command=%s",
			entry.taskID, entry.workerID, commandID)
		results = append(results, entryResult{entry: entry, action: actionEnqueued})
	}

	// Phase 3: Reacquire state lock and apply results.
	var repairs []Repair
	run.Deps.LockMap.WithLock(lockKey, func() {
		state, err := run.loadState(statePath)
		if err != nil {
			return
		}

		modified := false
		for _, r := range results {
			// Verify entry still exists in current state (may have been
			// cleared by a concurrent handler between Phase 1 and Phase 3).
			if _, ok := state.RetryEnqueueFailed[r.entry.taskID]; !ok {
				continue
			}

			switch r.action {
			case actionClearedExists:
				delete(state.RetryEnqueueFailed, r.entry.taskID)
				modified = true
				repairs = append(repairs, Repair{
					Pattern: PatternR1, CommandID: commandID, TaskID: r.entry.taskID,
					Detail: "retry_enqueue_failed cleared (task already in queue)",
				})

			case actionMaxRetriesFailed:
				if state.TaskStates == nil {
					state.TaskStates = make(map[string]model.Status)
				}
				state.TaskStates[r.entry.taskID] = model.StatusDeadLetter
				delete(state.RetryEnqueueFailed, r.entry.taskID)
				modified = true

				// Write dead-letter archive for exhausted retry enqueue.
				reason := fmt.Sprintf("retry_enqueue attempts (%d) >= max (%d)", r.entry.retryCount, maxRetryEnqueueAttempts)
				if err := r1WriteDeadLetterArchive(run, r.entry.workerID, r.entry.taskID, commandID, reason); err != nil {
					run.Log(core.LogLevelError, "R1 dead_letter_archive_failed task=%s command=%s error=%v",
						r.entry.taskID, commandID, err)
				}

				slog.Error("R1 retry_enqueue_dead_lettered",
					"task_id", r.entry.taskID,
					"command_id", commandID,
					"worker_id", r.entry.workerID,
					"retry_count", r.entry.retryCount,
					"reason", reason,
				)

				repairs = append(repairs, Repair{
					Pattern: PatternR1, CommandID: commandID, TaskID: r.entry.taskID,
					Detail: fmt.Sprintf("retry_enqueue_failed max attempts (%d) exceeded, dead-lettered", r.entry.retryCount),
				})

			case actionNoOriginalFailed:
				if state.TaskStates == nil {
					state.TaskStates = make(map[string]model.Status)
				}
				state.TaskStates[r.entry.taskID] = model.StatusFailed
				delete(state.RetryEnqueueFailed, r.entry.taskID)
				modified = true
				repairs = append(repairs, Repair{
					Pattern: PatternR1, CommandID: commandID, TaskID: r.entry.taskID,
					Detail: "retry_enqueue_failed original task not found, marked failed",
				})

			case actionEnqueued:
				delete(state.RetryEnqueueFailed, r.entry.taskID)
				modified = true
				repairs = append(repairs, Repair{
					Pattern: PatternR1, CommandID: commandID, TaskID: r.entry.taskID,
					Detail: fmt.Sprintf("retry_enqueue_failed re-enqueued to %s", r.entry.workerID),
				})

			case actionRetryIncremented:
				state.RetryEnqueueFailed[r.entry.taskID] = formatRetryEnqueueValue(
					r.entry.workerID, r.entry.retryCount+1)
				modified = true
			}
		}

		if modified {
			now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)
			state.LastReconciledAt = &now
			state.UpdatedAt = now
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				run.Log(core.LogLevelError, "R1 write_state_retry_enqueue command=%s error=%v", commandID, err)
			}
		}
	})

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
	queuePath := taskQueuePath(run.Deps.MaestroDir, workerID)

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
// r1FindOriginalTask locates the template task for rebuilding an orphaned
// retry. When the retry's lineage predecessor is known it is matched BY ID —
// a worker that processed several tasks of the same command would otherwise
// have the "latest terminal task" heuristic pick a different task's
// Content/ExpectedPaths and re-enqueue the wrong work under the orphan's ID.
// The heuristic remains only as a fallback for legacy entries without
// lineage.
func r1FindOriginalTask(run *Run, workerID, commandID, predecessorID string) *model.Task {
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

	if predecessorID != "" {
		for i := range tq.Tasks {
			task := &tq.Tasks[i]
			if task.CommandID == commandID && task.ID == predecessorID {
				cp := *task
				return &cp
			}
		}
		// The predecessor is known but absent from this queue (archived /
		// cleaned). Do NOT fall through to the heuristic: rebuilding from
		// an unrelated task is worse than dead-lettering for Planner
		// review.
		return nil
	}

	// Legacy fallback: find the most recent terminal task for this command.
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
	// §S0-1: classify retry tasks as repair operations so admission control
	// counts them against the repair concurrency limit, regardless of how the
	// original task's Purpose was named.
	retryTask.OperationType = model.OperationTypeRepair
	// A/B candidacy is per-race and never inherited (see CreateRetryTask):
	// a tagged rebuild would route to an orphan candidate worktree.
	retryTask.ABGroupID = ""
	retryTask.CreatedAt = now
	retryTask.UpdatedAt = now
	return retryTask
}

// r1AddTaskToQueue appends a task to the worker's queue file.
// Acquires queue lock internally.
func r1AddTaskToQueue(run *Run, workerID string, task *model.Task) error {
	queuePath := taskQueuePath(run.Deps.MaestroDir, workerID)

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

// r1WriteDeadLetterArchive writes a dead-letter archive entry for a retry task
// whose enqueue attempts have been exhausted.
func r1WriteDeadLetterArchive(run *Run, workerID, taskID, commandID, reason string) error {
	archiveDir := filepath.Join(run.Deps.MaestroDir, "dead_letters")
	if err := os.MkdirAll(archiveDir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for a dead_letters directory
		return fmt.Errorf("create dead_letters dir: %w", err)
	}

	type deadLetterEntry struct {
		SchemaVersion  int    `yaml:"schema_version"`
		FileType       string `yaml:"file_type"`
		QueueType      string `yaml:"queue_type"`
		TaskID         string `yaml:"task_id"`
		CommandID      string `yaml:"command_id"`
		DeadLetteredAt string `yaml:"dead_lettered_at"`
		Reason         string `yaml:"reason"`
	}

	now := run.Deps.Clock.Now().UTC()
	entry := deadLetterEntry{
		SchemaVersion:  1,
		FileType:       "dead_letter",
		QueueType:      workerID,
		TaskID:         taskID,
		CommandID:      commandID,
		DeadLetteredAt: now.Format(time.RFC3339),
		Reason:         reason,
	}

	filename := fmt.Sprintf("%s_%s_%s.yaml", workerID, now.Format("20060102T150405Z"), taskID)
	archivePath := filepath.Join(archiveDir, filename)

	return yamlutil.AtomicWrite(archivePath, entry)
}
