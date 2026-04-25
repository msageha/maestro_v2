package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// recordingVerifyRunner wraps a VerifyOutcome and records the parameters it
// was invoked with. Tests use it to confirm that Phase B routed a result
// through verify_pending (the only path that calls the runner).
type recordingVerifyRunner struct {
	outcome     VerifyOutcome
	err         error
	invocations atomic.Int32
	lastTaskID  string
}

func (r *recordingVerifyRunner) Run(_ context.Context, taskID, _ string, _ []string) (VerifyOutcome, error) {
	r.invocations.Add(1)
	r.lastTaskID = taskID
	return r.outcome, r.err
}

// setupWorkerQueueWithTask installs a custom in-progress task into worker's
// queue, allowing tests to populate fields like DefinitionOfAbort and
// ExecutionRetries that setupWorkerQueue does not expose.
func setupWorkerQueueWithTask(t *testing.T, d *Daemon, workerID string, task model.Task) {
	t.Helper()
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

func readCommandStateForLifecycle(t *testing.T, d *Daemon, commandID string) model.CommandState {
	t.Helper()
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st model.CommandState
	if err := yamlv3.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	return st
}

// TestResultWrite_CompletedGoesViaVerifyPending verifies that a worker-reported
// completed status is routed through verify_pending before reaching completed,
// per REQUIREMENTS.md §2.1. Observability comes from a recording VerifyRunner:
// the runner is invoked iff Phase B parked the task at verify_pending.
func TestResultWrite_CompletedGoesViaVerifyPending(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000010_completed"
	commandID := "cmd_0000000010_complete"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1)
	setupCommandState(t, d, commandID, []string{taskID})

	rec := &recordingVerifyRunner{outcome: VerifyOutcome{Passed: true}}
	d.api.result.SetVerifyRunner(rec)

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "ok",
		RetrySafe:  true,
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("handleResultWrite: %v", resp.Error)
	}

	if rec.invocations.Load() != 1 {
		t.Fatalf("VerifyRunner invocations = %d, want 1 (proves verify_pending was reached)",
			rec.invocations.Load())
	}
	if rec.lastTaskID != taskID {
		t.Errorf("VerifyRunner taskID = %q, want %q", rec.lastTaskID, taskID)
	}

	st := readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[taskID]; got != model.StatusCompleted {
		t.Errorf("final state = %q, want %q", got, model.StatusCompleted)
	}
}

// TestResultWrite_CompletedVerifyFailsGoesToRepairPending exercises the §2.1
// branch where the worker reports completed but the Verification Runner
// rejects the result, so the task ends up at repair_pending instead.
func TestResultWrite_CompletedVerifyFailsGoesToRepairPending(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000011_verifail"
	commandID := "cmd_0000000011_verifyfa"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1)
	setupCommandState(t, d, commandID, []string{taskID})

	d.api.result.SetVerifyRunner(NewFixedVerifyRunner(
		VerifyOutcome{Passed: false, Reason: "lint_failed"}, nil))

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "ok per worker",
		RetrySafe:  true,
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("handleResultWrite: %v", resp.Error)
	}

	st := readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[taskID]; got != model.StatusRepairPending {
		t.Errorf("final state = %q, want %q (verify failure routes to repair_pending)",
			got, model.StatusRepairPending)
	}
}

// TestResultWrite_VerifyRunnerErrorRoutesToRepairPending verifies that a
// VerifyRunner that crashes is treated as a verification failure (§S1-1: the
// pipeline must not stall in verify_pending if the runner itself errors).
func TestResultWrite_VerifyRunnerErrorRoutesToRepairPending(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000012_runerror"
	commandID := "cmd_0000000012_runerror"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1)
	setupCommandState(t, d, commandID, []string{taskID})

	d.api.result.SetVerifyRunner(NewFixedVerifyRunner(
		VerifyOutcome{}, errors.New("verify subprocess crashed")))

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "ok per worker",
		RetrySafe:  true,
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("handleResultWrite: %v", resp.Error)
	}

	st := readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[taskID]; got != model.StatusRepairPending {
		t.Errorf("final state = %q, want %q (runner error routes to repair_pending)",
			got, model.StatusRepairPending)
	}
}

