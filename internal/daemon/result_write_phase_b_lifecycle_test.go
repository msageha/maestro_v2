package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// recordingVerifyRunner wraps a VerifyOutcome and records the parameters it
// was invoked with. Tests use it to confirm that Phase B routed a result
// through verify_pending (the only path that calls the runner).
type recordingVerifyRunner struct {
	outcome        VerifyOutcome
	err            error
	invocations    atomic.Int32
	lastTaskID     string
	lastWorkingDir string
}

func (r *recordingVerifyRunner) Run(_ context.Context, taskID, _, workingDir string, _ []string) (VerifyOutcome, error) {
	r.invocations.Add(1)
	r.lastTaskID = taskID
	r.lastWorkingDir = workingDir
	return r.outcome, r.err
}

type recordingReviewDispatcher struct {
	enabled        bool
	calls          atomic.Int32
	precaptureCall atomic.Int32
}

func (r *recordingReviewDispatcher) Enabled() bool { return r.enabled }

func (r *recordingReviewDispatcher) DispatchIfEligible(_ context.Context, _ ResultWriteParams) {
	r.calls.Add(1)
}

func (r *recordingReviewDispatcher) PrecaptureDiff(_, _, _ string) {
	r.precaptureCall.Add(1)
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

func assertQueueTaskStatus(t *testing.T, d *Daemon, workerID, taskID string, want model.Status) {
	t.Helper()
	queuePath := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	for _, task := range tq.Tasks {
		if task.ID != taskID {
			continue
		}
		if task.Status != want {
			t.Fatalf("queue task %s status = %q, want %q", taskID, task.Status, want)
		}
		return
	}
	t.Fatalf("queue task %s not found in %s", taskID, workerID)
}

func readPlannerSignalsForLifecycle(t *testing.T, d *Daemon) model.PlannerSignalQueue {
	t.Helper()
	path := filepath.Join(d.maestroDir, "queue", "planner_signals.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return model.PlannerSignalQueue{}
	}
	var sq model.PlannerSignalQueue
	if err := yamlv3.Unmarshal(data, &sq); err != nil {
		t.Fatalf("unmarshal planner signals: %v", err)
	}
	return sq
}

func TestResultWrite_CompletedVerifyRunsAsyncInBackground(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000018_asyncvr"
	commandID := "cmd_0000000018_asyncvr"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1, withRunOnIntegration())
	setupCommandState(t, d, commandID, []string{taskID})

	rec := &recordingVerifyRunner{outcome: VerifyOutcome{Passed: true}}
	d.api.result.SetVerifyRunner(rec)
	d.api.result.SetVerifyAsync(true)
	var verifyFn func(context.Context)
	d.api.result.spawnTask = func(name string, fn func(context.Context)) bool {
		if name != "resultWriteVerify" {
			t.Fatalf("spawn name = %q, want resultWriteVerify", name)
		}
		verifyFn = fn
		return true
	}

	resp := d.api.handleResultWrite(makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "ok",
		RetrySafe:  true,
	}))
	if !resp.Success {
		t.Fatalf("handleResultWrite: %v", resp.Error)
	}
	var payload map[string]string
	if err := json.Unmarshal(resp.Data, &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if payload["verification"] != "pending" {
		t.Fatalf("verification response = %q, want pending", payload["verification"])
	}
	if verifyFn == nil {
		t.Fatal("verify background function was not captured")
	}
	if got := rec.invocations.Load(); got != 0 {
		t.Fatalf("verify runner invoked before background function ran: %d", got)
	}
	st := readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[taskID]; got != model.StatusVerifyPending {
		t.Fatalf("state before background verify = %q, want %q", got, model.StatusVerifyPending)
	}

	verifyFn(context.Background())
	if got := rec.invocations.Load(); got != 1 {
		t.Fatalf("verify runner invocations = %d, want 1", got)
	}
	st = readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[taskID]; got != model.StatusCompleted {
		t.Fatalf("state after background verify = %q, want %q", got, model.StatusCompleted)
	}
	assertQueueTaskStatus(t, d, workerID, taskID, model.StatusCompleted)
}

