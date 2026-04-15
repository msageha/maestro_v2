package plan

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// --- C1: retry_cascade.go — AssignWorkers empty assignments check ---

func TestCascadeRecoverRecursive_EmptyAssignments(t *testing.T) {
	// Setup: a cancelled task that depends on a failed task,
	// but worker assignment cannot assign because no workers have matching model.
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_c1_test",
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs: []string{"taskA", "taskB"},
			TaskStates: map[string]model.Status{
				"taskA": model.StatusFailed,
				"taskB": model.StatusCancelled,
			},
			TaskDependencies: map[string][]string{
				"taskA": {},
				"taskB": {"taskA"},
			},
			CancelledReasons: map[string]string{
				"taskB": "blocked_dependency_terminal:taskA",
			},
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: make(map[string]string),
		},
	}

	// Use a worker config where all workers are at max capacity
	workerConfig := model.WorkerConfig{
		Count:        1,
		DefaultModel: "sonnet",
	}
	limits := model.LimitsConfig{
		MaxPendingTasksPerWorker: 1,
	}
	// Worker already at capacity
	workerStates := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", PendingCount: 1},
	}
	origCache := map[string]model.Task{
		"taskB": {ID: "taskB", BloomLevel: 2},
	}

	_, err := cascadeRecover(state, "taskA", "retryA", workerConfig, limits, workerStates, origCache)
	if err == nil {
		t.Fatal("expected error when workers at capacity, got nil")
	}
	// Should get a worker assignment error, not a panic
	if !strings.Contains(err.Error(), "worker assignment") && !strings.Contains(err.Error(), "no available worker") {
		t.Errorf("expected worker assignment error, got: %v", err)
	}
}

// --- C2: inject.go — BlockedBy empty AddTask should add to phase ---

func TestAddTask_NoBlockedBy_AddedToPhase(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000040_c2test001"
	taskID1 := "task_0000000040_11111111"
	cfg := testConfig()
	lm := lock.NewMutexMap()

	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusSealed,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 1,
			RequiredTaskIDs:   []string{taskID1},
			OptionalTaskIDs:   []string{},
			TaskDependencies: map[string][]string{
				taskID1: {},
			},
			TaskStates: map[string]model.Status{
				taskID1: model.StatusCompleted,
			},
			CancelledReasons: make(map[string]string),
			AppliedResultIDs: make(map[string]string),
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "phase_001",
					Name:    "phase1",
					Type:    "concrete",
					Status:  model.PhaseStatusActive,
					TaskIDs: []string{taskID1},
				},
			},
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: make(map[string]string),
		},
		CreatedAt: "2025-01-01T00:00:00Z",
		UpdatedAt: "2025-01-01T00:00:00Z",
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	result, err := AddTask(InjectOptions{
		CommandID:          commandID,
		Purpose:            "independent task",
		Content:            "do something",
		AcceptanceCriteria: "done",
		BloomLevel:         2,
		Required:           true,
		BlockedBy:          []string{}, // empty!
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddTask returned error: %v", err)
	}

	// Verify the task was added to phase1
	sm := NewStateManager(maestroDir, lm)
	updatedState, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	found := false
	for _, tid := range updatedState.Phases[0].TaskIDs {
		if tid == result.TaskID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("task %s not added to phase; phase TaskIDs: %v", result.TaskID, updatedState.Phases[0].TaskIDs)
	}
}

// --- C3: worker_assign.go — BloomLevel=0 returns sonnet not opus ---

func TestGetModelForBloomLevel_Zero(t *testing.T) {
	got := GetModelForBloomLevel(0, false)
	if got != "sonnet" {
		t.Errorf("GetModelForBloomLevel(0, false) = %q, want %q", got, "sonnet")
	}
}

func TestGetModelForBloomLevel_NegativeDefaultsSonnet(t *testing.T) {
	got := GetModelForBloomLevel(-1, false)
	if got != "sonnet" {
		t.Errorf("GetModelForBloomLevel(-1, false) = %q, want %q", got, "sonnet")
	}
}

// --- H-bug5: retry_cascade.go — phaseIdx boundary check ---

func TestUpdateTaskStateForCascade_PhaseIdxBoundary(t *testing.T) {
	// When cancelledTaskID is not in any phase, findPhaseForTask returns nil, -1.
	// That path is fine (phase == nil → skipped).
	// This test verifies the function doesn't panic with valid data.
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_hbug5_test",
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs: []string{"tOld", "tNew"},
			TaskStates: map[string]model.Status{
				"tOld": model.StatusCancelled,
				"tNew": model.StatusPending,
			},
			TaskDependencies: map[string][]string{
				"tOld": {},
				"tNew": {},
			},
			CancelledReasons: map[string]string{},
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: map[string]string{"tNew": "tOld"},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{Name: "p1", Status: model.PhaseStatusActive, TaskIDs: []string{"tOld"}},
			},
		},
	}

	// Should succeed: tOld is in phase 0, which is valid
	err := updateTaskStateForCascade(state, "tOld", "tNew", []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify tNew was added to phase
	found := false
	for _, tid := range state.Phases[0].TaskIDs {
		if tid == "tNew" {
			found = true
			break
		}
	}
	if !found {
		t.Error("tNew not added to phase 0")
	}
}

// --- H-other12: state.go — CanComplete Phase-Task consistency ---

