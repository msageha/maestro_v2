package daemon

import (
	"bytes"
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

// workerQueueOption mutates the seeded test task before write.
type workerQueueOption func(*model.Task)

// withRunOnIntegration flips the seeded task's RunOnIntegration flag so the
// daemon's per-task verify runner is exercised (the post-2026-05-05 redesign
// short-circuits verify for normal worker tasks; tests that want to assert
// VerifyRunner behaviour must opt in via this flag).
func withRunOnIntegration() workerQueueOption {
	return func(task *model.Task) { task.RunOnIntegration = true }
}

// setupWorkerQueue creates a worker queue with one in-progress task for testing.
func setupWorkerQueue(t *testing.T, d *Daemon, workerID, taskID, commandID string, leaseEpoch int, opts ...workerQueueOption) {
	t.Helper()
	owner := workerID
	task := model.Task{
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
	}
	for _, opt := range opts {
		opt(&task)
	}
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks:         []model.Task{task},
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
	if state.TaskStates[taskID] != model.StatusPausedForReplan {
		t.Errorf("state task_states[%s] = %q, want %q", taskID, state.TaskStates[taskID], model.StatusPausedForReplan)
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

// TestResultWrite_FilesChangedOutsideExpectedPathsAccepted pins the
// policy that the daemon does NOT gate result_write on expected_paths.
// The previous gate read worker self-reported --files-changed and
// rejected entries that fell outside the declared paths, but a worker
// could simply re-submit with a narrowed list to bypass it while the
// integration commit (driven by `git add -A` against the worker
// worktree) silently merged the out-of-bounds files (Report 1 of
// 2026-05-03). The autonomous LLM Orchestration brief trusts worker
// output verbatim — defensive scope-limiting belongs in the worker's
// own ~/.claude policy hook, not in result_write — so the gate is
// removed and files_changed is treated as descriptive metadata.
func TestResultWrite_FilesChangedOutsideExpectedPathsAccepted(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1
	owner := workerID

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{{
			ID:            taskID,
			CommandID:     commandID,
			Purpose:       "test purpose",
			Content:       "test content",
			BloomLevel:    3,
			Status:        model.StatusInProgress,
			LeaseOwner:    &owner,
			LeaseEpoch:    leaseEpoch,
			ExpectedPaths: []string{"src/allowed/"},
			CreatedAt:     "2026-01-01T00:00:00Z",
			UpdatedAt:     "2026-01-01T00:00:00Z",
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", workerID+".yaml"), tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:     workerID,
		TaskID:       taskID,
		CommandID:    commandID,
		LeaseEpoch:   leaseEpoch,
		Status:       "completed",
		Summary:      "changed files",
		FilesChanged: []string{"src/allowed/main.go", "src/forbidden/main.go"},
	})

	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected result_write to accept files_changed regardless of expected_paths; got %+v", resp.Error)
	}

	rfPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, err := os.ReadFile(rfPath)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("unmarshal result file: %v", err)
	}
	if len(rf.Results) != 1 {
		t.Fatalf("expected 1 result entry, got %d", len(rf.Results))
	}
	if got := len(rf.Results[0].FilesChanged); got != 2 {
		t.Errorf("FilesChanged must be persisted verbatim as descriptive metadata; got %d entries (%v)", got, rf.Results[0].FilesChanged)
	}
}

// TestResultWrite_RunOnMainPreservesFilesChanged pins the policy that
// the daemon no longer strips a self-reported --files-changed for
// RunOnMain tasks. The previous strip-and-warn was a defensive gate
// against a Worker reporting bug; the autonomous LLM Orchestration
// brief instead trusts worker output verbatim. files_changed is
// descriptive metadata only, so the value is persisted as-is.
func TestResultWrite_RunOnMainPreservesFilesChanged(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000001_abcdef01"
	commandID := "cmd_0000000001_abcdef01"
	workerID := "worker1"
	leaseEpoch := 1
	owner := workerID

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{{
			ID:            taskID,
			CommandID:     commandID,
			Purpose:       "verify on main",
			Content:       "verify the published state",
			BloomLevel:    3,
			Status:        model.StatusInProgress,
			LeaseOwner:    &owner,
			LeaseEpoch:    leaseEpoch,
			ExpectedPaths: []string{"."},
			RunOnMain:     true,
			CreatedAt:     "2026-01-01T00:00:00Z",
			UpdatedAt:     "2026-01-01T00:00:00Z",
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", workerID+".yaml"), tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}
	setupCommandState(t, d, commandID, []string{taskID})

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:     workerID,
		TaskID:       taskID,
		CommandID:    commandID,
		LeaseEpoch:   leaseEpoch,
		Status:       "completed",
		Summary:      "verified",
		FilesChanged: []string{"feature.go"},
	})

	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("result write should accept run_on_main result; got error: %+v", resp.Error)
	}

	rfPath := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, err := os.ReadFile(rfPath)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("unmarshal result file: %v", err)
	}
	if len(rf.Results) != 1 {
		t.Fatalf("expected 1 result entry, got %d", len(rf.Results))
	}
	if got := len(rf.Results[0].FilesChanged); got != 1 {
		t.Errorf("FilesChanged must be preserved as descriptive metadata even on RunOnMain; got %d entries (%v)", got, rf.Results[0].FilesChanged)
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
	_, _, err := d.api.result.resultWritePhaseB(params, incomingResultID, model.StatusCompleted, false, "", false, false)
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

// testDaemonLogBuf returns the underlying bytes.Buffer attached to the daemon
// logger by newTestDaemon, allowing tests to assert on log output.
func testDaemonLogBuf(t *testing.T, d *Daemon) *bytes.Buffer {
	t.Helper()
	w, ok := d.logger.Writer().(*bytes.Buffer)
	if !ok {
		t.Fatalf("daemon logger writer is not a *bytes.Buffer (got %T)", d.logger.Writer())
	}
	return w
}

// setupCommandStateWithStatus is like setupCommandState but lets the test
// choose the initial status — useful for forcing forbidden source states
// (e.g., StatusPending → completed) to exercise the §2.1 validation.
func setupCommandStateWithStatus(t *testing.T, d *Daemon, commandID string, taskIDs []string, status model.Status) {
	t.Helper()
	taskStates := make(map[string]model.Status)
	for _, id := range taskIDs {
		taskStates[id] = status
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

// TestResultWrite_ValidateStateTransition_LogsForbidden exercises the new
// §2.1 validation hook: a task whose preexisting state is pending cannot
// directly transition to completed (workers must go through in_progress).
// The hook is in warn-mode, so the write still succeeds, but the violation
// must be logged so operators can detect a buggy worker/planner.
func TestResultWrite_ValidateStateTransition_LogsForbidden(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000002_abcdef02"
	commandID := "cmd_0000000002_abcdef02"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1)
	// Forbidden source state: pending → completed is not in §2.1 table.
	setupCommandStateWithStatus(t, d, commandID, []string{taskID}, model.StatusPending)

	logBuf := testDaemonLogBuf(t, d)
	logBuf.Reset()

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "done",
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success (warn-mode), got error: %v", resp.Error)
	}

	// The state IS still applied (warn-mode preserves the result write).
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	sdata, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st model.CommandState
	if err := yamlv3.Unmarshal(sdata, &st); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if st.TaskStates[taskID] != model.StatusCompleted {
		t.Errorf("state task_states[%s] = %q, want %q", taskID, st.TaskStates[taskID], model.StatusCompleted)
	}

	// And the violation was logged.
	if !strings.Contains(logBuf.String(), "invalid_state_transition") {
		t.Errorf("expected invalid_state_transition log entry, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "from=pending") {
		t.Errorf("expected from=pending in log entry, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "to=completed") {
		t.Errorf("expected to=completed in log entry, got: %s", logBuf.String())
	}
}

// TestResultWrite_ValidateStateTransition_AllowedTransitionSilent confirms a
// legitimate in_progress → completed transition does NOT trip the validator
// (no spurious warnings under the happy path).
func TestResultWrite_ValidateStateTransition_AllowedTransitionSilent(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000003_abcdef03"
	commandID := "cmd_0000000003_abcdef03"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1)
	setupCommandState(t, d, commandID, []string{taskID}) // defaults to in_progress

	logBuf := testDaemonLogBuf(t, d)
	logBuf.Reset()

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "done",
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}
	if strings.Contains(logBuf.String(), "invalid_state_transition") {
		t.Errorf("happy-path transition must not log invalid_state_transition, got: %s", logBuf.String())
	}
}

func TestResultWrite_ValidateStateTransition_RunningCompletedSilent(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000004_abcdef04"
	commandID := "cmd_0000000004_abcdef04"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1)
	setupCommandStateWithStatus(t, d, commandID, []string{taskID}, model.StatusRunning)

	logBuf := testDaemonLogBuf(t, d)
	logBuf.Reset()

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "done",
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("expected success, got error: %v", resp.Error)
	}
	if strings.Contains(logBuf.String(), "invalid_state_transition") {
		t.Errorf("running worker completion must not log invalid_state_transition, got: %s", logBuf.String())
	}
}

