package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func newTestDaemonWithCircuitBreaker(t *testing.T, threshold int) *Daemon {
	t.Helper()
	d := newTestDaemon(t)
	d.config.CircuitBreaker.Enabled = true
	d.config.CircuitBreaker.MaxConsecutiveFailures = &threshold
	d.circuitBreaker = circuitbreaker.NewHandler(d.config, d.logger, d.logLevel)
	return d
}

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
		TaskTracking: model.TaskTracking{
			TaskStates: taskStates,
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	path := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write command state: %v", err)
	}
}

func TestResultWrite_Basic(t *testing.T) {
	t.Parallel()
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

	resp := d.api.handleResultWrite(req)
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
	qdata, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(qdata, &tq); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	if tq.Tasks[0].Status != model.StatusCompleted {
		t.Errorf("queue task status = %q, want %q", tq.Tasks[0].Status, model.StatusCompleted)
	}
	if tq.Tasks[0].LeaseOwner != nil {
		t.Error("queue task lease_owner should be nil after result write")
	}

	// Verify state updated
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	sdata, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(sdata, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.TaskStates[taskID] != model.StatusCompleted {
		t.Errorf("state task_states[%s] = %q, want %q", taskID, state.TaskStates[taskID], model.StatusCompleted)
	}
	if state.AppliedResultIDs[taskID] == "" {
		t.Error("expected applied_result_ids to contain task result")
	}
}

func TestResultWrite_FencingReject_EpochMismatch(t *testing.T) {
	t.Parallel()
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

	resp := d.api.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected fencing rejection")
	}
	if resp.Error.Code != uds.ErrCodeFencingReject {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeFencingReject)
	}
}

func TestResultWrite_FencingReject_WrongOwner(t *testing.T) {
	t.Parallel()
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
	resp := d.api.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected error for task not found in wrong worker queue")
	}
}

func TestResultWrite_FencingReject_NotInProgress(t *testing.T) {
	t.Parallel()
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

	resp := d.api.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected fencing rejection for non-in_progress task")
	}
	if resp.Error.Code != uds.ErrCodeFencingReject {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeFencingReject)
	}
}

func TestResultWrite_Idempotency_SameStatus(t *testing.T) {
	t.Parallel()
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
	resp1 := d.api.handleResultWrite(req1)
	if !resp1.Success {
		t.Fatalf("write 1: expected success, got error: %v", resp1.Error)
	}

	var result1 map[string]string
	json.Unmarshal(resp1.Data, &result1)
	firstResultID := result1["result_id"]

	// Second write (idempotent retry)
	req2 := makeResultWriteRequest(t, params)
	resp2 := d.api.handleResultWrite(req2)
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
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(rf.Results) != 1 {
		t.Errorf("expected 1 result, got %d", len(rf.Results))
	}
}

// TestResultWrite_Idempotency_DuplicateShortCircuitResponse verifies the Bug H
// fix: a second (idempotent) submission of the same result returns the same
// result_id and carries the `duplicate=true` response marker, signalling to
// callers (and to log-analysis tooling) that Phase B / best-effort writes were
// skipped to avoid misleading double-recording of a single logical write.
func TestResultWrite_Idempotency_DuplicateShortCircuitResponse(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000099_abcdef01"
	commandID := "cmd_0000000099_abcdef01"
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

	// First write: fresh submission, no duplicate marker.
	resp1 := d.api.handleResultWrite(makeResultWriteRequest(t, params))
	if !resp1.Success {
		t.Fatalf("write 1: expected success, got error: %v", resp1.Error)
	}
	var r1 map[string]string
	if err := json.Unmarshal(resp1.Data, &r1); err != nil {
		t.Fatalf("unmarshal resp1: %v", err)
	}
	if r1["duplicate"] == "true" {
		t.Errorf("first write should NOT carry duplicate=true, got payload=%v", r1)
	}
	firstResultID := r1["result_id"]

	// Second write: must be detected as duplicate and carry duplicate=true.
	resp2 := d.api.handleResultWrite(makeResultWriteRequest(t, params))
	if !resp2.Success {
		t.Fatalf("write 2: expected idempotent success, got error: %v", resp2.Error)
	}
	var r2 map[string]string
	if err := json.Unmarshal(resp2.Data, &r2); err != nil {
		t.Fatalf("unmarshal resp2: %v", err)
	}
	if r2["result_id"] != firstResultID {
		t.Errorf("duplicate submission result_id = %q, want %q", r2["result_id"], firstResultID)
	}
	if r2["duplicate"] != "true" {
		t.Errorf("duplicate submission should carry duplicate=true (Bug H short-circuit), got payload=%v", r2)
	}

	// Result file must still contain exactly one entry (no double-record).
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(rf.Results) != 1 {
		t.Errorf("expected 1 result (duplicate suppressed), got %d", len(rf.Results))
	}
}

