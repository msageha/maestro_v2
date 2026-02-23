package plan

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

func newTestStateManager(t *testing.T) *StateManager {
	t.Helper()
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	if err := os.MkdirAll(filepath.Join(maestroDir, "state", "commands"), 0755); err != nil {
		t.Fatalf("create test dirs: %v", err)
	}
	return NewStateManager(maestroDir, lock.NewMutexMap())
}

func TestStateManager_SaveAndLoad(t *testing.T) {
	sm := newTestStateManager(t)

	original := &model.CommandState{
		SchemaVersion:     1,
		FileType:          "state_command",
		CommandID:         "cmd-save-load",
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1"},
		OptionalTaskIDs:   []string{"t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCompleted,
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	if err := sm.SaveState(original); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	loaded, err := sm.LoadState("cmd-save-load")
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}

	if loaded.CommandID != original.CommandID {
		t.Errorf("CommandID = %q, want %q", loaded.CommandID, original.CommandID)
	}
	if loaded.PlanStatus != original.PlanStatus {
		t.Errorf("PlanStatus = %q, want %q", loaded.PlanStatus, original.PlanStatus)
	}
	if loaded.ExpectedTaskCount != original.ExpectedTaskCount {
		t.Errorf("ExpectedTaskCount = %d, want %d", loaded.ExpectedTaskCount, original.ExpectedTaskCount)
	}
	if len(loaded.RequiredTaskIDs) != len(original.RequiredTaskIDs) {
		t.Errorf("RequiredTaskIDs length = %d, want %d", len(loaded.RequiredTaskIDs), len(original.RequiredTaskIDs))
	}
	if len(loaded.TaskStates) != len(original.TaskStates) {
		t.Errorf("TaskStates length = %d, want %d", len(loaded.TaskStates), len(original.TaskStates))
	}
}

func TestStateManager_StateExists(t *testing.T) {
	sm := newTestStateManager(t)

	if sm.StateExists("nonexistent") {
		t.Errorf("StateExists = true for nonexistent command, want false")
	}

	state := &model.CommandState{
		CommandID: "exists-cmd",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := sm.SaveState(state); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	if !sm.StateExists("exists-cmd") {
		t.Errorf("StateExists = false after save, want true")
	}
}

func TestStateManager_DeleteState(t *testing.T) {
	sm := newTestStateManager(t)

	state := &model.CommandState{
		CommandID: "delete-cmd",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := sm.SaveState(state); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	if !sm.StateExists("delete-cmd") {
		t.Fatalf("StateExists = false before delete, want true")
	}

	if err := sm.DeleteState("delete-cmd"); err != nil {
		t.Fatalf("DeleteState failed: %v", err)
	}

	if sm.StateExists("delete-cmd") {
		t.Errorf("StateExists = true after delete, want false")
	}
}

func TestCanComplete_AllCompleted(t *testing.T) {
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 3,
		RequiredTaskIDs:   []string{"t1", "t2"},
		OptionalTaskIDs:   []string{"t3"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCompleted,
			"t3": model.StatusCompleted,
		},
	}

	status, err := CanComplete(state)
	if err != nil {
		t.Fatalf("CanComplete returned error: %v", err)
	}
	if status != model.PlanStatusCompleted {
		t.Errorf("status = %q, want %q", status, model.PlanStatusCompleted)
	}
}

func TestCanComplete_HasFailed(t *testing.T) {
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1", "t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusFailed,
		},
	}

	status, err := CanComplete(state)
	if err != nil {
		t.Fatalf("CanComplete returned error: %v", err)
	}
	if status != model.PlanStatusFailed {
		t.Errorf("status = %q, want %q", status, model.PlanStatusFailed)
	}
}

func TestCanComplete_HasCancelled(t *testing.T) {
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1", "t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCancelled,
		},
	}

	status, err := CanComplete(state)
	if err != nil {
		t.Fatalf("CanComplete returned error: %v", err)
	}
	if status != model.PlanStatusCancelled {
		t.Errorf("status = %q, want %q", status, model.PlanStatusCancelled)
	}
}

func TestCanComplete_NotSealed(t *testing.T) {
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusPlanning,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatalf("CanComplete returned nil error for non-sealed plan")
	}
}

func TestCanComplete_TaskCountMismatch(t *testing.T) {
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 5,
		RequiredTaskIDs:   []string{"t1"},
		OptionalTaskIDs:   []string{"t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCompleted,
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatalf("CanComplete returned nil error for task count mismatch")
	}
}

func TestCanComplete_NonTerminalTask(t *testing.T) {
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1", "t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusInProgress,
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatalf("CanComplete returned nil error for non-terminal required task")
	}
}

func TestCanComplete_FillingPhase(t *testing.T) {
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		Phases: []model.Phase{
			{
				Name:   "phase-1",
				Status: model.PhaseStatusFilling,
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatalf("CanComplete returned nil error for filling phase")
	}

	var retryErr *RetryableError
	if !errors.As(err, &retryErr) {
		t.Errorf("error is not RetryableError, got %T: %v", err, err)
	}
}

func TestDeriveStatus_TimedOutPhase(t *testing.T) {
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		Phases: []model.Phase{
			{
				Name:   "phase-1",
				Status: model.PhaseStatusTimedOut,
			},
		},
	}

	status, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus returned error: %v", err)
	}
	if status != model.PlanStatusFailed {
		t.Errorf("status = %q, want %q", status, model.PlanStatusFailed)
	}
}
