package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// bridgeTestConfig returns a minimal Config suitable for bridge tests.
func bridgeTestConfig() model.Config {
	return model.Config{
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{
				Count:        2,
				DefaultModel: "sonnet",
				Models:       map[string]string{"worker2": "opus"},
			},
		},
		Limits: model.LimitsConfig{
			MaxPendingTasksPerWorker: 10,
		},
	}
}

// setupBridgeMaestroDir creates a temp maestro directory with empty worker queue files.
func setupBridgeMaestroDir(t *testing.T) string {
	t.Helper()
	return testutil.SetupDirWithQueues(t, 2)
}

// writeBridgePlannerQueue writes a planner queue file with a single command.
func writeBridgePlannerQueue(t *testing.T, maestroDir, commandID string, status model.Status) {
	t.Helper()
	leaseOwner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:         commandID,
				Content:    "test command",
				Priority:   100,
				Status:     status,
				LeaseOwner: &leaseOwner,
				CreatedAt:  "2025-01-01T00:00:00Z",
				UpdatedAt:  "2025-01-01T00:00:00Z",
			},
		},
	}
	path := filepath.Join(maestroDir, "queue", "planner.yaml")
	if err := yamlutil.AtomicWrite(path, cq); err != nil {
		t.Fatalf("write planner queue: %v", err)
	}
}

// writeBridgeTasksFile writes a tasks YAML file for Submit.
func writeBridgeTasksFile(t *testing.T, yamlContent string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("write tasks file: %v", err)
	}
	return path
}

// writeBridgeState writes a CommandState YAML to the state directory.
func writeBridgeState(t *testing.T, maestroDir string, state *model.CommandState) {
	t.Helper()
	statePath := filepath.Join(maestroDir, "state", "commands", state.CommandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func TestSubmitParams_Unmarshal(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		check   func(p submitParams) error
	}{
		{
			name:  "valid params",
			input: `{"command_id":"cmd1","tasks_file":"plan.yaml","phase_name":"phase1","dry_run":true}`,
			check: func(p submitParams) error {
				if p.CommandID != "cmd1" {
					t.Errorf("CommandID = %q, want cmd1", p.CommandID)
				}
				if p.TasksFile != "plan.yaml" {
					t.Errorf("TasksFile = %q, want plan.yaml", p.TasksFile)
				}
				if p.PhaseName != "phase1" {
					t.Errorf("PhaseName = %q, want phase1", p.PhaseName)
				}
				if !p.DryRun {
					t.Error("DryRun = false, want true")
				}
				return nil
			},
		},
		{
			name:  "minimal params",
			input: `{"command_id":"cmd2","tasks_file":"t.yaml"}`,
			check: func(p submitParams) error {
				if p.CommandID != "cmd2" {
					t.Errorf("CommandID = %q, want cmd2", p.CommandID)
				}
				if p.DryRun {
					t.Error("DryRun should default to false")
				}
				return nil
			},
		},
		{
			name:    "invalid JSON",
			input:   `{invalid`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p submitParams
			err := json.Unmarshal([]byte(tt.input), &p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(p)
			}
		})
	}
}

func TestCompleteParams_Unmarshal(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		check   func(p completeParams)
	}{
		{
			name:  "valid params",
			input: `{"command_id":"cmd1","summary":"all done"}`,
			check: func(p completeParams) {
				if p.CommandID != "cmd1" {
					t.Errorf("CommandID = %q, want cmd1", p.CommandID)
				}
				if p.Summary != "all done" {
					t.Errorf("Summary = %q, want 'all done'", p.Summary)
				}
			},
		},
		{
			name:    "invalid JSON",
			input:   `not json`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p completeParams
			err := json.Unmarshal([]byte(tt.input), &p)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(p)
			}
		})
	}
}

