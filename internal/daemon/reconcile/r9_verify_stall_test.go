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

// writeR9Fixture writes a state + worker result pair to maestroDir for R9 tests.
// resultCreatedAt is the TaskResult.CreatedAt timestamp; "" means skip the result file.
func writeR9Fixture(t *testing.T, maestroDir, commandID, taskID, workerID, resultCreatedAt string, taskStatus model.Status) {
	t.Helper()

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{taskID: taskStatus},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if resultCreatedAt == "" {
		return
	}
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_worker",
		Results: []model.TaskResult{{
			ID:        "res_0000000001_abcdef01",
			TaskID:    taskID,
			CommandID: commandID,
			Status:    model.StatusVerifyPending,
			CreatedAt: resultCreatedAt,
		}},
	}
	resultPath := filepath.Join(maestroDir, "results", workerID+".yaml")
	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		t.Fatalf("write result: %v", err)
	}
}

func writeR9QueueTask(t *testing.T, maestroDir, workerID string, task model.Task) {
	t.Helper()
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks:         []model.Task{task},
	}
	queuePath := filepath.Join(maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		t.Fatalf("write queue: %v", err)
	}
}

func TestR9VerifyStall_TransitionsToReplanWhenRetryDisabled(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{} // use default 600s

	commandID := "cmd_0000000001_abcdef01"
	taskID := "task_0000000001_abcdef01"
	workerID := "worker1"

	// Result was written 20 minutes ago — stall threshold is 600s (10min) by default.
	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(20 * time.Minute)
	setClock(&deps, now)

	writeR9Fixture(t, maestroDir, commandID, taskID, workerID,
		resultAt.Format(time.RFC3339), model.StatusVerifyPending)

	run := newRun(&deps)
	outcome := R9VerifyStall{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}
	if outcome.Repairs[0].Pattern != PatternR9 || outcome.Repairs[0].TaskID != taskID {
		t.Errorf("unexpected repair: %+v", outcome.Repairs[0])
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	got, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got.TaskStates[taskID] != model.StatusPausedForReplan {
		t.Errorf("expected paused_for_replan, got %s", got.TaskStates[taskID])
	}
	if got.LastReconciledAt == nil {
		t.Error("expected LastReconciledAt to be set")
	}

	signalPath := filepath.Join(maestroDir, "queue", "planner_signals.yaml")
	data, err := os.ReadFile(signalPath)
	if err != nil {
		t.Fatalf("read planner signal queue: %v", err)
	}
	var sq model.PlannerSignalQueue
	if err := yamlv3.Unmarshal(data, &sq); err != nil {
		t.Fatalf("unmarshal planner signal queue: %v", err)
	}
	if len(sq.Signals) != 1 {
		t.Fatalf("planner signals = %d, want 1", len(sq.Signals))
	}
	if sig := sq.Signals[0]; sig.Kind != "paused_for_replan" || sig.CommandID != commandID || sig.PhaseID != "__task_"+taskID {
		t.Fatalf("unexpected planner signal: %+v", sig)
	}
}

func TestR9VerifyStall_SchedulesRepairWhenRetryEnabled(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}
	deps.Config.Retry.TaskExecution = model.TaskRetryConfig{Enabled: true, MaxRetries: 2}

	commandID := "cmd_0000000007_r9repair"
	taskID := "task_0000000007_r9repair"
	workerID := "worker1"

	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(20 * time.Minute)
	setClock(&deps, now)

	writeR9Fixture(t, maestroDir, commandID, taskID, workerID,
		resultAt.Format(time.RFC3339), model.StatusVerifyPending)

	// Make the stalled task a required member of an active phase so the
	// repair-wiring assertions below exercise membership replacement and
	// phase tracking (the bug fixed here: R9 used to register only
	// TaskStates, leaving the predecessor required without lineage).
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	{
		st, err := newRun(&deps).loadState(statePath)
		if err != nil {
			t.Fatalf("read fixture state: %v", err)
		}
		st.RequiredTaskIDs = []string{taskID}
		st.Phases = []model.Phase{{
			PhaseID: "phase_001",
			Name:    "impl",
			Status:  model.PhaseStatusActive,
			TaskIDs: []string{taskID},
		}}
		if err := yamlutil.AtomicWrite(statePath, st); err != nil {
			t.Fatalf("rewrite fixture state: %v", err)
		}
	}

	writeR9QueueTask(t, maestroDir, workerID, model.Task{
		ID:                 taskID,
		CommandID:          commandID,
		Purpose:            "r9 source",
		Content:            "implement source",
		AcceptanceCriteria: "verified",
		ExpectedPaths:      []string{"source.go"},
		Status:             model.StatusCompleted,
		CreatedAt:          "2026-01-01T00:00:00Z",
		UpdatedAt:          "2026-01-01T00:00:00Z",
	})

	run := newRun(&deps)
	outcome := R9VerifyStall{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 R9 repair, got %d", len(outcome.Repairs))
	}

	state, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}

	queuePath := filepath.Join(maestroDir, "queue", workerID+".yaml")
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	var repair *model.Task
	for i := range tq.Tasks {
		if tq.Tasks[i].OriginalTaskID == taskID {
			repair = &tq.Tasks[i]
			break
		}
	}
	if repair == nil {
		t.Fatalf("expected repair task in queue, queue=%+v", tq.Tasks)
	}
	if repair.OperationType != model.OperationTypeRepair {
		t.Errorf("repair OperationType = %q, want %q", repair.OperationType, model.OperationTypeRepair)
	}
	if state.TaskStates[repair.ID] != model.StatusPlanned {
		t.Errorf("repair state = %q, want %q", state.TaskStates[repair.ID], model.StatusPlanned)
	}

	// Full retry wiring (regression: R9 used to register TaskStates only,
	// so the repair's success could not supersede the stalled predecessor
	// and the phase cascaded to Cancelled).
	if got := state.RetryLineage[repair.ID]; got != taskID {
		t.Errorf("RetryLineage[%s] = %q, want %q", repair.ID, got, taskID)
	}
	if len(state.RequiredTaskIDs) != 1 || state.RequiredTaskIDs[0] != repair.ID {
		t.Errorf("RequiredTaskIDs = %v, want [%s] (predecessor replaced)", state.RequiredTaskIDs, repair.ID)
	}
	if len(state.Phases) != 1 {
		t.Fatalf("phases = %d, want 1", len(state.Phases))
	}
	foundInPhase := false
	for _, id := range state.Phases[0].TaskIDs {
		if id == repair.ID {
			foundInPhase = true
		}
	}
	if !foundInPhase {
		t.Errorf("phase TaskIDs = %v, missing repair %s", state.Phases[0].TaskIDs, repair.ID)
	}
}

