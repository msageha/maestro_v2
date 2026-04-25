package plan

import (
	"fmt"
	"log/slog"
	"os"

	yaml "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
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
	// REQUIREMENTS.md §S3-1: every task MUST declare expected_paths and
	// definition_of_abort. These are required at injection time the same as
	// they are required for tasks introduced via plan submit.
	ExpectedPaths     []string
	DefinitionOfAbort *model.DefinitionOfAbort
	TargetWorkerID    string
	TargetPhase       string // phase ID to place the task in; overrides default fallback logic
	IdempotencyKey    string
	RunOnMain         bool // run task in main branch dir instead of worker worktree
	RunOnIntegration  bool // run task in integration worktree (for publish_conflict resolution)
	MaestroDir        string
	Config            model.Config
	LockMap           *lock.MutexMap
	ModelSelector     ModelSelector // optional: adaptive model selection
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
		assignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, workerStates, assignReqs, WithModelSelector(opts.ModelSelector))
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
	// §2.1: injected tasks enter the lifecycle at `planned`; AdvanceTaskState
	// walks them through ready / dispatched / running on dispatch and result.
	state.TaskStates[newTaskID] = model.StatusPlanned
	if len(opts.BlockedBy) > 0 {
		state.TaskDependencies[newTaskID] = opts.BlockedBy
	}

	// Add to phase:
	// 1. If TargetPhase is specified, place into that phase (conflict resolution use case).
	// 2. If blocked_by references exist, use the first dependency's phase.
	// 3. If TargetWorkerID is set, find the latest phase containing that worker's
	//    existing tasks (conflict resolution fallback — the task should land in the
	//    same phase where the conflict originated).
	// 4. Otherwise, add to the current (first non-terminal) phase or phase 0.
	selectedPhaseIdx := -1
	if opts.TargetPhase != "" {
		// TargetPhase is validated in validateInjectRequest, so PhaseIndex will always succeed here.
		if phaseIdx, ok := state.PhaseIndex(opts.TargetPhase); ok {
			state.Phases[phaseIdx].TaskIDs = append(state.Phases[phaseIdx].TaskIDs, newTaskID)
			selectedPhaseIdx = phaseIdx
		}
	} else if len(opts.BlockedBy) > 0 {
		if phase, phaseIdx := findPhaseForTask(state, opts.BlockedBy[0]); phase != nil {
			state.Phases[phaseIdx].TaskIDs = append(state.Phases[phaseIdx].TaskIDs, newTaskID)
			selectedPhaseIdx = phaseIdx
		}
	} else if opts.TargetWorkerID != "" {
		if phaseIdx := findPhaseForWorker(state, opts.MaestroDir, opts.TargetWorkerID); phaseIdx >= 0 {
			state.Phases[phaseIdx].TaskIDs = append(state.Phases[phaseIdx].TaskIDs, newTaskID)
			selectedPhaseIdx = phaseIdx
		} else if len(state.Phases) > 0 {
			// Worker has no existing tasks; fall through to generic fallback.
			targetIdx, err := findFirstNonTerminalPhase(state.Phases)
			if err != nil {
				if !opts.RunOnMain && !opts.RunOnIntegration {
					return nil, err
				}
				targetIdx = len(state.Phases) - 1
			}
			state.Phases[targetIdx].TaskIDs = append(state.Phases[targetIdx].TaskIDs, newTaskID)
			selectedPhaseIdx = targetIdx
		}
	} else if len(state.Phases) > 0 {
		targetIdx, err := findFirstNonTerminalPhase(state.Phases)
		if err != nil {
			if !opts.RunOnMain && !opts.RunOnIntegration {
				return nil, err
			}
			// RunOnMain post-publish verification / RunOnIntegration publish_conflict
			// resolution: append to the last phase (it will be reopened below).
			// This allows the Planner to add recovery tasks after all phases are
			// terminal without needing an explicit --target-phase.
			targetIdx = len(state.Phases) - 1
		}
		state.Phases[targetIdx].TaskIDs = append(state.Phases[targetIdx].TaskIDs, newTaskID)
		selectedPhaseIdx = targetIdx
	}

	// If the task was injected into a completed phase, reopen it so that
	// the new pending task is properly tracked by the phase lifecycle.
	if selectedPhaseIdx >= 0 && state.Phases[selectedPhaseIdx].Status == model.PhaseStatusCompleted {
		reopenedAt := nowUTC()
		state.Phases[selectedPhaseIdx].Status = model.PhaseStatusActive
		state.Phases[selectedPhaseIdx].CompletedAt = nil
		state.Phases[selectedPhaseIdx].ReopenedAt = &reopenedAt
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

	// Write queue entry. §S0-1: tasks injected to resolve publish/merge
	// conflicts run on the integration worktree and are classified as rollout
	// operations for admission control. RunOnMain は input.go godoc で
	// "read-only verification tasks that must evaluate the merged state on the
	// main branch" と定義されているため verify バケットに分類する。両方 false の
	// オペレータ手動注入タスクは未分類のまま (OpUnknown = 常時 admit)。
	opType := ""
	switch {
	case opts.RunOnMain:
		opType = model.OperationTypeVerify
	case opts.RunOnIntegration:
		opType = model.OperationTypeRollout
	}
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
		expectedPaths:      opts.ExpectedPaths,
		definitionOfAbort:  opts.DefinitionOfAbort,
		workerID:           assignedWorkerID,
		runOnMain:          opts.RunOnMain,
		runOnIntegration:   opts.RunOnIntegration,
		operationType:      opType,
	}
	if err := writeRetryQueueEntry(opts.MaestroDir, task, now, opts.LockMap); err != nil {
		if rsErr := restoreState(state, origStateBytes); rsErr != nil {
			slog.Error("state restore failed", "error", rsErr)
		}
		return nil, fmt.Errorf("write queue entry: %w", err)
	}

	// Persist state
	if err := sm.SaveState(state); err != nil {
		// Rollback queue entry
		rollbackRetryQueueEntries(opts.MaestroDir, []retryQueueTask{task}, opts.LockMap)
		if rsErr := restoreState(state, origStateBytes); rsErr != nil {
			slog.Error("state restore failed", "error", rsErr)
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

	// Bug G: sanity-check minimum lengths for add-task-injected fields.
	// An earlier incident showed the Planner submitting an add-task with
	// content / acceptance_criteria reduced to a few bytes (or empty) by
	// shell backtick expansion inside a broken double-quoted `--content`
	// invocation. The CLI's non-empty check let this through, producing a
	// corrupted queue entry. Rejecting obviously-truncated payloads here
	// prevents the Planner from accidentally spawning a repair loop of
	// malformed tasks. Thresholds are intentionally lax so only clearly
	// damaged input trips them; legitimate terse descriptions still pass.
	if len(opts.Purpose) < MinInjectedPurposeBytes {
		return &planValidationError{Msg: fmt.Sprintf(
			"purpose is too short (%d bytes, minimum %d): check shell quoting on the invocation — backticks and `$()` inside double quotes are expanded before being sent",
			len(opts.Purpose), MinInjectedPurposeBytes)}
	}
	if len(opts.Content) < MinInjectedContentBytes {
		return &planValidationError{Msg: fmt.Sprintf(
			"content is too short (%d bytes, minimum %d): check shell quoting on the invocation — backticks and `$()` inside double quotes are expanded before being sent",
			len(opts.Content), MinInjectedContentBytes)}
	}
	if len(opts.AcceptanceCriteria) < MinInjectedAcceptanceCriteriaBytes {
		return &planValidationError{Msg: fmt.Sprintf(
			"acceptance_criteria is too short (%d bytes, minimum %d): check shell quoting on the invocation — backticks and `$()` inside double quotes are expanded before being sent",
			len(opts.AcceptanceCriteria), MinInjectedAcceptanceCriteriaBytes)}
	}

	// Validate blocked_by references exist in state
	for _, dep := range opts.BlockedBy {
		if _, ok := state.TaskStates[dep]; !ok {
			return &planValidationError{Msg: fmt.Sprintf("blocked_by task %s not found in command state", dep)}
		}
	}

	// Validate target_phase exists in state
	if opts.TargetPhase != "" {
		if _, ok := state.PhaseIndex(opts.TargetPhase); !ok {
			return &planValidationError{Msg: fmt.Sprintf("target_phase %s not found in command state", opts.TargetPhase)}
		}
	}

	// REQUIREMENTS.md §S3-1: tasks injected via add-task MUST declare
	// expected_paths and definition_of_abort, the same as tasks submitted via
	// plan submit. Earlier the CLI did not surface these flags so injected
	// tasks bypassed the schema; that is now enforced here so the daemon
	// rejects malformed input even when called from non-CLI clients.
	if err := validateInjectedSchemaFields(opts.ExpectedPaths, opts.DefinitionOfAbort); err != nil {
		return err
	}

	return nil
}

// validateInjectedSchemaFields enforces that expected_paths and
// definition_of_abort are present and well-formed. Shared by add-task and
// add-retry-task entry points.
//
// REQUIREMENTS.md §S3-1: an empty slice is treated the same as a missing
// field — Path-overlap heuristic (§A-4) cannot reason about a task that
// claims to touch nothing, so the API must reject both nil and []string{}.
func validateInjectedSchemaFields(expectedPaths []string, doa *model.DefinitionOfAbort) error {
	errs := &ValidationErrors{}
	if expectedPaths == nil {
		errs.Add("expected_paths", "required field is missing")
	} else if len(expectedPaths) == 0 {
		errs.Add("expected_paths", "must contain at least one path")
	} else {
		validateExpectedPaths(expectedPaths, "expected_paths", errs)
	}
	if doa == nil {
		errs.Add("definition_of_abort", "required field is missing")
	} else {
		validateDefinitionOfAbort(doa, "definition_of_abort", errs)
	}
	if errs.HasErrors() {
		return &planValidationError{Msg: errs.Error()}
	}
	return nil
}

// findPhaseForWorker scans the target worker's queue file for tasks belonging
// to the same command and returns the index of the latest phase that contains
// one of those tasks. Returns -1 if no match is found.
// This is used as a fallback when TargetPhase and BlockedBy are both unset but
// TargetWorkerID is specified (e.g. conflict resolution tasks).
func findPhaseForWorker(state *model.CommandState, maestroDir string, workerID string) int {
	queueFile := fmt.Sprintf("%s/queue/%s.yaml", maestroDir, workerID)
	// queueFile is built from a controlled application directory + worker ID.
	data, err := os.ReadFile(queueFile) //nolint:gosec // controlled path
	if err != nil {
		return -1
	}
	var tq model.TaskQueue
	if err := yaml.Unmarshal(data, &tq); err != nil {
		return -1
	}
	workerTaskIDs := make(map[string]struct{})
	for _, task := range tq.Tasks {
		if task.CommandID == state.CommandID {
			workerTaskIDs[task.ID] = struct{}{}
		}
	}
	if len(workerTaskIDs) == 0 {
		return -1
	}
	bestIdx := -1
	for i, phase := range state.Phases {
		for _, taskID := range phase.TaskIDs {
			if _, ok := workerTaskIDs[taskID]; ok {
				bestIdx = i
				break
			}
		}
	}
	return bestIdx
}

// findFirstNonTerminalPhase returns the index of the first non-terminal phase.
// Returns an error if all phases are terminal.
func findFirstNonTerminalPhase(phases []model.Phase) (int, error) {
	for i, p := range phases {
		if !model.IsPhaseTerminal(p.Status) {
			return i, nil
		}
	}
	return -1, &planValidationError{Msg: "all phases are terminal; cannot add task without explicit target_phase"}
}

// lookupTaskAssignment finds the worker and model assigned to a task by scanning queue files.
// Used for idempotency dedup responses where the task already exists.
func lookupTaskAssignment(maestroDir string, taskID string, workers model.WorkerConfig) (string, string) {
	for i := 1; i <= workers.Count; i++ {
		wID := fmt.Sprintf("worker%d", i)
		queueFile := fmt.Sprintf("%s/queue/%s.yaml", maestroDir, wID)
		// queueFile is built from a controlled application directory + worker ID.
		data, err := os.ReadFile(queueFile) //nolint:gosec // controlled path
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