// TestComputeWorkerExpectedPathsForVerify_UnionsCompletedTasks asserts that the
// verify-time expected_paths surface matches Phase B's commit_policy worker-
// level union: it covers the source task being verified plus every other
// completed task on the same worker for the same command. Without this union,
// a worker holding multiple tasks would falsely flag earlier sibling-task
// dirty files (still uncommitted until phase merge) as out-of-scope.
func TestComputeWorkerExpectedPathsForVerify_UnionsCompletedTasks(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	commandID := "cmd_0000000001_uniontest"
	otherCommandID := "cmd_0000000002_other"
	workerID := "worker1"

	owner := workerID
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:            "task_completed_alpha",
				CommandID:     commandID,
				Status:        model.StatusCompleted,
				ExpectedPaths: []string{"version.go"},
				CreatedAt:     "2026-01-01T00:00:00Z",
				UpdatedAt:     "2026-01-01T00:00:00Z",
			},
			{
				ID:            "task_verify_pending_beta",
				CommandID:     commandID,
				Status:        model.StatusVerifyPending,
				LeaseOwner:    &owner,
				LeaseEpoch:    1,
				ExpectedPaths: []string{"version_test.go"},
				CreatedAt:     "2026-01-01T00:00:00Z",
				UpdatedAt:     "2026-01-01T00:00:00Z",
			},
			{
				ID:            "task_completed_gamma_dup",
				CommandID:     commandID,
				Status:        model.StatusCompleted,
				ExpectedPaths: []string{"version.go", "doc.go"},
				CreatedAt:     "2026-01-01T00:00:00Z",
				UpdatedAt:     "2026-01-01T00:00:00Z",
			},
			{
				// Different command — must not contribute to the union.
				ID:            "task_other_command",
				CommandID:     otherCommandID,
				Status:        model.StatusCompleted,
				ExpectedPaths: []string{"unrelated.go"},
				CreatedAt:     "2026-01-01T00:00:00Z",
				UpdatedAt:     "2026-01-01T00:00:00Z",
			},
		},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", workerID+".yaml"), tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	source := &model.Task{
		ID:            "task_verify_pending_beta",
		CommandID:     commandID,
		ExpectedPaths: []string{"version_test.go"},
	}
	got := d.api.result.computeWorkerExpectedPathsForVerify(commandID, workerID, source)

	want := map[string]struct{}{
		"version.go":      {},
		"version_test.go": {},
		"doc.go":          {},
	}
	if len(got) != len(want) {
		t.Fatalf("union size = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for _, p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected path in union: %q", p)
		}
		if p == "unrelated.go" {
			t.Errorf("union must not include cross-command path: %q", p)
		}
	}
}

