package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func TestFindPhaseForTask(t *testing.T) {
	tests := []struct {
		name       string
		state      *model.CommandState
		taskID     string
		wantPhase  string
		wantIdx    int
	}{
		{
			name: "task in first phase",
			state: &model.CommandState{
				Phases: []model.Phase{
					{Name: "phase1", TaskIDs: []string{"t1", "t2"}},
					{Name: "phase2", TaskIDs: []string{"t3"}},
				},
			},
			taskID:    "t2",
			wantPhase: "phase1",
			wantIdx:   0,
		},
		{
			name: "task in second phase",
			state: &model.CommandState{
				Phases: []model.Phase{
					{Name: "phase1", TaskIDs: []string{"t1"}},
					{Name: "phase2", TaskIDs: []string{"t2", "t3"}},
				},
			},
			taskID:    "t3",
			wantPhase: "phase2",
			wantIdx:   1,
		},
		{
			name: "task not in any phase",
			state: &model.CommandState{
				Phases: []model.Phase{
					{Name: "phase1", TaskIDs: []string{"t1"}},
				},
			},
			taskID:    "t99",
			wantPhase: "",
			wantIdx:   -1,
		},
		{
			name: "no phases",
			state: &model.CommandState{
				Phases: nil,
			},
			taskID:    "t1",
			wantPhase: "",
			wantIdx:   -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase, idx := findPhaseForTask(tt.state, tt.taskID)
			if idx != tt.wantIdx {
				t.Errorf("idx = %d, want %d", idx, tt.wantIdx)
			}
			if tt.wantPhase == "" {
				if phase != nil {
					t.Errorf("phase = %+v, want nil", phase)
				}
			} else {
				if phase == nil || phase.Name != tt.wantPhase {
					t.Errorf("phase.Name = %v, want %q", phase, tt.wantPhase)
				}
			}
		})
	}
}

func TestReplaceInRequiredOrOptional(t *testing.T) {
	tests := []struct {
		name            string
		requiredIDs     []string
		optionalIDs     []string
		oldID           string
		newID           string
		wantRequired    []string
		wantOptional    []string
		wantErr         bool
	}{
		{
			name:         "replace in required",
			requiredIDs:  []string{"t1", "t2", "t3"},
			optionalIDs:  []string{"t4"},
			oldID:        "t2",
			newID:        "t2_retry",
			wantRequired: []string{"t1", "t2_retry", "t3"},
			wantOptional: []string{"t4"},
		},
		{
			name:         "replace in optional",
			requiredIDs:  []string{"t1"},
			optionalIDs:  []string{"t4", "t5"},
			oldID:        "t5",
			newID:        "t5_retry",
			wantRequired: []string{"t1"},
			wantOptional: []string{"t4", "t5_retry"},
		},
		{
			name:         "not found returns error",
			requiredIDs:  []string{"t1"},
			optionalIDs:  []string{"t2"},
			oldID:        "t99",
			newID:        "t99_retry",
			wantRequired: []string{"t1"},
			wantOptional: []string{"t2"},
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &model.CommandState{
				RequiredTaskIDs: append([]string{}, tt.requiredIDs...),
				OptionalTaskIDs: append([]string{}, tt.optionalIDs...),
			}
			err := replaceInRequiredOrOptional(state, tt.oldID, tt.newID)

			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !sliceEqual(state.RequiredTaskIDs, tt.wantRequired) {
				t.Errorf("RequiredTaskIDs = %v, want %v", state.RequiredTaskIDs, tt.wantRequired)
			}
			if !sliceEqual(state.OptionalTaskIDs, tt.wantOptional) {
				t.Errorf("OptionalTaskIDs = %v, want %v", state.OptionalTaskIDs, tt.wantOptional)
			}
		})
	}
}

func TestReplaceInRequiredOrOptional_SystemCommitTaskID(t *testing.T) {
	commitID := "sys_commit_1"
	state := &model.CommandState{
		RequiredTaskIDs:    []string{"t1"},
		OptionalTaskIDs:    []string{},
		SystemCommitTaskID: &commitID,
	}
	err := replaceInRequiredOrOptional(state, "sys_commit_1", "sys_commit_2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state.SystemCommitTaskID == nil || *state.SystemCommitTaskID != "sys_commit_2" {
		t.Errorf("SystemCommitTaskID = %v, want sys_commit_2", state.SystemCommitTaskID)
	}
}

