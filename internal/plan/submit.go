package plan

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// SubmitOptions holds the configuration for a plan submission operation.
type SubmitOptions struct {
	CommandID     string
	TasksFile     string // path or "-" for stdin
	TasksData     []byte // inline YAML data; takes precedence over TasksFile when non-empty
	PhaseName     string // non-empty for phase fill
	DryRun        bool
	MaestroDir    string
	Config        model.Config
	LockMap       *lock.MutexMap
	ModelSelector ModelSelector // optional: adaptive model selection
	// RequireVerifySnapshot enforces the daemon contract that Planner writes a
	// command-scoped verify config before submitting executable work.
	RequireVerifySnapshot bool
}

// SubmitResult contains the output of a successful plan submission.
type SubmitResult struct {
	Valid     bool                `json:"valid,omitempty"`
	CommandID string              `json:"command_id,omitempty"`
	Tasks     []SubmitTaskResult  `json:"tasks,omitempty"`
	Phases    []SubmitPhaseResult `json:"phases,omitempty"`
	// Warnings carries advisory, non-blocking notices (e.g. a run_on_main
	// verification command submitted while other commands still hold
	// unpublished integration state). Rendered verbatim to the Planner via
	// the CLI JSON response.
	Warnings []string `json:"warnings,omitempty"`
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

	// Route phase-fill submissions BEFORE the generic empty-tasks check so
	// that submitPhaseFill (which loads state under lock) can produce a
	// state-aware diagnostic — e.g. "phase X status must be awaiting_fill,
	// got filling" — when a stale awaiting_fill signal triggers a duplicate
	// re-submit. Surfacing the structured planValidationError instead of a
	// bare "either tasks or phases must be specified" → ErrCodeInternal lets
	// the Planner distinguish "system error" from "request was redundant
	// because the phase has already moved on".
	if opts.PhaseName != "" {
		if err := validateRequiredVerifySnapshot(opts); err != nil {
			return nil, err
		}
		res, err := submitPhaseFill(opts, *input)
		if err == nil && res != nil {
			// A/B fan-out is additive: runs after the submit committed,
			// outside its locks, and only ever appends warnings.
			sm := NewStateManager(opts.MaestroDir, opts.LockMap)
			res.Warnings = append(res.Warnings,
				maybeCreateABCandidates(opts, sm, res, collectPinnedTaskNames(*input))...)
		}
		return res, err
	}

	if len(input.Tasks) > 0 && len(input.Phases) > 0 {
		return nil, &planValidationError{Msg: "tasks and phases are mutually exclusive"}
	}
	if len(input.Tasks) == 0 && len(input.Phases) == 0 {
		return nil, &planValidationError{Msg: "either tasks or phases must be specified"}
	}
	if err := validateRequiredVerifySnapshot(opts); err != nil {
		return nil, err
	}

	return submitInitial(opts, *input)
}

func validateRequiredVerifySnapshot(opts SubmitOptions) error {
	if opts.DryRun || !opts.RequireVerifySnapshot || !opts.Config.Verify.EffectiveEnabled() {
		return nil
	}
	path := filepath.Join(opts.MaestroDir, "state", "verify", opts.CommandID+".yaml")
	cfg, err := model.LoadVerifyConfig(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Include both the exact CLI form and a stdin form so the
			// planner's recovery path doesn't require operator lookups. The
			// stdin variant matches the daemonapi VerifyWrite contract.
			return &planValidationError{Msg: fmt.Sprintf(
				"verify config snapshot is required before plan submit for command %s.\n"+
					"  Quick fix: maestro verify write --command-id %s --config-file <verify.yaml>\n"+
					"  Or via stdin: cat verify.yaml | maestro verify write --command-id %s --config-file -",
				opts.CommandID, opts.CommandID, opts.CommandID)}
		}
		return &planValidationError{Msg: fmt.Sprintf(
			"verify config snapshot is invalid for command %s: %v\n"+
				"  Re-run: maestro verify write --command-id %s --config-file <verify.yaml>",
			opts.CommandID, err, opts.CommandID)}
	}
	if cfg.IsEmpty() {
		return &planValidationError{Msg: fmt.Sprintf(
			"verify config snapshot for command %s must contain at least one command.\n"+
				"  Add at least one entry under verify.build / verify.lint / verify.typecheck / verify.test\n"+
				"  then re-run: maestro verify write --command-id %s --config-file <verify.yaml>",
			opts.CommandID, opts.CommandID)}
	}
	return nil
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
		assignReqs = append(assignReqs, TaskAssignmentRequest{
			Name:                 t.Name,
			BloomLevel:           t.BloomLevel,
			PinnedWorkerID:       t.WorkerID,
			RequireClaudeRuntime: t.RunOnMain,
		})
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

	// TOCTOU fix: Acquire file-level lock for cross-process double-submit
	// prevention. The in-process MutexMap above protects within a single daemon;
	// this flock protects against concurrent CLI invocations submitting the
	// same command_id.
	stateFlock, flockErr := acquireStateFlock(opts.MaestroDir, opts.CommandID)
	if flockErr != nil {
		return nil, flockErr
	}
	defer releaseFlock(stateFlock)

	// Route by input type
	var res *SubmitResult
	var err error
	if len(input.Phases) > 0 {
		res, err = submitInitialPhases(opts, input.Phases, sm)
	} else {
		res, err = submitInitialTasks(opts, input.Tasks, sm)
	}
	if err == nil && res != nil {
		// A/B fan-out is additive: runs after the submit result is
		// committed, re-acquires queue→state locks per task (the
		// submit-scope file lock may still be held — the lock ORDER is
		// unchanged), and only ever appends warnings.
		res.Warnings = append(res.Warnings,
			maybeCreateABCandidates(opts, sm, res, collectPinnedTaskNames(input))...)
	}
	return res, err
}

