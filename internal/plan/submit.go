// Package plan handles plan submission, validation, state management, and completion logic.
package plan

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

type SubmitOptions struct {
	CommandID  string
	TasksFile  string // path or "-" for stdin
	PhaseName  string // non-empty for phase fill
	DryRun     bool
	MaestroDir string
	Config     model.Config
	LockMap    *lock.MutexMap
}

type SubmitResult struct {
	Valid     bool                `json:"valid,omitempty"`
	CommandID string              `json:"command_id,omitempty"`
	Tasks     []SubmitTaskResult  `json:"tasks,omitempty"`
	Phases    []SubmitPhaseResult `json:"phases,omitempty"`
}

type SubmitTaskResult struct {
	Name   string `json:"name"`
	TaskID string `json:"task_id"`
	Worker string `json:"worker"`
	Model  string `json:"model"`
}

type SubmitPhaseResult struct {
	Name    string             `json:"name"`
	PhaseID string             `json:"phase_id"`
	Type    string             `json:"type"`
	Status  string             `json:"status"`
	Tasks   []SubmitTaskResult `json:"tasks,omitempty"`
}

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

	// Double submit prevention
	if sm.StateExists(opts.CommandID) {
		return nil, fmt.Errorf("state already exists for command %s (double submit)", opts.CommandID)
	}

	// Check command not cancelled in queue
	if err := checkCommandNotCancelled(opts.MaestroDir, opts.CommandID); err != nil {
		return nil, err
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
		if opts.DryRun {
			return nil, verrs
		}
		return nil, verrs
	}

	if opts.DryRun {
		return &SubmitResult{Valid: true}, nil
	}

	// Insert __system_commit if continuous.enabled
	if opts.Config.Continuous.Enabled {
		tasks = insertSystemCommitTask(tasks)
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
	state := buildCommandState(opts.CommandID, tasks, nameToID, nil, now)

	// Atomic write: create state (planning)
	state.PlanStatus = model.PlanStatusPlanning
	if err := sm.SaveState(state); err != nil {
		return nil, fmt.Errorf("save state (planning): %w", err)
	}

	// Write queue entries
	if err := writeQueueEntries(opts.MaestroDir, assignments, tasks, nameToID, opts.CommandID, now); err != nil {
		rollbackQueueEntries(opts.MaestroDir, tasks, nameToID, assignMap)
		_ = sm.DeleteState(opts.CommandID)
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Seal
	state.PlanStatus = model.PlanStatusSealed
	state.PlanVersion = 1
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := sm.SaveState(state); err != nil {
		rollbackQueueEntries(opts.MaestroDir, tasks, nameToID, assignMap)
		_ = sm.DeleteState(opts.CommandID)
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
		commitTask := TaskInput{
			Name:               "__system_commit",
			Purpose:            "コマンド実行結果をリポジトリにコミットする",
			Content:            "変更ファイルを確認し、git add + git commit を実行する。コミットメッセージはタスク実行結果のサマリから生成する。",
			AcceptanceCriteria: "git commit が成功し、コミットハッシュが取得できる",
			BloomLevel:         2,
			Required:           true,
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
	}

	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)

	// Save state (planning)
	if err := sm.SaveState(state); err != nil {
		return nil, fmt.Errorf("save state (planning): %w", err)
	}

	// Write queue entries for concrete phase tasks + system commit
	if err := writeQueueEntries(opts.MaestroDir, allAssignments, allTasks, allNameToID, opts.CommandID, now); err != nil {
		rollbackQueueEntries(opts.MaestroDir, allTasks, allNameToID, allAssignMap)
		_ = sm.DeleteState(opts.CommandID)
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Seal
	state.PlanStatus = model.PlanStatusSealed
	state.PlanVersion = 1
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := sm.SaveState(state); err != nil {
		rollbackQueueEntries(opts.MaestroDir, allTasks, allNameToID, allAssignMap)
		_ = sm.DeleteState(opts.CommandID)
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
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
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
		_ = sm.SaveState(state) // persist rollback to disk
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
				depIDs = append(depIDs, nameToID[depName])
			}
			state.TaskDependencies[taskID] = depIDs
		}
	}
	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)

	// Write queue entries
	if err := writeQueueEntries(opts.MaestroDir, assignments, input.Tasks, nameToID, opts.CommandID, now); err != nil {
		// Rollback: revert to awaiting_fill and persist
		state.Phases[targetPhaseIdx].Status = model.PhaseStatusAwaitingFill
		rollbackPhaseFillState(state, targetPhaseIdx, input.Tasks, nameToID)
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		_ = sm.SaveState(state) // persist rollback to disk
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Activate phase
	state.Phases[targetPhaseIdx].Status = model.PhaseStatusActive
	state.Phases[targetPhaseIdx].ActivatedAt = &now
	state.PlanVersion++
	state.UpdatedAt = now

	if err := sm.SaveState(state); err != nil {
		state.Phases[targetPhaseIdx].Status = model.PhaseStatusAwaitingFill
		rollbackQueueEntries(opts.MaestroDir, input.Tasks, nameToID, assignMap)
		rollbackPhaseFillState(state, targetPhaseIdx, input.Tasks, nameToID)
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		_ = sm.SaveState(state) // persist rollback to disk
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
		data, err = io.ReadAll(os.Stdin)
	} else {
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

func insertSystemCommitTask(tasks []TaskInput) []TaskInput {
	allNames := make([]string, 0, len(tasks))
	for _, t := range tasks {
		allNames = append(allNames, t.Name)
	}

	commitTask := TaskInput{
		Name:               "__system_commit",
		Purpose:            "コマンド実行結果をリポジトリにコミットする",
		Content:            "変更ファイルを確認し、git add + git commit を実行する。コミットメッセージはタスク実行結果のサマリから生成する。",
		AcceptanceCriteria: "git commit が成功し、コミットハッシュが取得できる",
		BlockedBy:          allNames,
		BloomLevel:         2,
		Required:           true,
	}

	return append(tasks, commitTask)
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

func buildCommandState(commandID string, tasks []TaskInput, nameToID map[string]string, phases []model.Phase, now string) *model.CommandState {
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
				depIDs = append(depIDs, nameToID[depName])
			}
			state.TaskDependencies[taskID] = depIDs
		}

		if t.Name == "__system_commit" {
			systemCommitTaskID = &taskID
		}
	}

	state.SystemCommitTaskID = systemCommitTaskID
	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)
	return state
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

