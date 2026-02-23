package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

func makeResultWriteRequest(t *testing.T, params any) *uds.Request {
	t.Helper()
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return &uds.Request{
		ProtocolVersion: 1,
		Command:         "result_write",
		Params:          data,
	}
}

// setupWorkerQueue creates a worker queue with one in-progress task for testing.
func setupWorkerQueue(t *testing.T, d *Daemon, workerID, taskID, commandID string, leaseEpoch int) {
	t.Helper()
	owner := workerID
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:         taskID,
				CommandID:  commandID,
				Purpose:    "test purpose",
				Content:    "test content",
				BloomLevel: 3,
				Status:     model.StatusInProgress,
				LeaseOwner: &owner,
				LeaseEpoch: leaseEpoch,
				CreatedAt:  "2026-01-01T00:00:00Z",
				UpdatedAt:  "2026-01-01T00:00:00Z",
			},
		},
	}
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}
}

// setupCommandState creates a command state file for testing.
func setupCommandState(t *testing.T, d *Daemon, commandID string, taskIDs []string) {
	t.Helper()
	taskStates := make(map[string]model.Status)
	for _, id := range taskIDs {
		taskStates[id] = model.StatusInProgress
	}
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskStates:    taskStates,
		CreatedAt:     "2026-01-01T00:00:00Z",
		UpdatedAt:     "2026-01-01T00:00:00Z",
	}
	path := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write command state: %v", err)
	}
}

func TestResultWrite_Basic(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "task done",
		RetrySafe:  true,
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["result_id"] == "" {
		t.Error("expected non-empty result_id")
	}

	// Verify result file
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("parse result file: %v", err)
	}
	if len(rf.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(rf.Results))
	}
	if rf.Results[0].TaskID != taskID {
		t.Errorf("task_id = %q, want %q", rf.Results[0].TaskID, taskID)
	}
	if rf.Results[0].Status != model.StatusCompleted {
		t.Errorf("status = %q, want %q", rf.Results[0].Status, model.StatusCompleted)
	}
	if rf.Results[0].Summary != "task done" {
		t.Errorf("summary = %q, want %q", rf.Results[0].Summary, "task done")
	}

	// Verify queue entry updated to terminal
	queuePath := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	qdata, _ := os.ReadFile(queuePath)
	var tq model.TaskQueue
	yamlv3.Unmarshal(qdata, &tq)
	if tq.Tasks[0].Status != model.StatusCompleted {
		t.Errorf("queue task status = %q, want %q", tq.Tasks[0].Status, model.StatusCompleted)
	}
	if tq.Tasks[0].LeaseOwner != nil {
		t.Error("queue task lease_owner should be nil after result write")
	}

	// Verify state updated
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	sdata, _ := os.ReadFile(statePath)
	var state model.CommandState
	yamlv3.Unmarshal(sdata, &state)
	if state.TaskStates[taskID] != model.StatusCompleted {
		t.Errorf("state task_states[%s] = %q, want %q", taskID, state.TaskStates[taskID], model.StatusCompleted)
	}
	if state.AppliedResultIDs[taskID] == "" {
		t.Error("expected applied_result_ids to contain task result")
	}
}

func TestResultWrite_FencingReject_EpochMismatch(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 3)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1, // stale epoch
		Status:     "completed",
	})

	resp := d.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected fencing rejection")
	}
	if resp.Error.Code != uds.ErrCodeFencingReject {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeFencingReject)
	}
}

func TestResultWrite_FencingReject_WrongOwner(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"

	setupWorkerQueue(t, d, "worker1", taskID, commandID, 1)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   "worker2", // wrong reporter
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
	})

	// Create worker2 queue (task not there)
	resp := d.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected error for task not found in wrong worker queue")
	}
}

func TestResultWrite_FencingReject_NotInProgress(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"

	// Write a pending task instead of in_progress
	owner := workerID
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:         taskID,
				CommandID:  commandID,
				BloomLevel: 3,
				Status:     model.StatusPending, // not in_progress
				LeaseOwner: &owner,
				LeaseEpoch: 1,
				CreatedAt:  "2026-01-01T00:00:00Z",
				UpdatedAt:  "2026-01-01T00:00:00Z",
			},
		},
	}
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	yamlutil.AtomicWrite(path, tq)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
	})

	resp := d.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected fencing rejection for non-in_progress task")
	}
	if resp.Error.Code != uds.ErrCodeFencingReject {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeFencingReject)
	}
}

func TestResultWrite_Idempotency_SameStatus(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	params := ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
	}

	// First write
	req1 := makeResultWriteRequest(t, params)
	resp1 := d.handleResultWrite(req1)
	if !resp1.Success {
		t.Fatalf("write 1: expected success, got error: %v", resp1.Error)
	}

	var result1 map[string]string
	json.Unmarshal(resp1.Data, &result1)
	firstResultID := result1["result_id"]

	// Second write (idempotent retry)
	req2 := makeResultWriteRequest(t, params)
	resp2 := d.handleResultWrite(req2)
	if !resp2.Success {
		t.Fatalf("write 2: expected idempotent success, got error: %v", resp2.Error)
	}

	var result2 map[string]string
	json.Unmarshal(resp2.Data, &result2)

	if result2["result_id"] != firstResultID {
		t.Errorf("idempotent retry should return same result_id: got %q, want %q", result2["result_id"], firstResultID)
	}

	// Verify only 1 result in file
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, _ := os.ReadFile(resultPath)
	var rf model.TaskResultFile
	yamlv3.Unmarshal(data, &rf)
	if len(rf.Results) != 1 {
		t.Errorf("expected 1 result, got %d", len(rf.Results))
	}
}

