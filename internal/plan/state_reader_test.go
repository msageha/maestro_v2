package plan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func setupStateReaderTest(t *testing.T) (string, *PlanStateReader) {
	t.Helper()
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	if err := os.MkdirAll(filepath.Join(maestroDir, "state", "commands"), 0755); err != nil {
		t.Fatal(err)
	}
	sm := NewStateManager(maestroDir, lock.NewMutexMap())
	reader := NewPlanStateReader(sm)
	return maestroDir, reader
}

func writeState(t *testing.T, maestroDir string, state *model.CommandState) {
	t.Helper()
	path := filepath.Join(maestroDir, "state", "commands", state.CommandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatal(err)
	}
}

func TestIsSystemCommitReady_NotSystemCommitTask(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		TaskStates: map[string]model.Status{
			"task1": model.StatusCompleted,
		},
	})

	isSys, ready, err := reader.IsSystemCommitReady("cmd1", "task1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isSys {
		t.Error("expected isSystemCommit=false for non-system-commit task")
	}
	if ready {
		t.Error("expected ready=false")
	}
}

func TestIsSystemCommitReady_NonPhased_AllTerminal(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	sysTaskID := "task_sys"
	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:      1,
		FileType:           "state_command",
		CommandID:          "cmd1",
		SystemCommitTaskID: &sysTaskID,
		TaskStates: map[string]model.Status{
			"task1":    model.StatusCompleted,
			"task2":    model.StatusCompleted,
			"task_sys": model.StatusPending,
		},
	})

	isSys, ready, err := reader.IsSystemCommitReady("cmd1", "task_sys")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isSys {
		t.Error("expected isSystemCommit=true")
	}
	if !ready {
		t.Error("expected ready=true (all user tasks terminal)")
	}
}

func TestIsSystemCommitReady_NonPhased_NotAllTerminal(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	sysTaskID := "task_sys"
	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:      1,
		FileType:           "state_command",
		CommandID:          "cmd1",
		SystemCommitTaskID: &sysTaskID,
		TaskStates: map[string]model.Status{
			"task1":    model.StatusCompleted,
			"task2":    model.StatusPending,
			"task_sys": model.StatusPending,
		},
	})

	isSys, ready, err := reader.IsSystemCommitReady("cmd1", "task_sys")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isSys {
		t.Error("expected isSystemCommit=true")
	}
	if ready {
		t.Error("expected ready=false (task2 still pending)")
	}
}

func TestIsSystemCommitReady_Phased_AllTerminal(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	sysTaskID := "task_sys"
	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:      1,
		FileType:           "state_command",
		CommandID:          "cmd1",
		SystemCommitTaskID: &sysTaskID,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "research", Status: model.PhaseStatusCompleted},
			{PhaseID: "p2", Name: "implement", Status: model.PhaseStatusCompleted},
		},
		TaskStates: map[string]model.Status{
			"task1":    model.StatusCompleted,
			"task2":    model.StatusCompleted,
			"task_sys": model.StatusPending,
		},
	})

	isSys, ready, err := reader.IsSystemCommitReady("cmd1", "task_sys")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isSys {
		t.Error("expected isSystemCommit=true")
	}
	if !ready {
		t.Error("expected ready=true (all phases terminal)")
	}
}

func TestIsSystemCommitReady_Phased_SomeNonTerminal(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	sysTaskID := "task_sys"
	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:      1,
		FileType:           "state_command",
		CommandID:          "cmd1",
		SystemCommitTaskID: &sysTaskID,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "research", Status: model.PhaseStatusCompleted},
			{PhaseID: "p2", Name: "implement", Status: model.PhaseStatusActive},
		},
		TaskStates: map[string]model.Status{
			"task1":    model.StatusCompleted,
			"task2":    model.StatusPending,
			"task_sys": model.StatusPending,
		},
	})

	isSys, ready, err := reader.IsSystemCommitReady("cmd1", "task_sys")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isSys {
		t.Error("expected isSystemCommit=true")
	}
	if ready {
		t.Error("expected ready=false (phase p2 still active)")
	}
}

func TestIsSystemCommitReady_Phased_FailedPhase(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	sysTaskID := "task_sys"
	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:      1,
		FileType:           "state_command",
		CommandID:          "cmd1",
		SystemCommitTaskID: &sysTaskID,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "research", Status: model.PhaseStatusFailed},
			{PhaseID: "p2", Name: "implement", Status: model.PhaseStatusCancelled},
		},
		TaskStates: map[string]model.Status{
			"task1":    model.StatusFailed,
			"task2":    model.StatusCancelled,
			"task_sys": model.StatusPending,
		},
	})

	isSys, ready, err := reader.IsSystemCommitReady("cmd1", "task_sys")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isSys {
		t.Error("expected isSystemCommit=true")
	}
	if !ready {
		t.Error("expected ready=true (failed/cancelled are terminal)")
	}
}

