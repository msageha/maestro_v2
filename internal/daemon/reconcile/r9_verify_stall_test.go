package reconcile

import (
	"path/filepath"
	"testing"
	"time"

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

func TestR9VerifyStall_TransitionsAfterThreshold(t *testing.T) {
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
	if got.TaskStates[taskID] != model.StatusRepairPending {
		t.Errorf("expected repair_pending, got %s", got.TaskStates[taskID])
	}
	if got.LastReconciledAt == nil {
		t.Error("expected LastReconciledAt to be set")
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
	if got.TaskStates[taskID] != model.StatusRepairPending {
		t.Errorf("expected repair_pending fallback, got %s", got.TaskStates[taskID])
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
