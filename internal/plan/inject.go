package plan

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	yaml "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// hasIntegrationCleanedUp reports whether the command's integration was
// taken to a terminal post-publish state (Published or Cleanup-equivalent).
// Used by validateInjectRequest to distinguish "worktree mode off" from
// "worktree state cleaned up after publish" when the state file is absent.
//
// PlanStatusSealed remains the live state until the deferred completion
// handler observes publish success and flips it to Completed; the publish /
// cleanup flow can therefore retire the integration worktree while the
// command is still nominally sealed. To catch that window the cleanup
// detection considers any phase having reached a phase-terminal state on a
// sealed plan as evidence that the integration worktree is gone — the
// post-publish reconciler removes worktree state regardless of the eventual
// PlanStatus transition.
func hasIntegrationCleanedUp(state *model.CommandState) bool {
	if state == nil {
		return false
	}
	if state.PlanStatus == model.PlanStatusCompleted {
		return true
	}
	// During the publish→completion window plan_status is still sealed but
	// every phase is terminal. Treat that as "integration cleaned up" so
	// post-publish RunOnIntegration/RunOnMain injections fail fast at the
	// API boundary instead of dead-lettering on
	// integration_branch_check_failed.
	if state.PlanStatus == model.PlanStatusSealed && len(state.Phases) > 0 {
		allTerminal := true
		for _, p := range state.Phases {
			if !model.IsPhaseTerminal(p.Status) {
				allTerminal = false
				break
			}
		}
		if allTerminal {
			return true
		}
	}
	return false
}

