package daemon

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
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
	h := &TaskRetryHandler{
		maestroDir: maestroDir,
		config:     cfg,
		lockMap:    lockMap,
		clock:      RealClock{},
		dl:         NewDaemonLoggerFromLegacy("task_retry", logger, logLevel),
		logger:     logger,
		logLevel:   logLevel,
	}
	return h
}

// ShouldRetryTask determines if a failed task should be retried.
// retrySafe indicates whether the worker marked the result as safe to retry.
//
// REQUIREMENTS §S2-2 (multi-faceted Circuit Breaker): the per-task
// definition_of_abort thresholds (max_repair_count, max_wall_clock_sec,
// explicit_failure_conditions) MUST force termination once exceeded. This is
// enforced here in addition to the global Retry.TaskExecution.MaxRetries cap;
// either limit can stop the retry loop, but per-task limits cannot be
// loosened by global config.
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

	// REQUIREMENTS §S2-2: enforce per-task definition_of_abort BEFORE global
	// retry config. Task-level abort thresholds dominate so a Planner's stop
	// condition cannot be bypassed by relaxed daemon-wide retry settings.
	if task.DefinitionOfAbort != nil {
		if stop, reason := h.shouldAbortByDefinition(task); stop {
			return false, reason
		}
	}

	// Check if max retries exceeded (use ExecutionRetries for actual retry count)
	if retryConfig.MaxRetries > 0 && task.ExecutionRetries >= retryConfig.MaxRetries {
		return false, fmt.Sprintf("max retries exceeded (%d/%d)", task.ExecutionRetries, retryConfig.MaxRetries)
	}

	// Check if exit code is retryable
	if !slices.Contains(retryConfig.RetryableExitCodes, exitCode) {
		return false, fmt.Sprintf("exit code %d not retryable", exitCode)
	}

	return true, ""
}

// IsFutileIntegrationScopedRetry reports whether retrying a failed
// integration-scoped verification task identically has no chance of a
// different outcome.
//
// Tasks with run_on_integration / run_on_main observe the shared merged state
// instead of producing new work in an isolated worktree. When a task
// classified as OperationTypeVerify reports a semantic failure (exit code 1:
// "the task ran and its verdict is FAIL") *without having changed anything*
// (no files changed, no partial changes possible), an identical replacement
// re-observes the same unchanged state and necessarily reaches the same
// verdict. Skipping the retry routes the failure to paused_for_replan
// immediately (see applyTaskStateProgression's non-retryable branch) so the
// Planner can schedule a repair task or escalate, instead of burning the
// retry budget on identical re-runs that only delay the replan signal.
//
// The OperationTypeVerify gate matters: run_on_integration tasks default to
// OperationTypeRepair at queue write (publish_conflict resolution is their
// canonical use), and a write-intent repair task that happens to fail before
// editing anything still benefits from the normal retry budget. Only tasks
// explicitly classified as verification — run_on_main defaults to verify;
// run_on_integration verification tasks must set operation_type verify
// (plan submit tasks-file `operation_type:` / `plan add-task
// --operation-type verify`) — are treated as futile to retry. Abnormal
// terminations (timeout 124, SIGKILL 137, ...) are transient process
// failures, not verdicts, and also keep the normal retryable_exit_codes path.
func IsFutileIntegrationScopedRetry(task *model.Task, exitCode int, filesChanged []string, partialChangesPossible bool) bool {
	return task.OperationType == model.OperationTypeVerify &&
		(task.RunOnIntegration || task.RunOnMain) &&
		exitCode == 1 &&
		len(filesChanged) == 0 &&
		!partialChangesPossible
}

