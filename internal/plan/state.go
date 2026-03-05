package plan

import (
	"errors"
	"fmt"
	"log"
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
var errYAMLCorrupted = errors.New("yaml corrupted")

type StateManager struct {
	maestroDir string
	lockMap    *lock.MutexMap
}

func NewStateManager(maestroDir string, lockMap *lock.MutexMap) *StateManager {
	return &StateManager{
		maestroDir: maestroDir,
		lockMap:    lockMap,
	}
}

func (sm *StateManager) StatePath(commandID string) (string, error) {
	if err := validate.ValidateID(commandID); err != nil {
		return "", fmt.Errorf("invalid command ID for state path: %w", err)
	}
	return filepath.Join(sm.maestroDir, "state", "commands", commandID+".yaml"), nil
}

func (sm *StateManager) StateExists(commandID string) bool {
	path, err := sm.StatePath(commandID)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

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
	log.Printf("[WARN] LoadState: YAML corrupted for command %s, attempting backup recovery", commandID)
	if _, quarantineErr := yamlutil.Quarantine(sm.maestroDir, path); quarantineErr != nil {
		return nil, fmt.Errorf("parse state %s (quarantine failed: %v): %w", commandID, quarantineErr, parseErr)
	}
	if recoverErr := yamlutil.RestoreFromBackup(path); recoverErr != nil {
		return nil, fmt.Errorf("parse state %s (backup recovery also failed: %v): %w", commandID, recoverErr, parseErr)
	}

	log.Printf("[INFO] LoadState: restored command %s from backup", commandID)

	// Re-read and validate the restored file
	state, err = sm.loadAndParseState(path, commandID)
	if err != nil {
		return nil, fmt.Errorf("parse state %s after backup restore: %w", commandID, err)
	}
	return state, nil
}

// loadAndParseState reads and validates a state file. Returns errYAMLCorrupted
// (wrapped) for YAML parse errors to distinguish from I/O errors.
func (sm *StateManager) loadAndParseState(path, commandID string) (*model.CommandState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Preserve os.ErrNotExist for errors.Is() checks by callers
		return nil, fmt.Errorf("read state %s: %w", commandID, err)
	}

	// First pass: unmarshal to detect schema_version for migration
	var raw map[string]interface{}
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse state %s: %w: %w", commandID, errYAMLCorrupted, err)
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
	if schemaVersion > CurrentSchemaVersion {
		return nil, fmt.Errorf("unsupported schema_version %d for state %s (max %d): %w", schemaVersion, commandID, CurrentSchemaVersion, errYAMLCorrupted)
	}

	// Apply migrations if needed
	if DefaultMigrator.NeedsMigration(schemaVersion) {
		if err := DefaultMigrator.Migrate(raw, schemaVersion); err != nil {
			return nil, fmt.Errorf("migrate state %s from version %d: %w: %w", commandID, schemaVersion, errYAMLCorrupted, err)
		}
		// Re-serialize migrated data for structured unmarshal
		data, err = yamlv3.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("re-serialize state %s after migration: %w: %w", commandID, errYAMLCorrupted, err)
		}
		log.Printf("[INFO] loadAndParseState: migrated state %s from schema version %d to %d", commandID, schemaVersion, CurrentSchemaVersion)
	}

	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state %s: %w: %w", commandID, errYAMLCorrupted, err)
	}

	if state.FileType != "state_command" {
		return nil, fmt.Errorf("unexpected file_type %q for state %s (expected state_command): %w", state.FileType, commandID, errYAMLCorrupted)
	}

	return &state, nil
}

func (sm *StateManager) SaveState(state *model.CommandState) error {
	path, err := sm.StatePath(state.CommandID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	return yamlutil.AtomicWrite(path, state)
}

func (sm *StateManager) DeleteState(commandID string) error {
	path, err := sm.StatePath(commandID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete state %s: %w", commandID, err)
	}
	return nil
}

func (sm *StateManager) LockCommand(commandID string) {
	sm.lockMap.Lock("state:" + commandID)
}

func (sm *StateManager) UnlockCommand(commandID string) {
	sm.lockMap.Unlock("state:" + commandID)
}

type RetryableError struct {
	Err error
}

func (e *RetryableError) Error() string {
	return e.Err.Error()
}

func (e *RetryableError) Unwrap() error {
	return e.Err
}

func CanComplete(state *model.CommandState) (model.PlanStatus, error) {
	if state.PlanStatus != model.PlanStatusSealed {
		return "", &PlanValidationError{Msg: fmt.Sprintf("plan_status must be sealed, got %s", state.PlanStatus)}
	}

	totalExpected := len(state.RequiredTaskIDs) + len(state.OptionalTaskIDs)
	if totalExpected != state.ExpectedTaskCount {
		return "", &PlanValidationError{Msg: fmt.Sprintf("task count mismatch: required(%d) + optional(%d) = %d, expected %d",
			len(state.RequiredTaskIDs), len(state.OptionalTaskIDs), totalExpected, state.ExpectedTaskCount)}
	}

	// Check all phases are terminal (if phases exist)
	if len(state.Phases) > 0 {
		for _, phase := range state.Phases {
			if phase.Status == model.PhaseStatusFilling {
				return "", &RetryableError{
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
				return "", &PlanValidationError{Msg: fmt.Sprintf("phase %q is not terminal (status: %s)", phase.Name, phase.Status)}
			}
		}
	}

	// Check all required tasks are terminal
	var nonTerminal []string
	for _, taskID := range state.RequiredTaskIDs {
		status, ok := state.TaskStates[taskID]
		if !ok {
			nonTerminal = append(nonTerminal, taskID+" (unknown)")
			continue
		}
		if !model.IsTerminal(status) {
			nonTerminal = append(nonTerminal, fmt.Sprintf("%s (%s)", taskID, status))
		}
	}
	if len(nonTerminal) > 0 {
		return "", &PlanValidationError{Msg: fmt.Sprintf("required tasks not terminal: %s", strings.Join(nonTerminal, ", "))}
	}

	return DeriveStatus(state)
}

func DeriveStatus(state *model.CommandState) (model.PlanStatus, error) {
	// Check phases for timed_out — always fails regardless of policy
	for _, phase := range state.Phases {
		if phase.Status == model.PhaseStatusTimedOut {
			return model.PlanStatusFailed, nil
		}
	}

	hasFailed := false
	hasCancelled := false

	for _, taskID := range state.RequiredTaskIDs {
		status := state.TaskStates[taskID]
		switch status {
		case model.StatusFailed:
			hasFailed = true
		case model.StatusCancelled:
			hasCancelled = true
		}
	}

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

	hasOptionalFailed := false
	for _, taskID := range state.OptionalTaskIDs {
		status := state.TaskStates[taskID]
		if status == model.StatusFailed {
			hasOptionalFailed = true
			break
		}
	}

	if hasOptionalFailed {
		switch onOptionalFailed {
		case "ignore":
			// do nothing, succeed
		case "warn":
			log.Printf("[WARN] DeriveStatus: optional task(s) failed for command %s (policy: warn)", state.CommandID)
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
