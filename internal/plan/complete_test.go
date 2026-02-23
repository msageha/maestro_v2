package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func setupCompleteTest(t *testing.T, commandID string, taskStates map[string]model.Status, requiredIDs []string) string {
	t.Helper()
	maestroDir := setupMaestroDir(t)

	// Write state file with sealed plan and given task states
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   1,
		PlanStatus:    model.PlanStatusSealed,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			AllowDynamicTasks:       false,
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		ExpectedTaskCount: len(requiredIDs),
		RequiredTaskIDs:   requiredIDs,
		OptionalTaskIDs:   []string{},
		TaskDependencies:  make(map[string][]string),
		TaskStates:        taskStates,
		CancelledReasons:  make(map[string]string),
		AppliedResultIDs:  make(map[string]string),
		RetryLineage:      make(map[string]string),
		CreatedAt:         "2025-01-01T00:00:00Z",
		UpdatedAt:         "2025-01-01T00:00:00Z",
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// Write planner queue with in_progress command
	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	// Write worker results with matching task results
	var results []model.TaskResult
	for taskID, status := range taskStates {
		results = append(results, model.TaskResult{
			ID:        "res_0000000001_00000001",
			TaskID:    taskID,
			CommandID: commandID,
			Status:    status,
			Summary:   "task " + string(status),
			CreatedAt: "2025-01-01T00:00:00Z",
		})
	}
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results:       results,
	}
	resultPath := filepath.Join(maestroDir, "results", "worker0.yaml")
	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		t.Fatalf("write worker results: %v", err)
	}

	return maestroDir
}

func TestComplete_AllCompleted(t *testing.T) {
	commandID := "cmd_0000000010_aabbccdd"
	taskID1 := "task_0000000010_11111111"
	taskID2 := "task_0000000010_22222222"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
		taskID2: model.StatusCompleted,
	}
	requiredIDs := []string{taskID1, taskID2}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()

	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "all tasks done",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}

	if result.CommandID != commandID {
		t.Errorf("CommandID = %q, want %q", result.CommandID, commandID)
	}
	if result.Status != string(model.PlanStatusCompleted) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusCompleted)
	}

	// Verify state file was updated
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("state.PlanStatus = %q, want %q", state.PlanStatus, model.PlanStatusCompleted)
	}

	// Verify planner queue was updated
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	plannerData, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(plannerData, &cq); err != nil {
		t.Fatalf("unmarshal planner queue: %v", err)
	}
	if len(cq.Commands) != 1 {
		t.Fatalf("len(Commands) = %d, want 1", len(cq.Commands))
	}
	if cq.Commands[0].Status != model.StatusCompleted {
		t.Errorf("queue command Status = %q, want %q", cq.Commands[0].Status, model.StatusCompleted)
	}
	if cq.Commands[0].LeaseOwner != nil {
		t.Errorf("queue command LeaseOwner = %v, want nil", cq.Commands[0].LeaseOwner)
	}

	// Verify command result was written to results/planner.yaml
	cmdResultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	cmdResultData, err := os.ReadFile(cmdResultPath)
	if err != nil {
		t.Fatalf("read command result: %v", err)
	}
	var crf model.CommandResultFile
	if err := yamlv3.Unmarshal(cmdResultData, &crf); err != nil {
		t.Fatalf("unmarshal command result: %v", err)
	}
	if len(crf.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1", len(crf.Results))
	}
	if crf.Results[0].Status != model.StatusCompleted {
		t.Errorf("command result Status = %q, want %q", crf.Results[0].Status, model.StatusCompleted)
	}
	if crf.Results[0].Summary != "all tasks done" {
		t.Errorf("command result Summary = %q, want %q", crf.Results[0].Summary, "all tasks done")
	}
}

func TestComplete_HasFailed(t *testing.T) {
	commandID := "cmd_0000000011_aabbccdd"
	taskID1 := "task_0000000011_11111111"
	taskID2 := "task_0000000011_22222222"

	taskStates := map[string]model.Status{
		taskID1: model.StatusCompleted,
		taskID2: model.StatusFailed,
	}
	requiredIDs := []string{taskID1, taskID2}

	maestroDir := setupCompleteTest(t, commandID, taskStates, requiredIDs)
	cfg := testConfig()

	result, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "one task failed",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}

	if result.Status != string(model.PlanStatusFailed) {
		t.Errorf("Status = %q, want %q", result.Status, model.PlanStatusFailed)
	}

	// Verify state reflects failed plan status
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.PlanStatus != model.PlanStatusFailed {
		t.Errorf("state.PlanStatus = %q, want %q", state.PlanStatus, model.PlanStatusFailed)
	}

	// Verify planner queue reflects failed status
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	plannerData, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(plannerData, &cq); err != nil {
		t.Fatalf("unmarshal planner queue: %v", err)
	}
	if cq.Commands[0].Status != model.StatusFailed {
		t.Errorf("queue command Status = %q, want %q", cq.Commands[0].Status, model.StatusFailed)
	}
}

func TestComplete_NotSealed(t *testing.T) {
	maestroDir := setupMaestroDir(t)
	cfg := testConfig()
	commandID := "cmd_0000000012_aabbccdd"

	// Write state with planning status (not sealed)
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanVersion:   0,
		PlanStatus:    model.PlanStatusPlanning,
		CompletionPolicy: model.CompletionPolicy{
			Mode:                    "all_required_completed",
			AllowDynamicTasks:       false,
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		ExpectedTaskCount: 0,
		RequiredTaskIDs:   []string{},
		OptionalTaskIDs:   []string{},
		TaskDependencies:  make(map[string][]string),
		TaskStates:        make(map[string]model.Status),
		CancelledReasons:  make(map[string]string),
		AppliedResultIDs:  make(map[string]string),
		RetryLineage:      make(map[string]string),
		CreatedAt:         "2025-01-01T00:00:00Z",
		UpdatedAt:         "2025-01-01T00:00:00Z",
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	_, err := Complete(CompleteOptions{
		CommandID:  commandID,
		Summary:    "should fail",
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    lock.NewMutexMap(),
	})
	if err == nil {
		t.Fatalf("Complete returned nil error, want error for non-sealed plan")
	}
	if !strings.Contains(err.Error(), "sealed") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "sealed")
	}
}
