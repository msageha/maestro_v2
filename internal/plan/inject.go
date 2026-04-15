package plan

import (
	"fmt"
	"log"
	"os"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yaml "gopkg.in/yaml.v3"
)

// InjectOptions holds the configuration for injecting a new task into a sealed plan.
type InjectOptions struct {
	CommandID          string
	Purpose            string
	Content            string
	AcceptanceCriteria string
	Constraints        []string
	BlockedBy          []string // task IDs
	BloomLevel         int
	Required           bool
	ToolsHint          []string
	PersonaHint        string
	SkillRefs          []string
	TargetWorkerID     string
	IdempotencyKey     string
	MaestroDir         string
	Config             model.Config
	LockMap            *lock.MutexMap
}

// InjectResult contains the outcome of a task injection.
type InjectResult struct {
	TaskID       string `json:"task_id"`
	Worker       string `json:"worker"`
	Model        string `json:"model"`
	Deduplicated bool   `json:"deduplicated,omitempty"`
}

// AddTask injects a new task into an existing sealed plan. Unlike AddRetryTask,
// this does not replace an existing task — it adds a genuinely new task.
// Primary use case: conflict recovery, where the Planner needs to inject
// resolution tasks after merge_conflict detection.
func AddTask(opts InjectOptions) (*InjectResult, error) {
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

	if err := validateInjectRequest(state, opts); err != nil {
		return nil, err
	}

	// Idempotency check: if the same key was already used, return the existing task
	if opts.IdempotencyKey != "" && state.IdempotencyKeys != nil {
		if existingTaskID, ok := state.IdempotencyKeys[opts.IdempotencyKey]; ok {
			// Look up the assigned worker from queue files to populate the response
			worker, mdl := lookupTaskAssignment(opts.MaestroDir, existingTaskID, opts.Config.Agents.Workers)
			// If lookup fails (queue archived/cleaned), provide sensible defaults
			if worker == "" {
				worker = "unknown"
			}
			if mdl == "" {
				mdl = GetModelForBloomLevel(opts.BloomLevel, opts.Config.Agents.Workers.Boost)
			}
			return &InjectResult{
				TaskID:       existingTaskID,
				Worker:       worker,
				Model:        mdl,
				Deduplicated: true,
			}, nil
		}
	}

	// Assign worker
	var assignedWorkerID, assignedModel string
	if opts.TargetWorkerID != "" {
		// Validate the target worker exists in configuration
		found := false
		for i := 1; i <= opts.Config.Agents.Workers.Count; i++ {
			wID := fmt.Sprintf("worker%d", i)
			if wID == opts.TargetWorkerID {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("target worker %q not found in configured workers (count=%d)", opts.TargetWorkerID, opts.Config.Agents.Workers.Count)
		}
		assignedWorkerID = opts.TargetWorkerID
		assignedModel = GetWorkerModel(opts.TargetWorkerID, opts.Config.Agents.Workers)
	} else {
		workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
		if err != nil {
			return nil, fmt.Errorf("build worker states: %w", err)
		}
		assignReqs := []TaskAssignmentRequest{{Name: "__inject", BloomLevel: opts.BloomLevel}}
		assignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, workerStates, assignReqs)
		if err != nil {
			return nil, fmt.Errorf("worker assignment: %w", err)
		}
		assignedWorkerID = assignments[0].WorkerID
		assignedModel = assignments[0].Model
	}

	// Generate task ID
	newTaskID, err := model.NewTaskID(model.TaskIDCallerPlannerInject)
	if err != nil {
		return nil, fmt.Errorf("generate task ID: %w", err)
	}

	// Snapshot state for rollback
	origStateBytes, err := copyState(state)
	if err != nil {
		return nil, fmt.Errorf("copy state for rollback: %w", err)
	}

	// Update state
	if opts.Required {
		state.RequiredTaskIDs = append(state.RequiredTaskIDs, newTaskID)
	} else {
		state.OptionalTaskIDs = append(state.OptionalTaskIDs, newTaskID)
	}
	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)
	state.TaskStates[newTaskID] = model.StatusPending
	if len(opts.BlockedBy) > 0 {
		state.TaskDependencies[newTaskID] = opts.BlockedBy
	}

	// Add to phase: if blocked_by references exist, use the first dependency's phase;
	// otherwise, add to the current (first non-terminal) phase or phase 0.
	if len(opts.BlockedBy) > 0 {
		if phase, phaseIdx := findPhaseForTask(state, opts.BlockedBy[0]); phase != nil {
			state.Phases[phaseIdx].TaskIDs = append(state.Phases[phaseIdx].TaskIDs, newTaskID)
		}
	} else if len(state.Phases) > 0 {
		targetIdx := 0
		for i, p := range state.Phases {
			if !model.IsPhaseTerminal(p.Status) {
				targetIdx = i
				break
			}
		}
		state.Phases[targetIdx].TaskIDs = append(state.Phases[targetIdx].TaskIDs, newTaskID)
	}

	// Record idempotency key for deduplication on retry
	if opts.IdempotencyKey != "" {
		if state.IdempotencyKeys == nil {
			state.IdempotencyKeys = make(map[string]string)
		}
		state.IdempotencyKeys[opts.IdempotencyKey] = newTaskID
	}

	now := nowUTC()
	state.PlanVersion++
	state.UpdatedAt = now

	// Write queue entry
	task := retryQueueTask{
		taskID:             newTaskID,
		commandID:          opts.CommandID,
		purpose:            opts.Purpose,
		content:            opts.Content,
		acceptanceCriteria: opts.AcceptanceCriteria,
		constraints:        opts.Constraints,
		blockedBy:          opts.BlockedBy,
		bloomLevel:         opts.BloomLevel,
		toolsHint:          opts.ToolsHint,
		personaHint:        opts.PersonaHint,
		skillRefs:          opts.SkillRefs,
		workerID:           assignedWorkerID,
	}
	if err := writeRetryQueueEntry(opts.MaestroDir, task, now, opts.LockMap); err != nil {
		if rsErr := restoreState(state, origStateBytes); rsErr != nil {
			log.Printf("[ERROR] %v", rsErr)
		}
		return nil, fmt.Errorf("write queue entry: %w", err)
	}

	// Persist state
	if err := sm.SaveState(state); err != nil {
		// Rollback queue entry
		rollbackRetryQueueEntries(opts.MaestroDir, []retryQueueTask{task}, opts.LockMap)
		if rsErr := restoreState(state, origStateBytes); rsErr != nil {
			log.Printf("[ERROR] %v", rsErr)
		}
		return nil, fmt.Errorf("save state: %w", err)
	}

	return &InjectResult{
		TaskID: newTaskID,
		Worker: assignedWorkerID,
		Model:  assignedModel,
	}, nil
}