func TestResultWrite_Idempotency_DifferentStatus(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// First write: completed
	req1 := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
	})
	resp1 := d.handleResultWrite(req1)
	if !resp1.Success {
		t.Fatalf("write 1: expected success, got error: %v", resp1.Error)
	}

	// Second write: failed (different status — should be rejected)
	req2 := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "failed",
	})
	resp2 := d.handleResultWrite(req2)
	if resp2.Success {
		t.Fatal("expected error for different status on same task")
	}
	if resp2.Error.Code != uds.ErrCodeDuplicate {
		t.Errorf("error code = %q, want %q", resp2.Error.Code, uds.ErrCodeDuplicate)
	}
}

func TestResultWrite_TaskNotFound(t *testing.T) {
	d := newTestDaemon(t)

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   "worker1",
		TaskID:     "task_0000000001_nonexist",
		CommandID:  "cmd_0000000001_abcdef01",
		LeaseEpoch: 1,
		Status:     "completed",
	})

	resp := d.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected error for task not found")
	}
	if resp.Error.Code != uds.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeNotFound)
	}
}

func TestResultWrite_ValidationErrors(t *testing.T) {
	d := newTestDaemon(t)

	tests := []struct {
		name   string
		params ResultWriteParams
	}{
		{"missing reporter", ResultWriteParams{TaskID: "t1", CommandID: "c1", Status: "completed"}},
		{"missing task_id", ResultWriteParams{Reporter: "worker1", CommandID: "c1", Status: "completed"}},
		{"missing command_id", ResultWriteParams{Reporter: "worker1", TaskID: "t1", Status: "completed"}},
		{"invalid status", ResultWriteParams{Reporter: "worker1", TaskID: "t1", CommandID: "c1", Status: "in_progress"}},
		{"dead_letter status", ResultWriteParams{Reporter: "worker1", TaskID: "t1", CommandID: "c1", Status: "dead_letter"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeResultWriteRequest(t, tt.params)
			resp := d.handleResultWrite(req)
			if resp.Success {
				t.Fatal("expected validation error")
			}
			if resp.Error.Code != uds.ErrCodeValidation {
				t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
			}
		})
	}
}

func TestResultWrite_Failed(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "failed",
		Summary:    "compilation error",
		RetrySafe:  true,
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	// Verify result status
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, _ := os.ReadFile(resultPath)
	var rf model.TaskResultFile
	yamlv3.Unmarshal(data, &rf)
	if rf.Results[0].Status != model.StatusFailed {
		t.Errorf("result status = %q, want %q", rf.Results[0].Status, model.StatusFailed)
	}

	// Verify state updated
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	sdata, _ := os.ReadFile(statePath)
	var state model.CommandState
	yamlv3.Unmarshal(sdata, &state)
	if state.TaskStates[taskID] != model.StatusFailed {
		t.Errorf("state task_states[%s] = %q, want %q", taskID, state.TaskStates[taskID], model.StatusFailed)
	}
}

func TestResultWrite_FilesChanged(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:               workerID,
		TaskID:                 taskID,
		CommandID:              commandID,
		LeaseEpoch:             leaseEpoch,
		Status:                 "completed",
		Summary:                "changed files",
		FilesChanged:           []string{"src/main.go", "src/utils.go"},
		PartialChangesPossible: true,
	})

	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	// Verify files_changed persisted
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, _ := os.ReadFile(resultPath)
	var rf model.TaskResultFile
	yamlv3.Unmarshal(data, &rf)
	if len(rf.Results[0].FilesChanged) != 2 {
		t.Errorf("files_changed length = %d, want 2", len(rf.Results[0].FilesChanged))
	}
	if !rf.Results[0].PartialChangesPossible {
		t.Error("partial_changes_possible should be true")
	}
}

func TestResultWrite_CommandIDMismatch(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	wrongCommandID := "cmd_0000000001_different"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  wrongCommandID, // doesn't match queue task's command_id
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
	})

	resp := d.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected error for command_id mismatch")
	}
	if resp.Error.Code != uds.ErrCodeValidation {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
	}
}

func TestResultWrite_TaskNotRegisteredInState(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	unregisteredTaskID := "task_0000000001_unreg001"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	// Create queue with unregistered task
	setupWorkerQueue(t, d, workerID, unregisteredTaskID, commandID, leaseEpoch)
	// Create state with only the original task registered
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     unregisteredTaskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
	})

	resp := d.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected error for task not registered in state")
	}
	if resp.Error.Code != uds.ErrCodeValidation {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
	}
}

func TestResultWrite_PhaseBFailure(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	// Create state file but then remove it before Phase B runs
	setupCommandState(t, d, commandID, []string{taskID})

	// Remove state file to cause Phase B to fail
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	os.Remove(statePath)

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
	})

	resp := d.handleResultWrite(req)
	// Phase B failure should return error (not silently succeed)
	if resp.Success {
		t.Fatal("expected error when Phase B fails")
	}
	if resp.Error.Code != uds.ErrCodeInternal {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeInternal)
	}

	// Result should still be committed (Phase A succeeded)
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("result file should exist: %v", err)
	}
	var rf model.TaskResultFile
	yamlv3.Unmarshal(data, &rf)
	if len(rf.Results) != 1 {
		t.Errorf("expected 1 result (committed in Phase A), got %d", len(rf.Results))
	}
}
