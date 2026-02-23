package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

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

func (sm *StateManager) StatePath(commandID string) string {
	return filepath.Join(sm.maestroDir, "state", "commands", commandID+".yaml")
}

func (sm *StateManager) StateExists(commandID string) bool {
	_, err := os.Stat(sm.StatePath(commandID))
	return err == nil
}

func (sm *StateManager) LoadState(commandID string) (*model.CommandState, error) {
	path := sm.StatePath(commandID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("state not found for command %s", commandID)
		}
		return nil, fmt.Errorf("read state %s: %w", commandID, err)
	}

	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", commandID, err)
	}

	if state.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported schema_version %d for state %s (expected 1)", state.SchemaVersion, commandID)
	}
	if state.FileType != "state_command" {
		return nil, fmt.Errorf("unexpected file_type %q for state %s (expected state_command)", state.FileType, commandID)
	}

	return &state, nil
}

func (sm *StateManager) SaveState(state *model.CommandState) error {
	path := sm.StatePath(state.CommandID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	return yamlutil.AtomicWrite(path, state)
}

func (sm *StateManager) DeleteState(commandID string) error {
	path := sm.StatePath(commandID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
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
	// Check phases for timed_out
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

	if hasFailed {
		return model.PlanStatusFailed, nil
	}
	if hasCancelled {
		return model.PlanStatusCancelled, nil
	}
	return model.PlanStatusCompleted, nil
}
