package reconcile

// Regression tests for the D-F1〜D-F9 reconcile recovery findings:
// notification-loss recovery (guard rollback / durable signal), R4 retryable
// CanComplete deferral, R0 teardown visibility + compensation, R9 orphaned
// repair_pending reclaim, R10 per-task stale anchor, and the R1 upsert /
// stale-result fences.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
	"github.com/msageha/maestro_v2/internal/testutil/mocks"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func failingExecutorFactory(deps *Deps) {
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Error: fmt.Errorf("tmux pane unavailable")}}, nil
	}
}

func readPlannerSignalQueue(t *testing.T, maestroDir string) model.PlannerSignalQueue {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(maestroDir, "queue", "planner_signals.yaml"))
	if err != nil {
		t.Fatalf("read planner signal queue: %v", err)
	}
	var sq model.PlannerSignalQueue
	if err := yamlv3.Unmarshal(data, &sq); err != nil {
		t.Fatalf("unmarshal planner signal queue: %v", err)
	}
	return sq
}

// --- D-F1: R8 guard rollback on delivery failure ---

func TestR8PublishFailed_DeliveryFailure_RollsBackGuardAndReemits(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	failingExecutorFactory(&deps)

	commandID := "cmd_0000000001_r8roll01"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusQuarantined, nil)
	state.Integration.PublishFailureCount = 5
	state.Integration.QuarantineReason = "publish: push to base failed (failure_count=5)"
	writeWorktreeState(t, maestroDir, commandID, state)

	engine := NewEngine(deps, R8PublishFailed{})
	_, notifications := engine.Reconcile()
	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifications))
	}

	statePath := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	run := newRun(&deps)
	persisted, err := run.loadWorktreeState(statePath)
	if err != nil {
		t.Fatalf("reload worktree state: %v", err)
	}
	if !persisted.Integration.StallSignaled {
		t.Fatal("StallSignaled should be persisted optimistically before delivery")
	}

	failed := engine.ExecuteDeferredNotifications(notifications)
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed notification, got %d", len(failed))
	}

	rolledBack, err := run.loadWorktreeState(statePath)
	if err != nil {
		t.Fatalf("reload worktree state after rollback: %v", err)
	}
	if rolledBack.Integration.StallSignaled {
		t.Fatal("StallSignaled should be rolled back after delivery failure")
	}

	// Next scan must re-emit the escalation.
	_, reemitted := engine.Reconcile()
	if len(reemitted) != 1 || reemitted[0].Kind != NotifyPublishQuarantined {
		t.Fatalf("expected re-emitted publish_quarantined notification, got %+v", reemitted)
	}
}

// --- D-F1: R7 escalation guard rollback on delivery failure ---

func TestR7MergeConflict_EscalationDeliveryFailure_RollsBackGuardAndReemits(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	failingExecutorFactory(&deps)

	commandID := "cmd_0000000001_r7roll01"
	workerID := "worker1"
	worker := newWorkerState(commandID, workerID, model.WorktreeStatusConflict, maxConflictResolutionAttempts)
	state := newWorktreeCommandState(commandID, model.IntegrationStatusConflict, []model.WorktreeState{worker})
	writeWorktreeState(t, maestroDir, commandID, state)

	engine := NewEngine(deps, R7MergeConflict{})
	_, notifications := engine.Reconcile()
	if len(notifications) != 1 || notifications[0].Kind != NotifyConflictEscalation {
		t.Fatalf("expected 1 conflict_escalation notification, got %+v", notifications)
	}

	statePath := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	run := newRun(&deps)
	persisted, err := run.loadWorktreeState(statePath)
	if err != nil {
		t.Fatalf("reload worktree state: %v", err)
	}
	if !persisted.Workers[0].ConflictEscalated {
		t.Fatal("ConflictEscalated should be persisted optimistically before delivery")
	}

	failed := engine.ExecuteDeferredNotifications(notifications)
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed notification, got %d", len(failed))
	}

	rolledBack, err := run.loadWorktreeState(statePath)
	if err != nil {
		t.Fatalf("reload worktree state after rollback: %v", err)
	}
	if rolledBack.Workers[0].ConflictEscalated {
		t.Fatal("ConflictEscalated should be rolled back after delivery failure")
	}

	_, reemitted := engine.Reconcile()
	if len(reemitted) != 1 || reemitted[0].Kind != NotifyConflictEscalation {
		t.Fatalf("expected re-emitted conflict_escalation notification, got %+v", reemitted)
	}
}

// --- D-F5: R6 delivery failure hands over to the durable signal queue ---