func writeQueueEntries(maestroDir string, assignments []WorkerAssignment, tasks []TaskInput, nameToID map[string]string, commandID string, now string) error {
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

	// Write to each worker's queue file
	for workerID, newTasks := range workerTasks {
		queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(workerID))

		var tq model.TaskQueue
		data, err := os.ReadFile(queueFile)
		if err == nil {
			if err := yamlv3.Unmarshal(data, &tq); err != nil {
				return fmt.Errorf("parse existing queue %s: %w", workerID, err)
			}
		}
		if tq.SchemaVersion == 0 {
			tq.SchemaVersion = 1
			tq.FileType = "queue_task"
		}

		tq.Tasks = append(tq.Tasks, newTasks...)

		if err := yamlutil.AtomicWrite(queueFile, tq); err != nil {
			return fmt.Errorf("write queue %s: %w", workerID, err)
		}
	}

	return nil
}

func rollbackQueueEntries(maestroDir string, tasks []TaskInput, nameToID map[string]string, assignMap map[string]WorkerAssignment) {
	taskIDs := make(map[string]bool)
	for _, t := range tasks {
		taskIDs[nameToID[t.Name]] = true
	}

	// Group by worker
	workerFiles := make(map[string]bool)
	for _, a := range assignMap {
		workerFiles[a.WorkerID] = true
	}

	for workerID := range workerFiles {
		queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(workerID))

		data, err := os.ReadFile(queueFile)
		if err != nil {
			continue
		}

		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			continue
		}

		var kept []model.Task
		for _, t := range tq.Tasks {
			if !taskIDs[t.ID] {
				kept = append(kept, t)
			}
		}
		tq.Tasks = kept

		_ = yamlutil.AtomicWrite(queueFile, tq)
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
		return nil // no queue file = not cancelled
	}

	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return nil
	}

	for _, cmd := range cq.Commands {
		if cmd.ID == commandID && cmd.Status == model.StatusCancelled {
			return fmt.Errorf("command %s has been cancelled in queue", commandID)
		}
	}
	return nil
}