func TestRetryParams_Unmarshal(t *testing.T) {
	input := `{
		"command_id": "cmd1",
		"retry_of": "t1",
		"purpose": "fix bug",
		"content": "retry content",
		"acceptance_criteria": "tests pass",
		"constraints": ["no side effects"],
		"blocked_by": ["t0"],
		"bloom_level": 3,
		"tools_hint": ["bash"]
	}`

	var p retryParams
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if p.CommandID != "cmd1" {
		t.Errorf("CommandID = %q, want cmd1", p.CommandID)
	}
	if p.RetryOf != "t1" {
		t.Errorf("RetryOf = %q, want t1", p.RetryOf)
	}
	if p.BloomLevel != 3 {
		t.Errorf("BloomLevel = %d, want 3", p.BloomLevel)
	}
	if len(p.Constraints) != 1 || p.Constraints[0] != "no side effects" {
		t.Errorf("Constraints = %v, want [no side effects]", p.Constraints)
	}
	if len(p.BlockedBy) != 1 || p.BlockedBy[0] != "t0" {
		t.Errorf("BlockedBy = %v, want [t0]", p.BlockedBy)
	}
	if len(p.ToolsHint) != 1 || p.ToolsHint[0] != "bash" {
		t.Errorf("ToolsHint = %v, want [bash]", p.ToolsHint)
	}
}

func TestRebuildParams_Unmarshal(t *testing.T) {
	input := `{"command_id": "cmd_rebuild"}`

	var p rebuildParams
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if p.CommandID != "cmd_rebuild" {
		t.Errorf("CommandID = %q, want cmd_rebuild", p.CommandID)
	}
}

func TestPlanExecutorImpl_SubmitInvalidJSON(t *testing.T) {
	pe := &PlanExecutorImpl{
		MaestroDir: t.TempDir(),
	}

	_, err := pe.Submit(json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("Submit with invalid JSON should return error")
	}
}

func TestPlanExecutorImpl_CompleteInvalidJSON(t *testing.T) {
	pe := &PlanExecutorImpl{
		MaestroDir: t.TempDir(),
	}

	_, err := pe.Complete(json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("Complete with invalid JSON should return error")
	}
}

func TestPlanExecutorImpl_AddRetryTaskInvalidJSON(t *testing.T) {
	pe := &PlanExecutorImpl{
		MaestroDir: t.TempDir(),
	}

	_, err := pe.AddRetryTask(json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("AddRetryTask with invalid JSON should return error")
	}
}

func TestPlanExecutorImpl_RebuildInvalidJSON(t *testing.T) {
	pe := &PlanExecutorImpl{
		MaestroDir: t.TempDir(),
	}

	_, err := pe.Rebuild(json.RawMessage(`{invalid`))
	if err == nil {
		t.Fatal("Rebuild with invalid JSON should return error")
	}
}

// --- Success path tests ---

func TestPlanExecutorImpl_SubmitSuccess(t *testing.T) {
	maestroDir := setupBridgeMaestroDir(t)
	cfg := bridgeTestConfig()
	commandID := "cmd_0000000001_aabbccdd"

	writeBridgePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	tasksYAML := `tasks:
  - name: task_a
    purpose: do task a
    content: implement feature a
    acceptance_criteria: feature a works
    bloom_level: 2
    required: true
`
	tasksFile := writeBridgeTasksFile(t, tasksYAML)

	pe := &PlanExecutorImpl{
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	}

	params, _ := json.Marshal(submitParams{
		CommandID: commandID,
		TasksFile: tasksFile,
	})

	raw, err := pe.Submit(json.RawMessage(params))
	if err != nil {
		t.Fatalf("Submit returned error: %v", err)
	}

	var result struct {
		CommandID string `json:"command_id"`
		Tasks     []struct {
			Name   string `json:"name"`
			TaskID string `json:"task_id"`
			Worker string `json:"worker"`
			Model  string `json:"model"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.CommandID != commandID {
		t.Errorf("CommandID = %q, want %q", result.CommandID, commandID)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("len(Tasks) = %d, want 1", len(result.Tasks))
	}
	if result.Tasks[0].Name != "task_a" {
		t.Errorf("Tasks[0].Name = %q, want task_a", result.Tasks[0].Name)
	}
	if result.Tasks[0].TaskID == "" {
		t.Error("Tasks[0].TaskID is empty")
	}
	if result.Tasks[0].Worker == "" {
		t.Error("Tasks[0].Worker is empty")
	}
}

func TestPlanExecutorImpl_CompleteSuccess(t *testing.T) {
	maestroDir := setupBridgeMaestroDir(t)
	cfg := bridgeTestConfig()
	commandID := "cmd_0000000010_aabbccdd"
	taskID1 := "task_0000000010_11111111"

	writeBridgePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

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
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{taskID1},
		OptionalTaskIDs:   []string{},
		TaskDependencies:  make(map[string][]string),
		TaskStates:        map[string]model.Status{taskID1: model.StatusCompleted},
		CancelledReasons:  make(map[string]string),
		AppliedResultIDs:  make(map[string]string),
		RetryLineage:      make(map[string]string),
		CreatedAt:         "2025-01-01T00:00:00Z",
		UpdatedAt:         "2025-01-01T00:00:00Z",
	}
	writeBridgeState(t, maestroDir, state)

	// Write worker results to match state
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_0000000001_00000001",
				TaskID:    taskID1,
				CommandID: commandID,
				Status:    model.StatusCompleted,
				Summary:   "task completed",
				CreatedAt: "2025-01-01T00:00:00Z",
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker0.yaml")
	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		t.Fatalf("write worker results: %v", err)
	}

	pe := &PlanExecutorImpl{
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	}

	params, _ := json.Marshal(completeParams{
		CommandID: commandID,
		Summary:   "all done",
	})

	raw, err := pe.Complete(json.RawMessage(params))
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}

	var result struct {
		CommandID string `json:"command_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.CommandID != commandID {
		t.Errorf("CommandID = %q, want %q", result.CommandID, commandID)
	}
	if result.Status != string(model.PlanStatusCompleted) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusCompleted)
	}
}