func TestR6FillTimeout_DeliveryFailure_QueuesDurableSignal(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	failingExecutorFactory(&deps)
	now := time.Now().UTC()
	setClock(&deps, now)

	commandID := "cmd_0000000001_r6sig001"
	pastDeadline := now.Add(-1 * time.Hour).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  commandID,
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{PhaseID: "p1", Name: "implementation", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			},
		},
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339),
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", commandID+".yaml"), state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	engine := NewEngine(deps, R6FillTimeout{})
	_, notifications := engine.Reconcile()
	if len(notifications) != 1 || notifications[0].Kind != NotifyFillTimeout {
		t.Fatalf("expected 1 fill_timeout notification, got %+v", notifications)
	}

	failed := engine.ExecuteDeferredNotifications(notifications)
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed notification, got %d", len(failed))
	}

	sq := readPlannerSignalQueue(t, maestroDir)
	if len(sq.Signals) != 1 {
		t.Fatalf("planner signals = %d, want 1", len(sq.Signals))
	}
	sig := sq.Signals[0]
	if sig.Kind != "fill_timeout" || sig.CommandID != commandID || sig.PhaseID != "p1" || sig.PhaseName != "implementation" {
		t.Fatalf("unexpected planner signal: %+v", sig)
	}

	// A second failed delivery must not duplicate the signal (canonical dedup key).
	engine.ExecuteDeferredNotifications(notifications)
	sq = readPlannerSignalQueue(t, maestroDir)
	if len(sq.Signals) != 1 {
		t.Fatalf("planner signals after repeat failure = %d, want 1 (dedup)", len(sq.Signals))
	}
}

// --- D-F2: R4 retryable CanComplete failure defers instead of quarantining ---

func writeR4TerminalPlannerResult(t *testing.T, maestroDir, commandID, resultID string) {
	t.Helper()
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: resultID, CommandID: commandID, Status: model.StatusCompleted, CreatedAt: time.Now().UTC().Format(time.RFC3339)},
		},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf); err != nil {
		t.Fatalf("write planner result: %v", err)
	}
}

func TestR4PlanStatus_RetryableFillingPhase_NoQuarantine(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.CanComplete = func(*model.CommandState) (model.PlanStatus, error) {
		return "", fmt.Errorf("phase \"impl\" is in transient status filling, retry later")
	}

	commandID := "cmd_0000000001_r4retry1"
	resultID := "res_0000000001_r4retry1"
	writeR4TerminalPlannerResult(t, maestroDir, commandID, resultID)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  commandID,
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{PhaseID: "p1", Name: "impl", Status: model.PhaseStatusFilling, TaskIDs: []string{"task_a"}},
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", commandID+".yaml"), state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	outcome := NewR4PlanStatus(nil).Apply(newRun(&deps))
	if len(outcome.Repairs) != 0 || len(outcome.Notifications) != 0 {
		t.Fatalf("expected no repairs/notifications for retryable failure, got %+v / %+v", outcome.Repairs, outcome.Notifications)
	}

	// The planner result must NOT be quarantined.
	run := newRun(&deps)
	rf, err := run.loadCommandResultFile(filepath.Join(maestroDir, "results", "planner.yaml"))
	if err != nil {
		t.Fatalf("reload planner results: %v", err)
	}
	if len(rf.Results) != 1 || rf.Results[0].ID != resultID {
		t.Fatalf("planner result should be preserved, got %+v", rf.Results)
	}
}

func TestR4PlanStatus_RetryableUnresolvedCandidateGroup_NoQuarantine(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.CanComplete = func(*model.CommandState) (model.PlanStatus, error) {
		return "", fmt.Errorf("A/B candidate group g1 is racing (selection in progress)")
	}

	commandID := "cmd_0000000001_r4retry2"
	resultID := "res_0000000001_r4retry2"
	writeR4TerminalPlannerResult(t, maestroDir, commandID, resultID)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  commandID,
		PlanStatus: model.PlanStatusSealed,
		CandidateGroups: map[string]*model.CandidateGroup{
			"g1": {Status: model.ABGroupRacing},
		},
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", commandID+".yaml"), state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	outcome := NewR4PlanStatus(nil).Apply(newRun(&deps))
	if len(outcome.Repairs) != 0 || len(outcome.Notifications) != 0 {
		t.Fatalf("expected no repairs/notifications for retryable failure, got %+v / %+v", outcome.Repairs, outcome.Notifications)
	}

	run := newRun(&deps)
	rf, err := run.loadCommandResultFile(filepath.Join(maestroDir, "results", "planner.yaml"))
	if err != nil {
		t.Fatalf("reload planner results: %v", err)
	}
	if len(rf.Results) != 1 {
		t.Fatalf("planner result should be preserved, got %+v", rf.Results)
	}
}