func TestIsSystemCommitReady_NoSystemCommitField(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:      1,
		FileType:           "state_command",
		CommandID:          "cmd1",
		SystemCommitTaskID: nil,
		TaskStates: map[string]model.Status{
			"task1": model.StatusCompleted,
		},
	})

	isSys, ready, err := reader.IsSystemCommitReady("cmd1", "task1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isSys || ready {
		t.Error("expected false, false when SystemCommitTaskID is nil")
	}
}

// Test ErrStateNotFound handling for all StateReader methods

func TestGetTaskState_StateNotFound(t *testing.T) {
	_, reader := setupStateReaderTest(t)

	// State file doesn't exist
	_, err := reader.GetTaskState("nonexistent_cmd", "task1")
	if !errors.Is(err, daemon.ErrStateNotFound) {
		t.Errorf("expected ErrStateNotFound, got %v", err)
	}
}

func TestGetCommandPhases_StateNotFound(t *testing.T) {
	_, reader := setupStateReaderTest(t)

	// State file doesn't exist
	_, err := reader.GetCommandPhases("nonexistent_cmd")
	if !errors.Is(err, daemon.ErrStateNotFound) {
		t.Errorf("expected ErrStateNotFound, got %v", err)
	}
}

func TestGetTaskDependencies_StateNotFound(t *testing.T) {
	_, reader := setupStateReaderTest(t)

	// State file doesn't exist
	_, err := reader.GetTaskDependencies("nonexistent_cmd", "task1")
	if !errors.Is(err, daemon.ErrStateNotFound) {
		t.Errorf("expected ErrStateNotFound, got %v", err)
	}
}

func TestIsSystemCommitReady_StateNotFound(t *testing.T) {
	_, reader := setupStateReaderTest(t)

	// State file doesn't exist
	_, _, err := reader.IsSystemCommitReady("nonexistent_cmd", "task1")
	if !errors.Is(err, daemon.ErrStateNotFound) {
		t.Errorf("expected ErrStateNotFound, got %v", err)
	}
}

func TestIsCommandCancelRequested_StateNotFound(t *testing.T) {
	_, reader := setupStateReaderTest(t)

	// State file doesn't exist
	_, err := reader.IsCommandCancelRequested("nonexistent_cmd")
	if !errors.Is(err, daemon.ErrStateNotFound) {
		t.Errorf("expected ErrStateNotFound, got %v", err)
	}
}

// --- ApplyPhaseTransition tests ---

func TestApplyPhaseTransition_NormalTransition(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		PlanStatus:    model.PlanStatusSealed,
		TaskStates:    map[string]model.Status{},
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "build", Type: "concrete", Status: model.PhaseStatusActive},
		},
		UpdatedAt: "2025-01-01T00:00:00Z",
	})

	err := reader.ApplyPhaseTransition("cmd1", "p1", model.PhaseStatusCompleted)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Reload and verify
	sm := NewStateManager(maestroDir, lock.NewMutexMap())
	state, err := sm.LoadState("cmd1")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.Phases[0].Status != model.PhaseStatusCompleted {
		t.Errorf("phase status = %q, want %q", state.Phases[0].Status, model.PhaseStatusCompleted)
	}
	if state.UpdatedAt == "2025-01-01T00:00:00Z" {
		t.Error("UpdatedAt was not updated")
	}
}

func TestApplyPhaseTransition_TerminalSetsCompletedAt(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		PlanStatus:    model.PlanStatusSealed,
		TaskStates:    map[string]model.Status{},
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "build", Type: "concrete", Status: model.PhaseStatusActive},
		},
	})

	err := reader.ApplyPhaseTransition("cmd1", "p1", model.PhaseStatusFailed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sm := NewStateManager(maestroDir, lock.NewMutexMap())
	state, err := sm.LoadState("cmd1")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.Phases[0].CompletedAt == nil {
		t.Error("CompletedAt is nil, want non-nil for terminal transition")
	}
}