func TestResultWrite_Idempotency_DifferentStatus(t *testing.T) {
	t.Parallel()
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
	resp1 := d.api.handleResultWrite(req1)
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
	resp2 := d.api.handleResultWrite(req2)
	if resp2.Success {
		t.Fatal("expected error for different status on same task")
	}
	if resp2.Error.Code != uds.ErrCodeDuplicate {
		t.Errorf("error code = %q, want %q", resp2.Error.Code, uds.ErrCodeDuplicate)
	}
}

func TestResultWrite_TaskNotFound(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   "worker1",
		TaskID:     "task_0000000001_nonexist",
		CommandID:  "cmd_0000000001_abcdef01",
		LeaseEpoch: 1,
		Status:     "completed",
	})

	resp := d.api.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected error for task not found")
	}
	if resp.Error.Code != uds.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeNotFound)
	}
}

func TestResultWrite_ValidationErrors(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			req := makeResultWriteRequest(t, tt.params)
			resp := d.api.handleResultWrite(req)
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
	t.Parallel()
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

	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	// Verify result status
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if rf.Results[0].Status != model.StatusFailed {
		t.Errorf("result status = %q, want %q", rf.Results[0].Status, model.StatusFailed)
	}

	// Verify state updated
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	sdata, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(sdata, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if state.TaskStates[taskID] != model.StatusFailed {
		t.Errorf("state task_states[%s] = %q, want %q", taskID, state.TaskStates[taskID], model.StatusFailed)
	}
}

func TestResultWrite_FilesChanged(t *testing.T) {
	t.Parallel()
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

	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	// Verify files_changed persisted
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(rf.Results[0].FilesChanged) != 2 {
		t.Errorf("files_changed length = %d, want 2", len(rf.Results[0].FilesChanged))
	}
	if !rf.Results[0].PartialChangesPossible {
		t.Error("partial_changes_possible should be true")
	}
}

func TestResultWrite_CommandIDMismatch(t *testing.T) {
	t.Parallel()
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

	resp := d.api.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected error for command_id mismatch")
	}
	if resp.Error.Code != uds.ErrCodeValidation {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
	}
}

func TestResultWrite_TaskNotRegisteredInState(t *testing.T) {
	t.Parallel()
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

	resp := d.api.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected error for task not registered in state")
	}
	if resp.Error.Code != uds.ErrCodeValidation {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
	}
}

func TestResultWrite_StateNotFound(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	// No state file — Phase A should reject with VALIDATION_ERROR

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "done",
	})

	resp := d.api.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected error when state file does not exist")
	}
	if resp.Error.Code != uds.ErrCodeValidation {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeValidation)
	}

	// Result should NOT be written (Phase A rejected before writing)
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	if _, err := os.Stat(resultPath); err == nil {
		t.Error("result file should not exist when Phase A rejects")
	}
}