// --- D-F3: R0 teardown writes a synthetic failed planner result ---

func writeR0StuckFixture(t *testing.T, maestroDir, commandID string, createdAt string, extraTasks ...model.Task) {
	t.Helper()
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  commandID,
		PlanStatus: model.PlanStatusPlanning,
		CreatedAt:  createdAt, UpdatedAt: createdAt,
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", commandID+".yaml"), state); err != nil {
		t.Fatalf("write state: %v", err)
	}
	cq := model.CommandQueue{
		SchemaVersion: 1, FileType: "queue_command",
		Commands: []model.Command{
			{ID: commandID, Content: "stuck", Status: model.StatusInProgress, CreatedAt: createdAt, UpdatedAt: createdAt},
		},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq); err != nil {
		t.Fatalf("write planner queue: %v", err)
	}
	if len(extraTasks) > 0 {
		tq := model.TaskQueue{SchemaVersion: 1, FileType: "queue_task", Tasks: extraTasks}
		if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq); err != nil {
			t.Fatalf("write worker queue: %v", err)
		}
	}
}

func TestR0PlanningStuck_WritesSyntheticFailedPlannerResult(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	commandID := "cmd_0000000001_r0synth1"
	writeR0StuckFixture(t, maestroDir, commandID, now.Add(-20*time.Minute).Format(time.RFC3339))

	run := newRun(&deps)
	outcome := R0PlanningStuck{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}

	rf, err := run.loadCommandResultFile(filepath.Join(maestroDir, "results", "planner.yaml"))
	if err != nil {
		t.Fatalf("load planner results: %v", err)
	}
	if len(rf.Results) != 1 {
		t.Fatalf("expected 1 synthetic planner result, got %d", len(rf.Results))
	}
	res := rf.Results[0]
	if res.CommandID != commandID || res.Status != model.StatusFailed {
		t.Fatalf("unexpected synthetic result: %+v", res)
	}
	if res.Summary == "" {
		t.Error("synthetic result summary should describe the R0 teardown")
	}
}

func TestR0PlanningStuck_PlannerBusy_DefersTeardown(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)
	// Planner pane probes as busy; age (20m) is below the default 60m cap.
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	}

	commandID := "cmd_0000000001_r0busy01"
	writeR0StuckFixture(t, maestroDir, commandID, now.Add(-20*time.Minute).Format(time.RFC3339))

	run := newRun(&deps)
	outcome := R0PlanningStuck{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected 0 repairs while planner busy, got %d", len(outcome.Repairs))
	}
	if _, err := os.Stat(filepath.Join(maestroDir, "state", "commands", commandID+".yaml")); err != nil {
		t.Error("state file should be preserved while the teardown is deferred")
	}
}

func TestR0PlanningStuck_PlannerBusy_HardCapForcesTeardown(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	}

	commandID := "cmd_0000000001_r0busy02"
	// 90m old > default 60m hard cap → teardown proceeds despite busy.
	writeR0StuckFixture(t, maestroDir, commandID, now.Add(-90*time.Minute).Format(time.RFC3339))

	run := newRun(&deps)
	outcome := R0PlanningStuck{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair past the hard cap, got %d", len(outcome.Repairs))
	}
}

// --- D-F4: R0 vetoes on active worker rows and restores what it removed ---

func TestR0PlanningStuck_ActiveTaskVeto_RestoresQueueRows(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	commandID := "cmd_0000000001_r0veto01"
	oldTime := now.Add(-20 * time.Minute).Format(time.RFC3339)
	writeR0StuckFixture(t, maestroDir, commandID, oldTime,
		model.Task{ID: "task_pending", CommandID: commandID, Status: model.StatusPending, CreatedAt: oldTime, UpdatedAt: oldTime},
		model.Task{ID: "task_live", CommandID: commandID, Status: model.StatusInProgress, CreatedAt: oldTime, UpdatedAt: oldTime},
	)

	run := newRun(&deps)
	outcome := R0PlanningStuck{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected 0 repairs (active row vetoes teardown), got %d", len(outcome.Repairs))
	}

	// State must be preserved.
	if _, err := os.Stat(filepath.Join(maestroDir, "state", "commands", commandID+".yaml")); err != nil {
		t.Error("state file should be preserved on veto")
	}

	// The planner queue row must be restored.
	data, err := os.ReadFile(filepath.Join(maestroDir, "queue", "planner.yaml"))
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		t.Fatalf("parse planner queue: %v", err)
	}
	if len(cq.Commands) != 1 || cq.Commands[0].ID != commandID {
		t.Fatalf("planner queue row should be restored, got %+v", cq.Commands)
	}

	// Both worker rows must survive: the live row untouched, the pending
	// sibling restored by compensation.
	wdata, err := os.ReadFile(filepath.Join(maestroDir, "queue", "worker1.yaml"))
	if err != nil {
		t.Fatalf("read worker queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(wdata, &tq); err != nil {
		t.Fatalf("parse worker queue: %v", err)
	}
	got := map[string]bool{}
	for _, task := range tq.Tasks {
		got[task.ID] = true
	}
	if len(tq.Tasks) != 2 || !got["task_pending"] || !got["task_live"] {
		t.Fatalf("worker queue rows should be preserved/restored, got %+v", tq.Tasks)
	}
}