// TestComputeWorkerExpectedPathsForVerify_RestrictsToSamePhase asserts
// that when the source task lives in a phase, the verify allowed-path
// surface only unions with same-phase siblings. Earlier-phase tasks
// have already been auto-committed and merged at their phase boundary,
// so admitting their ExpectedPaths would falsely widen the surface for
// the current phase.
func TestComputeWorkerExpectedPathsForVerify_RestrictsToSamePhase(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	commandID := "cmd_phase_scope_001"
	workerID := "worker1"

	// State: two phases. taskAlpha is in phase 1 (already merged), taskBeta
	// and taskTest are in phase 2 (current). source task is taskTest.
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "phase_1",
					Name:    "phase_1",
					Type:    "concrete",
					Status:  model.PhaseStatusCompleted,
					TaskIDs: []string{"task_alpha"},
				},
				{
					PhaseID: "phase_2",
					Name:    "phase_2",
					Type:    "concrete",
					Status:  model.PhaseStatusActive,
					TaskIDs: []string{"task_beta", "task_test"},
				},
			},
		},
		TaskTracking: model.TaskTracking{TaskStates: map[string]model.Status{
			"task_alpha": model.StatusCompleted,
			"task_beta":  model.StatusCompleted,
			"task_test":  model.StatusVerifyPending,
		}},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:            "task_alpha", // earlier phase — must NOT be unioned
				CommandID:     commandID,
				Status:        model.StatusCompleted,
				ExpectedPaths: []string{"earlier_phase.go"},
				CreatedAt:     "2026-01-01T00:00:00Z",
				UpdatedAt:     "2026-01-01T00:00:00Z",
			},
			{
				ID:            "task_beta", // same phase, completed → unioned
				CommandID:     commandID,
				Status:        model.StatusCompleted,
				ExpectedPaths: []string{"same_phase_sibling.go"},
				CreatedAt:     "2026-01-01T00:00:00Z",
				UpdatedAt:     "2026-01-01T00:00:00Z",
			},
			{
				ID:            "task_test",
				CommandID:     commandID,
				Status:        model.StatusVerifyPending,
				ExpectedPaths: []string{"current.go"},
				CreatedAt:     "2026-01-01T00:00:00Z",
				UpdatedAt:     "2026-01-01T00:00:00Z",
			},
		},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", workerID+".yaml"), tq); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	source := &model.Task{
		ID:            "task_test",
		CommandID:     commandID,
		ExpectedPaths: []string{"current.go"},
	}
	got := d.api.result.computeWorkerExpectedPathsForVerify(commandID, workerID, source)

	want := map[string]struct{}{
		"current.go":            {},
		"same_phase_sibling.go": {},
	}
	if len(got) != len(want) {
		t.Fatalf("union size = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for _, p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected path in union: %q", p)
		}
		if p == "earlier_phase.go" {
			t.Errorf("earlier-phase task path leaked into current-phase verify surface: %q", p)
		}
	}
}