// validateInjectRequest checks preconditions for task injection.
func validateInjectRequest(state *model.CommandState, opts InjectOptions) error {
	if state.PlanStatus != model.PlanStatusSealed {
		return &planValidationError{Msg: fmt.Sprintf("plan_status must be sealed, got %s", state.PlanStatus)}
	}

	if err := ValidateNotCancelled(state); err != nil {
		return err
	}

	if opts.BloomLevel < BloomLevelMin || opts.BloomLevel > BloomLevelMax {
		return &planValidationError{Msg: fmt.Sprintf("bloom_level must be between %d and %d, got %d", BloomLevelMin, BloomLevelMax, opts.BloomLevel)}
	}

	if opts.Purpose == "" {
		return &planValidationError{Msg: "purpose is required"}
	}
	if opts.Content == "" {
		return &planValidationError{Msg: "content is required"}
	}
	if opts.AcceptanceCriteria == "" {
		return &planValidationError{Msg: "acceptance_criteria is required"}
	}

	// Validate blocked_by references exist in state
	for _, dep := range opts.BlockedBy {
		if _, ok := state.TaskStates[dep]; !ok {
			return &planValidationError{Msg: fmt.Sprintf("blocked_by task %s not found in command state", dep)}
		}
	}

	return nil
}

// lookupTaskAssignment finds the worker and model assigned to a task by scanning queue files.
// Used for idempotency dedup responses where the task already exists.
func lookupTaskAssignment(maestroDir string, taskID string, workers model.WorkerConfig) (string, string) {
	for i := 1; i <= workers.Count; i++ {
		wID := fmt.Sprintf("worker%d", i)
		queueFile := fmt.Sprintf("%s/queue/%s.yaml", maestroDir, wID)
		data, err := os.ReadFile(queueFile)
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if err := yaml.Unmarshal(data, &tq); err != nil {
			continue
		}
		for _, task := range tq.Tasks {
			if task.ID == taskID {
				return wID, GetWorkerModel(wID, workers)
			}
		}
	}
	return "", ""
}