// --- D-F6: R9 reclaims orphaned repair_pending entries ---

func TestR9VerifyStall_ReclaimsOrphanedRepairPending(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}

	commandID := "cmd_0000000001_r9orpha1"
	taskID := "task_0000000001_r9orpha1"
	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(30 * time.Minute)
	setClock(&deps, now)

	// repair_pending with no lineage successor and no RetryEnqueueFailed
	// marker: the crash-between-transition-and-registration artifact.
	writeR9Fixture(t, maestroDir, commandID, taskID, "worker1",
		resultAt.Format(time.RFC3339), model.StatusRepairPending)

	run := newRun(&deps)
	outcome := R9VerifyStall{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}
	if outcome.Repairs[0].TaskID != taskID || outcome.Repairs[0].Pattern != PatternR9 {
		t.Fatalf("unexpected repair: %+v", outcome.Repairs[0])
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	state, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got := state.TaskStates[taskID]; got != model.StatusPausedForReplan {
		t.Fatalf("task state = %q, want paused_for_replan", got)
	}

	sq := readPlannerSignalQueue(t, maestroDir)
	if len(sq.Signals) != 1 || sq.Signals[0].Kind != "paused_for_replan" || sq.Signals[0].Reason != "repair_pending_orphaned" {
		t.Fatalf("unexpected planner signals: %+v", sq.Signals)
	}
}

func TestR9VerifyStall_RepairPendingWithSuccessor_NotReclaimed(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}

	commandID := "cmd_0000000001_r9owned1"
	taskID := "task_0000000001_r9owned1"
	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(30 * time.Minute)
	setClock(&deps, now)

	writeR9Fixture(t, maestroDir, commandID, taskID, "worker1",
		resultAt.Format(time.RFC3339), model.StatusRepairPending)

	// Register a lineage successor: the repair pipeline owns the slot.
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	run := newRun(&deps)
	state, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	state.RetryLineage = map[string]string{"task_0000000002_repair01": taskID}
	state.TaskStates["task_0000000002_repair01"] = model.StatusPlanned
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	outcome := R9VerifyStall{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected 0 repairs for owned repair_pending, got %+v", outcome.Repairs)
	}
	reloaded, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got := reloaded.TaskStates[taskID]; got != model.StatusRepairPending {
		t.Fatalf("task state = %q, want repair_pending (untouched)", got)
	}
}

func TestR9VerifyStall_FreshRepairPending_NotReclaimedWithinGrace(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}

	commandID := "cmd_0000000001_r9grace1"
	taskID := "task_0000000001_r9grace1"
	resultAt := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	now := resultAt.Add(30 * time.Minute)
	setClock(&deps, now)

	writeR9Fixture(t, maestroDir, commandID, taskID, "worker1",
		resultAt.Format(time.RFC3339), model.StatusRepairPending)

	// A state write within the quiescence grace means a registration may be
	// in flight — the sweep must wait.
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	run := newRun(&deps)
	state, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	state.UpdatedAt = now.Add(-10 * time.Second).Format(time.RFC3339)
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	outcome := R9VerifyStall{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected 0 repairs within quiescence grace, got %+v", outcome.Repairs)
	}
}

// --- D-F7: R10 anchors staleness per task, not on command UpdatedAt ---