// ShouldRepairTask determines whether a verification failure should create an
// automatic repair task. Verification failures are semantic failures, not
// worker process exits, so retryable_exit_codes is intentionally not consulted.
func (h *TaskRetryHandler) ShouldRepairTask(task *model.Task, failureReason string) (bool, string) {
	retryConfig := h.config.Retry.TaskExecution
	if !retryConfig.Enabled {
		return false, "retry disabled"
	}

	taskForAbort := *task
	if failureReason != "" {
		taskForAbort.LastError = &failureReason
	}
	if taskForAbort.DefinitionOfAbort != nil {
		if stop, reason := h.shouldAbortByDefinition(&taskForAbort); stop {
			return false, reason
		}
	}

	if retryConfig.MaxRetries > 0 && task.ExecutionRetries >= retryConfig.MaxRetries {
		return false, fmt.Sprintf("max retries exceeded (%d/%d)", task.ExecutionRetries, retryConfig.MaxRetries)
	}

	return true, ""
}

// MaxRepairCountReasonPrefix is the leading substring of the reason string
// returned by ShouldRetryTask when retry is rejected due to
// definition_of_abort.max_repair_count being met or exceeded. Callers (notably
// result_write_phase_b) match against this prefix to decide whether to route
// the task to paused_for_replan instead of the generic failed terminal,
// satisfying §S2-2 (Circuit Breaker → planner replan signal).
const MaxRepairCountReasonPrefix = "definition_of_abort.max_repair_count"

// ErrPhaseMaxTasksExceeded is retained as a sentinel for backwards
// compatibility with tests / callers that still reference it. The
// daemon's RegisterRetryTaskInState no longer emits it because retry
// depth is already bounded by Retry.TaskExecution.MaxRetries and
// DefinitionOfAbort.MaxRepairCount; the per-phase MaxTasks guard was
// redundant and would block legitimate retries.
var ErrPhaseMaxTasksExceeded = fmt.Errorf("phase max_tasks exceeded; retry refused")

// IsAbortByMaxRepair reports whether reason originates from
// definition_of_abort.max_repair_count exceeding its threshold. The string
// prefix match is intentional — ShouldRetryTask formats the counter values
// after the prefix and we want to match every variant.
func IsAbortByMaxRepair(reason string) bool {
	return strings.HasPrefix(reason, MaxRepairCountReasonPrefix)
}

// shouldAbortByDefinition evaluates a task's definition_of_abort against its
// current execution counters and last error. Returns (stop, reason) where
// stop=true means the retry loop must terminate. Caller must ensure
// task.DefinitionOfAbort != nil.
//
// Semantics (REQUIREMENTS §2.2 / §S2-2):
//   - max_repair_count: ExecutionRetries already reflects how many times the
//     task has been retried. If the next retry would exceed the budget, abort.
//   - max_wall_clock_sec: total elapsed time since the task was first created
//     must not exceed the budget. Parse failures are non-fatal (treated as
//     unbounded) so that older state files do not break retry.
//   - explicit_failure_conditions: substring match against task.LastError. If
//     any condition matches, abort immediately regardless of counters.
func (h *TaskRetryHandler) shouldAbortByDefinition(task *model.Task) (bool, string) {
	doa := task.DefinitionOfAbort

	if doa.MaxRepairCount > 0 && task.ExecutionRetries >= doa.MaxRepairCount {
		return true, fmt.Sprintf("%s exceeded (%d/%d)",
			MaxRepairCountReasonPrefix, task.ExecutionRetries, doa.MaxRepairCount)
	}

	if doa.MaxWallClockSec > 0 && task.CreatedAt != "" {
		if createdAt, err := time.Parse(time.RFC3339, task.CreatedAt); err == nil {
			elapsed := h.clock.Now().UTC().Sub(createdAt.UTC())
			if elapsed >= time.Duration(doa.MaxWallClockSec)*time.Second {
				return true, fmt.Sprintf("definition_of_abort.max_wall_clock_sec exceeded (%ds/%ds)",
					int(elapsed.Seconds()), doa.MaxWallClockSec)
			}
		}
	}

	if len(doa.ExplicitFailureConditions) > 0 && task.LastError != nil && *task.LastError != "" {
		lastErr := *task.LastError
		for _, cond := range doa.ExplicitFailureConditions {
			cond = strings.TrimSpace(cond)
			if cond == "" {
				continue
			}
			if strings.Contains(lastErr, cond) {
				return true, fmt.Sprintf("definition_of_abort.explicit_failure_condition matched: %q", cond)
			}
		}
	}

	return false, ""
}