// TestResultWrite_Idempotency_CircuitBreakerNotDoubleCounted is a regression
// test for the bug where the CircuitBreaker counter was incremented before the
// AppliedResultIDs idempotency check, causing duplicate failed-result
// submissions to inflate consecutive_failures and trip the breaker spuriously.
func TestResultWrite_Idempotency_CircuitBreakerNotDoubleCounted(t *testing.T) {
	t.Parallel()
	// Enable circuit breaker with a low threshold so any double-count would trip it.
	threshold := 2
	d := newTestDaemonWithCircuitBreaker(t, threshold)

	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1
	exitCode := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	params := ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "failed",
		Summary:    "boom",
		ExitCode:   &exitCode,
		RetrySafe:  false,
	}

	// First write: failed.
	resp1 := d.api.handleResultWrite(makeResultWriteRequest(t, params))
	if !resp1.Success {
		t.Fatalf("write 1: expected success, got error: %v", resp1.Error)
	}

	// Read state and confirm exactly 1 failure was counted.
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	readState := func() model.CommandState {
		t.Helper()
		sdata, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("read state: %v", err)
		}
		var st model.CommandState
		if err := yamlv3.Unmarshal(sdata, &st); err != nil {
			t.Fatalf("parse state: %v", err)
		}
		return st
	}
	st1 := readState()
	if st1.CircuitBreaker.ConsecutiveFailures != 1 {
		t.Fatalf("after first write: ConsecutiveFailures = %d, want 1", st1.CircuitBreaker.ConsecutiveFailures)
	}
	if st1.CircuitBreaker.Tripped {
		t.Fatalf("breaker should not be tripped after one failure (threshold=2)")
	}

	// Second write: idempotent retry of the same failed result. PhaseA returns
	// the existing result_id; phaseB must NOT increment the counter again.
	resp2 := d.api.handleResultWrite(makeResultWriteRequest(t, params))
	if !resp2.Success {
		t.Fatalf("write 2 (idempotent): expected success, got error: %v", resp2.Error)
	}

	st2 := readState()
	if st2.CircuitBreaker.ConsecutiveFailures != 1 {
		t.Errorf("after idempotent retry: ConsecutiveFailures = %d, want 1 (counter must not double-count)",
			st2.CircuitBreaker.ConsecutiveFailures)
	}
	if st2.CircuitBreaker.Tripped {
		t.Errorf("breaker should not be tripped on duplicate submission (threshold=%d)", threshold)
	}
	if st2.AppliedResultIDs[taskID] == "" {
		t.Errorf("AppliedResultIDs[%s] should be populated", taskID)
	}
}

