package plan

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// errYAMLCorrupted is a sentinel used internally to distinguish YAML parse errors
// from other I/O errors so that backup recovery is only attempted for corruption.
var errYAMLCorrupted = errors.New("invalid command state YAML")

// ErrLockMapRequired is returned when a LockMap is required but nil.
var ErrLockMapRequired = errors.New("lockMap is required")

// currentSchemaVersion is defined in migrator.go.

// StateManager manages read/write access to command state files (state/commands/{id}.yaml).
// All state mutations are serialized through LockCommand/UnlockCommand.
type StateManager struct {
	maestroDir string
	lockMap    *lock.MutexMap
}

// NewStateManager creates a StateManager for the given maestro directory.
func NewStateManager(maestroDir string, lockMap *lock.MutexMap) *StateManager {
	return &StateManager{
		maestroDir: maestroDir,
		lockMap:    lockMap,
	}
}

// StatePath returns the filesystem path for a command's state file.
func (sm *StateManager) StatePath(commandID string) (string, error) {
	if err := validate.ID(commandID); err != nil {
		return "", fmt.Errorf("invalid command ID for state path: %w", err)
	}
	return filepath.Join(sm.maestroDir, "state", "commands", commandID+".yaml"), nil
}

// StateExists returns true if a state file exists for the given command.
func (sm *StateManager) StateExists(commandID string) bool {
	path, err := sm.StatePath(commandID)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// LoadState reads and parses a command state file. If the file is corrupted,
// it attempts recovery from the .bak backup.
func (sm *StateManager) LoadState(commandID string) (*model.CommandState, error) {
	path, err := sm.StatePath(commandID)
	if err != nil {
		return nil, err
	}

	state, parseErr := sm.loadAndParseState(path, commandID)
	if parseErr == nil {
		return state, nil
	}

	// Only attempt recovery for YAML corruption, not I/O errors
	if !errors.Is(parseErr, errYAMLCorrupted) {
		return nil, parseErr
	}

	// Attempt backup recovery: quarantine corrupted file first, then restore
	// from .bak. Quarantining removes the primary file so that RestoreFromBackup
	// (which uses AtomicWriteRaw) does not overwrite the good .bak with the
	// corrupted primary. Skeleton generation is NOT used for state_command
	// because it would silently discard command progress.
	slog.Warn("LoadState: YAML corrupted, attempting backup recovery", "command_id", commandID)
	if _, quarantineErr := yamlutil.Quarantine(sm.maestroDir, path); quarantineErr != nil {
		return nil, fmt.Errorf("parse state %s (quarantine failed: %s): %w", commandID, quarantineErr.Error(), parseErr)
	}
	if recoverErr := yamlutil.RestoreFromBackup(path); recoverErr != nil {
		return nil, fmt.Errorf("parse state %s (backup recovery also failed: %s): %w", commandID, recoverErr.Error(), parseErr)
	}

	slog.Info("LoadState: restored command from backup", "command_id", commandID)

	// Re-read and validate the restored file
	state, err = sm.loadAndParseState(path, commandID)
	if err != nil {
		return nil, fmt.Errorf("parse state %s after backup restore (primary was also corrupted): %w", commandID, errors.Join(parseErr, err))
	}
	return state, nil
}

// loadAndParseState reads and validates a state file. Returns errYAMLCorrupted
// (wrapped) for YAML parse errors to distinguish from I/O errors.
func (sm *StateManager) loadAndParseState(path, commandID string) (*model.CommandState, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application state directory
	if err != nil {
		// Preserve os.ErrNotExist for errors.Is() checks by callers
		return nil, fmt.Errorf("read state %s: %w", commandID, err)
	}

	// First pass: unmarshal to detect schema_version for migration
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", commandID, errors.Join(errYAMLCorrupted, err))
	}

	// Extract and validate schema_version from raw data
	schemaVersion := 0
	if sv, ok := raw["schema_version"]; ok {
		switch v := sv.(type) {
		case int:
			schemaVersion = v
		case float64:
			schemaVersion = int(v)
		}
	}
	if schemaVersion < 1 {
		return nil, fmt.Errorf("invalid schema_version %d for state %s: %w", schemaVersion, commandID, errYAMLCorrupted)
	}
	if schemaVersion > currentSchemaVersion {
		return nil, fmt.Errorf("unsupported schema_version %d for state %s (max %d): %w", schemaVersion, commandID, currentSchemaVersion, errYAMLCorrupted)
	}

	// Apply migrations if needed
	if defaultMigrator.NeedsMigration(schemaVersion) {
		if err := defaultMigrator.Migrate(raw, schemaVersion); err != nil {
			return nil, fmt.Errorf("migrate state %s from version %d: %w", commandID, schemaVersion, errors.Join(errYAMLCorrupted, err))
		}
		// Re-serialize migrated data for structured unmarshal
		data, err = yamlv3.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("re-serialize state %s after migration: %w", commandID, errors.Join(errYAMLCorrupted, err))
		}
		slog.Info("loadAndParseState: migrated state", "command_id", commandID, "from_version", schemaVersion, "to_version", currentSchemaVersion)
	}

	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", commandID, errors.Join(errYAMLCorrupted, err))
	}

	if state.FileType != "state_command" {
		return nil, fmt.Errorf("unexpected file_type %q for state %s (expected state_command): %w", state.FileType, commandID, errYAMLCorrupted)
	}

	return &state, nil
}