func TestCanComplete_PhaseTerminalButTaskActive(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 2,
			RequiredTaskIDs:   []string{"t1", "t2"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
				"t2": model.StatusInProgress, // non-terminal!
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					Name:    "phase1",
					Status:  model.PhaseStatusCompleted, // terminal
					TaskIDs: []string{"t1", "t2"},
				},
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatal("expected error for phase-task inconsistency, got nil")
	}
	if !strings.Contains(err.Error(), "non-terminal") {
		t.Errorf("expected error about non-terminal task, got: %v", err)
	}
}

func TestCanComplete_PhaseTerminalAllTasksTerminal(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 2,
			RequiredTaskIDs:   []string{"t1", "t2"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
				"t2": model.StatusCompleted,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					Name:    "phase1",
					Status:  model.PhaseStatusCompleted,
					TaskIDs: []string{"t1", "t2"},
				},
			},
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

// --- H-other13: validate.go / submit.go — buildAllowedBloomMap ---

func TestBuildAllowedBloomMap_WithLevels(t *testing.T) {
	m := buildAllowedBloomMap([]int{1, 3, 5})
	if !m[1] || !m[3] || !m[5] {
		t.Error("expected levels 1, 3, 5 to be allowed")
	}
	if m[2] || m[4] || m[6] {
		t.Error("expected levels 2, 4, 6 to not be allowed")
	}
}

func TestBuildAllowedBloomMap_Empty(t *testing.T) {
	m := buildAllowedBloomMap(nil)
	for l := BloomLevelMin; l <= BloomLevelMax; l++ {
		if !m[l] {
			t.Errorf("expected level %d to be allowed when empty", l)
		}
	}
}

// --- H-other14: worker_assign.go — BuildWorkerStates Count<=0 validation ---

func TestBuildWorkerStates_ZeroCount(t *testing.T) {
	maestroDir := t.TempDir()
	config := model.WorkerConfig{
		Count:        0,
		DefaultModel: "sonnet",
	}
	_, err := BuildWorkerStates(maestroDir, config)
	if err == nil {
		t.Fatal("expected error for Count=0, got nil")
	}
	if !strings.Contains(err.Error(), "must be greater than 0") {
		t.Errorf("expected error about count, got: %v", err)
	}
}

func TestBuildWorkerStates_NegativeCount(t *testing.T) {
	maestroDir := t.TempDir()
	config := model.WorkerConfig{
		Count:        -1,
		DefaultModel: "sonnet",
	}
	_, err := BuildWorkerStates(maestroDir, config)
	if err == nil {
		t.Fatal("expected error for negative Count, got nil")
	}
}

// --- H-other15: retry.go — resolveBlockedByViaLineage resolved dependency existence check ---

func TestValidateRetryRequest_ResolvedDepNotFound(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000050_h15test1"
	taskA := "task_0000000050_aaaaaaaa"
	taskB := "task_0000000050_bbbbbbbb"
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// taskB depends on taskA. taskA was retried to taskA_v2, but taskA_v2
	// was then deleted from task states (simulating a stale lineage).
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusSealed,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		TaskTracking: model.TaskTracking{
			ExpectedTaskCount: 2,
			RequiredTaskIDs:   []string{"taskA_v2", taskB},
			OptionalTaskIDs:   []string{},
			TaskDependencies: map[string][]string{
				taskA:     {},
				taskB:     {taskA},
				"taskA_v2": {},
			},
			TaskStates: map[string]model.Status{
				taskA: model.StatusFailed,
				taskB: model.StatusFailed,
				// taskA_v2 is NOT in TaskStates — simulates stale lineage
			},
			CancelledReasons: make(map[string]string),
			AppliedResultIDs: make(map[string]string),
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: map[string]string{
				"taskA_v2": taskA, // taskA was retried to taskA_v2
			},
		},
		CreatedAt: "2025-01-01T00:00:00Z",
		UpdatedAt: "2025-01-01T00:00:00Z",
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	_, err := AddRetryTask(RetryOptions{
		CommandID:          commandID,
		RetryOf:            taskB,
		Purpose:            "retry taskB",
		Content:            "redo B",
		AcceptanceCriteria: "B works",
		BloomLevel:         2,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected error for resolved dependency not found, got nil")
	}
	if !strings.Contains(err.Error(), "not found in command state") {
		t.Errorf("expected 'not found in command state' error, got: %v", err)
	}
}

// --- M32: inject.go — idempotency dedup returns non-empty Worker/Model ---

func TestAddTask_IdempotencyDedup_NonEmptyWorkerModel(t *testing.T) {
	maestroDir, commandID, completedTaskID := setupInjectFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	opts := InjectOptions{
		CommandID:          commandID,
		Purpose:            "resolve merge conflict",
		Content:            "fix conflicting files",
		AcceptanceCriteria: "build passes",
		BloomLevel:         3,
		Required:           true,
		BlockedBy:          []string{completedTaskID},
		IdempotencyKey:     "dedup-m32-test",
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	}

	// First call
	_, err := AddTask(opts)
	if err != nil {
		t.Fatalf("first AddTask: %v", err)
	}

	// Second call (dedup) — Worker and Model should never be empty
	result2, err := AddTask(opts)
	if err != nil {
		t.Fatalf("second AddTask: %v", err)
	}
	if !result2.Deduplicated {
		t.Error("expected dedup")
	}
	if result2.Worker == "" {
		t.Error("dedup result Worker should not be empty")
	}
	if result2.Model == "" {
		t.Error("dedup result Model should not be empty")
	}
}