// TestResultWrite_LateAfterPlanTerminal verifies that when a result_write
// arrives after the plan has already been transitioned to a terminal status
// (e.g. failed/cancelled by another path), the late task's state is coerced
// to cancelled instead of being applied verbatim. Without the fix, the
// resulting state would have plan_status=failed but TaskStates[taskID]=completed,
// an inconsistency that plan.Complete's idempotency skip path would not
// re-validate (Critical #5 in repo audit 20260407).
func TestResultWrite_LateAfterPlanTerminal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		planStatus     model.PlanStatus
		reportedStatus string
	}{
		{"plan_failed_late_completed", model.PlanStatusFailed, "completed"},
		{"plan_cancelled_late_completed", model.PlanStatusCancelled, "completed"},
		{"plan_failed_late_failed", model.PlanStatusFailed, "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newTestDaemon(t)
			taskID := "task_0000000001_abcdef01"
			commandID := "cmd_0000000001_abcdef01"
			workerID := "worker1"
			leaseEpoch := 1

			setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)

			// Setup state with plan already terminal and the task still in_progress
			// (a previously dispatched task whose result is now arriving late).
			statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
			state := model.CommandState{
				SchemaVersion: 1,
				FileType:      "state_command",
				CommandID:     commandID,
				PlanStatus:    tc.planStatus,
				TaskTracking: model.TaskTracking{
					TaskStates: map[string]model.Status{taskID: model.StatusInProgress},
				},
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
			}
			if err := yamlutil.AtomicWrite(statePath, state); err != nil {
				t.Fatalf("write state: %v", err)
			}

			req := makeResultWriteRequest(t, ResultWriteParams{
				Reporter:   workerID,
				TaskID:     taskID,
				CommandID:  commandID,
				LeaseEpoch: leaseEpoch,
				Status:     tc.reportedStatus,
				Summary:    "late result after plan terminal",
			})

			resp := d.api.handleResultWrite(req)
			if !resp.Success {
				t.Fatalf("expected success (late result is recorded), got error: %v", resp.Error)
			}

			// Verify result file received the original reporter status (audit trail)
			resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
			rdata, err := os.ReadFile(resultPath)
			if err != nil {
				t.Fatalf("read result: %v", err)
			}
			var rf model.TaskResultFile
			if err := yamlv3.Unmarshal(rdata, &rf); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			if len(rf.Results) != 1 {
				t.Fatalf("expected 1 result entry, got %d", len(rf.Results))
			}
			if string(rf.Results[0].Status) != tc.reportedStatus {
				t.Errorf("result file status = %q, want original reported %q",
					rf.Results[0].Status, tc.reportedStatus)
			}

			// Verify state remains consistent: plan_status unchanged, and the
			// task state is coerced to cancelled (not the reported status).
			sdata, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatalf("read state: %v", err)
			}
			var got model.CommandState
			if err := yamlv3.Unmarshal(sdata, &got); err != nil {
				t.Fatalf("unmarshal state: %v", err)
			}
			if got.PlanStatus != tc.planStatus {
				t.Errorf("plan_status mutated: got %q, want %q (unchanged)", got.PlanStatus, tc.planStatus)
			}
			if got.TaskStates[taskID] != model.StatusCancelled {
				t.Errorf("late task state = %q, want %q (coerced for plan/state consistency)",
					got.TaskStates[taskID], model.StatusCancelled)
			}
			if got.AppliedResultIDs[taskID] == "" {
				t.Error("expected applied_result_ids to record the late result")
			}
		})
	}
}

// TestResultWrite_QueueWriteFailed_RollbackSucceeds verifies that when phaseA's
// queue write fails on both attempts but the result rollback succeeds, an error
// is returned (the result entry is cleaned up). This is the atomicity recovery
// path: queue and result stay consistent (neither committed).
func TestResultWrite_QueueWriteFailed_RollbackSucceeds(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// Make the queue directory read-only so AtomicWrite (tempfile + rename in
	// the same dir) fails on both attempts in phaseA. Results dir is still
	// writable, so rollback succeeds.
	queueDir := filepath.Join(d.maestroDir, "queue")
	if err := os.Chmod(queueDir, 0o500); err != nil {
		t.Fatalf("chmod queue dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(queueDir, 0o755)
	})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "task done but queue write fails",
	})

	resp := d.api.handleResultWrite(req)
	// With atomicity recovery, rollback succeeds → error is returned.
	if resp.Success {
		t.Fatal("expected error (queue write failed, result rolled back)")
	}
	if resp.Error.Code != uds.ErrCodeInternal {
		t.Errorf("error code = %q, want %q", resp.Error.Code, uds.ErrCodeInternal)
	}
	if !strings.Contains(resp.Error.Message, "result rolled back successfully") {
		t.Errorf("error message should mention rollback: %q", resp.Error.Message)
	}

	// Restore perms for cleanup.
	if err := os.Chmod(queueDir, 0o755); err != nil {
		t.Fatalf("restore queue dir perms: %v", err)
	}

	// Queue should still be in_progress on disk (queue write was blocked).
	queuePath := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	qdata, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(qdata, &tq); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	if tq.Tasks[0].Status != model.StatusInProgress {
		t.Errorf("queue status = %q, want %q (writes were blocked)",
			tq.Tasks[0].Status, model.StatusInProgress)
	}

	// Result file should be empty or have no entries (rolled back).
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	if rdata, err := os.ReadFile(resultPath); err == nil {
		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(rdata, &rf); err == nil && len(rf.Results) > 0 {
			t.Errorf("result file should have no entries after rollback, got %d", len(rf.Results))
		}
	}
	// File not existing is also acceptable (was never written or was cleaned up).
}