// SaveState atomically writes the command state to disk.
func (sm *StateManager) SaveState(state *model.CommandState) error {
	path, err := sm.StatePath(state.CommandID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil { //nolint:gosec // 0755 is appropriate for a state directory
		return fmt.Errorf("create state dir: %w", err)
	}
	return yamlutil.AtomicWrite(path, state)
}

// DeleteState removes the state file for a command.
//
// This also removes the sibling <state>.bak rotation that yaml.AtomicWriteRaw
// produces. Without this, recoverStateDir would treat the stale .bak as a
// valid prior generation if a new state file is later created at the same
// path (commandID reuse) and then becomes corrupt — the ORC-3 epoch floor
// clamp would then re-inject a stale lease_epoch derived from the old
// generation.
func (sm *StateManager) DeleteState(commandID string) error {
	path, err := sm.StatePath(commandID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete state %s: %w", commandID, err)
	}
	// Best-effort .bak cleanup: a missing .bak is the common case (the file is
	// only written after the second SaveState), so swallow ErrNotExist.
	bakPath := path + ".bak"
	if err := os.Remove(bakPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete state bak %s: %w", commandID, err)
	}
	return nil
}

// LockCommand acquires the in-process mutex for a command's state.
func (sm *StateManager) LockCommand(commandID string) {
	sm.lockMap.Lock("state:" + commandID)
}

// UnlockCommand releases the in-process mutex for a command's state.
func (sm *StateManager) UnlockCommand(commandID string) {
	sm.lockMap.Unlock("state:" + commandID)
}

// retryableError wraps an error that indicates the operation can be safely retried.
type retryableError struct {
	Err error
}

func (e *retryableError) Error() string {
	return e.Err.Error()
}

func (e *retryableError) Unwrap() error {
	return e.Err
}

// CanComplete checks whether a command can transition to a terminal status.
// Returns the derived PlanStatus or an error if the command is not ready.
func CanComplete(state *model.CommandState) (model.PlanStatus, error) {
	if state.PlanStatus != model.PlanStatusSealed {
		return "", &planValidationError{Msg: fmt.Sprintf("plan_status must be sealed, got %s", state.PlanStatus)}
	}

	totalExpected := len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)
	if totalExpected != state.ExpectedTaskCount {
		return "", &planValidationError{Msg: fmt.Sprintf("task count mismatch: required(%d) + optional(%d) = %d, expected %d",
			len(state.RequiredTaskIDs), len(state.OptionalTaskIDs), totalExpected, state.ExpectedTaskCount)}
	}

	// Check all phases are terminal (if phases exist)
	if len(state.Phases) > 0 {
		for _, phase := range state.Phases {
			if phase.Status == model.PhaseStatusFilling {
				return "", &retryableError{
					Err: fmt.Errorf("phase %q is in transient status filling, retry later", phase.Name),
				}
			}
			if phase.Status == model.PhaseStatusAwaitingFill {
				return "", &ActionRequiredError{
					Reason:      "PHASE_AWAITING_FILL",
					CommandID:   state.CommandID,
					PhaseID:     phase.PhaseID,
					PhaseName:   phase.Name,
					PhaseStatus: string(phase.Status),
					NextAction: fmt.Sprintf("maestro plan submit --command-id %s --phase %s --tasks-file plan.yaml",
						state.CommandID, phase.Name),
				}
			}
			if !model.IsPhaseTerminal(phase.Status) {
				// Transient race: queue_scan_phase_a_phase.go defers
				// PhaseStatusCompleted on the merge_recorded gate (Phase B/C
				// records the merge, then the next scan flips status). When
				// every task in this phase has finished its work the daemon
				// is on the verge of moving the phase forward, so the
				// non-terminal status here reflects the gap between
				// task-result write and phase transition rather than a real
				// "Planner called complete too early" mistake. Surface as
				// retryable so Complete() can write a deferred_complete
				// intent — the daemon's deferredPlanCompleter then finalises
				// the plan once publish succeeds, and the Planner does not
				// have to poll/retry.
				//
				// **Eligibility**: the deferred-publish path is only valid
				// when Phase B will actually merge this phase. Phase B only
				// merges on the success path: it expects
				// every task to be at StatusCompleted (with StatusCancelled
				// admitted because cancellation here always means "superseded
				// by retry sibling"; a genuine plan-level cancel marks the
				// whole plan PlanStatusCancelled and never reaches this
				// branch). Tasks at StatusFailed / StatusDeadLetter /
				// StatusAborted indicate the phase is destined for
				// PhaseStatusFailed via the dependency resolver — there is no
				// pending merge for the deferred completer to ride on. Writing
				// a deferred_complete intent in that case would leave a
				// dangling intent file (the publish that would clear it never
				// runs) and return a misleading "publish 待ち" status to the
				// Planner. Fall through to planValidationError so Complete()
				// surfaces the real failure path instead.
				allOK := true
				for _, tid := range phase.TaskIDs {
					ts, ok := state.TaskStates[tid]
					if !ok || (ts != model.StatusCompleted && ts != model.StatusCancelled) {
						allOK = false
						break
					}
				}
				if allOK && len(phase.TaskIDs) > 0 {
					return "", &retryableError{
						Err: fmt.Errorf("phase %q transitioning to terminal (status: %s, all tasks completed/superseded, awaiting daemon merge_recorded gate)", phase.Name, phase.Status),
					}
				}
				return "", &planValidationError{Msg: fmt.Sprintf("phase %q is not terminal (status: %s)", phase.Name, phase.Status)}
			}

			// Verify Phase-Task consistency: all tasks within a terminal phase must also be terminal
			for _, tid := range phase.TaskIDs {
				if ts, ok := state.TaskStates[tid]; ok && !model.IsTerminal(ts) {
					return "", &planValidationError{Msg: fmt.Sprintf("phase %q is terminal (%s) but task %s is non-terminal (%s)", phase.Name, phase.Status, tid, ts)}
				}
			}
		}
	}

	// Check all required tasks are terminal
	var nonTerminal []string
	allPausedForReplan := true
	hasNonTerminal := false
	for _, taskID := range state.RequiredTaskIDs {
		status, ok := state.TaskStates[taskID]
		if !ok {
			nonTerminal = append(nonTerminal, taskID+" (unknown)")
			hasNonTerminal = true
			allPausedForReplan = false
			continue
		}
		if !model.IsTerminal(status) {
			nonTerminal = append(nonTerminal, fmt.Sprintf("%s (%s)", taskID, status))
			hasNonTerminal = true
			if status != model.StatusPausedForReplan {
				allPausedForReplan = false
			}
		}
	}
	if hasNonTerminal {
		// When the only thing keeping the plan from completing is one or
		// more paused_for_replan tasks, rephrase the error so the Planner
		// pane log explains the actionable next step instead of just
		// echoing "not terminal". The daemon will either inject a retry
		// task (Planner-driven) or escalate via R10 deadletter; calling
		// plan_complete now is premature, not a fundamental error.
		if allPausedForReplan {
			return "", &planValidationError{Msg: fmt.Sprintf(
				"plan_complete deferred: tasks awaiting replan/retry — %s "+
					"(submit a retry-task with `maestro plan add-retry-task` or wait for R10 deadletter)",
				strings.Join(nonTerminal, ", "))}
		}
		return "", &planValidationError{Msg: fmt.Sprintf("required tasks not terminal: %s", strings.Join(nonTerminal, ", "))}
	}

	return DeriveStatus(state)
}

