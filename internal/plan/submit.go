// Package plan handles plan submission, validation, state management, and completion logic.
package plan

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
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

	// QA-014: Acquire queue:planner lock BEFORE state lock to check cancellation.
	// This maintains the global lock ordering (queue:planner → state:<cmd>)
	// used by other code paths (complete.go, reconciler.go, queue_write_handler.go).
	opts.LockMap.Lock("queue:planner")
	cancelErr := checkCommandNotCancelled(opts.MaestroDir, opts.CommandID)
	opts.LockMap.Unlock("queue:planner")
	if cancelErr != nil {
		return nil, cancelErr
	}

	// Lock command state to prevent concurrent double submit (TOCTOU fix)
	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

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

func submitInitialTasks(opts SubmitOptions, tasks []TaskInput, sm *StateManager) (*SubmitResult, error) {
	// Validation
	if verrs := ValidateTasksInput(tasks); verrs != nil {
		return nil, verrs
	}

	if opts.DryRun {
		return &SubmitResult{Valid: true}, nil
	}

	// Insert __system_commit if continuous.enabled
	if opts.Config.Continuous.Enabled {
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

	// Write queue entries
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

func submitInitialPhases(opts SubmitOptions, phases []PhaseInput, sm *StateManager) (*SubmitResult, error) {
	// Validation
	if verrs := ValidatePhasesInput(phases); verrs != nil {
		return nil, verrs
	}

	// Check for cross-phase task name collisions (must run before dry-run return)
	globalTaskNames := make(map[string]string) // name → phase name
	for _, phase := range phases {
		if phase.Type != "concrete" {
			continue
		}
		for _, task := range phase.Tasks {
			if existingPhase, exists := globalTaskNames[task.Name]; exists {
				return nil, fmt.Errorf("task name %q appears in both phase %q and %q; task names must be unique across all phases",
					task.Name, existingPhase, phase.Name)
			}
			globalTaskNames[task.Name] = phase.Name
		}
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

	// Process concrete phases: generate task IDs, assign workers
	allNameToID := make(map[string]string)
	allAssignMap := make(map[string]WorkerAssignment)
	var allTasks []TaskInput
	var allAssignments []WorkerAssignment

	workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
	if err != nil {
		return nil, fmt.Errorf("build worker states: %w", err)
	}

	for _, phase := range phases {
		if phase.Type != "concrete" {
			continue
		}

		nameToID, err := resolveNames(phase.Tasks)
		if err != nil {
			return nil, fmt.Errorf("resolve names for phase %s: %w", phase.Name, err)
		}
		for k, v := range nameToID {
			allNameToID[k] = v
		}

		var assignReqs []TaskAssignmentRequest
		for _, t := range phase.Tasks {
			assignReqs = append(assignReqs, TaskAssignmentRequest{Name: t.Name, BloomLevel: t.BloomLevel})
		}

		assignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, workerStates, assignReqs)
		if err != nil {
			return nil, fmt.Errorf("worker assignment for phase %s: %w", phase.Name, err)
		}
		allAssignments = append(allAssignments, assignments...)
		for _, a := range assignments {
			allAssignMap[a.TaskName] = a
		}
		allTasks = append(allTasks, phase.Tasks...)

		// Update workerStates for next phase assignment
		for _, a := range assignments {
			for i := range workerStates {
				if workerStates[i].WorkerID == a.WorkerID {
					workerStates[i].PendingCount++
					break
				}
			}
		}
	}

	// Insert __system_commit outside phase structure if continuous enabled
	var systemCommitTaskID *string
	if opts.Config.Continuous.Enabled {
		// Collect all concrete-phase task names so __system_commit blocks on them
		allConcreteTaskNames := make([]string, 0, len(allTasks))
		for _, t := range allTasks {
			allConcreteTaskNames = append(allConcreteTaskNames, t.Name)
		}

		commitTask := buildSystemCommitTask(allConcreteTaskNames)
		if err := validateSystemTask(commitTask); err != nil {
			return nil, fmt.Errorf("validate system commit task: %w", err)
		}

		commitID, err := model.GenerateID(model.IDTypeTask)
		if err != nil {
			return nil, fmt.Errorf("generate system commit ID: %w", err)
		}
		allNameToID[commitTask.Name] = commitID
		systemCommitTaskID = &commitID

		assignReqs := []TaskAssignmentRequest{{Name: commitTask.Name, BloomLevel: commitTask.BloomLevel}}
		commitAssignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, workerStates, assignReqs)
		if err != nil {
			return nil, fmt.Errorf("worker assignment for system commit: %w", err)
		}
		allAssignments = append(allAssignments, commitAssignments...)
		for _, a := range commitAssignments {
			allAssignMap[a.TaskName] = a
		}
		allTasks = append(allTasks, commitTask)
	}

	// Build state with phases
	state := &model.CommandState{
		SchemaVersion:      1,
		FileType:           "state_command",
		CommandID:          opts.CommandID,
		PlanVersion:        0,
		PlanStatus:         model.PlanStatusPlanning,
		CompletionPolicy:   defaultCompletionPolicy(),
		TaskDependencies:   make(map[string][]string),
		TaskStates:         make(map[string]model.Status),
		CancelledReasons:   make(map[string]string),
		AppliedResultIDs:   make(map[string]string),
		RetryLineage:       make(map[string]string),
		SystemCommitTaskID: systemCommitTaskID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	// Build phases
	for _, p := range phases {
		phaseModel := model.Phase{
			PhaseID:         phaseNameToID[p.Name],
			Name:            p.Name,
			Type:            p.Type,
			DependsOnPhases: p.DependsOnPhases,
		}

		if p.Type == "concrete" {
			phaseModel.Status = model.PhaseStatusActive
			phaseModel.ActivatedAt = &now

			for _, t := range p.Tasks {
				taskID := allNameToID[t.Name]
				phaseModel.TaskIDs = append(phaseModel.TaskIDs, taskID)

				if t.Required {
					state.RequiredTaskIDs = append(state.RequiredTaskIDs, taskID)
				} else {
					state.OptionalTaskIDs = append(state.OptionalTaskIDs, taskID)
				}

				state.TaskStates[taskID] = model.StatusPending

				// Convert blocked_by names to IDs
				if len(t.BlockedBy) > 0 {
					depIDs := make([]string, 0, len(t.BlockedBy))
					for _, depName := range t.BlockedBy {
						depIDs = append(depIDs, allNameToID[depName])
					}
					state.TaskDependencies[taskID] = depIDs
				}
			}
		} else {
			// Deferred phase
			phaseModel.Status = model.PhaseStatusPending
			if p.Constraints != nil {
				phaseModel.Constraints = &model.PhaseConstraints{
					MaxTasks:           p.Constraints.MaxTasks,
					TimeoutMinutes:     p.Constraints.TimeoutMinutes,
					AllowedBloomLevels: p.Constraints.AllowedBloomLevels,
				}
				if len(phaseModel.Constraints.AllowedBloomLevels) == 0 {
					phaseModel.Constraints.AllowedBloomLevels = []int{1, 2, 3, 4, 5, 6}
				}
			}
		}

		state.Phases = append(state.Phases, phaseModel)
	}

	// Add system commit task to state (outside any phase)
	if systemCommitTaskID != nil {
		state.RequiredTaskIDs = append(state.RequiredTaskIDs, *systemCommitTaskID)
		state.TaskStates[*systemCommitTaskID] = model.StatusPending

		// Register dependencies: system commit blocks on all concrete-phase tasks
		depIDs := make([]string, 0, len(allTasks)-1) // exclude __system_commit itself
		for _, t := range allTasks {
			if t.Name != "__system_commit" {
				depIDs = append(depIDs, allNameToID[t.Name])
			}
		}
		if len(depIDs) > 0 {
			state.TaskDependencies[*systemCommitTaskID] = depIDs
		}
	}

	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)

	// Save state (planning)
	if err := sm.SaveState(state); err != nil {
		return nil, fmt.Errorf("save state (planning): %w", err)
	}

	// Write queue entries for concrete phase tasks + system commit
	if err := writeQueueEntries(opts.MaestroDir, allAssignments, allTasks, allNameToID, opts.CommandID, now, opts.LockMap); err != nil {
		rollbackQueueEntries(opts.MaestroDir, allTasks, allNameToID, allAssignMap, opts.LockMap)
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
		rollbackQueueEntries(opts.MaestroDir, allTasks, allNameToID, allAssignMap, opts.LockMap)
		if delErr := sm.DeleteState(opts.CommandID); delErr != nil {
			log.Printf("rollback: delete state for command %s: %v", opts.CommandID, delErr)
		}
		return nil, fmt.Errorf("save state (sealed): %w", err)
	}

	// Build output
	result := &SubmitResult{CommandID: opts.CommandID}
	for _, p := range phases {
		pr := SubmitPhaseResult{
			Name:    p.Name,
			PhaseID: phaseNameToID[p.Name],
			Type:    p.Type,
		}
		if p.Type == "concrete" {
			pr.Status = string(model.PhaseStatusActive)
			for _, t := range p.Tasks {
				a := allAssignMap[t.Name]
				pr.Tasks = append(pr.Tasks, SubmitTaskResult{
					Name:   t.Name,
					TaskID: allNameToID[t.Name],
					Worker: a.WorkerID,
					Model:  a.Model,
				})
			}
		} else {
			pr.Status = string(model.PhaseStatusPending)
		}
		result.Phases = append(result.Phases, pr)
	}

	if systemCommitTaskID != nil {
		a := allAssignMap["__system_commit"]
		result.Tasks = append(result.Tasks, SubmitTaskResult{
			Name:   "__system_commit",
			TaskID: *systemCommitTaskID,
			Worker: a.WorkerID,
			Model:  a.Model,
		})
	}

	return result, nil
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
		return nil, fmt.Errorf("resolve names: %w", err)
	}

	workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
	if err != nil {
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

	// Update state
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

	// Write queue entries
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

func readInput(tasksFile string) (*SubmitInput, error) {
	var data []byte
	var err error

	if tasksFile == "-" || tasksFile == "" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, model.DefaultMaxYAMLFileBytes+1))
		if err == nil && len(data) > model.DefaultMaxYAMLFileBytes {
			return nil, fmt.Errorf("stdin input exceeds maximum size of %d bytes", model.DefaultMaxYAMLFileBytes)
		}
	} else {
		// CRIT-05: Validate file path (no null bytes, clean path)
		cleaned, pathErr := validate.ValidateFilePath(tasksFile)
		if pathErr != nil {
			return nil, fmt.Errorf("invalid tasks file path: %w", pathErr)
		}
		tasksFile = cleaned

		// HIGH-05: Check file size before reading to prevent memory exhaustion
		info, statErr := os.Stat(tasksFile)
		if statErr != nil {
			return nil, fmt.Errorf("stat tasks file: %w", statErr)
		}
		if info.Size() > int64(model.DefaultMaxYAMLFileBytes) {
			return nil, fmt.Errorf("tasks file exceeds maximum size of %d bytes (got %d)", model.DefaultMaxYAMLFileBytes, info.Size())
		}

		data, err = os.ReadFile(tasksFile)
	}
	if err != nil {
		return nil, fmt.Errorf("read tasks file: %w", err)
	}

	var input SubmitInput
	if err := yamlv3.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("parse tasks YAML: %w", err)
	}
	return &input, nil
}