func TestR9VerifyStall_ExtendsThresholdForMultiCommandVerifySnapshot(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}

	commandID := "cmd_0000000011_multiver"
	taskID := "task_0000000011_multiver"
	workerID := "worker1"

	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(12 * time.Minute)
	setClock(&deps, now)

	writeR9Fixture(t, maestroDir, commandID, taskID, workerID,
		resultAt.Format(time.RFC3339), model.StatusVerifyPending)
	verifyPath := filepath.Join(maestroDir, "state", "verify", commandID+".yaml")
	if err := model.SaveVerifyConfig(verifyPath, &model.VerifyConfig{
		Build:     []string{"go test ./pkg/a"},
		Lint:      []string{"go test ./pkg/b"},
		Typecheck: []string{"go test ./pkg/c"},
	}); err != nil {
		t.Fatalf("write verify snapshot: %v", err)
	}

	outcome := R9VerifyStall{}.Apply(newRun(&deps))
	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected no repairs while multi-command verify can still be running, got %+v", outcome.Repairs)
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	got, err := newRun(&deps).loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got.TaskStates[taskID] != model.StatusVerifyPending {
		t.Fatalf("task state = %q, want verify_pending", got.TaskStates[taskID])
	}
}

func TestR9VerifyStall_SourceMissingPausesForReplanWhenRetryEnabled(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}
	deps.Config.Retry.TaskExecution = model.TaskRetryConfig{Enabled: true, MaxRetries: 2}

	commandID := "cmd_0000000008_r9nosrc1"
	taskID := "task_0000000008_r9nosrc1"
	workerID := "worker1"

	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(20 * time.Minute)
	setClock(&deps, now)

	writeR9Fixture(t, maestroDir, commandID, taskID, workerID,
		resultAt.Format(time.RFC3339), model.StatusVerifyPending)

	run := newRun(&deps)
	outcome := R9VerifyStall{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 R9 repair, got %d", len(outcome.Repairs))
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	state, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got := state.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Fatalf("task state = %q, want %q when source queue task is missing", got, model.StatusPausedForReplan)
	}
}