// TestReserveOrDeferHeavyVerify covers the phase-aware verify gating that
// defers heavy verify while the phase still has non-terminal sibling
// tasks. reserveOrDeferHeavyVerify is a state-lock-protected CAS that
// elects exactly one owner per phase, so parallel-completing phases
// don't end up with every sibling deferring (which would silently
// disable the §S1-1 Strong Signal).
func TestReserveOrDeferHeavyVerify(t *testing.T) {
	commandID := "cmd_0000000004_phasegate"
	phaseID := "phase_setup"
	taskAlpha := "task_alpha_0000000001"
	taskBeta := "task_beta_0000000002"
	taskTest := "task_test_0000000003"

	type reservation struct {
		phaseID string
		taskID  string
	}
	cases := []struct {
		name             string
		taskStates       map[string]model.Status
		phaseTaskIDs     []string
		preReserved      *reservation
		sourceTask       *model.Task
		wantDefer        bool
		wantOwnerForTest string // expected owner after the call (empty = no owner)
	}{
		{
			name: "intermediate_task_with_running_sibling_defers_heavy",
			taskStates: map[string]model.Status{
				taskAlpha: model.StatusVerifyPending,
				taskBeta:  model.StatusInProgress, // still actively running
				taskTest:  model.StatusPlanned,
			},
			phaseTaskIDs:     []string{taskAlpha, taskBeta, taskTest},
			sourceTask:       &model.Task{ID: taskAlpha, CommandID: commandID},
			wantDefer:        true,
			wantOwnerForTest: "", // no reservation while siblings still running
		},
		{
			name: "phase_final_task_reserves_and_runs_heavy",
			taskStates: map[string]model.Status{
				taskAlpha: model.StatusCompleted,
				taskBeta:  model.StatusCompleted,
				taskTest:  model.StatusVerifyPending,
			},
			phaseTaskIDs:     []string{taskAlpha, taskBeta, taskTest},
			sourceTask:       &model.Task{ID: taskTest, CommandID: commandID},
			wantDefer:        false,
			wantOwnerForTest: taskTest,
		},
		{
			name: "task_outside_any_phase_runs_full_verify",
			taskStates: map[string]model.Status{
				taskAlpha: model.StatusVerifyPending,
				taskBeta:  model.StatusInProgress,
			},
			phaseTaskIDs:     []string{taskBeta}, // alpha not in any phase
			sourceTask:       &model.Task{ID: taskAlpha, CommandID: commandID},
			wantDefer:        false,
			wantOwnerForTest: "",
		},
		{
			name: "unknown_sibling_status_runs_full_verify_defensively",
			taskStates: map[string]model.Status{
				taskAlpha: model.StatusVerifyPending,
				// taskBeta intentionally absent from TaskStates
			},
			phaseTaskIDs:     []string{taskAlpha, taskBeta},
			sourceTask:       &model.Task{ID: taskAlpha, CommandID: commandID},
			wantDefer:        false,
			wantOwnerForTest: "",
		},
		{
			name: "all_siblings_terminal_reserves_and_runs_heavy",
			taskStates: map[string]model.Status{
				taskAlpha: model.StatusCompleted,
				taskBeta:  model.StatusCancelled,
				taskTest:  model.StatusVerifyPending,
			},
			phaseTaskIDs:     []string{taskAlpha, taskBeta, taskTest},
			sourceTask:       &model.Task{ID: taskTest, CommandID: commandID},
			wantDefer:        false,
			wantOwnerForTest: taskTest,
		},
		// Race coverage: every sibling sits at verify_pending
		// simultaneously. Without the lock-protected reservation each
		// one would see the others as non-terminal and defer; with the
		// reservation the first caller wins ownership and runs heavy,
		// sibling callers observe the reservation and defer.
		{
			name: "race_first_caller_wins_reservation",
			taskStates: map[string]model.Status{
				taskAlpha: model.StatusVerifyPending,
				taskBeta:  model.StatusVerifyPending,
				taskTest:  model.StatusVerifyPending,
			},
			phaseTaskIDs:     []string{taskAlpha, taskBeta, taskTest},
			sourceTask:       &model.Task{ID: taskAlpha, CommandID: commandID},
			wantDefer:        false,
			wantOwnerForTest: taskAlpha,
		},
		{
			name: "race_second_caller_defers_to_reservation_holder",
			taskStates: map[string]model.Status{
				taskAlpha: model.StatusVerifyPending,
				taskBeta:  model.StatusVerifyPending,
				taskTest:  model.StatusVerifyPending,
			},
			phaseTaskIDs:     []string{taskAlpha, taskBeta, taskTest},
			preReserved:      &reservation{phaseID: phaseID, taskID: taskAlpha},
			sourceTask:       &model.Task{ID: taskBeta, CommandID: commandID},
			wantDefer:        true,
			wantOwnerForTest: taskAlpha, // unchanged
		},
		{
			name: "stale_owner_replaced_by_retry_re_evaluates",
			taskStates: map[string]model.Status{
				// Original owner taskAlpha is no longer in phase.TaskIDs
				// (replaced by retry). Its reservation must not block the
				// surviving sibling.
				taskBeta: model.StatusVerifyPending,
				taskTest: model.StatusCompleted,
			},
			phaseTaskIDs:     []string{taskBeta, taskTest}, // taskAlpha removed
			preReserved:      &reservation{phaseID: phaseID, taskID: taskAlpha},
			sourceTask:       &model.Task{ID: taskBeta, CommandID: commandID},
			wantDefer:        false,
			wantOwnerForTest: taskBeta, // re-claimed by surviving owner
		},
		// Retry handler keeps the predecessor in phase.TaskIDs (only
		// appends the retry), so ownerInPhase(owner) stays true after a
		// verify-failure retry. The reservation must also check the
		// owner's STATUS — only verify_pending or completed should
		// hold the reservation. Any other status means heavy verify did
		// not pass for the phase and the retry should be allowed to
		// claim ownership.
		{
			name: "owner_cancelled_by_retry_release_to_replacement",
			taskStates: map[string]model.Status{
				taskAlpha: model.StatusCancelled,     // original, superseded by retry
				taskBeta:  model.StatusCompleted,     // sibling, unaffected
				taskTest:  model.StatusVerifyPending, // taskTest is the retry of taskAlpha
			},
			// Predecessor stays in phase.TaskIDs alongside the retry.
			phaseTaskIDs:     []string{taskAlpha, taskBeta, taskTest},
			preReserved:      &reservation{phaseID: phaseID, taskID: taskAlpha},
			sourceTask:       &model.Task{ID: taskTest, CommandID: commandID},
			wantDefer:        false,
			wantOwnerForTest: taskTest, // retry re-claims; old owner Cancelled is stale
		},
		{
			name: "owner_failed_release_to_replacement",
			taskStates: map[string]model.Status{
				taskAlpha: model.StatusFailed,
				taskBeta:  model.StatusCompleted,
				taskTest:  model.StatusVerifyPending,
			},
			phaseTaskIDs:     []string{taskAlpha, taskBeta, taskTest},
			preReserved:      &reservation{phaseID: phaseID, taskID: taskAlpha},
			sourceTask:       &model.Task{ID: taskTest, CommandID: commandID},
			wantDefer:        false,
			wantOwnerForTest: taskTest,
		},
		{
			name: "owner_repair_pending_keeps_phase_unready",
			// Owner failed verify, sits at repair_pending while the retry
			// is still being created/dispatched. A second sibling that
			// reaches verify_pending should NOT claim heavy yet because
			// the owner's repair task hasn't completed — phase isn't
			// ready. The reservation gets cleared (so the eventual retry
			// can claim) but eligibility check defers this caller.
			taskStates: map[string]model.Status{
				taskAlpha: model.StatusRepairPending,
				taskBeta:  model.StatusVerifyPending,
				taskTest:  model.StatusCompleted,
			},
			phaseTaskIDs:     []string{taskAlpha, taskBeta, taskTest},
			preReserved:      &reservation{phaseID: phaseID, taskID: taskAlpha},
			sourceTask:       &model.Task{ID: taskBeta, CommandID: commandID},
			wantDefer:        true,
			wantOwnerForTest: "", // reservation cleared, no new owner yet
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDaemon(t)

			pt := model.PhaseTracking{
				Phases: []model.Phase{{
					PhaseID: phaseID,
					Name:    "setup",
					Type:    "concrete",
					Status:  model.PhaseStatusActive,
					TaskIDs: tc.phaseTaskIDs,
				}},
			}
			if tc.preReserved != nil {
				pt.HeavyVerifyOwners = map[string]string{tc.preReserved.phaseID: tc.preReserved.taskID}
			}
			state := model.CommandState{
				SchemaVersion: 1,
				FileType:      "state_command",
				CommandID:     commandID,
				PlanStatus:    model.PlanStatusSealed,
				PhaseTracking: pt,
				TaskTracking:  model.TaskTracking{TaskStates: tc.taskStates},
				CreatedAt:     "2026-01-01T00:00:00Z",
				UpdatedAt:     "2026-01-01T00:00:00Z",
			}
			path := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
			if err := yamlutil.AtomicWrite(path, state); err != nil {
				t.Fatalf("write command state: %v", err)
			}

			got := d.api.result.reserveOrDeferHeavyVerify(commandID, tc.sourceTask)
			if got != tc.wantDefer {
				t.Errorf("reserveOrDeferHeavyVerify = %v, want %v", got, tc.wantDefer)
			}

			// Verify the reservation persisted (or didn't) as expected.
			persisted, _, err := loadYAMLFile[model.CommandState](path, false)
			if err != nil {
				t.Fatalf("reload state: %v", err)
			}
			gotOwner := persisted.HeavyVerifyOwners[phaseID]
			if gotOwner != tc.wantOwnerForTest {
				t.Errorf("heavy_verify_owners[%s] = %q, want %q", phaseID, gotOwner, tc.wantOwnerForTest)
			}
		})
	}
}