// hasTaskWithStatusForCompletion returns true if any task in taskIDs has the
// given status when viewed through the completion-aware lens. Walks
// retry_lineage forward AND unwinds cascade-cancellations whose upstream
// dependency lineages have effectively completed (see
// EffectiveStatusForCompletion). This is the canonical view for plan
// completion decisions: a task whose work was structurally short-circuited
// by an upstream repair must not register as failed/cancelled to the plan
// machine, otherwise CompletionPolicy returns the plan to PlanStatusCancelled
// and publish is skipped while main has every required output.
func hasTaskWithStatusForCompletion(taskIDs []string, state *model.CommandState, target model.Status) bool {
	for _, taskID := range taskIDs {
		if EffectiveStatusForCompletion(taskID, state) == target {
			return true
		}
	}
	return false
}

// DeriveStatus determines the terminal PlanStatus based on task outcomes
// and the command's CompletionPolicy. Failed/cancelled checks walk
// retry_lineage forward so a task that was superseded by a successful
// retry — for example a verify-repair successor that completed — does
// not poison the plan with a synthetic_failure. The check additionally
// unwinds cascade-cancellations whose upstream lineage completed so that
// downstream cascade-stragglers do not block plan completion when every
// required predecessor has, in fact, been delivered (Bug-D'-prime).
func DeriveStatus(state *model.CommandState) (model.PlanStatus, error) {
	// Check phases for timed_out — always fails regardless of policy
	for _, phase := range state.Phases {
		if phase.Status == model.PhaseStatusTimedOut {
			return model.PlanStatusFailed, nil
		}
	}

	hasFailed := hasTaskWithStatusForCompletion(state.RequiredTaskIDs, state, model.StatusFailed)
	hasCancelled := hasTaskWithStatusForCompletion(state.RequiredTaskIDs, state, model.StatusCancelled)

	// Apply CompletionPolicy for required task failures
	onFailed := state.CompletionPolicy.OnRequiredFailed
	if onFailed == "" {
		onFailed = "fail_command" // default
	}
	onCancelled := state.CompletionPolicy.OnRequiredCancelled
	if onCancelled == "" {
		onCancelled = "cancel_command" // default
	}

	if hasFailed {
		switch onFailed {
		case "fail_command":
			return model.PlanStatusFailed, nil
		case "ignore":
			// do not force fail; fall through to check cancelled
		default:
			return "", fmt.Errorf("unsupported completion_policy.on_required_failed value: %q", onFailed)
		}
	}
	if hasCancelled {
		switch onCancelled {
		case "cancel_command":
			return model.PlanStatusCancelled, nil
		case "ignore":
			// do not force cancel
		default:
			return "", fmt.Errorf("unsupported completion_policy.on_required_cancelled value: %q", onCancelled)
		}
	}

	// Apply CompletionPolicy for optional task failures
	onOptionalFailed := state.CompletionPolicy.OnOptionalFailed
	if onOptionalFailed == "" {
		onOptionalFailed = "ignore" // default: backwards-compatible
	}

	hasOptionalFailed := hasTaskWithStatusForCompletion(state.OptionalTaskIDs, state, model.StatusFailed)

	if hasOptionalFailed {
		switch onOptionalFailed {
		case "ignore":
			// do nothing, succeed
		case "warn":
			slog.Warn("DeriveStatus: optional task(s) failed", "command_id", state.CommandID, "policy", "warn")
		case "fail_command":
			return model.PlanStatusFailed, nil
		default:
			return "", fmt.Errorf("unsupported completion_policy.on_optional_failed value: %q", onOptionalFailed)
		}
	}

	// Validate DependencyFailurePolicy value
	depPolicy := state.CompletionPolicy.DependencyFailurePolicy
	if depPolicy == "" {
		depPolicy = "cancel_dependents" // default
	}
	switch depPolicy {
	case "cancel_dependents":
		// Default: dependents of failed tasks are cancelled by the reconciler
	case "fail_dependents":
		// Dependents of failed tasks are marked as failed by the reconciler
	case "ignore":
		// Dependency failures are not propagated; dependents run regardless
	default:
		return "", fmt.Errorf("unsupported completion_policy.dependency_failure_policy value: %q", depPolicy)
	}

	return model.PlanStatusCompleted, nil
}