func buildSystemCommitTask(blockedByNames []string) TaskInput {
	return TaskInput{
		Name:               "__system_commit",
		Purpose:            "コマンド実行結果をリポジトリにコミットする",
		Content:            "変更ファイルを確認し、git add + git commit を実行する。コミットメッセージはタスク実行結果のサマリから生成する。",
		AcceptanceCriteria: "git commit が成功し、コミットハッシュが取得できる",
		BlockedBy:          blockedByNames,
		BloomLevel:         2,
		Required:           true,
	}
}

// validateSystemTask validates a system-generated task's fields without checking
// the reserved __ name prefix. This ensures system tasks meet the same field
// integrity requirements (non-empty fields, length limits, bloom_level range)
// as user-submitted tasks.
func validateSystemTask(task TaskInput) error {
	errs := &ValidationErrors{}
	// Reuse the package-private field validator (skips name-prefix check).
	validateTaskFields(task, "system_task", errs)
	if errs.HasErrors() {
		return errs
	}
	return nil
}

func insertSystemCommitTask(tasks []TaskInput) ([]TaskInput, error) {
	allNames := make([]string, 0, len(tasks))
	for _, t := range tasks {
		allNames = append(allNames, t.Name)
	}

	commitTask := buildSystemCommitTask(allNames)
	if err := validateSystemTask(commitTask); err != nil {
		return nil, fmt.Errorf("validate system commit task: %w", err)
	}

	return append(tasks, commitTask), nil
}