// failingSaveFileStore wraps a real ResultFileStore with failure injection for
// testing queue write failure + rollback failure scenarios.
type failingSaveFileStore struct {
	ResultFileStore
	saveQueueErr        error // if non-nil, SaveQueueFile always returns this error
	saveResultFailAfter int   // fail SaveResultFile after N successful calls (0 = never fail)
	saveResultCalls     int
}

func (f *failingSaveFileStore) SaveQueueFile(reporter string, tq model.TaskQueue) error {
	if f.saveQueueErr != nil {
		return f.saveQueueErr
	}
	return f.ResultFileStore.SaveQueueFile(reporter, tq)
}

func (f *failingSaveFileStore) SaveResultFile(reporter string, rf model.TaskResultFile) error {
	f.saveResultCalls++
	if f.saveResultFailAfter > 0 && f.saveResultCalls > f.saveResultFailAfter {
		return fmt.Errorf("injected result save failure (call %d)", f.saveResultCalls)
	}
	return f.ResultFileStore.SaveResultFile(reporter, rf)
}

// TestResultWrite_QueueWriteFailed_RollbackFailed_OrphanedMarker verifies that
// when phaseA's queue write fails AND the result rollback also fails, the result
// is committed as an orphaned entry: a sticky error marker is recorded in state,
// and an orphaned marker sidecar file is written for R1 reconciler detection.
func TestResultWrite_QueueWriteFailed_RollbackFailed_OrphanedMarker(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// Inject a FileStore that fails SaveQueueFile and fails SaveResultFile on
	// the 2nd call (rollback). The 1st SaveResultFile call (appendResultEntry)
	// succeeds normally.
	realStore := d.api.shared.fileStore
	mock := &failingSaveFileStore{
		ResultFileStore:     realStore,
		saveQueueErr:        fmt.Errorf("injected queue write failure"),
		saveResultFailAfter: 1, // 1st call succeeds (commit), 2nd fails (rollback)
	}
	d.api.shared.fileStore = mock
	t.Cleanup(func() { d.api.shared.fileStore = realStore })

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "task done but both queue write and rollback fail",
	})

	resp := d.api.handleResultWrite(req)
	// When rollback also fails, phaseA proceeds with queueWriteFailed=true.
	// PhaseB records the sticky error. handleResultWrite returns success.
	if !resp.Success {
		t.Fatalf("expected success (orphaned result committed), got error: %v", resp.Error)
	}
	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	resultID := result["result_id"]
	if resultID == "" {
		t.Fatal("expected non-empty result_id")
	}

	// Restore real store for state reads.
	d.api.shared.fileStore = realStore

	// State should carry the sticky marker keyed by task_id.
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	sdata, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(sdata, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	got, ok := state.QueueWriteFailed[taskID]
	if !ok {
		t.Fatalf("expected state.QueueWriteFailed[%s] to be set, got map=%v",
			taskID, state.QueueWriteFailed)
	}
	want := workerID + ":" + resultID
	if got != want {
		t.Errorf("state.QueueWriteFailed[%s] = %q, want %q", taskID, got, want)
	}

	// Result file should still have the committed entry (rollback failed).
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	rdata, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(rdata, &rf); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(rf.Results) != 1 || rf.Results[0].TaskID != taskID {
		t.Errorf("orphaned result entry should remain: got %+v", rf.Results)
	}

	// Orphaned marker file should exist.
	orphanPath := realStore.ResultFilePath(workerID) + ".orphaned"
	odata, err := os.ReadFile(orphanPath)
	if err != nil {
		t.Fatalf("orphaned marker file should exist: %v", err)
	}
	orphanContent := string(odata)
	if !strings.Contains(orphanContent, resultID) {
		t.Errorf("orphaned marker should contain result_id %s, got: %q", resultID, orphanContent)
	}
	if !strings.Contains(orphanContent, taskID) {
		t.Errorf("orphaned marker should contain task_id %s, got: %q", taskID, orphanContent)
	}
}

