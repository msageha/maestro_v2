package plan

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// SubmitOptions holds the configuration for a plan submission operation.
type SubmitOptions struct {
	CommandID  string
	TasksFile  string // path or "-" for stdin
	TasksData  []byte // inline YAML data; takes precedence over TasksFile when non-empty
	PhaseName  string // non-empty for phase fill
	DryRun     bool
	MaestroDir    string
	Config        model.Config
	LockMap       *lock.MutexMap
	ModelSelector ModelSelector // optional: adaptive model selection
}

// SubmitResult contains the output of a successful plan submission.
type SubmitResult struct {
	Valid     bool                `json:"valid,omitempty"`
	CommandID string              `json:"command_id,omitempty"`
	Tasks     []SubmitTaskResult  `json:"tasks,omitempty"`
	Phases    []SubmitPhaseResult `json:"phases,omitempty"`
}

// SubmitTaskResult describes a single task's assignment after submission.
type SubmitTaskResult struct {
	Name   string `json:"name"`
	TaskID string `json:"task_id"`
	Worker string `json:"worker"`
	Model  string `json:"model"`
}

// SubmitPhaseResult describes a single phase's status after submission.
type SubmitPhaseResult struct {
	Name    string             `json:"name"`
	PhaseID string             `json:"phase_id"`
	Type    string             `json:"type"`
	Status  string             `json:"status"`
	Tasks   []SubmitTaskResult `json:"tasks,omitempty"`
}

// Submit validates and persists a plan, assigning tasks to workers and writing queue entries.
func Submit(opts SubmitOptions) (*SubmitResult, error) {
	var input *SubmitInput
	var err error
	if len(opts.TasksData) > 0 {
		input, err = parseInput(opts.TasksData)
	} else {
		input, err = readInput(opts.TasksFile)
	}
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}

	if len(input.Tasks) > 0 && len(input.Phases) > 0 {
		return nil, fmt.Errorf("tasks and phases are mutually exclusive")
	}
	if len(input.Tasks) == 0 && len(input.Phases) == 0 {
		return nil, fmt.Errorf("either tasks or phases must be specified")
	}

	if opts.PhaseName != "" {
		return submitPhaseFill(opts, *input)
	}
	return submitInitial(opts, *input)
}

// resolveAndAssignTasks generates task IDs, builds worker states, and assigns
// tasks to workers. This is the shared pipeline used by both submitInitialTasks
// and submitPhaseFill.
func resolveAndAssignTasks(opts SubmitOptions, tasks []TaskInput) (nameToID map[string]string, assignments []WorkerAssignment, assignMap map[string]WorkerAssignment, err error) {
	nameToID, err = resolveNames(tasks)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve names: %w", err)
	}

	workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build worker states: %w", err)
	}

	assignReqs := make([]TaskAssignmentRequest, 0, len(tasks))
	for _, t := range tasks {
		assignReqs = append(assignReqs, TaskAssignmentRequest{Name: t.Name, BloomLevel: t.BloomLevel})
	}

	assignments, err = AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, workerStates, assignReqs, WithModelSelector(opts.ModelSelector))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("worker assignment: %w", err)
	}

	assignMap = make(map[string]WorkerAssignment)
	for _, a := range assignments {
		assignMap[a.TaskName] = a
	}

	return nameToID, assignments, assignMap, nil
}