func resolveNames(tasks []TaskInput) (map[string]string, error) {
	nameToID := make(map[string]string, len(tasks))
	for _, t := range tasks {
		id, err := model.GenerateID(model.IDTypeTask)
		if err != nil {
			return nil, fmt.Errorf("generate task ID for %s: %w", t.Name, err)
		}
		nameToID[t.Name] = id
	}
	return nameToID, nil
}

func buildCommandState(commandID string, tasks []TaskInput, nameToID map[string]string, phases []model.Phase, now string) (*model.CommandState, error) {
	state := &model.CommandState{
		SchemaVersion:    1,
		FileType:         "state_command",
		CommandID:        commandID,
		CompletionPolicy: defaultCompletionPolicy(),
		TaskDependencies: make(map[string][]string),
		TaskStates:       make(map[string]model.Status),
		CancelledReasons: make(map[string]string),
		AppliedResultIDs: make(map[string]string),
		RetryLineage:     make(map[string]string),
		Phases:           phases,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	var systemCommitTaskID *string

	for _, t := range tasks {
		taskID := nameToID[t.Name]

		if t.Required {
			state.RequiredTaskIDs = append(state.RequiredTaskIDs, taskID)
		} else {
			state.OptionalTaskIDs = append(state.OptionalTaskIDs, taskID)
		}

		state.TaskStates[taskID] = model.StatusPending

		if len(t.BlockedBy) > 0 {
			depIDs := make([]string, 0, len(t.BlockedBy))
			for _, depName := range t.BlockedBy {
				id, ok := nameToID[depName]
				if !ok {
					return nil, fmt.Errorf("blocked_by references unknown task %q", depName)
				}
				depIDs = append(depIDs, id)
			}
			state.TaskDependencies[taskID] = depIDs
		}

		if t.Name == "__system_commit" {
			systemCommitTaskID = &taskID
		}
	}

	state.SystemCommitTaskID = systemCommitTaskID
	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)
	return state, nil
}