func lockInitialStateForWrite(opts SubmitOptions, sm *StateManager) (func(), error) {
	if err := checkCommandNotCancelled(opts.MaestroDir, opts.CommandID); err != nil {
		return nil, err
	}
	sm.LockCommand(opts.CommandID)
	unlock := func() { sm.UnlockCommand(opts.CommandID) }
	if err := checkCommandNotCancelled(opts.MaestroDir, opts.CommandID); err != nil {
		unlock()
		return nil, err
	}
	if sm.StateExists(opts.CommandID) {
		unlock()
		return nil, fmt.Errorf("%w: state already exists for command %s", ErrDoubleSubmit, opts.CommandID)
	}
	return unlock, nil
}

// rollbackStateAndQueue performs the common rollback sequence for initial submissions:
// remove partial queue entries, then delete the state file.
// Returns a combined error if any rollback step fails.
func rollbackStateAndQueueLocked(sm stateStore, maestroDir string, commandID string, tasks []TaskInput, nameToID map[string]string, assignMap map[string]WorkerAssignment) error {
	var errs []error
	if queueErr := rollbackQueueEntriesLocked(maestroDir, tasks, nameToID, assignMap); queueErr != nil {
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
	// A failed plan_submit returns the phase to awaiting_fill; restart the
	// watchdog clock from this moment so a Planner that keeps producing
	// invalid task lists is detected on the same elapsed schedule as a
	// Planner that never started filling. AwaitingFillStallNotifiedAt is
	// reset so the next watchdog check can re-emit a fresh signal.
	now := nowUTC()
	state.Phases[phaseIdx].AwaitingFillSince = &now
	state.Phases[phaseIdx].AwaitingFillStallNotifiedAt = nil
	state.UpdatedAt = now
	if saveErr := sm.SaveState(state); saveErr != nil {
		return fmt.Errorf("rollback: save state for command %s: %w", commandID, saveErr)
	}
	return nil
}

// rollbackFullPhaseFill reverts queue entries, phase state, and task state
// additions, then persists the rolled-back state.
//
// Lock-order contract: the caller must NOT hold the state lock when calling
// this function — the queue rollback acquires `queue:<worker>` locks, and
// the canonical order is queue → state (lock.go). Holding state while
// taking queue locks here used to ABBA-deadlock against AddRetryTask
// (which locks every worker queue before the state). This function takes
// the state lock itself for the state-revert half.
func rollbackFullPhaseFill(sm *StateManager, state *model.CommandState, phaseIdx int, opts SubmitOptions, tasks []TaskInput, nameToID map[string]string, assignMap map[string]WorkerAssignment) error {
	var errs []error
	if queueErr := rollbackQueueEntries(opts.MaestroDir, tasks, nameToID, assignMap, opts.LockMap); queueErr != nil {
		errs = append(errs, fmt.Errorf("rollback: queue entries for command %s: %w", opts.CommandID, queueErr))
	}
	phaseID := state.Phases[phaseIdx].PhaseID

	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	// Reload from disk: the caller released the state lock before invoking
	// this rollback, so saving the caller's in-memory snapshot would
	// clobber any state mutation that landed in between. Revert the fill
	// against the CURRENT on-disk state instead.
	fresh, loadErr := sm.LoadState(opts.CommandID)
	if loadErr != nil {
		errs = append(errs, fmt.Errorf("rollback: reload state for command %s: %w", opts.CommandID, loadErr))
		return errors.Join(errs...)
	}
	freshIdx := -1
	for i := range fresh.Phases {
		if fresh.Phases[i].PhaseID == phaseID {
			freshIdx = i
			break
		}
	}
	if freshIdx == -1 {
		errs = append(errs, fmt.Errorf("rollback: phase %s missing from reloaded state for command %s", phaseID, opts.CommandID))
		return errors.Join(errs...)
	}
	now := nowUTC()
	// Only revert the phase status when this fill still owns it: a status
	// other than filling means another producer has since acted on the
	// phase and reverting would clobber that decision. The added task IDs
	// are stripped regardless — their queue entries are gone (never
	// written, or rolled back above), so leaving them in the state would
	// strand phantom entries.
	if fresh.Phases[freshIdx].Status == model.PhaseStatusFilling {
		fresh.Phases[freshIdx].Status = model.PhaseStatusAwaitingFill
		// Restart the awaiting-fill watchdog clock — see
		// rollbackPhaseFillToAwaiting for the rationale.
		fresh.Phases[freshIdx].AwaitingFillSince = &now
		fresh.Phases[freshIdx].AwaitingFillStallNotifiedAt = nil
	}
	rollbackPhaseFillState(fresh, freshIdx, tasks, nameToID)
	fresh.UpdatedAt = now
	if saveErr := sm.SaveState(fresh); saveErr != nil {
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
	slogc().Error("rollback failed",
		"op", op,
		"command_id", commandID,
		"error", err,
		"recoverable", recoverable,
		"suggested_action", suggestedAction,
		"affected_resource", affectedResource,
	)
}

func submitInitialTasks(opts SubmitOptions, tasks []TaskInput, sm *StateManager) (*SubmitResult, error) {
	// Validation
	if verrs := ValidateTasksInput(tasks); verrs != nil {
		return nil, verrs
	}

	// Worker pin existence: format-only validation lives in
	// validateTaskFieldsCore; the count-aware existence check happens here
	// so it runs even on the dry-run path. Without this, `plan submit
	// --dry-run` reports valid=true for `worker_id: worker99` and the
	// operator only learns the pin is bogus on the real submit.
	if verr := validateTaskWorkerPins(tasks, opts.Config.Agents.Workers.Count, "tasks"); verr != nil {
		return nil, verr
	}

	// Cross-command ordering advisory for run_on_main-only verification
	// commands. The mechanical guards (isolation, add-task publish gate,
	// dispatch pre-flight) only see THIS command's integration state; they
	// cannot tell whether the verification targets another command whose
	// outputs have not been published yet. Blocking here would
	// false-positive on genuinely unrelated in-flight commands, so this is
	// a warning the Planner must judge.
	warnings := runOnMainCrossCommandWarnings(opts.MaestroDir, opts.CommandID, tasks)

	if opts.DryRun {
		return &SubmitResult{Valid: true, Warnings: warnings}, nil
	}

	// Insert __system_commit when worktree mode is off, so the Worker
	// commits its in-place edits to main before the command terminates.
	// Worktree mode delegates commits to the Daemon and is exempt.
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
	queueKeys := []string{"queue:planner"}
	for _, workerID := range workerIDsFromAssignments(assignments) {
		queueKeys = append(queueKeys, "queue:"+workerID)
	}
	unlockQueues := lockQueueKeys(opts.LockMap, queueKeys)
	defer unlockQueues()
	unlockState, err := lockInitialStateForWrite(opts, sm)
	if err != nil {
		return nil, err
	}
	defer unlockState()

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

	if err := writeQueueEntriesLocked(opts.MaestroDir, assignments, tasks, nameToID, opts.CommandID, now); err != nil {
		if rbErr := rollbackStateAndQueueLocked(sm, opts.MaestroDir, opts.CommandID, tasks, nameToID, assignMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "initial_tasks_queue_write", false, "manual_cleanup", "command_state+queue_entries")
		}
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Seal
	state.PlanStatus = model.PlanStatusSealed
	state.PlanVersion = 1
	state.UpdatedAt = nowUTC()
	if err := sm.SaveState(state); err != nil {
		if rbErr := rollbackStateAndQueueLocked(sm, opts.MaestroDir, opts.CommandID, tasks, nameToID, assignMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "initial_tasks_seal", false, "manual_cleanup", "command_state+queue_entries")
		}
		return nil, fmt.Errorf("save state (sealed): %w", err)
	}

	// Build output
	result := &SubmitResult{CommandID: opts.CommandID, Warnings: warnings}
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

// runOnMainCrossCommandWarnings returns an advisory warning when a
// run_on_main-only verification command is submitted while OTHER commands
// still hold unpublished integration state. run_on_main tasks read the
// current published main: if the verification actually targets one of those
// in-flight commands' outputs, it will run against main without them and
// fail spuriously. Cross-command ordering cannot be enforced mechanically
// without false-positives on unrelated concurrent commands, so this stays a
// warning (the Planner contract is to wait for the relevant
// publish_completed notification). Best-effort: unreadable or unparsable
// state files are skipped — this function must never block a submission.
func runOnMainCrossCommandWarnings(maestroDir, commandID string, tasks []TaskInput) []string {
	if maestroDir == "" || len(tasks) == 0 {
		return nil
	}
	for _, t := range tasks {
		if !t.RunOnMain {
			return nil
		}
	}
	entries, err := os.ReadDir(filepath.Join(maestroDir, "state", "worktrees"))
	if err != nil {
		return nil
	}
	var pending []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		id := strings.TrimSuffix(name, ".yaml")
		if id == commandID {
			continue
		}
		data, err := os.ReadFile(filepath.Join(maestroDir, "state", "worktrees", name)) //nolint:gosec // controlled application state dir
		if err != nil {
			continue
		}
		var ws model.WorktreeCommandState
		if yamlutil.SafeUnmarshal(data, &ws) != nil {
			continue
		}
		if ws.Integration.Status != model.IntegrationStatusPublished {
			pending = append(pending, fmt.Sprintf("%s (integration %s)", id, ws.Integration.Status))
		}
	}
	if len(pending) == 0 {
		return nil
	}
	sort.Strings(pending)
	return []string{fmt.Sprintf(
		"run_on_main verification reads the CURRENT published main, but %d other command(s) still hold unpublished integration state: %s. "+
			"If this verification targets their outputs, wait for their publish_completed notification and submit afterwards — running now would verify main without those changes.",
		len(pending), strings.Join(pending, ", "))}
}

func submitInitialPhases(opts SubmitOptions, phases []PhaseInput, sm *StateManager) (*SubmitResult, error) {
	// Validation
	if verrs := ValidatePhasesInput(phases); verrs != nil {
		return nil, verrs
	}

	if err := validateCrossPhaseTaskNames(phases); err != nil {
		return nil, err
	}

	// Worker-pin existence check runs before the dry-run early return so a
	// phased plan submitted with worker_id: worker99 cannot pass `--dry-run`.
	for _, p := range phases {
		fieldPrefix := fmt.Sprintf("phases[%s].tasks", p.Name)
		if verr := validateTaskWorkerPins(p.Tasks, opts.Config.Agents.Workers.Count, fieldPrefix); verr != nil {
			return nil, verr
		}
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

	// Insert __system_commit outside phase structure when worktree mode
	// is off (skipped in worktree mode: Daemon manages commits directly).
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
	queueKeys := []string{"queue:planner"}
	for _, workerID := range workerIDsFromAssignments(cpd.assignments) {
		queueKeys = append(queueKeys, "queue:"+workerID)
	}
	unlockQueues := lockQueueKeys(opts.LockMap, queueKeys)
	defer unlockQueues()
	unlockState, err := lockInitialStateForWrite(opts, sm)
	if err != nil {
		return nil, err
	}
	defer unlockState()

	// Save state (planning)
	if err := sm.SaveState(state); err != nil {
		return nil, fmt.Errorf("save state (planning): %w", err)
	}

	// Write queue entries for concrete phase tasks + system commit
	if err := writeQueueEntriesLocked(opts.MaestroDir, cpd.assignments, cpd.tasks, cpd.nameToID, opts.CommandID, now); err != nil {
		if rbErr := rollbackStateAndQueueLocked(sm, opts.MaestroDir, opts.CommandID, cpd.tasks, cpd.nameToID, cpd.assignMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "initial_phases_queue_write", false, "manual_cleanup", "command_state+queue_entries")
		}
		return nil, fmt.Errorf("write queue: %w", err)
	}

	// Seal
	state.PlanStatus = model.PlanStatusSealed
	state.PlanVersion = 1
	state.UpdatedAt = nowUTC()
	if err := sm.SaveState(state); err != nil {
		if rbErr := rollbackStateAndQueueLocked(sm, opts.MaestroDir, opts.CommandID, cpd.tasks, cpd.nameToID, cpd.assignMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "initial_phases_seal", false, "manual_cleanup", "command_state+queue_entries")
		}
		return nil, fmt.Errorf("save state (sealed): %w", err)
	}

	return buildPhaseSubmitResult(opts.CommandID, phases, phaseNameToID, cpd, systemCommitTaskID), nil
}