func submitInitial(opts SubmitOptions, input SubmitInput) (*SubmitResult, error) {
	if opts.LockMap == nil {
		return nil, ErrLockMapRequired
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	// QA-014: Acquire queue:planner lock BEFORE state lock to maintain the
	// global lock ordering (queue:planner → state:<cmd>) used by other code
	// paths (complete.go, reconciler.go, queue_write_handler.go).
	// Hold queue:planner through state lock acquisition to close the TOCTOU
	// window where cancellation could arrive between unlock and state lock.
	opts.LockMap.Lock("queue:planner")
	cancelErr := checkCommandNotCancelled(opts.MaestroDir, opts.CommandID)
	if cancelErr != nil {
		opts.LockMap.Unlock("queue:planner")
		return nil, cancelErr
	}

	// Lock command state while still holding queue:planner (canonical order)
	sm.LockCommand(opts.CommandID)
	// Release queue:planner now that state lock is held — ordering satisfied
	opts.LockMap.Unlock("queue:planner")
	defer sm.UnlockCommand(opts.CommandID)

	// TOCTOU fix: Acquire file-level lock for cross-process double-submit
	// prevention. The in-process MutexMap above protects within a single daemon;
	// this flock protects against concurrent CLI invocations submitting the
	// same command_id.
	stateFlock, flockErr := acquireStateFlock(opts.MaestroDir, opts.CommandID)
	if flockErr != nil {
		return nil, flockErr
	}
	defer releaseFlock(stateFlock)

	// Re-check cancellation under state lock to close any remaining race
	if cancelErr := checkCommandNotCancelled(opts.MaestroDir, opts.CommandID); cancelErr != nil {
		return nil, cancelErr
	}

	// Double submit prevention (now under both in-process lock and flock)
	if sm.StateExists(opts.CommandID) {
		return nil, fmt.Errorf("%w: state already exists for command %s", ErrDoubleSubmit, opts.CommandID)
	}

	// Route by input type
	if len(input.Phases) > 0 {
		return submitInitialPhases(opts, input.Phases, sm)
	}
	return submitInitialTasks(opts, input.Tasks, sm)
}

// rollbackStateAndQueue performs the common rollback sequence for initial submissions:
// remove partial queue entries, then delete the state file.
// Returns a combined error if any rollback step fails.
func rollbackStateAndQueue(sm stateStore, maestroDir string, commandID string, tasks []TaskInput, nameToID map[string]string, assignMap map[string]WorkerAssignment, lockMap *lock.MutexMap) error {
	var errs []error
	if queueErr := rollbackQueueEntries(maestroDir, tasks, nameToID, assignMap, lockMap); queueErr != nil {
		errs = append(errs, fmt.Errorf("rollback queue entries: %w", queueErr))
	}
	if delErr := sm.DeleteState(commandID); delErr != nil {
		errs = append(errs, fmt.Errorf("rollback delete state for command %s: %w", commandID, delErr))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// rollbackPhaseFillToAwaiting reverts a phase from filling to awaiting_fill and persists.
func rollbackPhaseFillToAwaiting(sm stateStore, state *model.CommandState, phaseIdx int, commandID string) error {
	state.Phases[phaseIdx].Status = model.PhaseStatusAwaitingFill
	state.UpdatedAt = nowUTC()
	if saveErr := sm.SaveState(state); saveErr != nil {
		return fmt.Errorf("rollback: save state for command %s: %w", commandID, saveErr)
	}
	return nil
}

// rollbackFullPhaseFill reverts queue entries, phase state, and task state additions,
// then persists the rolled-back state.
func rollbackFullPhaseFill(sm stateStore, state *model.CommandState, phaseIdx int, opts SubmitOptions, tasks []TaskInput, nameToID map[string]string, assignMap map[string]WorkerAssignment) error {
	var errs []error
	if queueErr := rollbackQueueEntries(opts.MaestroDir, tasks, nameToID, assignMap, opts.LockMap); queueErr != nil {
		errs = append(errs, fmt.Errorf("rollback: queue entries for command %s: %w", opts.CommandID, queueErr))
	}
	state.Phases[phaseIdx].Status = model.PhaseStatusAwaitingFill
	rollbackPhaseFillState(state, phaseIdx, tasks, nameToID)
	state.UpdatedAt = nowUTC()
	if saveErr := sm.SaveState(state); saveErr != nil {
		errs = append(errs, fmt.Errorf("rollback: save state for command %s: %w", opts.CommandID, saveErr))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// logRollbackFailure logs a rollback failure with structured context about
// system recovery state and recommended actions.
func logRollbackFailure(commandID string, err error, op string, recoverable bool, suggestedAction, affectedResource string) {
	slog.Error("rollback failed",
		"op", op,
		"command_id", commandID,
		"error", err,
		"recoverable", recoverable,
		"suggested_action", suggestedAction,
		"affected_resource", affectedResource,
	)
}

func submitInitialTasks(opts SubmitOptions, tasks []TaskInput, sm stateStore) (*SubmitResult, error) {
	// Auto-complete defaults before validation
	ApplyTaskDefaults(tasks)

	// Validation
	if verrs := ValidateTasksInput(tasks); verrs != nil {
		return nil, verrs
	}

	if opts.DryRun {
		return &SubmitResult{Valid: true}, nil
	}

	// Insert __system_commit if continuous.enabled (and worktree mode is off,
	// since worktree mode delegates commits to the Daemon directly).
	if shouldInsertSystemCommit(opts.Config) {
		var commitErr error
		tasks, commitErr = insertSystemCommitTask(tasks)
		if commitErr != nil {
			return nil, commitErr
		}
	}

	nameToID, assignments, assignMap, err := resolveAndAssignTasks(opts, tasks)
	if err != nil {
		return nil, err
	}

	// Build state
	now := nowUTC()
	state, err := buildCommandState(opts.CommandID, tasks, nameToID, nil, now)
	if err != nil {
		return nil, fmt.Errorf("build state: %w", err)
	}

	// Atomic write: create state (planning)
	state.PlanStatus = model.PlanStatusPlanning
	if err := sm.SaveState(state); err != nil {
		return nil, fmt.Errorf("save state (planning): %w", err)
	}

	if err := writeQueueEntries(opts.MaestroDir, assignments, tasks, nameToID, opts.CommandID, now, opts.LockMap); err != nil {
		if rbErr := rollbackStateAndQueue(sm, opts.MaestroDir, opts.CommandID, tasks, nameToID, assignMap, opts.LockMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "initial_tasks_queue_write", false, "manual_cleanup", "command_state+queue_entries")
		}
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Seal
	state.PlanStatus = model.PlanStatusSealed
	state.PlanVersion = 1
	state.UpdatedAt = nowUTC()
	if err := sm.SaveState(state); err != nil {
		if rbErr := rollbackStateAndQueue(sm, opts.MaestroDir, opts.CommandID, tasks, nameToID, assignMap, opts.LockMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "initial_tasks_seal", false, "manual_cleanup", "command_state+queue_entries")
		}
		return nil, fmt.Errorf("save state (sealed): %w", err)
	}

	// Build output
	result := &SubmitResult{CommandID: opts.CommandID}
	for _, t := range tasks {
		a, ok := assignMap[t.Name]
		if !ok {
			return nil, fmt.Errorf("no worker assignment found for task %q", t.Name)
		}
		result.Tasks = append(result.Tasks, SubmitTaskResult{
			Name:   t.Name,
			TaskID: nameToID[t.Name],
			Worker: a.WorkerID,
			Model:  a.Model,
		})
	}
	return result, nil
}

func submitInitialPhases(opts SubmitOptions, phases []PhaseInput, sm stateStore) (*SubmitResult, error) {
	// Auto-complete defaults for tasks within phases before validation
	for i := range phases {
		ApplyTaskDefaults(phases[i].Tasks)
	}

	// Validation
	if verrs := ValidatePhasesInput(phases); verrs != nil {
		return nil, verrs
	}

	if err := validateCrossPhaseTaskNames(phases); err != nil {
		return nil, err
	}

	if opts.DryRun {
		return &SubmitResult{Valid: true}, nil
	}

	now := nowUTC()

	// Generate phase IDs
	phaseNameToID := make(map[string]string)
	for _, p := range phases {
		id, err := model.GenerateID(model.IDTypePhase)
		if err != nil {
			return nil, fmt.Errorf("generate phase ID: %w", err)
		}
		phaseNameToID[p.Name] = id
	}

	// Process concrete phases: resolve names, assign workers
	cpd, err := processConcretePhases(opts, phases)
	if err != nil {
		return nil, err
	}

	// Insert __system_commit outside phase structure if continuous enabled
	// (skipped in worktree mode: Daemon manages commits directly).
	var systemCommitTaskID *string
	if shouldInsertSystemCommit(opts.Config) {
		var scErr error
		systemCommitTaskID, scErr = addSystemCommitForPhases(opts, cpd)
		if scErr != nil {
			return nil, scErr
		}
	}

	// Build state
	state, err := buildPhaseCommandState(opts, phases, phaseNameToID, cpd, systemCommitTaskID, now)
	if err != nil {
		return nil, fmt.Errorf("build phase state: %w", err)
	}

	// Save state (planning)
	if err := sm.SaveState(state); err != nil {
		return nil, fmt.Errorf("save state (planning): %w", err)
	}

	// Write queue entries for concrete phase tasks + system commit
	if err := writeQueueEntries(opts.MaestroDir, cpd.assignments, cpd.tasks, cpd.nameToID, opts.CommandID, now, opts.LockMap); err != nil {
		if rbErr := rollbackStateAndQueue(sm, opts.MaestroDir, opts.CommandID, cpd.tasks, cpd.nameToID, cpd.assignMap, opts.LockMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "initial_phases_queue_write", false, "manual_cleanup", "command_state+queue_entries")
		}
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Seal
	state.PlanStatus = model.PlanStatusSealed
	state.PlanVersion = 1
	state.UpdatedAt = nowUTC()
	if err := sm.SaveState(state); err != nil {
		if rbErr := rollbackStateAndQueue(sm, opts.MaestroDir, opts.CommandID, cpd.tasks, cpd.nameToID, cpd.assignMap, opts.LockMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "initial_phases_seal", false, "manual_cleanup", "command_state+queue_entries")
		}
		return nil, fmt.Errorf("save state (sealed): %w", err)
	}

	return buildPhaseSubmitResult(opts.CommandID, phases, phaseNameToID, cpd, systemCommitTaskID), nil
}

func submitPhaseFill(opts SubmitOptions, input SubmitInput) (*SubmitResult, error) {
	if len(input.Phases) > 0 {
		return nil, fmt.Errorf("phase fill only accepts tasks, not phases")
	}

	if opts.LockMap == nil {
		return nil, ErrLockMapRequired
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	if state.PlanStatus != model.PlanStatusSealed {
		return nil, &planValidationError{Msg: fmt.Sprintf("plan_status must be sealed, got %s", state.PlanStatus)}
	}

	if err := ValidateNotCancelled(state); err != nil {
		return nil, err
	}

	// Find target phase
	var targetPhase *model.Phase
	var targetPhaseIdx int
	for i := range state.Phases {
		if state.Phases[i].Name == opts.PhaseName {
			targetPhase = &state.Phases[i]
			targetPhaseIdx = i
			break
		}
	}
	if targetPhase == nil {
		return nil, &planValidationError{Msg: fmt.Sprintf("phase %q not found", opts.PhaseName)}
	}
	if targetPhase.Type != "deferred" {
		return nil, &planValidationError{Msg: fmt.Sprintf("phase %q is not deferred (type: %s)", opts.PhaseName, targetPhase.Type)}
	}
	if targetPhase.Status != model.PhaseStatusAwaitingFill {
		return nil, &planValidationError{Msg: fmt.Sprintf("phase %q status must be awaiting_fill, got %s", opts.PhaseName, targetPhase.Status)}
	}

	// Auto-complete defaults before validation
	ApplyTaskDefaults(input.Tasks)

	// Validate input against constraints
	if verrs := ValidatePhaseFillInput(input.Tasks, *targetPhase); verrs != nil {
		return nil, verrs
	}

	if opts.DryRun {
		return &SubmitResult{Valid: true}, nil
	}

	// Transition to filling and persist to disk (R0b recovery depends on this)
	state.Phases[targetPhaseIdx].Status = model.PhaseStatusFilling
	nowStr := nowUTC()
	state.Phases[targetPhaseIdx].FillingStartedAt = &nowStr
	state.UpdatedAt = nowStr
	if err := sm.SaveState(state); err != nil {
		state.Phases[targetPhaseIdx].Status = model.PhaseStatusAwaitingFill
		return nil, fmt.Errorf("save state (filling): %w", err)
	}

	// Generate IDs and assign workers
	nameToID, assignments, assignMap, err := resolveAndAssignTasks(opts, input.Tasks)
	if err != nil {
		if rbErr := rollbackPhaseFillToAwaiting(sm, state, targetPhaseIdx, opts.CommandID); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "phase_fill_assign", true, "await_automatic_recovery", "phase_state")
		}
		return nil, err
	}

	// Re-validate constraints at task insertion time (defense-in-depth)
	if targetPhase.Constraints != nil {
		if len(input.Tasks) > targetPhase.Constraints.MaxTasks {
			if rbErr := rollbackPhaseFillToAwaiting(sm, state, targetPhaseIdx, opts.CommandID); rbErr != nil {
				logRollbackFailure(opts.CommandID, rbErr, "phase_fill_max_tasks", true, "await_automatic_recovery", "phase_state")
			}
			return nil, &planValidationError{Msg: fmt.Sprintf("task count %d exceeds phase constraint max_tasks %d for phase %q",
				len(input.Tasks), targetPhase.Constraints.MaxTasks, opts.PhaseName)}
		}
		if len(targetPhase.Constraints.AllowedBloomLevels) > 0 {
			allowedBloom := buildAllowedBloomMap(targetPhase.Constraints.AllowedBloomLevels)
			for _, t := range input.Tasks {
				if t.BloomLevel > 0 && !allowedBloom[t.BloomLevel] {
					if rbErr := rollbackPhaseFillToAwaiting(sm, state, targetPhaseIdx, opts.CommandID); rbErr != nil {
						logRollbackFailure(opts.CommandID, rbErr, "phase_fill_bloom_levels", true, "await_automatic_recovery", "phase_state")
					}
					return nil, &planValidationError{Msg: fmt.Sprintf("bloom_level %d not in allowed levels for phase %q",
						t.BloomLevel, opts.PhaseName)}
				}
			}
		}
	}

	now := nowUTC()
	for _, t := range input.Tasks {
		taskID := nameToID[t.Name]
		state.Phases[targetPhaseIdx].TaskIDs = append(state.Phases[targetPhaseIdx].TaskIDs, taskID)

		if t.Required {
			state.RequiredTaskIDs = append(state.RequiredTaskIDs, taskID)
		} else {
			state.OptionalTaskIDs = append(state.OptionalTaskIDs, taskID)
		}

		state.TaskStates[taskID] = model.StatusPending

		if len(t.BlockedBy) > 0 {
			depIDs := make([]string, 0, len(t.BlockedBy))
			for _, depName := range t.BlockedBy {
				depID, ok := nameToID[depName]
				if !ok {
					return nil, fmt.Errorf("blocked_by %q not found in fill tasks for phase %q (cross-phase references are not supported in phase fill)", depName, opts.PhaseName)
				}
				depIDs = append(depIDs, depID)
			}
			state.TaskDependencies[taskID] = depIDs
		}
	}
	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)

	if err := writeQueueEntries(opts.MaestroDir, assignments, input.Tasks, nameToID, opts.CommandID, now, opts.LockMap); err != nil {
		if rbErr := rollbackFullPhaseFill(sm, state, targetPhaseIdx, opts, input.Tasks, nameToID, assignMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "phase_fill_queue_write", false, "manual_intervention", "phase_state+queue_entries")
		}
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Activate phase
	state.Phases[targetPhaseIdx].Status = model.PhaseStatusActive
	state.Phases[targetPhaseIdx].ActivatedAt = &now
	state.PlanVersion++
	state.UpdatedAt = now

	if err := sm.SaveState(state); err != nil {
		if rbErr := rollbackFullPhaseFill(sm, state, targetPhaseIdx, opts, input.Tasks, nameToID, assignMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "phase_fill_save_state", false, "manual_intervention", "phase_state+queue_entries")
		}
		return nil, fmt.Errorf("save state: %w", err)
	}

	// Build output
	result := &SubmitResult{CommandID: opts.CommandID}
	for _, t := range input.Tasks {
		a, ok := assignMap[t.Name]
		if !ok {
			return nil, fmt.Errorf("no worker assignment found for task %q", t.Name)
		}
		result.Tasks = append(result.Tasks, SubmitTaskResult{
			Name:   t.Name,
			TaskID: nameToID[t.Name],
			Worker: a.WorkerID,
			Model:  a.Model,
		})
	}
	return result, nil
}

// acquireStateFlock acquires a file-level exclusive lock for a command's state,
// providing cross-process mutual exclusion for double-submit prevention.
func acquireStateFlock(maestroDir, commandID string) (*os.File, error) {
	lockPath := filepath.Join(maestroDir, "locks", "state_"+commandID+".flock")
	return acquireFlock(lockPath, syscall.LOCK_EX)
}