// ActivateDeferredPhases checks all deferred phases in "pending" status and
// transitions them to "awaiting_fill" if all their dependency phases have
// completed. Returns the list of phase names that were activated.
// The caller must hold the command-state lock before calling this function.
func ActivateDeferredPhases(state *model.CommandState) []string {
	if state == nil || len(state.Phases) == 0 {
		return nil
	}

	phaseStatusByName := make(map[string]model.PhaseStatus, len(state.Phases))
	for _, p := range state.Phases {
		phaseStatusByName[p.Name] = p.Status
	}

	var activated []string

	for i := range state.Phases {
		p := &state.Phases[i]

		if p.Type != "deferred" || p.Status != model.PhaseStatusPending {
			continue
		}
		if len(p.DependsOnPhases) == 0 {
			continue
		}

		allCompleted := true
		for _, depName := range p.DependsOnPhases {
			depStatus, ok := phaseStatusByName[depName]
			if !ok || depStatus != model.PhaseStatusCompleted {
				allCompleted = false
				break
			}
		}
		if !allCompleted {
			continue
		}

		p.Status = model.PhaseStatusAwaitingFill

		// Set fill deadline from phase constraints
		if p.Constraints != nil && p.Constraints.TimeoutMinutes > 0 {
			deadline := time.Now().UTC().
				Add(time.Duration(p.Constraints.TimeoutMinutes) * time.Minute).
				Format(time.RFC3339)
			p.FillDeadlineAt = &deadline
		}

		activated = append(activated, p.Name)
		phaseStatusByName[p.Name] = model.PhaseStatusAwaitingFill
	}

	return activated
}

func defaultCompletionPolicy() model.CompletionPolicy {
	return model.CompletionPolicy{
		Mode:                    "all_required_completed",
		AllowDynamicTasks:       false,
		OnRequiredFailed:        "fail_command",
		OnRequiredCancelled:     "cancel_command",
		OnOptionalFailed:        "ignore",
		DependencyFailurePolicy: "cancel_dependents",
	}
}

