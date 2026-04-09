package plan

import (
	"fmt"
	"log"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// SubmitOptions holds the configuration for a plan submission operation.
type SubmitOptions struct {
	CommandID  string
	TasksFile  string // path or "-" for stdin
	PhaseName  string // non-empty for phase fill
	DryRun     bool
	MaestroDir string
	Config     model.Config
	LockMap    *lock.MutexMap
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
	input, err := readInput(opts.TasksFile)
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

func submitInitial(opts SubmitOptions, input SubmitInput) (*SubmitResult, error) {
	if opts.LockMap == nil {
		return nil, fmt.Errorf("LockMap is required")
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

	// Re-check cancellation under state lock to close any remaining race
	if cancelErr := checkCommandNotCancelled(opts.MaestroDir, opts.CommandID); cancelErr != nil {
		return nil, cancelErr
	}

	// Double submit prevention (now under lock)
	if sm.StateExists(opts.CommandID) {
		return nil, fmt.Errorf("state already exists for command %s (double submit)", opts.CommandID)
	}

	// Route by input type
	if len(input.Phases) > 0 {
		return submitInitialPhases(opts, input.Phases, sm)
	}
	return submitInitialTasks(opts, input.Tasks, sm)
}

func submitInitialTasks(opts SubmitOptions, tasks []TaskInput, sm StateStore) (*SubmitResult, error) {
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

	// Generate IDs and resolve names
	nameToID, err := resolveNames(tasks)
	if err != nil {
		return nil, fmt.Errorf("resolve names: %w", err)
	}

	// Build worker states and assign
	workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
	if err != nil {
		return nil, fmt.Errorf("build worker states: %w", err)
	}

	var assignReqs []TaskAssignmentRequest
	for _, t := range tasks {
		assignReqs = append(assignReqs, TaskAssignmentRequest{Name: t.Name, BloomLevel: t.BloomLevel})
	}

	assignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, workerStates, assignReqs)
	if err != nil {
		return nil, fmt.Errorf("worker assignment: %w", err)
	}

	assignMap := make(map[string]WorkerAssignment)
	for _, a := range assignments {
		assignMap[a.TaskName] = a
	}

	// Build state
	now := time.Now().UTC().Format(time.RFC3339)
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
		rollbackQueueEntries(opts.MaestroDir, tasks, nameToID, assignMap, opts.LockMap)
		if delErr := sm.DeleteState(opts.CommandID); delErr != nil {
			log.Printf("rollback: delete state for command %s: %v", opts.CommandID, delErr)
		}
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Seal
	state.PlanStatus = model.PlanStatusSealed
	state.PlanVersion = 1
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := sm.SaveState(state); err != nil {
		rollbackQueueEntries(opts.MaestroDir, tasks, nameToID, assignMap, opts.LockMap)
		if delErr := sm.DeleteState(opts.CommandID); delErr != nil {
			log.Printf("rollback: delete state for command %s: %v", opts.CommandID, delErr)
		}
		return nil, fmt.Errorf("save state (sealed): %w", err)
	}

	// Build output
	result := &SubmitResult{CommandID: opts.CommandID}
	for _, t := range tasks {
		a := assignMap[t.Name]
		result.Tasks = append(result.Tasks, SubmitTaskResult{
			Name:   t.Name,
			TaskID: nameToID[t.Name],
			Worker: a.WorkerID,
			Model:  a.Model,
		})
	}
	return result, nil
}

func submitInitialPhases(opts SubmitOptions, phases []PhaseInput, sm StateStore) (*SubmitResult, error) {
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

	now := time.Now().UTC().Format(time.RFC3339)

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
	state := buildPhaseCommandState(opts, phases, phaseNameToID, cpd, systemCommitTaskID, now)

	// Save state (planning)
	if err := sm.SaveState(state); err != nil {
		return nil, fmt.Errorf("save state (planning): %w", err)
	}

	// Write queue entries for concrete phase tasks + system commit
	if err := writeQueueEntries(opts.MaestroDir, cpd.assignments, cpd.tasks, cpd.nameToID, opts.CommandID, now, opts.LockMap); err != nil {
		rollbackQueueEntries(opts.MaestroDir, cpd.tasks, cpd.nameToID, cpd.assignMap, opts.LockMap)
		if delErr := sm.DeleteState(opts.CommandID); delErr != nil {
			log.Printf("rollback: delete state for command %s: %v", opts.CommandID, delErr)
		}
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Seal
	state.PlanStatus = model.PlanStatusSealed
	state.PlanVersion = 1
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := sm.SaveState(state); err != nil {
		rollbackQueueEntries(opts.MaestroDir, cpd.tasks, cpd.nameToID, cpd.assignMap, opts.LockMap)
		if delErr := sm.DeleteState(opts.CommandID); delErr != nil {
			log.Printf("rollback: delete state for command %s: %v", opts.CommandID, delErr)
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
		return nil, fmt.Errorf("LockMap is required")
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	if state.PlanStatus != model.PlanStatusSealed {
		return nil, &PlanValidationError{Msg: fmt.Sprintf("plan_status must be sealed, got %s", state.PlanStatus)}
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
		return nil, &PlanValidationError{Msg: fmt.Sprintf("phase %q not found", opts.PhaseName)}
	}
	if targetPhase.Type != "deferred" {
		return nil, &PlanValidationError{Msg: fmt.Sprintf("phase %q is not deferred (type: %s)", opts.PhaseName, targetPhase.Type)}
	}
	if targetPhase.Status != model.PhaseStatusAwaitingFill {
		return nil, &PlanValidationError{Msg: fmt.Sprintf("phase %q status must be awaiting_fill, got %s", opts.PhaseName, targetPhase.Status)}
	}

	// Validate input against constraints
	if verrs := ValidatePhaseFillInput(input.Tasks, *targetPhase); verrs != nil {
		return nil, verrs
	}

	if opts.DryRun {
		return &SubmitResult{Valid: true}, nil
	}

	// Transition to filling and persist to disk (R0b recovery depends on this)
	state.Phases[targetPhaseIdx].Status = model.PhaseStatusFilling
	nowStr := time.Now().UTC().Format(time.RFC3339)
	state.Phases[targetPhaseIdx].FillingStartedAt = &nowStr
	state.UpdatedAt = nowStr
	if err := sm.SaveState(state); err != nil {
		state.Phases[targetPhaseIdx].Status = model.PhaseStatusAwaitingFill
		return nil, fmt.Errorf("save state (filling): %w", err)
	}

	// Generate IDs and assign workers
	nameToID, err := resolveNames(input.Tasks)
	if err != nil {
		// Rollback filling → awaiting_fill
		state.Phases[targetPhaseIdx].Status = model.PhaseStatusAwaitingFill
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if saveErr := sm.SaveState(state); saveErr != nil {
			log.Printf("rollback: save state for command %s: %v", opts.CommandID, saveErr)
		}
		return nil, fmt.Errorf("resolve names: %w", err)
	}

	workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
	if err != nil {
		// Rollback filling → awaiting_fill
		state.Phases[targetPhaseIdx].Status = model.PhaseStatusAwaitingFill
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if saveErr := sm.SaveState(state); saveErr != nil {
			log.Printf("rollback: save state for command %s: %v", opts.CommandID, saveErr)
		}
		return nil, fmt.Errorf("build worker states: %w", err)
	}

	var assignReqs []TaskAssignmentRequest
	for _, t := range input.Tasks {
		assignReqs = append(assignReqs, TaskAssignmentRequest{Name: t.Name, BloomLevel: t.BloomLevel})
	}

	assignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, workerStates, assignReqs)
	if err != nil {
		state.Phases[targetPhaseIdx].Status = model.PhaseStatusAwaitingFill
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if saveErr := sm.SaveState(state); saveErr != nil {
			log.Printf("rollback: save state for command %s: %v", opts.CommandID, saveErr)
		}
		return nil, fmt.Errorf("worker assignment: %w", err)
	}

	assignMap := make(map[string]WorkerAssignment)
	for _, a := range assignments {
		assignMap[a.TaskName] = a
	}

	now := time.Now().UTC().Format(time.RFC3339)
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
		// Rollback: remove partial queue writes, revert state to awaiting_fill, and persist
		rollbackQueueEntries(opts.MaestroDir, input.Tasks, nameToID, assignMap, opts.LockMap)
		state.Phases[targetPhaseIdx].Status = model.PhaseStatusAwaitingFill
		rollbackPhaseFillState(state, targetPhaseIdx, input.Tasks, nameToID)
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if saveErr := sm.SaveState(state); saveErr != nil {
			log.Printf("rollback: save state for command %s: %v", opts.CommandID, saveErr)
		}
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Activate phase
	state.Phases[targetPhaseIdx].Status = model.PhaseStatusActive
	state.Phases[targetPhaseIdx].ActivatedAt = &now
	state.PlanVersion++
	state.UpdatedAt = now

	if err := sm.SaveState(state); err != nil {
		state.Phases[targetPhaseIdx].Status = model.PhaseStatusAwaitingFill
		rollbackQueueEntries(opts.MaestroDir, input.Tasks, nameToID, assignMap, opts.LockMap)
		rollbackPhaseFillState(state, targetPhaseIdx, input.Tasks, nameToID)
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if saveErr := sm.SaveState(state); saveErr != nil {
			log.Printf("rollback: save state for command %s: %v", opts.CommandID, saveErr)
		}
		return nil, fmt.Errorf("save state: %w", err)
	}

	// Build output
	result := &SubmitResult{CommandID: opts.CommandID}
	for _, t := range input.Tasks {
		a := assignMap[t.Name]
		result.Tasks = append(result.Tasks, SubmitTaskResult{
			Name:   t.Name,
			TaskID: nameToID[t.Name],
			Worker: a.WorkerID,
			Model:  a.Model,
		})
	}
	return result, nil
}