// TestResultWrite_PhaseBIdempotency_ConflictRejected verifies that when Phase B
// detects a different result_id already applied for the same task_id (TOCTOU
// race between two concurrent Phase A writes), the second result is rejected
// and the state is not updated. This closes the TOCTOU window between Phase A's
// optimistic idempotency check and Phase B's authoritative state update.
func TestResultWrite_PhaseBIdempotency_ConflictRejected(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"

	// Set up state with a pre-existing AppliedResultIDs entry (simulating a
	// prior Phase B completion for a different result_id).
	existingResultID := "res_0000000001_existing1"
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates:       map[string]model.Status{taskID: model.StatusCompleted},
			AppliedResultIDs: map[string]string{taskID: existingResultID},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	params := ResultWriteParams{
		Reporter:   "worker1",
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "conflicting result",
	}

	// Call Phase B directly with a different result_id.
	incomingResultID := "res_0000000001_incoming2"
	err := d.api.result.resultWritePhaseB(params, incomingResultID, model.StatusCompleted, false, "")
	// updateYAMLFile converts errNoUpdate to nil, so no error is returned.
	if err != nil {
		t.Fatalf("expected nil error (errNoUpdate), got %v", err)
	}

	// Verify state was NOT updated — AppliedResultIDs still has the old value.
	sdata, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var got model.CommandState
	if err := yamlv3.Unmarshal(sdata, &got); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if applied, ok := got.AppliedResultIDs[taskID]; !ok || applied != existingResultID {
		t.Errorf("AppliedResultIDs[%s] = %q, want %q (should not be overwritten)",
			taskID, applied, existingResultID)
	}
}

// TestResultWrite_RetryLineage_OriginalTaskSuperseded verifies that when a
// retry task's result is written, the original task's state is updated to
// cancelled (superseded) in the command state.
func TestResultWrite_RetryLineage_OriginalTaskSuperseded(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	originalTaskID := "task_0000000001_original1"
	retryTaskID := "task_0000000001_retry0001"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	// Create queue with retry task that has OriginalTaskID set.
	owner := workerID
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             retryTaskID,
				CommandID:      commandID,
				Purpose:        "retry of original",
				Content:        "retry content",
				BloomLevel:     3,
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseEpoch:     leaseEpoch,
				OriginalTaskID: originalTaskID,
				CreatedAt:      "2026-01-01T00:00:00Z",
				UpdatedAt:      "2026-01-01T00:00:00Z",
			},
		},
	}
	queuePath := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	// State has both original (in_progress) and retry task registered.
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				originalTaskID: model.StatusInProgress,
				retryTaskID:    model.StatusInProgress,
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     retryTaskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "retry task succeeded",
	})

	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	// Verify original task state is cancelled (superseded).
	sdata, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var got model.CommandState
	if err := yamlv3.Unmarshal(sdata, &got); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if got.TaskStates[originalTaskID] != model.StatusCancelled {
		t.Errorf("original task state = %q, want %q (superseded by retry)",
			got.TaskStates[originalTaskID], model.StatusCancelled)
	}
	if got.TaskStates[retryTaskID] != model.StatusCompleted {
		t.Errorf("retry task state = %q, want %q",
			got.TaskStates[retryTaskID], model.StatusCompleted)
	}
}