func writeQueueEntries(maestroDir string, assignments []WorkerAssignment, tasks []TaskInput, nameToID map[string]string, commandID string, now string, lockMap *lock.MutexMap) error {
	// Group tasks by worker
	workerTasks := make(map[string][]model.Task)
	taskMap := make(map[string]TaskInput)
	for _, t := range tasks {
		taskMap[t.Name] = t
	}

	for _, a := range assignments {
		t := taskMap[a.TaskName]
		taskID := nameToID[a.TaskName]

		depIDs := make([]string, 0, len(t.BlockedBy))
		for _, depName := range t.BlockedBy {
			depIDs = append(depIDs, nameToID[depName])
		}

		queueTask := model.Task{
			ID:                 taskID,
			CommandID:          commandID,
			Purpose:            t.Purpose,
			Content:            t.Content,
			AcceptanceCriteria: t.AcceptanceCriteria,
			Constraints:        t.Constraints,
			BlockedBy:          depIDs,
			BloomLevel:         t.BloomLevel,
			ToolsHint:          t.ToolsHint,
			Priority:           100,
			Status:             model.StatusPending,
			CreatedAt:          now,
			UpdatedAt:          now,
		}

		workerTasks[a.WorkerID] = append(workerTasks[a.WorkerID], queueTask)
	}

	// CRIT-02: Write to each worker's queue file under per-queue lock
	// to prevent concurrent read-modify-write data loss.
	for workerID, newTasks := range workerTasks {
		if err := func() error {
			if lockMap != nil {
				lockMap.Lock("queue:" + workerID)
				defer lockMap.Unlock("queue:" + workerID)
			}

			queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(workerID))

			var tq model.TaskQueue
			data, err := os.ReadFile(queueFile)
			if err == nil {
				if err := yamlv3.Unmarshal(data, &tq); err != nil {
					return fmt.Errorf("parse existing queue %s: %w", workerID, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read queue %s: %w", workerID, err)
			}
			if tq.SchemaVersion == 0 {
				tq.SchemaVersion = 1
				tq.FileType = "queue_task"
			}

			tq.Tasks = append(tq.Tasks, newTasks...)

			if err := yamlutil.AtomicWrite(queueFile, tq); err != nil {
				return fmt.Errorf("write queue %s: %w", workerID, err)
			}
			return nil
		}(); err != nil {
			return err
		}
	}

	return nil
}

func rollbackQueueEntries(maestroDir string, tasks []TaskInput, nameToID map[string]string, assignMap map[string]WorkerAssignment, lockMap *lock.MutexMap) {
	taskIDs := make(map[string]bool)
	for _, t := range tasks {
		taskIDs[nameToID[t.Name]] = true
	}

	// Group by worker
	workerFiles := make(map[string]bool)
	for _, a := range assignMap {
		workerFiles[a.WorkerID] = true
	}

	// CRIT-02: Lock per-queue to prevent concurrent read-modify-write data loss.
	for workerID := range workerFiles {
		func() {
			if lockMap != nil {
				lockMap.Lock("queue:" + workerID)
				defer lockMap.Unlock("queue:" + workerID)
			}

			queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(workerID))

			data, err := os.ReadFile(queueFile)
			if err != nil {
				return
			}

			var tq model.TaskQueue
			if err := yamlv3.Unmarshal(data, &tq); err != nil {
				return
			}

			var kept []model.Task
			for _, t := range tq.Tasks {
				if !taskIDs[t.ID] {
					kept = append(kept, t)
				}
			}
			tq.Tasks = kept

			if writeErr := yamlutil.AtomicWrite(queueFile, tq); writeErr != nil {
				log.Printf("rollback: write queue %s: %v", workerID, writeErr)
			}
		}()
	}
}

func rollbackPhaseFillState(state *model.CommandState, phaseIdx int, tasks []TaskInput, nameToID map[string]string) {
	for _, t := range tasks {
		taskID := nameToID[t.Name]

		// Remove from task_ids
		var filtered []string
		for _, id := range state.Phases[phaseIdx].TaskIDs {
			if id != taskID {
				filtered = append(filtered, id)
			}
		}
		state.Phases[phaseIdx].TaskIDs = filtered

		// Remove from required/optional
		state.RequiredTaskIDs = removeFromSlice(state.RequiredTaskIDs, taskID)
		state.OptionalTaskIDs = removeFromSlice(state.OptionalTaskIDs, taskID)

		delete(state.TaskStates, taskID)
		delete(state.TaskDependencies, taskID)
	}
	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)
}

func removeFromSlice(s []string, target string) []string {
	var result []string
	for _, v := range s {
		if v != target {
			result = append(result, v)
		}
	}
	return result
}

func checkCommandNotCancelled(maestroDir string, commandID string) error {
	plannerQueuePath := filepath.Join(maestroDir, "queue", "planner.yaml")
	data, err := os.ReadFile(plannerQueuePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no queue file = not cancelled
		}
		return fmt.Errorf("read planner queue: %w", err)
	}

	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return fmt.Errorf("parse planner queue: %w", err)
	}

	for _, cmd := range cq.Commands {
		if cmd.ID == commandID && cmd.Status == model.StatusCancelled {
			return fmt.Errorf("command %s has been cancelled in queue", commandID)
		}
	}
	return nil
}