func TestResultWrite_AsyncVerifyHonorsAdmissionLimit(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000019_admitvr"
	commandID := "cmd_0000000019_admitvr"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1, withRunOnIntegration())
	setupCommandState(t, d, commandID, []string{taskID})

	ac := admission.NewController(model.AdmissionControl{MaxConcurrentVerify: 1})
	if !ac.TryAcquire(admission.OpVerify) {
		t.Fatal("pre-acquire verify slot failed")
	}
	d.api.result.SetAdmissionController(ac)
	rec := &recordingVerifyRunner{outcome: VerifyOutcome{Passed: true}}
	d.api.result.SetVerifyRunner(rec)
	d.api.result.SetVerifyAsync(true)
	var verifyFn func(context.Context)
	d.api.result.spawnTask = func(name string, fn func(context.Context)) bool {
		verifyFn = fn
		return true
	}

	resp := d.api.handleResultWrite(makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "ok",
		RetrySafe:  true,
	}))
	if !resp.Success {
		t.Fatalf("handleResultWrite: %v", resp.Error)
	}
	if verifyFn == nil {
		t.Fatal("verify background task was not spawned")
	}
	st := readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[taskID]; got != model.StatusVerifyPending {
		t.Fatalf("state after admitted=false = %q, want verify_pending", got)
	}
	if got := ac.ActiveCount(admission.OpVerify); got != 1 {
		t.Fatalf("verify admission slots = %d, want 1 (pre-acquired slot only)", got)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	verifyFn(cancelled)
	if got := rec.invocations.Load(); got != 0 {
		t.Fatalf("verify runner invoked while admission was saturated: %d", got)
	}
	if got := ac.ActiveCount(admission.OpVerify); got != 1 {
		t.Fatalf("verify admission slots after cancelled wait = %d, want 1", got)
	}

	ac.Release(admission.OpVerify)
	verifyFn(context.Background())
	if got := rec.invocations.Load(); got != 1 {
		t.Fatalf("verify runner invocations after slot release = %d, want 1", got)
	}
	if got := ac.ActiveCount(admission.OpVerify); got != 0 {
		t.Fatalf("verify admission slots after background verify = %d, want 0", got)
	}
}

func TestResultWrite_DuplicateDoesNotDispatchReviewBeforeAsyncVerifyCompletes(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000020_dupreview"
	commandID := "cmd_0000000020_dupreview"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1, withRunOnIntegration())
	setupCommandState(t, d, commandID, []string{taskID})

	rec := &recordingVerifyRunner{outcome: VerifyOutcome{Passed: true}}
	review := &recordingReviewDispatcher{enabled: true}
	d.api.result.SetVerifyRunner(rec)
	d.api.result.SetVerifyAsync(true)
	d.api.result.reviewCoord = func() reviewDispatcher { return review }
	var verifyFn func(context.Context)
	d.api.result.spawnTask = func(name string, fn func(context.Context)) bool {
		verifyFn = fn
		return true
	}

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
		t.Fatalf("initial handleResultWrite: %v", resp.Error)
	}
	if verifyFn == nil {
		t.Fatal("verify background function was not captured")
	}
	dupResp := d.api.handleResultWrite(req)
	if !dupResp.Success {
		t.Fatalf("duplicate handleResultWrite: %v", dupResp.Error)
	}
	if got := review.calls.Load(); got != 0 {
		t.Fatalf("review dispatched before async verify completed: %d", got)
	}

	verifyFn(context.Background())
	if got := rec.invocations.Load(); got != 1 {
		t.Fatalf("verify runner invocations = %d, want 1", got)
	}
	if got := review.calls.Load(); got != 1 {
		t.Fatalf("review dispatches after verify completed = %d, want 1", got)
	}
}

// TestResultWrite_CompletedGoesViaVerifyPending verifies that a worker-reported
// completed status is routed through verify_pending before reaching completed,
// per REQUIREMENTS.md §2.1. Observability comes from a recording VerifyRunner:
// the runner is invoked iff Phase B parked the task at verify_pending.
//
// The test uses RunOnIntegration: true because the post-2026-05-05 redesign
// short-circuits per-task verify for normal worker tasks; only RunOnIntegration /
// RunOnMain tasks still drive the VerifyRunner. The §2.1 routing under test
// (completed → verify_pending → terminal) is identical for both task kinds.
func TestResultWrite_CompletedGoesViaVerifyPending(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000010_completed"
	commandID := "cmd_0000000010_complete"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1, withRunOnIntegration())
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

// TestResultWrite_CompletedVerifyFailsWithoutRetryPausesForReplan exercises the
// §2.1 branch where the worker reports completed but the Verification Runner
// rejects the result and automatic repair is disabled. The daemon must not leave
// the task parked in repair_pending with no repair producer.
func TestResultWrite_CompletedVerifyFailsWithoutRetryPausesForReplan(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000011_verifail"
	commandID := "cmd_0000000011_verifyfa"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1, withRunOnIntegration())
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
	if got := st.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Errorf("final state = %q, want %q (verify failure without auto repair routes to replan)",
			got, model.StatusPausedForReplan)
	}
	assertQueueTaskStatus(t, d, workerID, taskID, model.StatusFailed)
}