// InjectOptions holds the configuration for injecting a new task into a sealed plan.
type InjectOptions struct {
	CommandID          string
	Purpose            string
	Content            string
	AcceptanceCriteria string
	DefinitionOfDone   []string
	Constraints        []string
	BlockedBy          []string // task IDs
	BloomLevel         int
	Required           bool
	ToolsHint          []string
	PersonaHint        string
	SkillRefs          []string
	ExpectedPaths      []string
	DefinitionOfAbort  *model.DefinitionOfAbort
	TargetWorkerID     string
	TargetPhase        string // phase ID to place the task in; overrides default fallback logic
	IdempotencyKey     string
	RunOnMain          bool // run task in main branch dir instead of worker worktree
	RunOnIntegration   bool // run task in integration worktree (for publish_conflict resolution)
	MaestroDir         string
	Config             model.Config
	LockMap            *lock.MutexMap
	ModelSelector      ModelSelector // optional: adaptive model selection
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
		if opts.RunOnMain && !isClaudeRuntimeModel(assignedModel) {
			return nil, &planValidationError{Msg: fmt.Sprintf(
				"worker %q runs model %q (non-claude runtime); run_on_main tasks require a claude-code worker because only claude-code enforces the read-only main guard",
				opts.TargetWorkerID, assignedModel)}
		}
	} else {
		workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
		if err != nil {
			return nil, fmt.Errorf("build worker states: %w", err)
		}
		assignReqs := []TaskAssignmentRequest{{Name: "__inject", BloomLevel: opts.BloomLevel, RequireClaudeRuntime: opts.RunOnMain}}
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

	sm.UnlockCommand(opts.CommandID)
	unlockQueues := lockWorkerQueues(opts.LockMap, []string{assignedWorkerID})
	defer unlockQueues()
	sm.LockCommand(opts.CommandID)

	state, err = sm.LoadState(opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("reload state: %w", err)
	}
	if err := validateInjectRequest(state, opts); err != nil {
		return nil, err
	}
	if opts.IdempotencyKey != "" && state.IdempotencyKeys != nil {
		if existingTaskID, ok := state.IdempotencyKeys[opts.IdempotencyKey]; ok {
			worker, mdl := lookupTaskAssignment(opts.MaestroDir, existingTaskID, opts.Config.Agents.Workers)
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

	// max_tasks enforcement at injection time. RunOnMain / RunOnIntegration
	// injections bypass the cap because they are recovery operations
	// (post-publish verification, publish_conflict resolution) — the
	// Planner *must* be able to insert them even when the phase has
	// reached its declared parallelism budget. Ordinary new tasks are
	// hard-rejected so a Planner that emits add-task in a loop after
	// the phase went active does not silently inflate the phase past
	// its contract (Report 2026-05-03 issue-3 observed cycle2 receiving 5
	// tasks against max_tasks=2).
	//
	// The Planner sees a clear validation error and can either:
	//   - re-balance scope across remaining phases
	//   - submit an add-retry-task instead (replaces an existing task)
	//   - issue a fresh command with a revised plan
	// Each of those options preserves autonomous flow without letting
	// the constraint quietly degrade.
	if selectedPhaseIdx >= 0 && state.Phases[selectedPhaseIdx].Constraints != nil {
		maxT := state.Phases[selectedPhaseIdx].Constraints.MaxTasks
		if maxT > 0 && len(state.Phases[selectedPhaseIdx].TaskIDs) > maxT {
			recoveryExempt := opts.RunOnMain || opts.RunOnIntegration
			if recoveryExempt {
				slog.Warn("add-task exceeds declared max_tasks (recovery exempt)",
					"phase_id", state.Phases[selectedPhaseIdx].PhaseID,
					"task_count_after_injection", len(state.Phases[selectedPhaseIdx].TaskIDs),
					"max_tasks", maxT,
					"new_task_id", newTaskID,
					"exemption_reason", "run_on_main_or_run_on_integration_recovery")
			} else {
				// Compute existing count BEFORE restoreState. After
				// restoreState the in-memory state has been reverted to
				// the pre-append snapshot, so `len(...)` is the original
				// count. Using `len(...) - 1` after restoreState produced
				// an off-by-one in the error message (Report 2026-05-04
				// observed "already contains 1 task(s)" on a phase with 2).
				// We reload from origStateBytes for the count instead of
				// trusting the in-memory state, which decouples the count
				// from whichever side of restoreState the message is
				// rendered on.
				existingCount := len(state.Phases[selectedPhaseIdx].TaskIDs) - 1
				if rsErr := restoreState(state, origStateBytes); rsErr != nil {
					slog.Error("state restore failed during max_tasks rejection", "error", rsErr)
				}
				return nil, &planValidationError{Msg: fmt.Sprintf(
					"phase %q has constraints.max_tasks=%d and already contains %d task(s); injecting another would exceed the cap. Either replace an existing task via add-retry-task, target a different phase via --target-phase, or submit a fresh command with a revised plan.",
					state.Phases[selectedPhaseIdx].PhaseID, maxT, existingCount)}
			}
		}
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

	// Write queue entry. RunOnIntegration タスクは publish/merge 競合解決を
	// integration worktree 上で行うため repair バケットに分類する。RunOnMain は
	// input.go godoc で "read-only verification tasks that must evaluate the
	// merged state on the main branch" と定義されているため verify バケットに
	// 分類する。両方 false のオペレータ手動注入タスクは未分類のまま
	// (OpUnknown = 常時 admit)。
	opType := ""
	switch {
	case opts.RunOnMain:
		opType = model.OperationTypeVerify
	case opts.RunOnIntegration:
		opType = model.OperationTypeRepair
	}
	task := retryQueueTask{
		taskID:             newTaskID,
		commandID:          opts.CommandID,
		purpose:            opts.Purpose,
		content:            opts.Content,
		acceptanceCriteria: opts.AcceptanceCriteria,
		definitionOfDone:   opts.DefinitionOfDone,
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
	if err := writeRetryQueueEntry(opts.MaestroDir, task, now, nil); err != nil {
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

// validateRunOnMainPublishGate reads the worktree state file and rejects a
// run_on_main injection unless the integration branch has been published to
// base. Fail-closed: an unreadable or unparsable state file rejects the
// injection (the Planner can retry; dispatching against unknown publish
// state risks verifying stale main).
func validateRunOnMainPublishGate(worktreeStatePath, commandID string) error {
	data, err := os.ReadFile(worktreeStatePath) //nolint:gosec // controlled application state path
	if err != nil {
		return &planValidationError{Msg: fmt.Sprintf(
			"cannot inject run_on_main task into command %s: worktree state is unreadable (%v). Retry once the daemon has settled the integration state.",
			commandID, err)}
	}
	var ws model.WorktreeCommandState
	if err := yaml.Unmarshal(data, &ws); err != nil {
		return &planValidationError{Msg: fmt.Sprintf(
			"cannot inject run_on_main task into command %s: worktree state is unparsable (%v). Retry once the daemon has settled the integration state.",
			commandID, err)}
	}
	if ws.Integration.Status != model.IntegrationStatusPublished {
		return &planValidationError{Msg: fmt.Sprintf(
			"cannot inject run_on_main task into command %s: integration status is %q, not published. "+
				"run_on_main tasks verify the published main branch — wait for the publish_completed notification, "+
				"or submit a fresh verification command after publish. "+
				"For pre-publish verification of the merged integration branch use --run-on-integration instead.",
			commandID, ws.Integration.Status)}
	}
	return nil
}

// validateInjectRequest checks preconditions for task injection.
func validateInjectRequest(state *model.CommandState, opts InjectOptions) error {
	if state.PlanStatus != model.PlanStatusSealed {
		return &planValidationError{Msg: fmt.Sprintf("plan_status must be sealed, got %s", state.PlanStatus)}
	}

	// RunOnIntegration / RunOnMain tasks dispatch against the integration
	// worktree or the published main branch. Once the worktree state has
	// been cleaned up (publish completed → cleanup ran), the integration
	// worktree no longer exists; injecting such a task would dispatch into
	// missing state, exhaust retry attempts on `integration_branch_check_failed`,
	// dead-letter, and flip plan_status to failed (Report 2026-05-03 issue-2).
	// Reject the injection at the API boundary so the Planner sees a clear
	// validation error instead of a delayed dead-letter cascade. The
	// post-publish recovery story is "submit a fresh command", not
	// "extend a finished plan".
	if opts.RunOnIntegration || opts.RunOnMain {
		// RunOnIntegration/RunOnMain require an integration worktree. If
		// worktree mode is not enabled for this formation, the dispatcher
		// has nowhere to place the task and would dead-letter on
		// integration_branch_check_failed. Reject at the boundary so the
		// Planner sees a clear validation error rather than a delayed
		// cascade that flips plan_status to failed (Report 2026-05-03 issue-2).
		if !opts.Config.Worktree.Enabled {
			return &planValidationError{Msg: fmt.Sprintf(
				"cannot inject run_on_integration/run_on_main task into command %s: worktree mode is disabled for this formation. Enable worktree.enabled in config.yaml or submit a regular task without --run-on-integration/--run-on-main.",
				opts.CommandID)}
		}
		if opts.MaestroDir != "" && opts.CommandID != "" {
			worktreeStatePath := filepath.Join(opts.MaestroDir, "state", "worktrees", opts.CommandID+".yaml")
			_, statErr := os.Stat(worktreeStatePath)
			switch {
			case statErr != nil && os.IsNotExist(statErr):
				// worktree state absent on a worktree-enabled formation
				// can only mean cleanup ran after publish (the
				// reconciler removes the file once integration is
				// retired). Either way the dispatcher has nothing to
				// run against — reject explicitly. hasIntegrationCleanedUp
				// also catches the publish→completion window where
				// plan_status is still sealed but every phase is terminal.
				if model.IsPlanTerminal(state.PlanStatus) || hasIntegrationCleanedUp(state) {
					return &planValidationError{Msg: fmt.Sprintf(
						"cannot inject run_on_integration/run_on_main task into command %s: integration worktree has been cleaned up after publish. Submit a fresh command rather than extending a published plan.",
						opts.CommandID)}
				}
				return &planValidationError{Msg: fmt.Sprintf(
					"cannot inject run_on_integration/run_on_main task into command %s: integration worktree state file is missing. The integration worktree may have been cleaned up; submit a fresh command instead.",
					opts.CommandID)}
			case opts.RunOnMain:
				// run_on_main tasks inspect the published main branch. While
				// the integration is still pre-publish, the merged outputs of
				// this command are not on main yet: dispatching now would
				// verify stale main and fail spuriously, and the new pending
				// task would simultaneously block the publish gate (which
				// waits for every task to terminate) — a deadlock. Enforce
				// the ordering mechanically instead of trusting the Planner
				// to wait for the publish_completed notification.
				//
				// This case is also reached when os.Stat failed with a
				// non-NotExist error (permissions, IO): the gate re-reads
				// the file itself and fails closed on any read error, so a
				// transient blip cannot silently skip the ordering check.
				if err := validateRunOnMainPublishGate(worktreeStatePath, opts.CommandID); err != nil {
					return err
				}
			}
		}
	}

	// CompletionPolicy.AllowDynamicTasks intentionally NOT enforced here:
	// the bool's zero value (false) does not unambiguously distinguish
	// "operator declared false" from "field absent in YAML / older state",
	// and gating injection on the zero value would break every legacy
	// state file. A schema-level fix (e.g. `DisallowDynamicTasks bool`,
	// default false=allow, or a tri-state pointer) is the right way to
	// honour the planner's declared intent and is filed as a follow-up.
	// Until then, the max_tasks gate added below is the load-bearing
	// constraint that prevents runaway phase growth.

	if err := ValidateNotCancelled(state); err != nil {
		return err
	}

	if opts.BloomLevel < BloomLevelMin || opts.BloomLevel > BloomLevelMax {
		return &planValidationError{Msg: fmt.Sprintf("bloom_level must be between %d and %d, got %d", BloomLevelMin, BloomLevelMax, opts.BloomLevel)}
	}

	if opts.Purpose == "" {
		return &planValidationError{Msg: "purpose is required (pass --purpose <text> or --purpose-file -)"}
	}
	if opts.Content == "" {
		return &planValidationError{Msg: "content is required (pass --content <text> or --content-file -)"}
	}
	// Hint at the stdin form so a Planner whose shell quoting stripped a
	// multi-line --acceptance-criteria value picks the safe path on retry
	// instead of guessing more shell-quoting permutations.
	if opts.AcceptanceCriteria == "" {
		return &planValidationError{Msg: "acceptance_criteria is required (pass --acceptance-criteria <text> or --acceptance-criteria-file -)"}
	}

	// Sanity-check minimum lengths for add-task-injected fields. Shell
	// backtick / $() expansion inside broken double-quoted invocations can
	// reduce content / acceptance_criteria to a few bytes; rejecting
	// obviously-truncated payloads prevents the Planner from spawning a
	// repair loop of malformed tasks. Thresholds are intentionally lax so
	// only clearly damaged input trips them; legitimate terse descriptions
	// still pass.
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

	if err := validateInjectedSchemaFields(opts.ExpectedPaths, opts.DefinitionOfAbort, opts.DefinitionOfDone); err != nil {
		return err
	}

	return nil
}

// validateInjectedSchemaFields enforces that expected_paths and
// definition_of_abort are present and well-formed. Shared by add-task and
// add-retry-task entry points.
func validateInjectedSchemaFields(expectedPaths []string, doa *model.DefinitionOfAbort, definitionOfDone []string) error {
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
	validateDefinitionOfDone(definitionOfDone, "definition_of_done", errs)
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
