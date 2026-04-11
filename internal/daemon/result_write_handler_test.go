package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
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
				TaskStates:    map[string]model.Status{taskID: model.StatusInProgress},
				CreatedAt:     "2026-01-01T00:00:00Z",
				UpdatedAt:     "2026-01-01T00:00:00Z",
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

// TestResultWrite_QueueWriteFailed_StickyError verifies that when phaseA's
// queue write fails on both attempts (queue dir made read-only after the
// result file is committed), the sticky error marker is persisted to
// state.QueueWriteFailed so the R1 reconciler can repair the
// result-terminal/queue-in_progress mismatch durably across daemon restarts.
// Regression test for H2 (audit cmd_1775522508_05ee0f69).
func TestResultWrite_QueueWriteFailed_StickyError(t *testing.T) {
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1

	setupWorkerQueue(t, d, workerID, taskID, commandID, leaseEpoch)
	setupCommandState(t, d, commandID, []string{taskID})

	// Make the queue directory read-only so AtomicWrite (tempfile + rename in
	// the same dir) fails on both attempts in phaseA. Reads still work because
	// 0500 grants r-x.
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
	if !resp.Success {
		t.Fatalf("expected success (result file committed), got error: %v", resp.Error)
	}
	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	resultID := result["result_id"]
	if resultID == "" {
		t.Fatal("expected non-empty result_id")
	}

	// Restore perms before reading queue file (read alone is fine but we want
	// to leave the dir in a clean state for any later assertions/cleanup).
	if err := os.Chmod(queueDir, 0o755); err != nil {
		t.Fatalf("restore queue dir perms: %v", err)
	}

	// Queue should still be in_progress on disk (the writes failed).
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

	// Result file should still be committed (the durable record).
	resultPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	rdata, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(rdata, &rf); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(rf.Results) != 1 || rf.Results[0].TaskID != taskID || rf.Results[0].Status != model.StatusCompleted {
		t.Errorf("result file unexpected: %+v", rf.Results)
	}
}
