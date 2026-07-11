package reconcile

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// writeR10Fixture writes a paused_for_replan command state for R10 tests.
// updatedAt sets state.UpdatedAt — the anchor R10 measures elapsed time from.
// queueTask, when non-nil, is also installed in the worker's queue file so
// the test can observe queue-side overrides.
func writeR10Fixture(t *testing.T, maestroDir, commandID, taskID, updatedAt string, queueTask *model.Task) {
	t.Helper()
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{taskID: model.StatusPausedForReplan},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: updatedAt,
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if queueTask == nil {
		return
	}
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks:         []model.Task{*queueTask},
	}
	workerID := "worker1"
	queuePath := filepath.Join(maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		t.Fatalf("write queue: %v", err)
	}
}

func TestR10PausedForReplan_EscalatesAfterThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	tenMinSec := 600
	deps.Config.Verify = model.VerifyDaemonConfig{PausedForReplanDeadletterSec: &tenMinSec}

	commandID := "cmd_r10_001"
	taskID := "task_r10_001"

	updatedAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := updatedAt.Add(15 * time.Minute) // > 10m default threshold
	setClock(&deps, now)

	writeR10Fixture(t, maestroDir, commandID, taskID, updatedAt.Format(time.RFC3339),
		&model.Task{
			ID:        taskID,
			CommandID: commandID,
			Status:    model.StatusPausedForReplan,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		})

	run := newRun(&deps)
	outcome := R10PausedForReplanDeadletter{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}
	rep := outcome.Repairs[0]
	if rep.Pattern != PatternR10 || rep.TaskID != taskID || rep.CommandID != commandID {
		t.Errorf("unexpected repair: %+v", rep)
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	st, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got := st.TaskStates[taskID]; got != model.StatusFailed {
		t.Errorf("state task status = %q, want failed", got)
	}
	if st.LastReconciledAt == nil {
		t.Error("LastReconciledAt must be set after R10 escalation")
	}

	queuePath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	if len(tq.Tasks) != 1 || tq.Tasks[0].Status != model.StatusFailed {
		t.Errorf("queue task status = %v, want failed (full task=%+v)", tq.Tasks, tq.Tasks)
	}
}

func TestR10PausedForReplan_NoEscalationWithinThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	tenMinSec := 600
	deps.Config.Verify = model.VerifyDaemonConfig{PausedForReplanDeadletterSec: &tenMinSec}

	commandID := "cmd_r10_within"
	taskID := "task_r10_within"

	updatedAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := updatedAt.Add(3 * time.Minute) // well within 10m
	setClock(&deps, now)

	writeR10Fixture(t, maestroDir, commandID, taskID, updatedAt.Format(time.RFC3339), nil)

	run := newRun(&deps)
	outcome := R10PausedForReplanDeadletter{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected no repairs while within threshold, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	st, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got := st.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Errorf("state task status = %q, want paused_for_replan (within threshold)", got)
	}
}