// TestResultWrite_FailedRetryableGoesViaRepairPending verifies that a
// retryable failure is routed through repair_pending and a retry task is
// scheduled. The final state of the original task is repair_pending; the
// retry task is registered as pending.
func TestResultWrite_FailedRetryableGoesViaRepairPending(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	d.config.Retry.TaskExecution = model.TaskRetryConfig{
		Enabled:            true,
		RetryableExitCodes: []int{1},
		MaxRetries:         3,
	}
	origTaskID := "task_0000000020_origfail"
	commandID := "cmd_0000000020_failretr"
	workerID := "worker1"

	owner := workerID
	setupWorkerQueueWithTask(t, d, workerID, model.Task{
		ID:               origTaskID,
		CommandID:        commandID,
		Purpose:          "fail and retry",
		Content:          "trigger retryable failure",
		BloomLevel:       3,
		Status:           model.StatusInProgress,
		LeaseOwner:       &owner,
		LeaseEpoch:       1,
		ExecutionRetries: 0,
		CreatedAt:        "2026-01-01T00:00:00Z",
		UpdatedAt:        "2026-01-01T00:00:00Z",
	})
	setupCommandState(t, d, commandID, []string{origTaskID})

	exitCode := 1
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     origTaskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "failed",
		Summary:    "transient failure",
		ExitCode:   &exitCode,
		RetrySafe:  true,
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("handleResultWrite: %v", resp.Error)
	}

	st := readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[origTaskID]; got != model.StatusRepairPending {
		t.Errorf("original task state = %q, want %q (failure with retry routes to repair_pending)",
			got, model.StatusRepairPending)
	}

	// A retry task is registered at §2.1 `planned` (worker queue picks it up
	// later). Queue entry status is independent and stays `pending`.
	var retryTaskCount int
	for id, status := range st.TaskStates {
		if id == origTaskID {
			continue
		}
		if status == model.StatusPlanned {
			retryTaskCount++
		}
	}
	if retryTaskCount != 1 {
		t.Errorf("expected exactly 1 retry task in planned, got %d (state=%v)",
			retryTaskCount, st.TaskStates)
	}
}

// TestResultWrite_MaxRepairCountTriggersPausedForReplan verifies that a
// failure rejected for retry because definition_of_abort.max_repair_count
// was reached routes through verify_pending → repair_pending →
// paused_for_replan, signalling the planner per §S2-2.
func TestResultWrite_MaxRepairCountTriggersPausedForReplan(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	d.config.Retry.TaskExecution = model.TaskRetryConfig{
		Enabled:            true,
		RetryableExitCodes: []int{1},
		MaxRetries:         10, // global cap higher than per-task
	}
	taskID := "task_0000000030_maxrepai"
	commandID := "cmd_0000000030_maxrepai"
	workerID := "worker1"

	owner := workerID
	setupWorkerQueueWithTask(t, d, workerID, model.Task{
		ID:               taskID,
		CommandID:        commandID,
		Purpose:          "exhausted task",
		Content:          "definition_of_abort triggered",
		BloomLevel:       3,
		Status:           model.StatusInProgress,
		LeaseOwner:       &owner,
		LeaseEpoch:       1,
		ExecutionRetries: 2, // == DefinitionOfAbort.MaxRepairCount
		DefinitionOfAbort: &model.DefinitionOfAbort{
			MaxRepairCount: 2,
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	})
	setupCommandState(t, d, commandID, []string{taskID})

	exitCode := 1
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "failed",
		Summary:    "exhausted",
		ExitCode:   &exitCode,
		RetrySafe:  true,
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("handleResultWrite: %v", resp.Error)
	}

	st := readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Errorf("final state = %q, want %q (max_repair_count routes to paused_for_replan)",
			got, model.StatusPausedForReplan)
	}
}

// TestResultWrite_FailedNonRetryableStaysFailed verifies that a failure
// rejected for retry for reasons other than max_repair_count (e.g.,
// non-retryable exit code, RetrySafe=false) lands at the failed terminal
// directly, preserving backwards compatibility with the legacy pipeline.
func TestResultWrite_FailedNonRetryableStaysFailed(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	d.config.Retry.TaskExecution = model.TaskRetryConfig{
		Enabled:            true,
		RetryableExitCodes: []int{1}, // exit 99 is NOT retryable
		MaxRetries:         3,
	}
	taskID := "task_0000000040_nonretry"
	commandID := "cmd_0000000040_nonretry"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1)
	setupCommandState(t, d, commandID, []string{taskID})

	exitCode := 99 // not in RetryableExitCodes
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "failed",
		Summary:    "permanent",
		ExitCode:   &exitCode,
		RetrySafe:  true,
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("handleResultWrite: %v", resp.Error)
	}

	st := readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[taskID]; got != model.StatusFailed {
		t.Errorf("final state = %q, want %q (non-retryable failure stays failed)",
			got, model.StatusFailed)
	}
}