func TestR9VerifyStall_MaxRetriesPausesForReplanWhenRetryEnabled(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}
	deps.Config.Retry.TaskExecution = model.TaskRetryConfig{Enabled: true, MaxRetries: 1}

	commandID := "cmd_0000000009_r9maxret"
	taskID := "task_0000000009_r9maxret"
	workerID := "worker1"

	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(20 * time.Minute)
	setClock(&deps, now)

	writeR9Fixture(t, maestroDir, commandID, taskID, workerID,
		resultAt.Format(time.RFC3339), model.StatusVerifyPending)
	writeR9QueueTask(t, maestroDir, workerID, model.Task{
		ID:               taskID,
		CommandID:        commandID,
		Purpose:          "r9 exhausted source",
		Content:          "implement source",
		Status:           model.StatusCompleted,
		ExecutionRetries: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
		UpdatedAt:        "2026-01-01T00:00:00Z",
	})

	run := newRun(&deps)
	outcome := R9VerifyStall{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 R9 repair, got %d", len(outcome.Repairs))
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	state, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got := state.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Fatalf("task state = %q, want %q when repair budget is exhausted", got, model.StatusPausedForReplan)
	}
}

func TestR9VerifyStall_DoesNotTransitionWithinThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}

	commandID := "cmd_0000000002_abcdef02"
	taskID := "task_0000000002_abcdef02"

	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(2 * time.Minute) // well below 10-minute default
	setClock(&deps, now)

	writeR9Fixture(t, maestroDir, commandID, taskID, "worker1",
		resultAt.Format(time.RFC3339), model.StatusVerifyPending)

	run := newRun(&deps)
	outcome := R9VerifyStall{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected no repairs, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	got, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got.TaskStates[taskID] != model.StatusVerifyPending {
		t.Errorf("expected verify_pending, got %s", got.TaskStates[taskID])
	}
}

func TestR9VerifyStall_IgnoresNonVerifyPending(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}

	commandID := "cmd_0000000003_abcdef03"
	taskID := "task_0000000003_abcdef03"

	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(1 * time.Hour)
	setClock(&deps, now)

	// Task is in_progress, not verify_pending — must be ignored.
	writeR9Fixture(t, maestroDir, commandID, taskID, "worker1",
		resultAt.Format(time.RFC3339), model.StatusInProgress)

	run := newRun(&deps)
	outcome := R9VerifyStall{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR9VerifyStall_DisabledByZeroThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	zero := 0
	deps.Config.Verify = model.VerifyDaemonConfig{StallThresholdSec: &zero}

	commandID := "cmd_0000000004_abcdef04"
	taskID := "task_0000000004_abcdef04"

	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(24 * time.Hour)
	setClock(&deps, now)

	writeR9Fixture(t, maestroDir, commandID, taskID, "worker1",
		resultAt.Format(time.RFC3339), model.StatusVerifyPending)

	run := newRun(&deps)
	outcome := R9VerifyStall{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Fatalf("R9 should be a no-op when threshold=0, got %d repairs", len(outcome.Repairs))
	}
}

func TestR9VerifyStall_FallsBackToStateUpdatedAt(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}

	commandID := "cmd_0000000005_abcdef05"
	taskID := "task_0000000005_abcdef05"

	// Manually write state with no matching result file. UpdatedAt is 30 min in the past.
	stateUpdated := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := stateUpdated.Add(30 * time.Minute)
	setClock(&deps, now)

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{taskID: model.StatusVerifyPending},
		},
		CreatedAt: stateUpdated.Format(time.RFC3339),
		UpdatedAt: stateUpdated.Format(time.RFC3339),
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	run := newRun(&deps)
	outcome := R9VerifyStall{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected fallback to UpdatedAt to trigger transition, got %d repairs", len(outcome.Repairs))
	}

	got, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got.TaskStates[taskID] != model.StatusPausedForReplan {
		t.Errorf("expected paused_for_replan fallback, got %s", got.TaskStates[taskID])
	}
}

func TestR9VerifyStall_UsesLatestResultPerTask(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}

	commandID := "cmd_0000000006_abcdef06"
	taskID := "task_0000000006_abcdef06"

	older := time.Date(2026, 4, 25, 8, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 4, 25, 10, 30, 0, 0, time.UTC)
	now := newer.Add(2 * time.Minute) // 2 min after newer — within threshold
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
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_worker",
		Results: []model.TaskResult{
			{ID: "res_old", TaskID: taskID, CommandID: commandID, Status: model.StatusVerifyPending, CreatedAt: older.Format(time.RFC3339)},
			{ID: "res_new", TaskID: taskID, CommandID: commandID, Status: model.StatusVerifyPending, CreatedAt: newer.Format(time.RFC3339)},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		t.Fatalf("write result: %v", err)
	}

	run := newRun(&deps)
	outcome := R9VerifyStall{}.Apply(run)

	// Newer result is within threshold → no transition, even though older is past it.
	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected newest result to gate stall detection, got %d repairs (%+v)", len(outcome.Repairs), outcome.Repairs)
	}
}