func TestApplyPhaseTransition_InvalidTransition(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		PlanStatus:    model.PlanStatusSealed,
		TaskStates:    map[string]model.Status{},
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "build", Type: "deferred", Status: model.PhaseStatusPending},
		},
	})

	// pending → active is not a valid phase transition (must go through awaiting_fill first)
	err := reader.ApplyPhaseTransition("cmd1", "p1", model.PhaseStatusActive)
	if err == nil {
		t.Fatal("expected error for invalid transition, got nil")
	}
}

func TestApplyPhaseTransition_UnknownPhaseID(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		PlanStatus:    model.PlanStatusSealed,
		TaskStates:    map[string]model.Status{},
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "build", Type: "concrete", Status: model.PhaseStatusActive},
		},
	})

	err := reader.ApplyPhaseTransition("cmd1", "p_nonexistent", model.PhaseStatusCompleted)
	if err == nil {
		t.Fatal("expected error for unknown phase ID, got nil")
	}
	if !errors.Is(err, daemon.ErrPhaseNotFound) {
		t.Errorf("error = %v, want errors.Is(err, daemon.ErrPhaseNotFound)", err)
	}
}

// --- UpdateTaskState tests ---

func TestUpdateTaskState_NilTaskStates(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	// Write state with nil TaskStates to test initialization
	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       "cmd1",
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: []string{"task1"},
		TaskStates:      nil,
	})

	err := reader.UpdateTaskState("cmd1", "task1", model.StatusPending, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sm := NewStateManager(maestroDir, lock.NewMutexMap())
	state, err := sm.LoadState("cmd1")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.TaskStates == nil {
		t.Fatal("TaskStates is nil, expected map initialization")
	}
	if state.TaskStates["task1"] != model.StatusPending {
		t.Errorf("TaskStates[task1] = %q, want %q", state.TaskStates["task1"], model.StatusPending)
	}
}

func TestUpdateTaskState_UnknownTaskID(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       "cmd1",
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: []string{"task1"},
		TaskStates:      map[string]model.Status{"task1": model.StatusPending},
	})

	err := reader.UpdateTaskState("cmd1", "unknown_task", model.StatusPending, "")
	if err == nil {
		t.Fatal("expected error for unknown task ID, got nil")
	}
	if !strings.Contains(err.Error(), "unknown task ID") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "unknown task ID")
	}
}

func TestUpdateTaskState_NormalTransition(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       "cmd1",
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: []string{"task1"},
		TaskStates: map[string]model.Status{
			"task1": model.StatusPending,
		},
		UpdatedAt: "2025-01-01T00:00:00Z",
	})

	err := reader.UpdateTaskState("cmd1", "task1", model.StatusInProgress, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sm := NewStateManager(maestroDir, lock.NewMutexMap())
	state, err := sm.LoadState("cmd1")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.TaskStates["task1"] != model.StatusInProgress {
		t.Errorf("TaskStates[task1] = %q, want %q", state.TaskStates["task1"], model.StatusInProgress)
	}
	if state.UpdatedAt == "2025-01-01T00:00:00Z" {
		t.Error("UpdatedAt was not updated")
	}
}

func TestUpdateTaskState_InvalidTransition(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       "cmd1",
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: []string{"task1"},
		TaskStates: map[string]model.Status{
			"task1": model.StatusPending,
		},
	})

	// pending → completed is not valid (must go through in_progress first)
	err := reader.UpdateTaskState("cmd1", "task1", model.StatusCompleted, "")
	if err == nil {
		t.Fatal("expected error for invalid transition, got nil")
	}
}

func TestUpdateTaskState_CancelledReason(t *testing.T) {
	maestroDir, reader := setupStateReaderTest(t)

	writeState(t, maestroDir, &model.CommandState{
		SchemaVersion:    1,
		FileType:         "state_command",
		CommandID:        "cmd1",
		PlanStatus:       model.PlanStatusSealed,
		RequiredTaskIDs:  []string{"task1"},
		TaskStates:       map[string]model.Status{"task1": model.StatusPending},
		CancelledReasons: map[string]string{},
	})

	err := reader.UpdateTaskState("cmd1", "task1", model.StatusCancelled, "dependency failed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sm := NewStateManager(maestroDir, lock.NewMutexMap())
	state, err := sm.LoadState("cmd1")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.TaskStates["task1"] != model.StatusCancelled {
		t.Errorf("TaskStates[task1] = %q, want %q", state.TaskStates["task1"], model.StatusCancelled)
	}
	if state.CancelledReasons["task1"] != "dependency failed" {
		t.Errorf("CancelledReasons[task1] = %q, want %q", state.CancelledReasons["task1"], "dependency failed")
	}
}