func TestRewriteDependencies(t *testing.T) {
	state := &model.CommandState{
		TaskDependencies: map[string][]string{
			"t2": {"t1"},
			"t3": {"t1", "t2"},
			"t4": {"t3"},
		},
	}

	rewriteDependencies(state, "t1", "t1_v2")

	if !sliceEqual(state.TaskDependencies["t2"], []string{"t1_v2"}) {
		t.Errorf("t2 deps = %v, want [t1_v2]", state.TaskDependencies["t2"])
	}
	if !sliceEqual(state.TaskDependencies["t3"], []string{"t1_v2", "t2"}) {
		t.Errorf("t3 deps = %v, want [t1_v2, t2]", state.TaskDependencies["t3"])
	}
	// t4 should be unchanged (doesn't depend on t1)
	if !sliceEqual(state.TaskDependencies["t4"], []string{"t3"}) {
		t.Errorf("t4 deps = %v, want [t3]", state.TaskDependencies["t4"])
	}
}

func TestFindCascadeCandidates(t *testing.T) {
	state := &model.CommandState{
		CancelledReasons: map[string]string{
			"t2": "blocked_dependency_terminal:t1",
			"t3": "blocked_dependency_terminal:t1",
			"t4": "blocked_dependency_terminal:t5",
			"t5": "command_cancel_requested",
		},
	}

	candidates := findCascadeCandidates(state, "t1")

	candidateSet := make(map[string]bool)
	for _, c := range candidates {
		candidateSet[c] = true
	}

	if !candidateSet["t2"] || !candidateSet["t3"] {
		t.Errorf("candidates = %v, want t2 and t3", candidates)
	}
	if candidateSet["t4"] || candidateSet["t5"] {
		t.Errorf("candidates should not include t4 or t5, got %v", candidates)
	}
}