func TestR10PausedForReplan_FreshStateUpdatedAt_EscalatesViaResultAnchor(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.Config.Verify = model.VerifyDaemonConfig{}

	commandID := "cmd_0000000001_r10anch1"
	taskID := "task_0000000001_r10anch1"
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	setClock(&deps, now)

	// state.UpdatedAt is fresh (a sibling task's reconcile just bumped it) —
	// the old command-level anchor deferred escalation indefinitely.
	writeR10Fixture(t, maestroDir, commandID, taskID, now.Add(-1*time.Minute).Format(time.RFC3339), nil)

	// The task's own result is 2h old — past the default 1h deadletter window.
	rf := model.TaskResultFile{
		SchemaVersion: 1, FileType: "result_worker",
		Results: []model.TaskResult{{
			ID: "res_0000000001_r10anch1", TaskID: taskID, CommandID: commandID,
			Status: model.StatusFailed, CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf); err != nil {
		t.Fatalf("write result: %v", err)
	}

	run := newRun(&deps)
	outcome := R10PausedForReplanDeadletter{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair via per-task anchor, got %d (%+v)", len(outcome.Repairs), outcome.Repairs)
	}

	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	state, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got := state.TaskStates[taskID]; got != model.StatusFailed {
		t.Fatalf("task state = %q, want failed", got)
	}
}

// --- D-F8: r1AddTaskToQueue is an upsert ---

func TestR1AddTaskToQueue_Upsert_NoDuplicateRow(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)

	task := model.Task{ID: "task_0000000001_upsert01", CommandID: "cmd_1", Status: model.StatusPending}
	if err := r1AddTaskToQueue(run, "worker1", &task); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := r1AddTaskToQueue(run, "worker1", &task); err != nil {
		t.Fatalf("second add (upsert) should succeed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(maestroDir, "queue", "worker1.yaml"))
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	if len(tq.Tasks) != 1 {
		t.Fatalf("expected 1 row after double add, got %d", len(tq.Tasks))
	}
}

// --- D-F9: R1 fences stale results against re-dispatched rows ---

func TestR1ResultQueue_StaleResult_DoesNotClobberRedispatchedTask(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	taskID := "task_0000000001_fence001"
	// Terminal result from a previous attempt, 10 minutes old.
	rf := model.TaskResultFile{
		SchemaVersion: 1, FileType: "result_worker",
		Results: []model.TaskResult{{
			ID: "res_0000000001_fence001", TaskID: taskID, CommandID: "cmd_1",
			Status: model.StatusFailed, CreatedAt: now.Add(-10 * time.Minute).Format(time.RFC3339),
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf); err != nil {
		t.Fatalf("write result: %v", err)
	}
	// The row was re-dispatched AFTER that result was written.
	inProgressAt := now.Add(-1 * time.Minute).Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1, FileType: "queue_task",
		Tasks: []model.Task{{
			ID: taskID, CommandID: "cmd_1", Status: model.StatusInProgress,
			InProgressAt: &inProgressAt,
			CreatedAt:    now.Add(-30 * time.Minute).Format(time.RFC3339),
			UpdatedAt:    inProgressAt,
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	outcome := R1ResultQueue{}.Apply(newRun(&deps))
	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected 0 repairs (stale result fenced), got %+v", outcome.Repairs)
	}

	data, err := os.ReadFile(filepath.Join(maestroDir, "queue", "worker1.yaml"))
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var got model.TaskQueue
	if err := yamlv3.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	if got.Tasks[0].Status != model.StatusInProgress {
		t.Fatalf("queue status = %s, want in_progress (live attempt preserved)", got.Tasks[0].Status)
	}
}

func TestR1ResultQueue_FreshResult_RepairsCrashArtifact(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	taskID := "task_0000000001_fence002"
	// Result written AFTER the dispatch: the normal crash-recovery case.
	rf := model.TaskResultFile{
		SchemaVersion: 1, FileType: "result_worker",
		Results: []model.TaskResult{{
			ID: "res_0000000001_fence002", TaskID: taskID, CommandID: "cmd_1",
			Status: model.StatusCompleted, CreatedAt: now.Add(-1 * time.Minute).Format(time.RFC3339),
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf); err != nil {
		t.Fatalf("write result: %v", err)
	}
	inProgressAt := now.Add(-10 * time.Minute).Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1, FileType: "queue_task",
		Tasks: []model.Task{{
			ID: taskID, CommandID: "cmd_1", Status: model.StatusInProgress,
			InProgressAt: &inProgressAt,
			CreatedAt:    now.Add(-30 * time.Minute).Format(time.RFC3339),
			UpdatedAt:    inProgressAt,
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	outcome := R1ResultQueue{}.Apply(newRun(&deps))
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair for post-dispatch result, got %+v", outcome.Repairs)
	}

	data, err := os.ReadFile(filepath.Join(maestroDir, "queue", "worker1.yaml"))
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var got model.TaskQueue
	if err := yamlv3.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	if got.Tasks[0].Status != model.StatusCompleted {
		t.Fatalf("queue status = %s, want completed", got.Tasks[0].Status)
	}
}