func TestPlanExecutorImpl_AddRetryTaskSuccess(t *testing.T) {
	maestroDir := setupBridgeMaestroDir(t)
	cfg := bridgeTestConfig()
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
	writeBridgeState(t, maestroDir, state)

	pe := &PlanExecutorImpl{
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	}

	params, _ := json.Marshal(retryParams{
		CommandID:          commandID,
		RetryOf:            taskID2,
		Purpose:            "retry task 2",
		Content:            "redo task 2",
		AcceptanceCriteria: "task 2 passes",
		BloomLevel:         2,
	})

	raw, err := pe.AddRetryTask(json.RawMessage(params))
	if err != nil {
		t.Fatalf("AddRetryTask returned error: %v", err)
	}

	var result struct {
		TaskID   string `json:"task_id"`
		Worker   string `json:"worker"`
		Model    string `json:"model"`
		Replaced string `json:"replaced"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.TaskID == "" {
		t.Error("result.TaskID is empty")
	}
	if result.Worker == "" {
		t.Error("result.Worker is empty")
	}
	if result.Replaced != taskID2 {
		t.Errorf("Replaced = %q, want %q", result.Replaced, taskID2)
	}
}

func TestPlanExecutorImpl_RebuildSuccess(t *testing.T) {
	maestroDir := setupBridgeMaestroDir(t)
	commandID := "cmd_0000000030_aabbccdd"

	state := &model.CommandState{
		SchemaVersion:    1,
		FileType:         "state_command",
		CommandID:        commandID,
		TaskStates:       map[string]model.Status{"task1": model.StatusPending},
		AppliedResultIDs: make(map[string]string),
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	data, err := yamlv3.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	pe := &PlanExecutorImpl{
		MaestroDir: maestroDir,
		LockMap:    lock.NewMutexMap(),
	}

	params, _ := json.Marshal(rebuildParams{CommandID: commandID})

	raw, err := pe.Rebuild(json.RawMessage(params))
	if err != nil {
		t.Fatalf("Rebuild returned error: %v", err)
	}

	var result rebuildResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.CommandID != commandID {
		t.Errorf("CommandID = %q, want %q", result.CommandID, commandID)
	}
	if result.Status != "rebuilt" {
		t.Errorf("Status = %q, want rebuilt", result.Status)
	}
}

func TestParseAndExecute_MarshalErrorWrapped(t *testing.T) {
	// Use a type that json.Marshal cannot serialize (e.g., channel) to trigger marshal error.
	type unmarshalable struct {
		Ch chan int `json:"ch"`
	}

	_, err := parseAndExecute("test_op", []byte(`{}`), func(_ struct{}) (unmarshalable, error) {
		return unmarshalable{Ch: make(chan int)}, nil
	})
	if err == nil {
		t.Fatal("expected error from marshal failure, got nil")
	}
	if !strings.Contains(err.Error(), "failed to marshal plan result") {
		t.Errorf("error should contain 'failed to marshal plan result', got: %q", err)
	}
	if !strings.Contains(err.Error(), "test_op") {
		t.Errorf("error should contain operation name 'test_op', got: %q", err)
	}
}