// TestResultWrite_RetryLineage_OriginalAlreadyTerminal verifies that when the
// original task is already in a terminal state (e.g. failed), the retry
// lineage update is skipped (no overwrite).
func TestResultWrite_RetryLineage_OriginalAlreadyTerminal(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	originalTaskID := "task_0000000001_original1"
	retryTaskID := "task_0000000001_retry0001"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	// Create queue with retry task.
	owner := workerID
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             retryTaskID,
				CommandID:      commandID,
				Purpose:        "retry of original",
				Content:        "retry content",
				BloomLevel:     3,
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseEpoch:     leaseEpoch,
				OriginalTaskID: originalTaskID,
				CreatedAt:      "2026-01-01T00:00:00Z",
				UpdatedAt:      "2026-01-01T00:00:00Z",
			},
		},
	}
	queuePath := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	// State has original already terminal (failed) — should not be overwritten.
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				originalTaskID: model.StatusFailed, // already terminal
				retryTaskID:    model.StatusInProgress,
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     retryTaskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     "completed",
		Summary:    "retry succeeded but original already terminal",
	})

	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}

	// Verify original task state is unchanged (still failed, not overwritten).
	sdata, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var got model.CommandState
	if err := yamlv3.Unmarshal(sdata, &got); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if got.TaskStates[originalTaskID] != model.StatusFailed {
		t.Errorf("original task state = %q, want %q (should remain unchanged)",
			got.TaskStates[originalTaskID], model.StatusFailed)
	}
}

// TestResultWrite_AdvisoryLock_Contention verifies that the lease epoch
// advisory check correctly handles lock contention by skipping the check
// (allowing writes to proceed) when the queue lock is held by another operation.
func TestResultWrite_AdvisoryLock_Contention(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	workerID := "worker1"
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)

	// Hold the queue lock to simulate contention from a concurrent Phase A.
	lockKey := "queue:" + workerID
	d.lockMap.Lock(lockKey)
	defer d.lockMap.Unlock(lockKey)

	params := ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
	}

	// checkLeaseEpochForBestEffort should detect lock contention and return
	// (false, "") — meaning "allow writes, don't skip".
	skip, reason := d.api.result.checkLeaseEpochForBestEffort(params)
	if skip {
		t.Error("expected skip=false when lock contention (writes should proceed)")
	}
	if reason != "" {
		t.Errorf("expected empty reason on lock contention, got %q", reason)
	}
}

// TestResultWrite_AdvisoryLock_EpochMismatch verifies that the advisory lease
// check detects epoch mismatches and returns skip=true when the queue task's
// lease epoch differs from the request.
func TestResultWrite_AdvisoryLock_EpochMismatch(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	workerID := "worker1"
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"

	// Queue has epoch=3
	setupWorkerQueue(t, d, workerID, taskID, commandID, 3)

	params := ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1, // stale epoch
	}

	skip, reason := d.api.result.checkLeaseEpochForBestEffort(params)
	if !skip {
		t.Error("expected skip=true for epoch mismatch")
	}
	if !strings.Contains(reason, "lease_epoch_mismatch") {
		t.Errorf("expected reason to contain 'lease_epoch_mismatch', got %q", reason)
	}
}

func TestSanitizeContentForLog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		check func(t *testing.T, out string)
	}{
		{
			name:  "short string unchanged",
			input: "simple content",
			check: func(t *testing.T, out string) {
				if out != "simple content" {
					t.Errorf("expected unchanged, got %q", out)
				}
			},
		},
		{
			name:  "truncated at 200 chars",
			input: strings.Repeat("x", 250),
			check: func(t *testing.T, out string) {
				if len(out) != 203 { // 200 + "..."
					t.Errorf("expected len 203, got %d", len(out))
				}
			},
		},
		{
			name:  "control chars replaced",
			input: "line1\nline2\ttab\x00null",
			check: func(t *testing.T, out string) {
				if strings.ContainsAny(out, "\n\t\x00") {
					t.Errorf("output still contains control chars: %q", out)
				}
				if !strings.Contains(out, "line1?line2?tab?null") {
					t.Errorf("expected control chars replaced with '?', got %q", out)
				}
			},
		},
		{
			name:  "empty string",
			input: "",
			check: func(t *testing.T, out string) {
				if out != "" {
					t.Errorf("expected empty, got %q", out)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := sanitizeContentForLog(tt.input)
			tt.check(t, out)
		})
	}
}