// TestR10PausedForReplan_RequiredEscalation_WritesSyntheticPlannerResult
// pins the invariant: when R10 escalates a *required* task past the
// paused_for_replan deadletter window, R10 must publish a synthetic
// failed CommandResult to results/planner.yaml so R3 can reconcile the
// planner queue command (in_progress -> failed) on the next scan
// without waiting for an unresponsive Planner to write a real result.
func TestR10PausedForReplan_RequiredEscalation_WritesSyntheticPlannerResult(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	tenMinSec := 600
	deps.Config.Verify = model.VerifyDaemonConfig{PausedForReplanDeadletterSec: &tenMinSec}

	commandID := "cmd_r10_required"
	taskID := "task_r10_required"

	updatedAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := updatedAt.Add(15 * time.Minute) // > 10m default threshold
	setClock(&deps, now)

	// Required task — escalation must trigger synthetic result.
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates:      map[string]model.Status{taskID: model.StatusPausedForReplan},
			RequiredTaskIDs: []string{taskID},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: updatedAt.Format(time.RFC3339),
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	run := newRun(&deps)
	outcome := R10PausedForReplanDeadletter{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}

	resultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	rf, err := run.loadCommandResultFile(resultPath)
	if err != nil {
		t.Fatalf("load planner result: %v", err)
	}
	var found *model.CommandResult
	for i := range rf.Results {
		if rf.Results[i].CommandID == commandID {
			found = &rf.Results[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected synthetic planner result for command %s, got %+v", commandID, rf.Results)
	}
	if found.Status != model.StatusFailed {
		t.Errorf("synthetic planner result status = %q, want failed", found.Status)
	}
}

// TestR10PausedForReplan_OptionalEscalation_NoSyntheticResult pins that
// optional-only escalations must NOT write a synthetic planner result —
// the command may still complete via remaining required tasks, and a
// premature command-failed write would race the legitimate completion.
func TestR10PausedForReplan_OptionalEscalation_NoSyntheticResult(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	tenMinSec2 := 600
	deps.Config.Verify = model.VerifyDaemonConfig{PausedForReplanDeadletterSec: &tenMinSec2}

	commandID := "cmd_r10_optional"
	taskID := "task_r10_optional"

	updatedAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := updatedAt.Add(15 * time.Minute)
	setClock(&deps, now)

	// Task is NOT in RequiredTaskIDs (optional).
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates:      map[string]model.Status{taskID: model.StatusPausedForReplan},
			RequiredTaskIDs: []string{}, // empty — task is optional
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: updatedAt.Format(time.RFC3339),
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	run := newRun(&deps)
	outcome := R10PausedForReplanDeadletter{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair (escalation still happens), got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}

	resultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	rf, err := run.loadCommandResultFile(resultPath)
	if err != nil {
		t.Fatalf("load planner result: %v", err)
	}
	for _, r := range rf.Results {
		if r.CommandID == commandID {
			t.Errorf("optional-only escalation must not write synthetic planner result, got %+v", r)
		}
	}
}

func TestR10PausedForReplan_DisabledByZeroThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	zero := 0
	deps.Config.Verify = model.VerifyDaemonConfig{PausedForReplanDeadletterSec: &zero}

	commandID := "cmd_r10_disabled"
	taskID := "task_r10_disabled"

	updatedAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := updatedAt.Add(48 * time.Hour) // way beyond any default
	setClock(&deps, now)

	writeR10Fixture(t, maestroDir, commandID, taskID, updatedAt.Format(time.RFC3339), nil)

	run := newRun(&deps)
	outcome := R10PausedForReplanDeadletter{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Fatalf("R10 should be a no-op when threshold=0, got %d repairs", len(outcome.Repairs))
	}
}

// TestR10PausedForReplan_QueueWriteFailure_LeavesStateRetryable pins the
// invariant: when r10MarkQueueTaskFailed fails, the state transition for
// that task must NOT be applied. Otherwise the next R10 scan would skip
// the task (state already failed, no longer paused_for_replan), leaving
// the queue stuck at non-terminal forever and the publish gate blocked.
//
// We simulate the queue write failure by making the queue file's parent
// directory read-only while keeping the file path valid for the read. R10
// reads the queue successfully, attempts an atomic write, and the OS
// returns EACCES from the rename. The expectation is:
//   - state.TaskStates[task] stays at paused_for_replan
//   - no Repair record is emitted (so reconciler doesn't think it is done)
//   - the next scan (read-only restored) re-attempts and succeeds
func TestR10PausedForReplan_QueueWriteFailure_LeavesStateRetryable(t *testing.T) {
	if os.Getuid() == 0 {
		// root bypasses directory write-permission bits entirely, so the
		// chmod-based failure injection below is a no-op and the write
		// would succeed instead of failing as this test requires.
		t.Skip("test requires non-root user")
	}
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	tenMinSec2 := 600
	deps.Config.Verify = model.VerifyDaemonConfig{PausedForReplanDeadletterSec: &tenMinSec2}

	commandID := "cmd_r10_queue_fail"
	taskID := "task_r10_queue_fail"

	updatedAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := updatedAt.Add(5 * time.Hour)
	setClock(&deps, now)

	writeR10Fixture(t, maestroDir, commandID, taskID, updatedAt.Format(time.RFC3339),
		&model.Task{
			ID:        taskID,
			CommandID: commandID,
			Status:    model.StatusPausedForReplan,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		})

	queueDir := filepath.Join(maestroDir, "queue")
	// Drop write permissions on the queue directory so AtomicWrite's
	// rename(2) fails with EACCES on the first scan.
	origMode := os.FileMode(0o755)
	if err := os.Chmod(queueDir, 0o555); err != nil {
		t.Fatalf("chmod queue dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(queueDir, origMode) })

	run := newRun(&deps)
	outcome := R10PausedForReplanDeadletter{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected zero repairs when queue write fails, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	st, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got := st.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Errorf("after queue write failure: state task status = %q, want paused_for_replan (so next scan retries)", got)
	}

	// Restore write permission and run R10 again — it must succeed now.
	if err := os.Chmod(queueDir, origMode); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	outcome = R10PausedForReplanDeadletter{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("retry after restore: expected 1 repair, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}
	st, err = run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state after retry: %v", err)
	}
	if got := st.TaskStates[taskID]; got != model.StatusFailed {
		t.Errorf("after successful retry: state task status = %q, want failed", got)
	}
}

// TestR10PausedForReplan_QueueUnmarshalFailure_LeavesStateRetryable pins
// the invariant: r10FindOwningWorker must distinguish "queue entry
// definitively absent" from "transient read/unmarshal error". A
// corrupted queue file must propagate the error so the caller skips
// the task and the next scan retries; otherwise R10 would advance
// state to failed while the queue task stayed at non-terminal,
// blocking the publish gate.
func TestR10PausedForReplan_QueueUnmarshalFailure_LeavesStateRetryable(t *testing.T) {
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	tenMinSec2 := 600
	deps.Config.Verify = model.VerifyDaemonConfig{PausedForReplanDeadletterSec: &tenMinSec2}

	commandID := "cmd_r10_unmarshal_fail"
	taskID := "task_r10_unmarshal_fail"

	updatedAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := updatedAt.Add(5 * time.Hour)
	setClock(&deps, now)

	writeR10Fixture(t, maestroDir, commandID, taskID, updatedAt.Format(time.RFC3339), nil)

	// Corrupt worker1.yaml so the first queue scan fails with an
	// unmarshal error rather than an os.IsNotExist.
	queuePath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	if err := os.WriteFile(queuePath, []byte(":\n  not\nyaml::"), 0o600); err != nil {
		t.Fatalf("write corrupted queue: %v", err)
	}

	run := newRun(&deps)
	outcome := R10PausedForReplanDeadletter{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected zero repairs when queue unmarshal fails, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	st, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got := st.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Errorf("after queue unmarshal failure: state task status = %q, want paused_for_replan (so next scan retries)", got)
	}

	// Restore a valid queue with the task entry — next R10 scan must
	// succeed and escalate cleanly.
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{{
			ID:        taskID,
			CommandID: commandID,
			Status:    model.StatusPausedForReplan,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		}},
	}
	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		t.Fatalf("restore queue: %v", err)
	}
	outcome = R10PausedForReplanDeadletter{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("retry after restore: expected 1 repair, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}
}

func TestR10PausedForReplan_IgnoresNonPausedTasks(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	tenMinSec2 := 600
	deps.Config.Verify = model.VerifyDaemonConfig{PausedForReplanDeadletterSec: &tenMinSec2}

	commandID := "cmd_r10_ignore"
	taskID := "task_r10_ignore"

	updatedAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := updatedAt.Add(10 * time.Hour)
	setClock(&deps, now)

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{taskID: model.StatusVerifyPending},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: updatedAt.Format(time.RFC3339),
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	run := newRun(&deps)
	outcome := R10PausedForReplanDeadletter{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Fatalf("R10 must not touch non-paused_for_replan tasks, got %+v", outcome.Repairs)
	}
}
