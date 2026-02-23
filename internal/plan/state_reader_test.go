package plan

import (
	"os"
	"path/filepath"
	"testing"

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