// TestEmitVerifyOutcomeChangedPlannerSignal_QueuesSupplementarySignal asserts
// the supplementary signal lands in the planner_signals queue with the
// `verify_outcome_changed` kind and per-task PhaseID dedup scope. This signal
// closes the Planner/daemon divergence where notifyPlannerOfWorkerResult
// had already told Planner the task was completed, but verify subsequently
// scheduled a retry — without this signal Planner would keep progressing
// while the publish gate stayed blocked.
func TestEmitVerifyOutcomeChangedPlannerSignal_QueuesSupplementarySignal(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	params := ResultWriteParams{
		Reporter:  "worker1",
		TaskID:    "task_outcome_changed_001",
		CommandID: "cmd_outcome_changed_001",
		Status:    "completed",
	}
	d.api.result.emitVerifyOutcomeChangedPlannerSignal(params,
		"verify_failed category=test", model.StatusRepairPending)

	signalsPath := filepath.Join(d.maestroDir, "queue", "planner_signals.yaml")
	data, err := os.ReadFile(signalsPath)
	if err != nil {
		t.Fatalf("read planner_signals: %v", err)
	}
	var sq model.PlannerSignalQueue
	if err := yamlv3.Unmarshal(data, &sq); err != nil {
		t.Fatalf("unmarshal planner_signals: %v", err)
	}
	if len(sq.Signals) != 1 {
		t.Fatalf("signals = %d, want 1 (got=%v)", len(sq.Signals), sq.Signals)
	}
	sig := sq.Signals[0]
	if sig.Kind != "verify_outcome_changed" {
		t.Errorf("Kind = %q, want verify_outcome_changed", sig.Kind)
	}
	if sig.CommandID != params.CommandID {
		t.Errorf("CommandID = %q, want %q", sig.CommandID, params.CommandID)
	}
	if sig.PhaseID != "__task_"+params.TaskID {
		t.Errorf("PhaseID = %q, want __task_<task_id>", sig.PhaseID)
	}
	if !strings.Contains(sig.Message, "worker_reported: completed") {
		t.Errorf("message must echo worker-reported status, got %q", sig.Message)
	}
	if !strings.Contains(sig.Message, "verify_final_status: repair_pending") {
		t.Errorf("message must echo verify final status, got %q", sig.Message)
	}

	// Re-emit with the same dedup key — must be a no-op (signalDedupKey collapses
	// duplicate signals so the Planner doesn't re-receive the same divergence).
	d.api.result.emitVerifyOutcomeChangedPlannerSignal(params,
		"verify_failed category=test", model.StatusRepairPending)
	data, err = os.ReadFile(signalsPath)
	if err != nil {
		t.Fatalf("re-read planner_signals: %v", err)
	}
	var sq2 model.PlannerSignalQueue
	if err := yamlv3.Unmarshal(data, &sq2); err != nil {
		t.Fatalf("unmarshal planner_signals: %v", err)
	}
	if len(sq2.Signals) != 1 {
		t.Errorf("signals after duplicate emit = %d, want 1 (dedup must collapse)", len(sq2.Signals))
	}
}

// TestComputeWorkerExpectedPathsForVerify_FallsBackOnQueueLoadFailure asserts
// that when the worker queue cannot be loaded (e.g. missing file), the verify
// expected_paths still includes the source task's paths so a transient I/O
// hiccup never silently widens the allowed surface to "anything".
func TestComputeWorkerExpectedPathsForVerify_FallsBackOnQueueLoadFailure(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	source := &model.Task{
		ID:            "task_only_source",
		CommandID:     "cmd_0000000003_fallback",
		ExpectedPaths: []string{"only.go"},
	}
	got := d.api.result.computeWorkerExpectedPathsForVerify(source.CommandID, "missing_worker", source)
	if len(got) != 1 || got[0] != "only.go" {
		t.Fatalf("fallback union = %v, want [only.go]", got)
	}
}
