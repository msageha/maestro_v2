package plan

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// stateSaveTimeout is the maximum duration allowed for a SaveState call
// before it is considered hung and an error is returned.
const stateSaveTimeout = 30 * time.Second

// saveStateWithContext runs saveFn in a goroutine and returns its result,
// or returns an error if ctx is cancelled/expired before saveFn completes.
// This prevents a hung filesystem from blocking the caller indefinitely
// while holding locks.
//
// When ctx fires before saveFn returns, saveFn keeps running. AtomicWrite
// cannot be cancelled mid-rename, so any persistence side effect from a late
// completion is still observable on disk. We can't prevent that here, but we
// flip an atomic flag so the goroutine logs a `state_save_late_completion`
// warning (vs silently leaking a goroutine and racing with rollback writes).
// Callers MUST treat stateSaveTimeoutError as "state on disk is now
// ambiguous" and recover by re-saving the rolled-back snapshot after the late
// completion has been observed.
func saveStateWithContext(ctx context.Context, saveFn func() error) error {
	var timedOut atomic.Bool
	done := make(chan error, 1)
	go func() {
		err := saveFn()
		if timedOut.Load() {
			slog.Warn("state save completed after context timeout (late write)",
				"event", "state_save_late_completion",
				"error", err,
			)
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		timedOut.Store(true)
		return &stateSaveTimeoutError{cause: ctx.Err(), done: done}
	}
}

// stateSaveTimeoutError carries the late completion handle for a timed-out
// SaveState call. errors.Is still matches the wrapped context error.
type stateSaveTimeoutError struct {
	cause error
	done  <-chan error
}

func (e *stateSaveTimeoutError) Error() string {
	return fmt.Sprintf("state save timed out: %v", e.cause)
}

func (e *stateSaveTimeoutError) Unwrap() error {
	return e.cause
}

func (e *stateSaveTimeoutError) wait() error {
	return <-e.done
}

// logSuppressor limits the rate of repeated log messages within a time window.
// It tracks emissions per key and suppresses after a burst threshold,
// reporting the count of suppressed entries when the next emission is allowed.
type logSuppressor struct {
	mu     sync.Mutex
	window time.Duration
	burst  int
	counts map[string]*suppressEntry
	// nowFn lets tests inject a fake clock so window-expiry behaviour can be
	// asserted without `time.Sleep`. Production paths use newLogSuppressor
	// which leaves nowFn nil; allow() then defaults to time.Now.
	nowFn func() time.Time
}

type suppressEntry struct {
	windowStart time.Time
	emitted     int
	suppressed  int
}

func newLogSuppressor(window time.Duration, burst int) *logSuppressor {
	return &logSuppressor{
		window: window,
		burst:  burst,
		counts: make(map[string]*suppressEntry),
	}
}

// newLogSuppressorWithClock is the test-only constructor that injects a
// custom now() function. Mirrors newLogSuppressor in every other respect.
//
// linters' unused checker flags this as dead because it has no
// production caller, but retry_test.go relies on it for deterministic
// time-windowed assertions. Keep it as test infrastructure.
//
//nolint:unused // test-only helper consumed by retry_test.go
func newLogSuppressorWithClock(window time.Duration, burst int, nowFn func() time.Time) *logSuppressor {
	s := newLogSuppressor(window, burst)
	s.nowFn = nowFn
	return s
}

func (s *logSuppressor) allow(key string) (emit bool, suppressed int) {
	var now time.Time
	if s.nowFn != nil {
		now = s.nowFn()
	} else {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.counts[key]
	if !ok || now.Sub(e.windowStart) >= s.window {
		prev := 0
		if e != nil {
			prev = e.suppressed
		}
		s.counts[key] = &suppressEntry{windowStart: now, emitted: 1}
		return true, prev
	}
	if e.emitted < s.burst {
		e.emitted++
		return true, 0
	}
	e.suppressed++
	return false, 0
}

// restoreLogSuppressor rate-limits "state restore failed" error logs to prevent
// log spam during cascade failures where multiple restore attempts fail.
var restoreLogSuppressor = newLogSuppressor(10*time.Second, 3)

// restoreStateOrLog attempts to restore state from origBytes and logs on failure
// with rate limiting and unique operation context for diagnostics.
func restoreStateOrLog(state *model.CommandState, origBytes []byte, op string) {
	rsErr := restoreState(state, origBytes)
	if rsErr == nil {
		return
	}
	emit, suppressed := restoreLogSuppressor.allow(op)
	if suppressed > 0 {
		slog.Warn("suppressed repeated state restore errors",
			"event", "state_restore_failed",
			"op", op,
			"suppressed_count", suppressed,
		)
	}
	if emit {
		slog.Error("state restore failed",
			"event", "state_restore_failed",
			"op", op,
			"error", rsErr,
		)
	}
}

// RetryOptions holds the configuration for retrying a failed task.
type RetryOptions struct {
	CommandID          string
	RetryOf            string
	Purpose            string
	Content            string
	AcceptanceCriteria string
	DefinitionOfDone   []string
	Constraints        []string
	BlockedBy          []string // task IDs (not names)
	BloomLevel         int
	ToolsHint          []string
	PersonaHint        string
	SkillRefs          []string
	ExpectedPaths     []string
	DefinitionOfAbort *model.DefinitionOfAbort
	MaestroDir        string
	Config            model.Config
	LockMap           *lock.MutexMap
	ModelSelector     ModelSelector // optional: adaptive model selection
}

// RetryResult contains the outcome of a task retry including any cascade-recovered tasks.
type RetryResult struct {
	TaskID           string                 `json:"task_id"`
	Worker           string                 `json:"worker"`
	Model            string                 `json:"model"`
	Replaced         string                 `json:"replaced"`
	CascadeRecovered []CascadeRecoveredTask `json:"cascade_recovered,omitempty"`
}

// CascadeRecoveredTask describes a downstream task that was automatically recovered during a retry.
type CascadeRecoveredTask struct {
	TaskID   string `json:"task_id"`
	Worker   string `json:"worker"`
	Model    string `json:"model"`
	Replaced string `json:"replaced"`
}

// AddRetryTask creates a replacement task for a failed task, rewires dependencies, and performs cascade recovery.
func AddRetryTask(opts RetryOptions) (*RetryResult, error) {
	if opts.LockMap == nil {
		return nil, ErrLockMapRequired
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	assignment, workerStates, err := assignWorkerForRetry(opts)
	if err != nil {
		return nil, err
	}

	newTaskID, err := model.NewTaskID(model.TaskIDCallerPlannerRetry)
	if err != nil {
		return nil, fmt.Errorf("generate task ID: %w", err)
	}
	now := nowUTC()
	unlockQueues := lockWorkerQueues(opts.LockMap, allConfiguredWorkerIDs(opts.Config.Agents.Workers))
	defer unlockQueues()
	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	rc, err := validateRetryRequest(sm, opts)
	if err != nil {
		return nil, fmt.Errorf("validate retry: %w", err)
	}

	origTaskCache, err := loadOriginalTasksFromQueue(opts.MaestroDir, opts.CommandID, nil)
	if err != nil {
		return nil, fmt.Errorf("load original tasks from queue: %w", err)
	}

	cascadeRecovered, origStateBytes, err := applyRetryStateChanges(
		rc.state, opts, newTaskID, rc, now, workerStates, origTaskCache,
	)
	if err != nil {
		return nil, fmt.Errorf("apply retry state changes: %w", err)
	}

	primaryTask := buildPrimaryRetryTask(opts, newTaskID, rc.blockedBy, assignment.WorkerID)
	queueLockedOpts := opts
	queueLockedOpts.LockMap = nil
	if err := writeAndCommitRetryQueue(
		sm, queueLockedOpts, rc.state, primaryTask, cascadeRecovered, origTaskCache, origStateBytes, now,
	); err != nil {
		return nil, err
	}

	return &RetryResult{
		TaskID:           newTaskID,
		Worker:           assignment.WorkerID,
		Model:            assignment.Model,
		Replaced:         opts.RetryOf,
		CascadeRecovered: cascadeRecovered,
	}, nil
}

// assignWorkerForRetry builds worker states and assigns a worker for the retry task.
// The returned workerStates reflect the primary assignment's PendingCount increment
// so that downstream consumers (e.g., cascade recovery) see a consistent snapshot.
func assignWorkerForRetry(opts RetryOptions) (WorkerAssignment, []WorkerState, error) {
	workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
	if err != nil {
		return WorkerAssignment{}, nil, fmt.Errorf("build worker states: %w", err)
	}
	assignReqs := []TaskAssignmentRequest{{Name: "__retry", BloomLevel: opts.BloomLevel}}
	assignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, workerStates, assignReqs, WithModelSelector(opts.ModelSelector))
	if err != nil {
		return WorkerAssignment{}, nil, fmt.Errorf("worker assignment: %w", err)
	}

	// Reflect the primary assignment in workerStates so that cascade recovery
	// sees the correct PendingCount and avoids overloading the same worker.
	for i := range workerStates {
		if workerStates[i].WorkerID == assignments[0].WorkerID {
			workerStates[i].PendingCount++
			break
		}
	}

	return assignments[0], workerStates, nil
}

// buildPrimaryRetryTask constructs the retryQueueTask for the primary retry task.
func buildPrimaryRetryTask(opts RetryOptions, taskID string, blockedBy []string, workerID string) retryQueueTask {
	return retryQueueTask{
		taskID:             taskID,
		commandID:          opts.CommandID,
		purpose:            opts.Purpose,
		content:            opts.Content,
		acceptanceCriteria: opts.AcceptanceCriteria,
		definitionOfDone:   opts.DefinitionOfDone,
		constraints:        opts.Constraints,
		blockedBy:          blockedBy,
		bloomLevel:         opts.BloomLevel,
		toolsHint:          opts.ToolsHint,
		personaHint:        opts.PersonaHint,
		skillRefs:          opts.SkillRefs,
		expectedPaths:      opts.ExpectedPaths,
		definitionOfAbort:  opts.DefinitionOfAbort,
		workerID:           workerID,
		operationType:      model.OperationTypeRepair,
	}
}

// writeAndCommitRetryQueue writes all queue entries (primary + cascade), cancels the original task,
// and saves state. On any failure, it performs a full rollback of queue entries and state.
func writeAndCommitRetryQueue(
	sm *StateManager, opts RetryOptions, state *model.CommandState,
	primaryTask retryQueueTask, cascadeRecovered []CascadeRecoveredTask,
	origTaskCache map[string]model.Task, origStateBytes []byte, now string,
) error {
	writtenTasks := make([]retryQueueTask, 0, 1+len(cascadeRecovered))

	if err := writeRetryQueueEntry(opts.MaestroDir, primaryTask, now, opts.LockMap); err != nil {
		restoreStateOrLog(state, origStateBytes, "write_primary_queue_entry")
		return fmt.Errorf("write queue entry for %s: %w", primaryTask.taskID, err)
	}
	writtenTasks = append(writtenTasks, primaryTask)

	for _, cr := range cascadeRecovered {
		crTask := buildCascadeQueueTask(cr, opts, state, origTaskCache)
		if err := writeRetryQueueEntry(opts.MaestroDir, crTask, now, opts.LockMap); err != nil {
			rollbackRetryQueueEntries(opts.MaestroDir, writtenTasks, opts.LockMap)
			restoreStateOrLog(state, origStateBytes, "write_cascade_queue_entry")
			return fmt.Errorf("write queue entry for cascade %s: %w", cr.TaskID, err)
		}
		writtenTasks = append(writtenTasks, crTask)
	}

	if err := updateOriginalTaskInQueue(opts.MaestroDir, opts.RetryOf, opts.CommandID, model.StatusCancelled, now, opts.LockMap); err != nil {
		rollbackRetryQueueEntries(opts.MaestroDir, writtenTasks, opts.LockMap)
		restoreStateOrLog(state, origStateBytes, "cancel_original_task")
		return fmt.Errorf("cancel original task in queue: %w", err)
	}

	saveCtx, saveCancel := context.WithTimeout(context.Background(), stateSaveTimeout)
	defer saveCancel()
	attemptedStateBytes, copyErr := copyState(state)
	if copyErr != nil {
		rollbackRetryQueueEntries(opts.MaestroDir, writtenTasks, opts.LockMap)
		restoreStateOrLog(state, origStateBytes, "copy_attempted_state")
		return fmt.Errorf("copy attempted state for rollback guard: %w", copyErr)
	}
	if err := saveStateWithContext(saveCtx, func() error { return sm.SaveState(state) }); err != nil {
		if restoreErr := updateOriginalTaskInQueue(opts.MaestroDir, opts.RetryOf, opts.CommandID, model.StatusFailed, now, opts.LockMap); restoreErr != nil {
			slog.Warn("failed to restore original task queue status", "task_id", opts.RetryOf, "error", restoreErr)
		}
		rollbackRetryQueueEntries(opts.MaestroDir, writtenTasks, opts.LockMap)
		restoreStateOrLog(state, origStateBytes, "save_state")
		var timeoutErr *stateSaveTimeoutError
		if errors.As(err, &timeoutErr) {
			scheduleRollbackResaveAfterLateStateSave(sm, opts.CommandID, origStateBytes, attemptedStateBytes, timeoutErr)
		}
		return fmt.Errorf("save state: %w", err)
	}

	return nil
}

// scheduleRollbackResaveAfterLateStateSave waits for a timed-out SaveState to
// finish, then repairs the on-disk state if that late save actually installed
// the attempted snapshot. The current-state guard prevents this recovery path
// from clobbering a newer legitimate state transition that landed after the
// caller returned its timeout error.
func scheduleRollbackResaveAfterLateStateSave(
	sm *StateManager,
	commandID string,
	origStateBytes, attemptedStateBytes []byte,
	timeoutErr *stateSaveTimeoutError,
) {
	origStateBytes = append([]byte(nil), origStateBytes...)
	attemptedStateBytes = append([]byte(nil), attemptedStateBytes...)
	go func() {
		lateErr := timeoutErr.wait()
		if outcome, err := resaveRollbackSnapshotIfCurrentMatches(sm, commandID, origStateBytes, attemptedStateBytes); err != nil {
			slog.Error("state rollback resave after late completion failed",
				"event", "state_save_rollback_resave_failed",
				"command_id", commandID,
				"late_save_error", lateErr,
				"error", err,
			)
		} else {
			slog.Warn("state rollback resave after late completion finished",
				"event", "state_save_rollback_resave_finished",
				"command_id", commandID,
				"late_save_error", lateErr,
				"outcome", outcome,
			)
		}
	}()
}

func resaveRollbackSnapshotIfCurrentMatches(sm *StateManager, commandID string, origStateBytes, attemptedStateBytes []byte) (string, error) {
	origState, err := decodeStateSnapshot(origStateBytes)
	if err != nil {
		return "", fmt.Errorf("decode original snapshot: %w", err)
	}
	attemptedState, err := decodeStateSnapshot(attemptedStateBytes)
	if err != nil {
		return "", fmt.Errorf("decode attempted snapshot: %w", err)
	}

	sm.LockCommand(commandID)
	defer sm.UnlockCommand(commandID)

	current, err := sm.LoadState(commandID)
	if err != nil {
		return "", fmt.Errorf("load current state: %w", err)
	}
	switch {
	case reflect.DeepEqual(*current, *attemptedState):
		if err := sm.SaveState(origState); err != nil {
			return "", fmt.Errorf("save rollback snapshot: %w", err)
		}
		return "resaved_rollback_snapshot", nil
	case reflect.DeepEqual(*current, *origState):
		return "already_rolled_back", nil
	default:
		return "skipped_current_state_changed", nil
	}
}

func decodeStateSnapshot(data []byte) (*model.CommandState, error) {
	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// retryContext holds validated state and metadata for a retry operation.
type retryContext struct {
	state     *model.CommandState
	phase     *model.Phase
	phaseIdx  int
	blockedBy []string
}

// validateRetryRequest loads and validates state, task status, phase membership, and blocked_by references.
func validateRetryRequest(sm *StateManager, opts RetryOptions) (*retryContext, error) {
	if opts.BloomLevel < BloomLevelMin || opts.BloomLevel > BloomLevelMax {
		return nil, &planValidationError{Msg: fmt.Sprintf("bloom_level must be between %d and %d, got %d", BloomLevelMin, BloomLevelMax, opts.BloomLevel)}
	}

	if err := validateInjectedSchemaFields(opts.ExpectedPaths, opts.DefinitionOfAbort, opts.DefinitionOfDone); err != nil {
		return nil, err
	}

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	if state.PlanStatus != model.PlanStatusSealed {
		return nil, &planValidationError{Msg: fmt.Sprintf("plan_status must be sealed, got %s", state.PlanStatus)}
	}

	if err := ValidateNotCancelled(state); err != nil {
		return nil, fmt.Errorf("validate not cancelled: %w", err)
	}

	retryOfStatus, ok := state.TaskStates[opts.RetryOf]
	if !ok {
		return nil, &planValidationError{Msg: fmt.Sprintf("task %s not found in state", opts.RetryOf)}
	}
	// Accepted retry-of statuses: failed, paused_for_replan, repair_pending,
	// and a constrained subset of cancelled.
	//
	// repair_pending: the daemon's auto-repair sequence can leave a task
	// at repair_pending without successfully enqueuing the repair task
	// (e.g. RetryTaskAtomically hit ErrPhaseMaxTasksExceeded, or the
	// worker queue write raced with a scan). Planner-side add_retry_task
	// is the operator-visible escape hatch for those cases. The
	// replaceTaskMembership / phase.TaskIDs append below supersede any
	// daemon-side pending repair, so double-retry is not possible: state
	// transitions to the new retry's lifecycle.
	//
	// cancelled handling: a daemon-side verify-repair injects a repair
	// task without going through AddRetryTask, so cascade-cancelled
	// downstream tasks (CancelledReasons[id] =
	// "blocked_dependency_terminal:<dep>") stay cancelled even after the
	// upstream lineage completes. If the Planner chooses to *re-execute*
	// such a task explicitly, AddRetryTask is the natural entry point.
	// Allow the call when the cancellation reason is either empty or a
	// cascade marker; refuse for operator-initiated cancellation
	// (command_cancel_requested) and for tasks already superseded by
	// another retry/repair (superseded_by_retry,
	// superseded_by_verify_repair).
	cancelledReason := state.CancelledReasons[opts.RetryOf]
	switch retryOfStatus {
	case model.StatusFailed, model.StatusPausedForReplan, model.StatusRepairPending:
		// allowed
	case model.StatusCancelled:
		if cancelledReason == "command_cancel_requested" ||
			strings.HasPrefix(cancelledReason, "superseded_by_retry") ||
			strings.HasPrefix(cancelledReason, "superseded_by_verify_repair") {
			return nil, &planValidationError{Msg: fmt.Sprintf(
				"retry-of task %s is cancelled with reason %q (operator-cancel or already-superseded; not eligible for retry)",
				opts.RetryOf, cancelledReason)}
		}
		// fallthrough: cascade-cancelled or unmarked cancellation — allowed.
	default:
		return nil, &planValidationError{Msg: fmt.Sprintf(
			"retry-of task %s must be failed, paused_for_replan, repair_pending, or cancelled (cascade), got %s",
			opts.RetryOf, retryOfStatus)}
	}

	// Active-retry uniqueness: at most one *active* retry chain may be
	// outstanding for a given predecessor. Pre-2026-05-06 this guard was
	// missing and a race between daemon auto-retry and Planner
	// `plan add-retry-task` could enqueue two replacement tasks for the
	// same failed predecessor — both inheriting the original's
	// dependencies — which then competed for the same downstream
	// blocked-by edges and surfaced as `result_write
	// invalid_state_transition planned → completed` when the second
	// retry's worker reported in (Report 2026-05-06 P1-2).
	//
	// Walk RetryLineage looking for any descendant whose predecessor is
	// opts.RetryOf and whose state is non-terminal (planned, ready,
	// dispatched, running, verify_pending, repair_pending). If one
	// exists, refuse the new retry — the operator should let the
	// existing retry run to completion or cancel it explicitly first.
	for descendantID, predecessor := range state.RetryLineage {
		if predecessor != opts.RetryOf {
			continue
		}
		descendantStatus, ok := state.TaskStates[descendantID]
		if !ok {
			continue
		}
		if model.IsTerminal(descendantStatus) {
			// A terminal descendant means the prior retry chain has
			// resolved (failed / cancelled / completed) — chaining a
			// new retry off the original is acceptable. RetryLineage
			// keeps the historical record; the new entry simply joins
			// the chain at opts.RetryOf again.
			continue
		}
		return nil, &planValidationError{Msg: fmt.Sprintf(
			"retry-of task %s already has an active retry %s (status=%s); only one outstanding retry per predecessor is allowed — wait for it to terminate or cancel it explicitly first",
			opts.RetryOf, descendantID, descendantStatus)}
	}

	// Find phase membership
	phase, phaseIdx := findPhaseForTask(state, opts.RetryOf)
	if phase != nil {
		if phase.Status != model.PhaseStatusActive && phase.Status != model.PhaseStatusFailed {
			return nil, &planValidationError{Msg: fmt.Sprintf("phase %q status must be active or failed, got %s",
				phase.Name, phase.Status)}
		}
	}

	// Resolve blocked_by: default to original task's dependencies
	blockedBy := opts.BlockedBy
	if len(blockedBy) == 0 {
		if deps, ok := state.TaskDependencies[opts.RetryOf]; ok {
			var err error
			blockedBy, err = resolveBlockedByViaLineage(deps, state.RetryLineage)
			if err != nil {
				return nil, fmt.Errorf("resolve blocked_by via lineage: %w", err)
			}
			// Verify resolved dependencies still exist in task states
			for _, dep := range blockedBy {
				if _, ok := state.TaskStates[dep]; !ok {
					return nil, &planValidationError{Msg: fmt.Sprintf("resolved dependency %s (via lineage) not found in command state", dep)}
				}
			}
		}
	}

	// Validate blocked_by references
	if len(blockedBy) > 0 {
		if err := validateRetryBlockedBy(state, blockedBy, phase); err != nil {
			return nil, fmt.Errorf("validate blocked_by: %w", err)
		}
	}

	return &retryContext{
		state:     state,
		phase:     phase,
		phaseIdx:  phaseIdx,
		blockedBy: blockedBy,
	}, nil
}

// validateRetryBlockedBy checks that blocked_by references are valid within the phase or command scope.
func validateRetryBlockedBy(state *model.CommandState, blockedBy []string, phase *model.Phase) error {
	if phase != nil {
		// Phase-scoped: blocked_by must be within same phase (or system commit)
		phaseTaskSet := make(map[string]bool)
		for _, tid := range phase.TaskIDs {
			phaseTaskSet[tid] = true
		}
		for _, dep := range blockedBy {
			if !phaseTaskSet[dep] {
				if state.SystemCommitTaskID == nil || dep != *state.SystemCommitTaskID {
					return &planValidationError{Msg: fmt.Sprintf("blocked_by task %s is not in phase %q", dep, phase.Name)}
				}
			}
		}
	} else {
		// No phase: blocked_by must exist in command's task states
		for _, dep := range blockedBy {
			if _, ok := state.TaskStates[dep]; !ok {
				return &planValidationError{Msg: fmt.Sprintf("blocked_by task %s not found in command state", dep)}
			}
		}
	}
	return nil
}

// applyRetryStateChanges modifies state for the retry task and performs cascade recovery.
// Returns the cascade recovered tasks and the original state bytes for rollback.
func applyRetryStateChanges(
	state *model.CommandState, opts RetryOptions, newTaskID string,
	rc *retryContext, now string,
	workerStates []WorkerState, origTaskCache map[string]model.Task,
) ([]CascadeRecoveredTask, []byte, error) {

	origStateBytes, err := copyState(state)
	if err != nil {
		return nil, nil, fmt.Errorf("copy state for rollback: %w", err)
	}

	// Replace in required/optional task IDs
	replaceInRequiredOrOptional(state, opts.RetryOf, newTaskID)

	// Record retry lineage
	state.RetryLineage[newTaskID] = opts.RetryOf

	// Rewrite dependencies
	rewriteDependencies(state, opts.RetryOf, newTaskID)

	// Set new task state. §2.1: retry tasks enter the lifecycle at `planned`
	// just like fresh tasks; the retry distinction is captured in
	// state.RetryLineage above, not by initialising at a different state.
	state.TaskStates[newTaskID] = model.StatusPlanned
	state.TaskDependencies[newTaskID] = rc.blockedBy
	if state.TaskStates[opts.RetryOf] == model.StatusPausedForReplan {
		state.TaskStates[opts.RetryOf] = model.StatusCancelled
		if state.CancelledReasons == nil {
			state.CancelledReasons = make(map[string]string)
		}
		state.CancelledReasons[opts.RetryOf] = "superseded_by_retry:" + newTaskID
	}

	// Re-validate DAG after dependency rewriting for the primary retry task.
	if err := ValidateTaskDAGAfterMutation(state); err != nil {
		restoreStateOrLog(state, origStateBytes, "post_rewrite_dag_validation")
		return nil, nil, fmt.Errorf("post-rewrite DAG validation: %w", err)
	}

	// Add to phase
	if rc.phase != nil {
		state.Phases[rc.phaseIdx].TaskIDs = append(state.Phases[rc.phaseIdx].TaskIDs, newTaskID)

		// Reopen phase if failed
		if rc.phase.Status == model.PhaseStatusFailed {
			if err := reopenPhase(state, rc.phaseIdx, now); err != nil {
				restoreStateOrLog(state, origStateBytes, "reopen_phase")
				return nil, nil, fmt.Errorf("reopen phase: %w", err)
			}
		}
	}

	// Cascade recovery
	cascadeRecovered, err := cascadeRecover(
		state, opts.RetryOf, newTaskID,
		opts.Config.Agents.Workers, opts.Config.Limits, workerStates, origTaskCache,
		opts.ModelSelector,
	)
	if err != nil {
		restoreStateOrLog(state, origStateBytes, "cascade_recovery")
		return nil, nil, fmt.Errorf("cascade recovery: %w", err)
	}

	// Post-recovery DAG validation (covers both cycle detection and cross-phase refs).
	if err := ValidateTaskDAGAfterMutation(state); err != nil {
		restoreStateOrLog(state, origStateBytes, "post_recovery_dag_validation")
		return nil, nil, fmt.Errorf("post-recovery DAG validation: %w", err)
	}

	// Purge stale map entries for superseded tasks to prevent unbounded growth.
	purgeSupersededRetryEntries(state)

	state.UpdatedAt = now
	return cascadeRecovered, origStateBytes, nil
}

// buildCascadeQueueTask constructs a retryQueueTask for a cascade-recovered task,
// inheriting content from the original task when available.
func buildCascadeQueueTask(cr CascadeRecoveredTask, opts RetryOptions, state *model.CommandState, origTaskCache map[string]model.Task) retryQueueTask {
	purpose := "cascade recovery of " + cr.Replaced
	content := purpose
	acceptanceCriteria := purpose
	var definitionOfDone []string
	bloomLevel := opts.BloomLevel
	var constraints []string
	var toolsHint []string
	var personaHint string
	var skillRefs []string
	// REQUIREMENTS.md §S3-1: cascade-recovered tasks must also declare
	// expected_paths / definition_of_abort. Inherit from the original task when
	// available; fall back to the values supplied for the primary retry so the
	// replacement passes schema validation. The originals already passed
	// validation when they were submitted, so they are guaranteed to be
	// well-formed.
	expectedPaths := opts.ExpectedPaths
	definitionOfAbort := opts.DefinitionOfAbort

	if orig, ok := origTaskCache[cr.Replaced]; ok {
		purpose = orig.Purpose
		content = orig.Content
		acceptanceCriteria = orig.AcceptanceCriteria
		definitionOfDone = orig.DefinitionOfDone
		bloomLevel = orig.BloomLevel
		constraints = orig.Constraints
		toolsHint = orig.ToolsHint
		personaHint = orig.PersonaHint
		skillRefs = orig.SkillRefs
		if orig.ExpectedPaths != nil {
			expectedPaths = orig.ExpectedPaths
		}
		if orig.DefinitionOfAbort != nil {
			definitionOfAbort = orig.DefinitionOfAbort
		}
	}

	return retryQueueTask{
		taskID:             cr.TaskID,
		commandID:          opts.CommandID,
		purpose:            purpose,
		content:            content,
		acceptanceCriteria: acceptanceCriteria,
		definitionOfDone:   definitionOfDone,
		constraints:        constraints,
		blockedBy:          state.TaskDependencies[cr.TaskID],
		bloomLevel:         bloomLevel,
		toolsHint:          toolsHint,
		personaHint:        personaHint,
		skillRefs:          skillRefs,
		expectedPaths:      expectedPaths,
		definitionOfAbort:  definitionOfAbort,
		workerID:           cr.Worker,
		operationType:      model.OperationTypeRepair,
	}
}