func TestResolveBlockedByViaLineage(t *testing.T) {
	tests := []struct {
		name      string
		blockedBy []string
		lineage   map[string]string
		want      []string
		wantErr   bool
	}{
		{
			name:      "no lineage",
			blockedBy: []string{"t1", "t2"},
			lineage:   map[string]string{},
			want:      []string{"t1", "t2"},
		},
		{
			name:      "single hop",
			blockedBy: []string{"t1"},
			lineage:   map[string]string{"t1_v2": "t1"},
			want:      []string{"t1_v2"},
		},
		{
			name:      "multi hop",
			blockedBy: []string{"t1"},
			lineage:   map[string]string{"t1_v2": "t1", "t1_v3": "t1_v2"},
			want:      []string{"t1_v3"},
		},
		{
			name:      "mixed resolved and unresolved",
			blockedBy: []string{"t1", "t2"},
			lineage:   map[string]string{"t1_v2": "t1"},
			want:      []string{"t1_v2", "t2"},
		},
		{
			name:      "cycle returns error",
			blockedBy: []string{"t1"},
			lineage:   map[string]string{"t2": "t1", "t1": "t2"},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBlockedByViaLineage(tt.blockedBy, tt.lineage)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveBlockedByViaLineage error = %v, wantErr = %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !sliceEqual(got, tt.want) {
				t.Errorf("resolveBlockedByViaLineage = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetLatestDescendant(t *testing.T) {
	tests := []struct {
		name    string
		taskID  string
		lineage map[string]string
		want    string
		wantErr bool
	}{
		{
			name:    "no descendants",
			taskID:  "t1",
			lineage: map[string]string{},
			want:    "t1",
		},
		{
			name:    "one descendant",
			taskID:  "t1",
			lineage: map[string]string{"t1_v2": "t1"},
			want:    "t1_v2",
		},
		{
			name:    "chain of descendants",
			taskID:  "t1",
			lineage: map[string]string{"t1_v2": "t1", "t1_v3": "t1_v2"},
			want:    "t1_v3",
		},
		{
			name:    "cycle returns error",
			taskID:  "t1",
			lineage: map[string]string{"t2": "t1", "t1": "t2"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build reverse lineage (old→new) from lineage (new→old)
			reverseLineage := make(map[string]string, len(tt.lineage))
			for newID, oldID := range tt.lineage {
				reverseLineage[oldID] = newID
			}
			got, err := getLatestDescendant(tt.taskID, reverseLineage)
			if (err != nil) != tt.wantErr {
				t.Errorf("getLatestDescendant(%q) error = %v, wantErr = %v", tt.taskID, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("getLatestDescendant(%q) = %q, want %q", tt.taskID, got, tt.want)
			}
			if tt.wantErr && err != nil {
				if !strings.Contains(err.Error(), "lineage cycle detected") {
					t.Errorf("error = %q, want to contain %q", err.Error(), "lineage cycle detected")
				}
			}
		})
	}
}

func TestReopenPhase(t *testing.T) {
	now := "2024-01-01T00:00:00Z"

	tests := []struct {
		name      string
		status    model.PhaseStatus
		wantErr   bool
		wantStatus model.PhaseStatus
	}{
		{
			name:       "reopen failed phase",
			status:     model.PhaseStatusFailed,
			wantErr:    false,
			wantStatus: model.PhaseStatusActive,
		},
		{
			name:    "cannot reopen active phase",
			status:  model.PhaseStatusActive,
			wantErr: true,
		},
		{
			name:    "cannot reopen completed phase",
			status:  model.PhaseStatusCompleted,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &model.CommandState{
				Phases: []model.Phase{
					{Name: "p1", Status: tt.status},
				},
			}
			err := reopenPhase(state, 0, now)
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if state.Phases[0].Status != tt.wantStatus {
					t.Errorf("status = %s, want %s", state.Phases[0].Status, tt.wantStatus)
				}
				if state.Phases[0].ReopenedAt == nil || *state.Phases[0].ReopenedAt != now {
					t.Error("ReopenedAt should be set")
				}
				if state.Phases[0].CompletedAt != nil {
					t.Error("CompletedAt should be nil after reopen")
				}
			}
		})
	}
}

func TestCopyAndRestoreState(t *testing.T) {
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		TaskStates:    map[string]model.Status{"t1": model.StatusPending},
	}

	backup, err := copyState(state)
	if err != nil {
		t.Fatalf("copyState error: %v", err)
	}

	// Mutate state
	state.TaskStates["t1"] = model.StatusCompleted
	state.TaskStates["t2"] = model.StatusFailed

	// Restore
	restoreState(state, backup)

	if state.TaskStates["t1"] != model.StatusPending {
		t.Errorf("t1 status = %s after restore, want pending", state.TaskStates["t1"])
	}
	if _, exists := state.TaskStates["t2"]; exists {
		t.Error("t2 should not exist after restore")
	}
}

func TestAddRetryTask_NilLockMap(t *testing.T) {
	_, err := AddRetryTask(RetryOptions{
		CommandID:  "cmd1",
		RetryOf:    "t1",
		MaestroDir: t.TempDir(),
		LockMap:    nil,
	})
	if err == nil {
		t.Fatal("expected error for nil LockMap")
	}
}

// --- AddRetryTask integration tests ---

// setupRetryFixture creates a maestro directory with a sealed state containing
// a failed task suitable for retrying. Returns (maestroDir, commandID, failedTaskID).
func setupRetryFixture(t *testing.T) (string, string, string) {
	t.Helper()
	maestroDir := setupMaestroDir(t)
	commandID := "cmd_0000000020_aabbccdd"
	taskID1 := "task_0000000020_11111111"
	taskID2 := "task_0000000020_22222222"

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
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{taskID1, taskID2},
		OptionalTaskIDs:   []string{},
		TaskDependencies: map[string][]string{
			taskID1: {},
			taskID2: {taskID1},
		},
		TaskStates: map[string]model.Status{
			taskID1: model.StatusCompleted,
			taskID2: model.StatusFailed,
		},
		CancelledReasons: make(map[string]string),
		AppliedResultIDs: make(map[string]string),
		RetryLineage:     make(map[string]string),
		CreatedAt:        "2025-01-01T00:00:00Z",
		UpdatedAt:        "2025-01-01T00:00:00Z",
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	return maestroDir, commandID, taskID2
}

func TestAddRetryTask_HappyPath(t *testing.T) {
	maestroDir, commandID, failedTaskID := setupRetryFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	result, err := AddRetryTask(RetryOptions{
		CommandID:          commandID,
		RetryOf:            failedTaskID,
		Purpose:            "retry task 2",
		Content:            "redo task 2",
		AcceptanceCriteria: "task 2 passes",
		BloomLevel:         2,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddRetryTask returned error: %v", err)
	}

	// Verify result fields
	if result.TaskID == "" {
		t.Error("result.TaskID is empty")
	}
	if result.Worker == "" {
		t.Error("result.Worker is empty")
	}
	if result.Model == "" {
		t.Error("result.Model is empty")
	}
	if result.Replaced != failedTaskID {
		t.Errorf("result.Replaced = %q, want %q", result.Replaced, failedTaskID)
	}

	// Verify state was persisted
	sm := NewStateManager(maestroDir, lm)
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// New task ID should replace old in RequiredTaskIDs
	foundNew := false
	foundOld := false
	for _, id := range state.RequiredTaskIDs {
		if id == result.TaskID {
			foundNew = true
		}
		if id == failedTaskID {
			foundOld = true
		}
	}
	if !foundNew {
		t.Errorf("new task %s not found in RequiredTaskIDs %v", result.TaskID, state.RequiredTaskIDs)
	}
	if foundOld {
		t.Errorf("old task %s still in RequiredTaskIDs %v", failedTaskID, state.RequiredTaskIDs)
	}

	// Lineage recorded
	if state.RetryLineage[result.TaskID] != failedTaskID {
		t.Errorf("RetryLineage[%s] = %q, want %q", result.TaskID, state.RetryLineage[result.TaskID], failedTaskID)
	}

	// Dependencies rewritten: tasks that depended on failedTaskID now depend on newTaskID
	for taskID, deps := range state.TaskDependencies {
		for _, dep := range deps {
			if dep == failedTaskID && taskID != result.TaskID {
				t.Errorf("task %s still depends on old task %s", taskID, failedTaskID)
			}
		}
	}

	// New task is pending
	if state.TaskStates[result.TaskID] != model.StatusPending {
		t.Errorf("new task state = %s, want pending", state.TaskStates[result.TaskID])
	}

	// Queue entry written
	totalQueueTasks := 0
	for i := 1; i <= 2; i++ {
		queueFile := filepath.Join(maestroDir, "queue", fmt.Sprintf("worker%d.yaml", i))
		data, err := os.ReadFile(queueFile)
		if err != nil {
			continue
		}
		var tq model.TaskQueue
		if yamlv3.Unmarshal(data, &tq) != nil {
			continue
		}
		for _, task := range tq.Tasks {
			if task.ID == result.TaskID {
				totalQueueTasks++
				if task.Status != model.StatusPending {
					t.Errorf("queue task status = %s, want pending", task.Status)
				}
				if task.Purpose != "retry task 2" {
					t.Errorf("queue task purpose = %q, want %q", task.Purpose, "retry task 2")
				}
			}
		}
	}
	if totalQueueTasks != 1 {
		t.Errorf("queue entries for new task = %d, want 1", totalQueueTasks)
	}
}

func TestAddRetryTask_CascadeRecover(t *testing.T) {
	// Setup: A→B→C chain where A failed and B,C were cancelled due to dependency failure
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()
	commandID := "cmd_0000000021_aabbccdd"
	taskA := "task_0000000021_aaaaaaaa"
	taskB := "task_0000000021_bbbbbbbb"
	taskC := "task_0000000021_cccccccc"

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
		ExpectedTaskCount: 3,
		RequiredTaskIDs:   []string{taskA, taskB, taskC},
		OptionalTaskIDs:   []string{},
		TaskDependencies: map[string][]string{
			taskA: {},
			taskB: {taskA},
			taskC: {taskB},
		},
		TaskStates: map[string]model.Status{
			taskA: model.StatusFailed,
			taskB: model.StatusCancelled,
			taskC: model.StatusCancelled,
		},
		CancelledReasons: map[string]string{
			taskB: "blocked_dependency_terminal:" + taskA,
			taskC: "blocked_dependency_terminal:" + taskB,
		},
		AppliedResultIDs: make(map[string]string),
		RetryLineage:     make(map[string]string),
		CreatedAt:        "2025-01-01T00:00:00Z",
		UpdatedAt:        "2025-01-01T00:00:00Z",
	}

	// Write original tasks to queue so cascade can inherit content
	for i := 1; i <= 2; i++ {
		queueFile := filepath.Join(maestroDir, "queue", fmt.Sprintf("worker%d.yaml", i))
		data, _ := os.ReadFile(queueFile)
		var tq model.TaskQueue
		yamlv3.Unmarshal(data, &tq)
		if i == 1 {
			tq.Tasks = append(tq.Tasks, model.Task{
				ID: taskB, CommandID: commandID, Purpose: "original B",
				Content: "do B", AcceptanceCriteria: "B done", BloomLevel: 2,
				Status: model.StatusCancelled, CreatedAt: "2025-01-01T00:00:00Z", UpdatedAt: "2025-01-01T00:00:00Z",
			})
			tq.Tasks = append(tq.Tasks, model.Task{
				ID: taskC, CommandID: commandID, Purpose: "original C",
				Content: "do C", AcceptanceCriteria: "C done", BloomLevel: 2,
				Status: model.StatusCancelled, CreatedAt: "2025-01-01T00:00:00Z", UpdatedAt: "2025-01-01T00:00:00Z",
			})
		}
		yamlutil.AtomicWrite(queueFile, tq)
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	result, err := AddRetryTask(RetryOptions{
		CommandID:          commandID,
		RetryOf:            taskA,
		Purpose:            "retry A",
		Content:            "redo A",
		AcceptanceCriteria: "A works",
		BloomLevel:         2,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("AddRetryTask returned error: %v", err)
	}

	// Should have cascade recovered B and C
	if len(result.CascadeRecovered) != 2 {
		t.Fatalf("CascadeRecovered count = %d, want 2", len(result.CascadeRecovered))
	}

	replacedSet := make(map[string]string) // replaced → new task ID
	for _, cr := range result.CascadeRecovered {
		replacedSet[cr.Replaced] = cr.TaskID
		if cr.Worker == "" {
			t.Errorf("cascade recovered task %s has empty Worker", cr.TaskID)
		}
	}

	if _, ok := replacedSet[taskB]; !ok {
		t.Errorf("taskB (%s) not in cascade recovered", taskB)
	}
	if _, ok := replacedSet[taskC]; !ok {
		t.Errorf("taskC (%s) not in cascade recovered", taskC)
	}

	// Verify state consistency
	sm := NewStateManager(maestroDir, lm)
	updatedState, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// All new tasks should be pending
	if updatedState.TaskStates[result.TaskID] != model.StatusPending {
		t.Errorf("new A state = %s, want pending", updatedState.TaskStates[result.TaskID])
	}
	for _, cr := range result.CascadeRecovered {
		if updatedState.TaskStates[cr.TaskID] != model.StatusPending {
			t.Errorf("cascade %s state = %s, want pending", cr.TaskID, updatedState.TaskStates[cr.TaskID])
		}
	}

	// Lineage should be recorded for all
	if updatedState.RetryLineage[result.TaskID] != taskA {
		t.Errorf("lineage for new A = %q, want %q", updatedState.RetryLineage[result.TaskID], taskA)
	}
	newB := replacedSet[taskB]
	if updatedState.RetryLineage[newB] != taskB {
		t.Errorf("lineage for new B = %q, want %q", updatedState.RetryLineage[newB], taskB)
	}
	newC := replacedSet[taskC]
	if updatedState.RetryLineage[newC] != taskC {
		t.Errorf("lineage for new C = %q, want %q", updatedState.RetryLineage[newC], taskC)
	}
}

func TestAddRetryTask_ValidationFailures(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) (string, RetryOptions)
		wantErr string
	}{
		{
			name: "plan not sealed",
			setup: func(t *testing.T) (string, RetryOptions) {
				maestroDir := setupMaestroDir(t)
				commandID := "cmd_0000000030_aabbccdd"
				state := &model.CommandState{
					SchemaVersion:    1,
					FileType:         "state_command",
					CommandID:        commandID,
					PlanStatus:       model.PlanStatusPlanning,
					TaskStates:       map[string]model.Status{"t1": model.StatusFailed},
					TaskDependencies: make(map[string][]string),
					CancelledReasons: make(map[string]string),
					AppliedResultIDs: make(map[string]string),
					RetryLineage:     make(map[string]string),
					CreatedAt:        "2025-01-01T00:00:00Z",
					UpdatedAt:        "2025-01-01T00:00:00Z",
				}
				statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
				yamlutil.AtomicWrite(statePath, state)
				return maestroDir, RetryOptions{
					CommandID: commandID, RetryOf: "t1", Purpose: "p", Content: "c",
					AcceptanceCriteria: "ac", BloomLevel: 2, MaestroDir: maestroDir,
					Config: testConfig(), LockMap: lock.NewMutexMap(),
				}
			},
			wantErr: "sealed",
		},
		{
			name: "command cancelled",
			setup: func(t *testing.T) (string, RetryOptions) {
				maestroDir := setupMaestroDir(t)
				commandID := "cmd_0000000031_aabbccdd"
				now := "2025-01-01T00:00:00Z"
				cancelBy := "user"
				cancelReason := "test cancel"
				state := &model.CommandState{
					SchemaVersion: 1,
					FileType:      "state_command",
					CommandID:     commandID,
					PlanStatus:    model.PlanStatusSealed,
					Cancel: model.CancelState{
						Requested:   true,
						RequestedAt: &now,
						RequestedBy: &cancelBy,
						Reason:      &cancelReason,
					},
					TaskStates:       map[string]model.Status{"t1": model.StatusFailed},
					TaskDependencies: make(map[string][]string),
					CancelledReasons: make(map[string]string),
					AppliedResultIDs: make(map[string]string),
					RetryLineage:     make(map[string]string),
					CreatedAt:        now,
					UpdatedAt:        now,
				}
				statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
				yamlutil.AtomicWrite(statePath, state)
				return maestroDir, RetryOptions{
					CommandID: commandID, RetryOf: "t1", Purpose: "p", Content: "c",
					AcceptanceCriteria: "ac", BloomLevel: 2, MaestroDir: maestroDir,
					Config: testConfig(), LockMap: lock.NewMutexMap(),
				}
			},
			wantErr: "cancelled",
		},
		{
			name: "retry-of task not found",
			setup: func(t *testing.T) (string, RetryOptions) {
				maestroDir := setupMaestroDir(t)
				commandID := "cmd_0000000032_aabbccdd"
				state := &model.CommandState{
					SchemaVersion:    1,
					FileType:         "state_command",
					CommandID:        commandID,
					PlanStatus:       model.PlanStatusSealed,
					TaskStates:       map[string]model.Status{},
					TaskDependencies: make(map[string][]string),
					CancelledReasons: make(map[string]string),
					AppliedResultIDs: make(map[string]string),
					RetryLineage:     make(map[string]string),
					CreatedAt:        "2025-01-01T00:00:00Z",
					UpdatedAt:        "2025-01-01T00:00:00Z",
				}
				statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
				yamlutil.AtomicWrite(statePath, state)
				return maestroDir, RetryOptions{
					CommandID: commandID, RetryOf: "task_nonexistent_00000000", Purpose: "p", Content: "c",
					AcceptanceCriteria: "ac", BloomLevel: 2, MaestroDir: maestroDir,
					Config: testConfig(), LockMap: lock.NewMutexMap(),
				}
			},
			wantErr: "not found",
		},
		{
			name: "retry-of task not failed",
			setup: func(t *testing.T) (string, RetryOptions) {
				maestroDir := setupMaestroDir(t)
				commandID := "cmd_0000000033_aabbccdd"
				taskID := "task_0000000033_11111111"
				state := &model.CommandState{
					SchemaVersion:    1,
					FileType:         "state_command",
					CommandID:        commandID,
					PlanStatus:       model.PlanStatusSealed,
					RequiredTaskIDs:  []string{taskID},
					TaskStates:       map[string]model.Status{taskID: model.StatusCompleted},
					TaskDependencies: map[string][]string{taskID: {}},
					CancelledReasons: make(map[string]string),
					AppliedResultIDs: make(map[string]string),
					RetryLineage:     make(map[string]string),
					CreatedAt:        "2025-01-01T00:00:00Z",
					UpdatedAt:        "2025-01-01T00:00:00Z",
				}
				statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
				yamlutil.AtomicWrite(statePath, state)
				return maestroDir, RetryOptions{
					CommandID: commandID, RetryOf: taskID, Purpose: "p", Content: "c",
					AcceptanceCriteria: "ac", BloomLevel: 2, MaestroDir: maestroDir,
					Config: testConfig(), LockMap: lock.NewMutexMap(),
				}
			},
			wantErr: "must be failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, opts := tt.setup(t)
			_, err := AddRetryTask(opts)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestAddRetryTask_Rollback_OnSaveStateFailure(t *testing.T) {
	// Setup a valid retry scenario, but make SaveState fail by removing the state directory
	maestroDir, commandID, failedTaskID := setupRetryFixture(t)
	cfg := testConfig()
	lm := lock.NewMutexMap()

	// Remove the state/commands directory after loading to cause SaveState to fail
	// We do this by making the directory read-only
	stateDir := filepath.Join(maestroDir, "state", "commands")

	// First, do a successful retry to confirm the fixture works
	_, err := AddRetryTask(RetryOptions{
		CommandID:          commandID,
		RetryOf:            failedTaskID,
		Purpose:            "retry first",
		Content:            "redo",
		AcceptanceCriteria: "works",
		BloomLevel:         2,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err != nil {
		t.Fatalf("first retry failed: %v", err)
	}

	// Load state to find the new task ID (which is now failed's replacement)
	sm := NewStateManager(maestroDir, lm)
	state, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// Find the new failed task to retry again
	var newFailedTaskID string
	for _, id := range state.RequiredTaskIDs {
		if state.TaskStates[id] == model.StatusPending {
			// Manually mark it as failed for the next retry attempt
			state.TaskStates[id] = model.StatusFailed
			newFailedTaskID = id
			break
		}
	}
	if newFailedTaskID == "" {
		t.Fatal("no pending task found to mark as failed")
	}
	if err := sm.SaveState(state); err != nil {
		t.Fatalf("save state: %v", err)
	}

	// Now make the state directory unwritable to force SaveState failure
	if err := os.Chmod(stateDir, 0555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(stateDir, 0755)

	// This retry should fail due to SaveState failure
	_, err = AddRetryTask(RetryOptions{
		CommandID:          commandID,
		RetryOf:            newFailedTaskID,
		Purpose:            "retry second",
		Content:            "redo again",
		AcceptanceCriteria: "works",
		BloomLevel:         2,
		MaestroDir:         maestroDir,
		Config:             cfg,
		LockMap:            lm,
	})
	if err == nil {
		t.Fatal("expected error from SaveState failure, got nil")
	}
	if !strings.Contains(err.Error(), "save state") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "save state")
	}

	// Restore permissions and verify state was rolled back
	os.Chmod(stateDir, 0755)
	stateAfter, err := sm.LoadState(commandID)
	if err != nil {
		t.Fatalf("load state after rollback: %v", err)
	}

	// The failed task should still be in the state (not replaced)
	if stateAfter.TaskStates[newFailedTaskID] != model.StatusFailed {
		t.Errorf("task %s state = %s after rollback, want failed", newFailedTaskID, stateAfter.TaskStates[newFailedTaskID])
	}
}

// sliceEqual compares two string slices for equality.
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