func submitPhaseFill(opts SubmitOptions, input SubmitInput) (*SubmitResult, error) {
	if len(input.Phases) > 0 {
		return nil, &planValidationError{Msg: "phase fill only accepts tasks, not phases"}
	}

	if opts.LockMap == nil {
		return nil, ErrLockMapRequired
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	sm.LockCommand(opts.CommandID)
	stateLocked := true
	defer func() {
		if stateLocked {
			sm.UnlockCommand(opts.CommandID)
		}
	}()

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
	// Clear awaiting-fill watchdog tracking now that filling has begun. The
	// raw mutation here bypasses ApplyPhaseTransition (where the symmetric
	// clear lives), so we must reset these fields explicitly to keep audit
	// reads from reporting a phase at filling/completed with stale
	// "awaiting_fill_since" data.
	state.Phases[targetPhaseIdx].AwaitingFillSince = nil
	state.Phases[targetPhaseIdx].AwaitingFillStallNotifiedAt = nil
	state.UpdatedAt = nowStr
	if err := sm.SaveState(state); err != nil {
		// In-memory revert when the SaveState write itself fails — disk
		// state is unchanged, so the on-disk fields are still consistent.
		// Restore awaiting_fill watchdog tracking too: cleared above in
		// the optimistic transition, restored here so the next plan_submit
		// attempt sees the same elapsed window.
		state.Phases[targetPhaseIdx].Status = model.PhaseStatusAwaitingFill
		state.Phases[targetPhaseIdx].AwaitingFillSince = &nowStr
		state.Phases[targetPhaseIdx].AwaitingFillStallNotifiedAt = nil
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
	sm.UnlockCommand(opts.CommandID)
	stateLocked = false
	queueErr := writeQueueEntries(opts.MaestroDir, assignments, input.Tasks, nameToID, opts.CommandID, now, opts.LockMap)
	sm.LockCommand(opts.CommandID)
	stateLocked = true
	if queueErr != nil {
		// Drop the state lock BEFORE the rollback: rollbackFullPhaseFill
		// acquires queue locks first (canonical order queue → state), and
		// holding state:{command} here would ABBA-deadlock against
		// AddRetryTask (all queue locks → state).
		sm.UnlockCommand(opts.CommandID)
		stateLocked = false
		if rbErr := rollbackFullPhaseFill(sm, state, targetPhaseIdx, opts, input.Tasks, nameToID, assignMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "phase_fill_queue_write", false, "manual_intervention", "phase_state+queue_entries")
			emitPausedForReplanSignal(opts.MaestroDir, opts.CommandID,
				phaseSignalID(opts.PhaseName),
				"phase_fill_queue_write_rollback_failed", opts.LockMap)
		}
		return nil, fmt.Errorf("write queue: %w", queueErr)
	}

	state, targetPhaseIdx, err = reloadPhaseFillState(sm, opts, targetPhaseIdx, nowStr)
	if err != nil {
		// Queue rollback takes queue locks — release state first
		// (canonical order queue → state; see rollbackFullPhaseFill).
		sm.UnlockCommand(opts.CommandID)
		stateLocked = false
		if rbErr := rollbackQueueEntries(opts.MaestroDir, input.Tasks, nameToID, assignMap, opts.LockMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "phase_fill_reload_queue_rollback", false, "manual_intervention", "queue_entries")
			emitPausedForReplanSignal(opts.MaestroDir, opts.CommandID,
				phaseSignalID(opts.PhaseName),
				"phase_fill_reload_queue_rollback_failed", opts.LockMap)
		}
		return nil, err
	}
	if err := applyPhaseFillTasks(state, targetPhaseIdx, opts, input.Tasks, nameToID); err != nil {
		sm.UnlockCommand(opts.CommandID)
		stateLocked = false
		if rbErr := rollbackQueueEntries(opts.MaestroDir, input.Tasks, nameToID, assignMap, opts.LockMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "phase_fill_apply_queue_rollback", false, "manual_intervention", "queue_entries")
			emitPausedForReplanSignal(opts.MaestroDir, opts.CommandID,
				phaseSignalID(opts.PhaseName),
				"phase_fill_apply_queue_rollback_failed", opts.LockMap)
		}
		return nil, err
	}

	// Activate phase
	state.Phases[targetPhaseIdx].Status = model.PhaseStatusActive
	state.Phases[targetPhaseIdx].ActivatedAt = &now
	state.PlanVersion++
	state.UpdatedAt = now

	if err := sm.SaveState(state); err != nil {
		// Release state before the rollback's queue-lock acquisition
		// (canonical order queue → state; see rollbackFullPhaseFill).
		sm.UnlockCommand(opts.CommandID)
		stateLocked = false
		if rbErr := rollbackFullPhaseFill(sm, state, targetPhaseIdx, opts, input.Tasks, nameToID, assignMap); rbErr != nil {
			logRollbackFailure(opts.CommandID, rbErr, "phase_fill_save_state", false, "manual_intervention", "phase_state+queue_entries")
			emitPausedForReplanSignal(opts.MaestroDir, opts.CommandID,
				phaseSignalID(opts.PhaseName),
				"phase_fill_save_state_rollback_failed", opts.LockMap)
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

func reloadPhaseFillState(sm *StateManager, opts SubmitOptions, _ int, fillingStartedAt string) (*model.CommandState, int, error) {
	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return nil, 0, fmt.Errorf("reload state after queue write: %w", err)
	}
	if state.PlanStatus != model.PlanStatusSealed {
		return nil, 0, &planValidationError{Msg: fmt.Sprintf("plan_status changed during phase fill: %s", state.PlanStatus)}
	}
	targetPhaseIdx := -1
	for i := range state.Phases {
		if state.Phases[i].Name == opts.PhaseName {
			targetPhaseIdx = i
			break
		}
	}
	if targetPhaseIdx == -1 {
		return nil, 0, &planValidationError{Msg: fmt.Sprintf("phase %q not found after queue write", opts.PhaseName)}
	}
	phase := state.Phases[targetPhaseIdx]
	if phase.Status != model.PhaseStatusFilling {
		return nil, 0, &planValidationError{Msg: fmt.Sprintf("phase %q status changed during fill: %s", opts.PhaseName, phase.Status)}
	}
	if phase.FillingStartedAt == nil || *phase.FillingStartedAt != fillingStartedAt {
		return nil, 0, &planValidationError{Msg: fmt.Sprintf("phase %q filling epoch changed during fill", opts.PhaseName)}
	}
	return state, targetPhaseIdx, nil
}

func applyPhaseFillTasks(state *model.CommandState, targetPhaseIdx int, opts SubmitOptions, tasks []TaskInput, nameToID map[string]string) error {
	now := nowUTC()
	for _, t := range tasks {
		taskID := nameToID[t.Name]
		state.Phases[targetPhaseIdx].TaskIDs = append(state.Phases[targetPhaseIdx].TaskIDs, taskID)
		if t.Required {
			state.RequiredTaskIDs = append(state.RequiredTaskIDs, taskID)
		} else {
			state.OptionalTaskIDs = append(state.OptionalTaskIDs, taskID)
		}
		state.SetTaskState(taskID, model.StatusPlanned, now)
		if len(t.BlockedBy) > 0 {
			depIDs := make([]string, 0, len(t.BlockedBy))
			for _, depName := range t.BlockedBy {
				depID, ok := nameToID[depName]
				if !ok {
					return fmt.Errorf("blocked_by %q not found in fill tasks for phase %q (cross-phase references are not supported in phase fill)", depName, opts.PhaseName)
				}
				depIDs = append(depIDs, depID)
			}
			state.TaskDependencies[taskID] = depIDs
		}
	}
	state.ExpectedTaskCount = len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)
	return nil
}

// acquireStateFlock acquires a file-level exclusive lock for a command's state,
// providing cross-process mutual exclusion for double-submit prevention.
func acquireStateFlock(maestroDir, commandID string) (*os.File, error) {
	lockPath := filepath.Join(maestroDir, "locks", "state_"+commandID+".flock")
	return acquireFlock(lockPath, syscall.LOCK_EX)
}