func TestResultWrite_CompletedVerifyFailsSchedulesRepairTask(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	d.config.Retry.TaskExecution = model.TaskRetryConfig{
		Enabled:    true,
		MaxRetries: 3,
	}
	taskID := "task_0000000013_verirepa"
	commandID := "cmd_0000000013_verirepa"
	workerID := "worker1"

	owner := workerID
	setupWorkerQueueWithTask(t, d, workerID, model.Task{
		ID:                 taskID,
		CommandID:          commandID,
		Purpose:            "verify repair",
		Content:            "implement feature",
		AcceptanceCriteria: "tests pass",
		ExpectedPaths:      []string{"feature.go"},
		BloomLevel:         3,
		Status:             model.StatusInProgress,
		LeaseOwner:         &owner,
		LeaseEpoch:         1,
		// RunOnIntegration drives the per-task VerifyRunner under the
		// post-2026-05-05 contract (normal worker tasks bypass verify).
		RunOnIntegration: true,
		CreatedAt:        "2026-01-01T00:00:00Z",
		UpdatedAt:        "2026-01-01T00:00:00Z",
	})
	setupCommandState(t, d, commandID, []string{taskID})

	d.api.result.SetVerifyRunner(NewFixedVerifyRunner(
		VerifyOutcome{Passed: false, Reason: "go test ./... failed"}, nil))

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "ok per worker",
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("handleResultWrite: %v", resp.Error)
	}

	st := readCommandStateForLifecycle(t, d, commandID)
	// Once the verify repair successor is enqueued, the original task's
	// state is auto-flipped from repair_pending to cancelled
	// (superseded_by_verify_repair) — eliminating the plan-complete
	// validation failure where phase=terminal would coexist with
	// task=repair_pending and force the Planner to burn LLM round-trips
	// on plan_complete retries.
	if got := st.TaskStates[taskID]; got != model.StatusCancelled {
		t.Fatalf("original task state = %q, want %q (superseded by verify repair successor)",
			got, model.StatusCancelled)
	}
	if reason, ok := st.CancelledReasons[taskID]; !ok {
		t.Errorf("expected CancelledReasons[%s] to be populated, got missing", taskID)
	} else if !strings.Contains(reason, "superseded_by_verify_repair") {
		t.Errorf("CancelledReasons[%s] = %q, want superseded_by_verify_repair prefix", taskID, reason)
	}

	queuePath := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	var repair *model.Task
	var original *model.Task
	for i := range tq.Tasks {
		if tq.Tasks[i].ID == taskID {
			original = &tq.Tasks[i]
		}
		if tq.Tasks[i].OriginalTaskID == taskID {
			repair = &tq.Tasks[i]
		}
	}
	if original == nil {
		t.Fatalf("expected original task in queue, queue=%+v", tq.Tasks)
	}
	if original.Status != model.StatusCancelled {
		t.Fatalf("original queue status = %q, want %q (superseded by verify repair task)",
			original.Status, model.StatusCancelled)
	}
	if repair == nil {
		t.Fatalf("expected verify repair task in queue, queue=%+v", tq.Tasks)
	}
	if repair.OperationType != model.OperationTypeRepair {
		t.Errorf("repair OperationType = %q, want %q", repair.OperationType, model.OperationTypeRepair)
	}
	if !strings.Contains(repair.Content, "go test ./... failed") {
		t.Errorf("repair content does not include verify reason: %q", repair.Content)
	}
	if st.TaskStates[repair.ID] != model.StatusPlanned {
		t.Errorf("repair state = %q, want %q", st.TaskStates[repair.ID], model.StatusPlanned)
	}
}

func TestResultWrite_VerifyRepairMaxRetriesPausesForReplan(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	d.config.Retry.TaskExecution = model.TaskRetryConfig{
		Enabled:    true,
		MaxRetries: 1,
	}
	taskID := "task_0000000014_verimaxr"
	commandID := "cmd_0000000014_verimaxr"
	workerID := "worker1"

	owner := workerID
	setupWorkerQueueWithTask(t, d, workerID, model.Task{
		ID:               taskID,
		CommandID:        commandID,
		Purpose:          "verify repair exhausted",
		Content:          "implement feature",
		BloomLevel:       3,
		Status:           model.StatusInProgress,
		LeaseOwner:       &owner,
		LeaseEpoch:       1,
		ExecutionRetries: 1,
		// RunOnIntegration: see the corresponding comment in
		// TestResultWrite_CompletedVerifyFailsSchedulesRepairTask.
		RunOnIntegration: true,
		CreatedAt:        "2026-01-01T00:00:00Z",
		UpdatedAt:        "2026-01-01T00:00:00Z",
	})
	setupCommandState(t, d, commandID, []string{taskID})

	d.api.result.SetVerifyRunner(NewFixedVerifyRunner(
		VerifyOutcome{Passed: false, Reason: "verification still failing"}, nil))

	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "ok per worker",
	})
	resp := d.api.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("handleResultWrite: %v", resp.Error)
	}

	st := readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Fatalf("task state = %q, want %q when verify repair budget is exhausted", got, model.StatusPausedForReplan)
	}
	assertQueueTaskStatus(t, d, workerID, taskID, model.StatusFailed)

	queuePath := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	for _, task := range tq.Tasks {
		if task.OriginalTaskID == taskID {
			t.Fatalf("repair task should not be queued after max retries, got %+v", task)
		}
	}
}

