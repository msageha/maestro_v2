package plan

import (
	"context"
	"fmt"
	"log"
	"time"

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
func saveStateWithContext(ctx context.Context, saveFn func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- saveFn()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("state save timed out: %w", ctx.Err())
	}
}

// RetryOptions holds the configuration for retrying a failed task.
type RetryOptions struct {
	CommandID          string
	RetryOf            string
	Purpose            string
	Content            string
	AcceptanceCriteria string
	Constraints        []string
	BlockedBy          []string // task IDs (not names)
	BloomLevel         int
	ToolsHint          []string
	PersonaHint        string
	SkillRefs          []string
	MaestroDir         string
	Config             model.Config
	LockMap            *lock.MutexMap
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
	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	rc, err := validateRetryRequest(sm, opts)
	if err != nil {
		return nil, fmt.Errorf("validate retry: %w", err)
	}

	assignment, workerStates, err := assignWorkerForRetry(opts)
	if err != nil {
		return nil, err
	}

	newTaskID, err := model.NewTaskID(model.TaskIDCallerPlannerRetry)
	if err != nil {
		return nil, fmt.Errorf("generate task ID: %w", err)
	}
	now := nowUTC()
	origTaskCache, err := loadOriginalTasksFromQueue(opts.MaestroDir, opts.CommandID)
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
	if err := writeAndCommitRetryQueue(
		sm, opts, rc.state, primaryTask, cascadeRecovered, origTaskCache, origStateBytes, now,
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
func assignWorkerForRetry(opts RetryOptions) (WorkerAssignment, []WorkerState, error) {
	workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
	if err != nil {
		return WorkerAssignment{}, nil, fmt.Errorf("build worker states: %w", err)
	}
	assignReqs := []TaskAssignmentRequest{{Name: "__retry", BloomLevel: opts.BloomLevel}}
	assignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, workerStates, assignReqs)
	if err != nil {
		return WorkerAssignment{}, nil, fmt.Errorf("worker assignment: %w", err)
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
		constraints:        opts.Constraints,
		blockedBy:          blockedBy,
		bloomLevel:         opts.BloomLevel,
		toolsHint:          opts.ToolsHint,
		personaHint:        opts.PersonaHint,
		skillRefs:          opts.SkillRefs,
		workerID:           workerID,
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
		restoreState(state, origStateBytes)
		return fmt.Errorf("write queue entry for %s: %w", primaryTask.taskID, err)
	}
	writtenTasks = append(writtenTasks, primaryTask)

	for _, cr := range cascadeRecovered {
		crTask := buildCascadeQueueTask(cr, opts, state, origTaskCache)
		if err := writeRetryQueueEntry(opts.MaestroDir, crTask, now, opts.LockMap); err != nil {
			rollbackRetryQueueEntries(opts.MaestroDir, writtenTasks, opts.LockMap)
			restoreState(state, origStateBytes)
			return fmt.Errorf("write queue entry for cascade %s: %w", cr.TaskID, err)
		}
		writtenTasks = append(writtenTasks, crTask)
	}

	if err := cancelOriginalTaskInQueue(opts.MaestroDir, opts.RetryOf, opts.CommandID, now, opts.LockMap); err != nil {
		rollbackRetryQueueEntries(opts.MaestroDir, writtenTasks, opts.LockMap)
		restoreState(state, origStateBytes)
		return fmt.Errorf("cancel original task in queue: %w", err)
	}

	saveCtx, saveCancel := context.WithTimeout(context.Background(), stateSaveTimeout)
	defer saveCancel()
	if err := saveStateWithContext(saveCtx, func() error { return sm.SaveState(state) }); err != nil {
		if restoreErr := restoreOriginalTaskInQueue(opts.MaestroDir, opts.RetryOf, opts.CommandID, model.StatusFailed, now, opts.LockMap); restoreErr != nil {
			log.Printf("[WARN] failed to restore original task %s queue status: %v", opts.RetryOf, restoreErr)
		}
		rollbackRetryQueueEntries(opts.MaestroDir, writtenTasks, opts.LockMap)
		restoreState(state, origStateBytes)
		return fmt.Errorf("save state: %w", err)
	}

	return nil
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
	if retryOfStatus != model.StatusFailed {
		return nil, &planValidationError{Msg: fmt.Sprintf("retry-of task %s must be failed, got %s", opts.RetryOf, retryOfStatus)}
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
	if err := replaceInRequiredOrOptional(state, opts.RetryOf, newTaskID); err != nil {
		restoreState(state, origStateBytes)
		return nil, nil, fmt.Errorf("replace in required/optional: %w", err)
	}

	// Record retry lineage
	state.RetryLineage[newTaskID] = opts.RetryOf

	// Rewrite dependencies
	rewriteDependencies(state, opts.RetryOf, newTaskID)

	// Set new task state
	state.TaskStates[newTaskID] = model.StatusPending
	state.TaskDependencies[newTaskID] = rc.blockedBy

	// Add to phase
	if rc.phase != nil {
		state.Phases[rc.phaseIdx].TaskIDs = append(state.Phases[rc.phaseIdx].TaskIDs, newTaskID)

		// Reopen phase if failed
		if rc.phase.Status == model.PhaseStatusFailed {
			if err := reopenPhase(state, rc.phaseIdx, now); err != nil {
				restoreState(state, origStateBytes)
				return nil, nil, fmt.Errorf("reopen phase: %w", err)
			}
		}
	}

	// Cascade recovery
	cascadeRecovered, err := cascadeRecover(
		state, opts.RetryOf, newTaskID,
		opts.Config.Agents.Workers, opts.Config.Limits, workerStates, origTaskCache,
	)
	if err != nil {
		restoreState(state, origStateBytes)
		return nil, nil, fmt.Errorf("cascade recovery: %w", err)
	}

	// Post-recovery DAG validation
	allNames := make([]string, 0, len(state.TaskStates))
	for k := range state.TaskStates {
		allNames = append(allNames, k)
	}
	if _, err := ValidateTaskDAG(allNames, state.TaskDependencies); err != nil {
		restoreState(state, origStateBytes)
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
	bloomLevel := opts.BloomLevel
	var constraints []string
	var toolsHint []string
	var personaHint string
	var skillRefs []string

	if orig, ok := origTaskCache[cr.Replaced]; ok {
		purpose = orig.Purpose
		content = orig.Content
		acceptanceCriteria = orig.AcceptanceCriteria
		bloomLevel = orig.BloomLevel
		constraints = orig.Constraints
		toolsHint = orig.ToolsHint
		personaHint = orig.PersonaHint
		skillRefs = orig.SkillRefs
	}

	return retryQueueTask{
		taskID:             cr.TaskID,
		commandID:          opts.CommandID,
		purpose:            purpose,
		content:            content,
		acceptanceCriteria: acceptanceCriteria,
		constraints:        constraints,
		blockedBy:          state.TaskDependencies[cr.TaskID],
		bloomLevel:         bloomLevel,
		toolsHint:          toolsHint,
		personaHint:        personaHint,
		skillRefs:          skillRefs,
		workerID:           cr.Worker,
	}
}