// CreateRetryTask creates a new task for retry with cooldown.
func (h *TaskRetryHandler) CreateRetryTask(originalTask *model.Task, _ string, exitCode int) (*model.Task, error) {
	retryConfig := h.config.Retry.TaskExecution

	// Generate a proper task ID for the retry task via the audited entrypoint.
	retryTaskID, err := model.NewTaskID(model.TaskIDCallerDaemonRetry)
	if err != nil {
		return nil, fmt.Errorf("generate retry task ID: %w", err)
	}

	// Create retry task with same content but new ID and increased retry count
	retryTask := *originalTask
	// QA-009: Deep copy slice fields to avoid shared backing arrays
	retryTask.BlockedBy = slices.Clone(originalTask.BlockedBy)
	retryTask.ToolsHint = slices.Clone(originalTask.ToolsHint)
	retryTask.SkillRefs = slices.Clone(originalTask.SkillRefs)
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
	retryTask.OperationType = model.OperationTypeRepair
	// A/B candidacy is per-race and never inherited: a tagged retry would be
	// routed to an orphan candidate worktree (no group membership → nobody
	// ever intakes its work) and would dodge the selection barrier.
	retryTask.ABGroupID = ""

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

// CreateVerifyRepairTask creates a repair task that carries the verification
// failure reason into the worker prompt instead of replaying the original task
// blindly.
func (h *TaskRetryHandler) CreateVerifyRepairTask(originalTask *model.Task, failureReason string) (*model.Task, error) {
	const verifyRepairExitCode = 1
	repairTask, err := h.CreateRetryTask(originalTask, "", verifyRepairExitCode)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(failureReason)
	if reason == "" {
		reason = "verify_failed"
	}
	repairTask.Content = fmt.Sprintf(
		"Repair the previous implementation because daemon verification failed.\n\nVerification failure reason:\n%s\n\nOriginal task:\n%s",
		reason,
		originalTask.Content,
	)
	return repairTask, nil
}

// RegisterRetryTaskInState registers a retry task in the command state.
//
// The retry task is wired into command state the same way the planner-side
// retry path (internal/plan/retry.go) does so that the daemon's queue scan
// recognises it as a known task:
//
//   - predecessorTaskID is replaced by retryTask.ID in RequiredTaskIDs /
//     OptionalTaskIDs / SystemCommitTaskID, preserving the original
//     membership semantics (a retry of a required task stays required).
//   - retryTask.ID is appended to the phase that contained the predecessor;
//     if that phase had already terminated as `failed`, it is reopened to
//     `active` so the retry is allowed to run.
//   - TaskDependencies, RetryLineage, and TaskStates are populated for the
//     new ID.
//
// Without these updates state_reader.isKnownTaskID would reject the retry
// every scan, producing a `task_ready_state_update_failed ... task not
// found` log loop at queue_scan_collect.markTaskReady (the original cause of
// the orphan retry stuck in the worker queue).
func (h *TaskRetryHandler) RegisterRetryTaskInState(retryTask *model.Task, predecessorTaskID, commandID string) error {
	stateLockKey := fmt.Sprintf("state:%s", commandID)
	h.lockMap.Lock(stateLockKey)
	defer h.lockMap.Unlock(stateLockKey)

	statePath := filepath.Join(h.maestroDir, "state", "commands", commandID+".yaml")
	if err := updateYAMLFile(statePath, func(state *model.CommandState) error {
		now := h.clock.Now().UTC().Format(time.RFC3339)

		// PhaseConstraints.MaxTasks is intentionally NOT enforced here. The
		// constraint applies to Planner's explicit task list at plan submit /
		// add-task time (see internal/plan/validate.go); enforcing it again
		// during retry/repair would turn recovery into a structural failure
		// mode (e.g. a max_tasks=3 phase has no headroom for a fourth slot
		// when verify fails). Retry depth is already bounded by
		// Retry.TaskExecution.MaxRetries and per-task DefinitionOfAbort
		// .MaxRepairCount.
		if !model.WireRetryTaskIntoState(state, retryTask.ID, predecessorTaskID, now) {
			h.log(LogLevelWarn,
				"retry_task_predecessor_unmembered command=%s retry=%s predecessor=%q (adding retry to optional_task_ids as fallback)",
				commandID, retryTask.ID, predecessorTaskID)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("update state file: %w", err)
	}

	h.log(LogLevelInfo, "retry_task_registered task=%s predecessor=%s command=%s",
		retryTask.ID, predecessorTaskID, commandID)
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
	queuePath := taskQueuePath(h.maestroDir, workerID)

	if err := updateYAMLFile(queuePath, func(queue *model.TaskQueue) error {
		if queue.SchemaVersion == 0 {
			queue.SchemaVersion = 1
			queue.FileType = "queue_task"
		}
		queue.Tasks = append(queue.Tasks, *task)
		return nil
	}); err != nil {
		return fmt.Errorf("update queue: %w", err)
	}

	h.log(LogLevelInfo, "retry_task_added task=%s worker=%s attempt=%d",
		task.ID, workerID, task.Attempts)

	return nil
}

// RetryTaskAtomically performs the complete retry task registration as a single
// logical operation: register in command state, add to worker queue, and on
// queue failure attempt rollback before falling back to reconciler recovery.
//
// predecessorTaskID identifies the just-failed task whose membership /
// dependencies the retry should inherit. It is threaded into both
// RegisterRetryTaskInState and rollbackRetryTaskFromState so that the
// required/optional/phase rewiring can be reversed exactly on rollback.
//
// Error handling strategy (C-A7):
//
//	(a) Register retry task in state, then immediately attempt queue add.
//	(b) On queue failure, rollback the state entry (delete the retry task).
//	(c) If rollback also fails, mark as RetryEnqueueFailed for R1 reconciler.
//
// This minimises the window where an orphaned state entry exists without
// a corresponding queue entry.
func (h *TaskRetryHandler) RetryTaskAtomically(retryTask *model.Task, predecessorTaskID, commandID, workerID string) error {
	// Step 1: Register in state
	if err := h.RegisterRetryTaskInState(retryTask, predecessorTaskID, commandID); err != nil {
		return fmt.Errorf("register retry task in state: %w", err)
	}

	// Step 2: Add to queue
	if err := h.AddRetryTaskToQueue(retryTask, workerID); err != nil {
		h.log(LogLevelError, "retry_atomic_queue_failed task=%s worker=%s command=%s error=%v",
			retryTask.ID, workerID, commandID, err)

		// Step 3a: Attempt to rollback state entry
		if rollbackErr := h.rollbackRetryTaskFromState(retryTask.ID, predecessorTaskID, commandID); rollbackErr != nil {
			// Step 3b: Rollback failed — mark for R1 reconciler recovery
			h.log(LogLevelError, "retry_atomic_rollback_failed task=%s command=%s error=%v",
				retryTask.ID, commandID, rollbackErr)
			if markErr := h.MarkRetryEnqueueFailed(retryTask.ID, workerID, commandID); markErr != nil {
				h.log(LogLevelError, "retry_atomic_mark_failed task=%s command=%s error=%v",
					retryTask.ID, commandID, markErr)
				return errors.Join(
					fmt.Errorf("queue add failed: %w", err),
					fmt.Errorf("rollback failed: %w", rollbackErr),
					fmt.Errorf("marking enqueue-failed also failed: %w", markErr),
				)
			}
			return fmt.Errorf("queue add failed (rollback failed, marked for reconciler recovery): %w", err)
		}

		h.log(LogLevelInfo, "retry_atomic_state_rolled_back task=%s command=%s",
			retryTask.ID, commandID)
		return fmt.Errorf("queue add failed (state rolled back): %w", err)
	}

	h.log(LogLevelInfo, "retry_task_atomic_completed task=%s worker=%s command=%s",
		retryTask.ID, workerID, commandID)
	return nil
}

// rollbackRetryTaskFromState removes a retry task entry from the command
// state, undoing the additions made by RegisterRetryTaskInState. Used when
// queue add fails after state registration to restore consistency.
//
// The phase status is intentionally not reverted from active back to failed:
// the next CheckPhaseTransitions scan re-evaluates the phase from its current
// task statuses (active → failed is a permitted transition), so leaving the
// phase active for one extra scan is safe and avoids racing the transition
// validator. ReopenedAt is cleared so audit reads don't see a phantom reopen.
func (h *TaskRetryHandler) rollbackRetryTaskFromState(retryTaskID, predecessorTaskID, commandID string) error {
	stateLockKey := fmt.Sprintf("state:%s", commandID)
	h.lockMap.Lock(stateLockKey)
	defer h.lockMap.Unlock(stateLockKey)

	statePath := filepath.Join(h.maestroDir, "state", "commands", commandID+".yaml")
	if err := updateYAMLFile(statePath, func(state *model.CommandState) error {
		model.UnwireRetryTaskFromState(state, retryTaskID, predecessorTaskID,
			h.clock.Now().UTC().Format(time.RFC3339))
		return nil
	}); err != nil {
		return fmt.Errorf("rollback state file: %w", err)
	}

	return nil
}

// MarkRetryEnqueueFailed marks a retry task in the command state as having failed
// to enqueue. This allows the R1 reconciler to detect the orphaned task and
// either re-enqueue it or transition it to dead_letter.
func (h *TaskRetryHandler) MarkRetryEnqueueFailed(taskID, workerID, commandID string) error {
	stateLockKey := fmt.Sprintf("state:%s", commandID)
	h.lockMap.Lock(stateLockKey)
	defer h.lockMap.Unlock(stateLockKey)

	statePath := filepath.Join(h.maestroDir, "state", "commands", commandID+".yaml")
	if err := updateYAMLFile(statePath, func(state *model.CommandState) error {
		if state.RetryEnqueueFailed == nil {
			state.RetryEnqueueFailed = make(map[string]string)
		}
		state.RetryEnqueueFailed[taskID] = workerID
		state.UpdatedAt = h.clock.Now().UTC().Format(time.RFC3339)
		return nil
	}); err != nil {
		return fmt.Errorf("update state file: %w", err)
	}

	h.log(LogLevelWarn, "retry_enqueue_failed_marked task=%s worker=%s command=%s "+
		"(R1 reconciler should detect and re-enqueue or mark dead_letter)", taskID, workerID, commandID)
	return nil
}

// partitionStrings splits ss into two groups based on pred.
// Elements matching pred go to trueGroup; the rest go to falseGroup.
func partitionStrings(ss []string, pred func(string) bool) (falseGroup, trueGroup []string) {
	for _, s := range ss {
		if pred(s) {
			trueGroup = append(trueGroup, s)
		} else {
			falseGroup = append(falseGroup, s)
		}
	}
	return
}

// withCappedRetryMeta returns a new constraints slice with retry metadata entries
// capped at maxRetryMeta (keeping the newest). Non-retry constraints are preserved.
func withCappedRetryMeta(constraints []string, newMeta string) []string {
	plain, meta := partitionStrings(constraints, func(c string) bool {
		return strings.HasPrefix(c, retryMetaPrefix)
	})

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

func (h *TaskRetryHandler) log(level LogLevel, format string, args ...any) {
	h.dl.Logf(level, format, args...)
}