func TestResultWrite_VerifyRepairSourceMissingPausesForReplan(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000015_verinosr"
	commandID := "cmd_0000000015_verinosr"

	setupCommandState(t, d, commandID, []string{taskID})
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	if err := updateYAMLFile(statePath, func(state *model.CommandState) error {
		state.TaskStates[taskID] = model.StatusRepairPending
		return nil
	}); err != nil {
		t.Fatalf("seed repair_pending state: %v", err)
	}

	d.api.result.handleVerifyRepairRegistration(nil, ResultWriteParams{
		TaskID:    taskID,
		CommandID: commandID,
		Reporter:  "worker1",
	}, "verify_failed")

	st := readCommandStateForLifecycle(t, d, commandID)
	if got := st.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Fatalf("task state = %q, want %q when verify repair source task is missing",
			got, model.StatusPausedForReplan)
	}
}

// TestResultWrite_VerifyRunnerErrorWithoutRetryPausesForReplan verifies that a
// VerifyRunner crash is treated as a verification failure and, when automatic
// repair is disabled, converted to a Planner replan signal instead of a stuck
// repair_pending state.
func TestResultWrite_VerifyRunnerErrorWithoutRetryPausesForReplan(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)
	taskID := "task_0000000012_runerror"
	commandID := "cmd_0000000012_runerror"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1, withRunOnIntegration())
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
	if got := st.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Errorf("final state = %q, want %q (runner error without auto repair routes to replan)",
			got, model.StatusPausedForReplan)
	}
}

// TestResultWrite_FailedRetryableGoesDirectlyToSupersededCancelled
// pins the post-2026-05-06 Bug #1 fix: a retryable failure with a retry
// already enqueued must mark the original task terminal-cancelled
// (superseded_by_retry) immediately, NOT route through repair_pending.
// The previous behaviour parked the original at repair_pending awaiting
// applyVerifyOutcome / R9_VerifyStall, both of which are no-ops when
// verify.enabled=false — the task wedged forever and R2 kept skipping
// it as "repair pipeline owns this slot", blocking the publish gate
// for the whole iteration.
func TestResultWrite_FailedRetryableGoesDirectlyToSupersededCancelled(t *testing.T) {
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
	if got := st.TaskStates[origTaskID]; got != model.StatusCancelled {
		t.Errorf("original task state = %q, want %q (failure with retry routes directly to cancelled-superseded; verify-independent terminal)",
			got, model.StatusCancelled)
	}
	if reason, ok := st.CancelledReasons[origTaskID]; !ok ||
		!strings.HasPrefix(reason, "superseded_by_retry") {
		t.Errorf("original task cancelled_reason = %q, want prefix %q", reason, "superseded_by_retry")
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

// TestResultWrite_FailedNonRetryablePausesForReplan verifies that a failure
// rejected for retry for reasons other than max_repair_count (e.g.,
// non-retryable exit code, RetrySafe=false) still goes through the §2.1
// repair pipeline and parks for replanning.
func TestResultWrite_FailedNonRetryablePausesForReplan(t *testing.T) {
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
	if got := st.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Errorf("final state = %q, want %q (non-retryable failure pauses for replan)",
			got, model.StatusPausedForReplan)
	}
	signals := readPlannerSignalsForLifecycle(t, d)
	if len(signals.Signals) != 1 {
		t.Fatalf("planner signals = %d, want 1", len(signals.Signals))
	}
	if got := signals.Signals[0].Kind; got != "paused_for_replan" {
		t.Fatalf("planner signal kind = %q, want paused_for_replan", got)
	}
	if got := signals.Signals[0].CommandID; got != commandID {
		t.Fatalf("planner signal command = %q, want %q", got, commandID)
	}
	if !strings.Contains(signals.Signals[0].Message, taskID) {
		t.Fatalf("planner signal message %q does not mention task %s", signals.Signals[0].Message, taskID)
	}
}
