package reconcile

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
	"github.com/msageha/maestro_v2/internal/testutil/mocks"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// --- Test Helpers ---
// TODO(DRY): fakeClock, setupTestDir, and newTestDeps are common test patterns
// shared across reconcile and daemon test files. Consider consolidating into
// a shared test helper package when the reconcile test suite stabilizes.

type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time { return f.now }

type mockResultNotifier struct {
	calls []struct {
		resultID, commandID string
		status              model.Status
	}
	err error
}

func (m *mockResultNotifier) WriteNotificationToOrchestratorQueue(resultID, commandID string, status model.Status) error {
	m.calls = append(m.calls, struct {
		resultID, commandID string
		status              model.Status
	}{resultID, commandID, status})
	return m.err
}

func setupTestDir(t *testing.T) string {
	t.Helper()
	return testutil.SetupDir(t)
}

func newTestDeps(t *testing.T, maestroDir string) Deps {
	t.Helper()
	logger := log.New(&bytes.Buffer{}, "", 0)
	return Deps{
		MaestroDir: maestroDir,
		Config:     model.Config{Watcher: model.WatcherConfig{DispatchLeaseSec: 60}},
		LockMap:    lock.NewMutexMap(),
		DL:         core.NewDaemonLoggerFromLegacy("test", logger, core.LogLevelDebug),
		Clock:      &fakeClock{now: time.Now().UTC()},
	}
}

func setClock(deps *Deps, t time.Time) {
	deps.Clock = &fakeClock{now: t}
}

// --- Run helper tests ---

func Test_extractWorkerID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"worker1.yaml", "worker1"},
		{"worker10.yaml", "worker10"},
		{"planner.yaml", ""},
		{"orchestrator.yaml", ""},
		{"worker1.txt", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := extractWorkerID(tt.input)
		if got != tt.want {
			t.Errorf("extractWorkerID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func Test_removeFromSlice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		s      []string
		target string
		want   int
	}{
		{[]string{"a", "b", "c"}, "b", 2},
		{[]string{"a", "b", "c"}, "d", 3},
		{[]string{"a", "a", "a"}, "a", 0},
		{nil, "x", 0},
	}
	for _, tt := range tests {
		got := removeFromSlice(tt.s, tt.target)
		if len(got) != tt.want {
			t.Errorf("removeFromSlice(%v, %q): len=%d, want %d", tt.s, tt.target, len(got), tt.want)
		}
	}
}

func TestStuckThresholdSec(t *testing.T) {
	t.Parallel()
	deps := Deps{Config: model.Config{Watcher: model.WatcherConfig{DispatchLeaseSec: 100}}}
	run := newRun(&deps)
	if got := run.stuckThresholdSec(); got != 200 {
		t.Errorf("got %d, want 200", got)
	}

	// Zero defaults to 300
	deps.Config.Watcher.DispatchLeaseSec = 0
	run2 := newRun(&deps)
	if got := run2.stuckThresholdSec(); got != 600 {
		t.Errorf("got %d, want 600 (300*2)", got)
	}
}

func TestCachedReadDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(""), 0644)

	deps := Deps{}
	run := newRun(&deps)

	entries1, err := run.cachedReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries1) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries1))
	}

	// Create another file — cached result should still return 1
	os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(""), 0644)
	entries2, err := run.cachedReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries2) != 1 {
		t.Errorf("cached dir should still return 1 entry, got %d", len(entries2))
	}
}

func TestCachedReadDir_NonExistent(t *testing.T) {
	t.Parallel()
	deps := Deps{}
	run := newRun(&deps)
	_, err := run.cachedReadDir("/nonexistent/path")
	if err == nil {
		t.Error("expected error for non-existent directory")
	}
}

// --- Engine tests ---

func TestEngine_Reconcile_EmptyPatterns(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, setupTestDir(t))
	engine := NewEngine(deps)
	repairs, notifications := engine.Reconcile()
	if len(repairs) != 0 || len(notifications) != 0 {
		t.Errorf("expected empty results, got repairs=%d notifications=%d", len(repairs), len(notifications))
	}
}

type fakePattern struct {
	outcome Outcome
	called  bool
}

func (f *fakePattern) Apply(*Run) Outcome {
	if f.called {
		return Outcome{} // converge: no repairs on subsequent passes
	}
	f.called = true
	return f.outcome
}

func TestEngine_Reconcile_AggregatesPatterns(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, setupTestDir(t))
	p1 := &fakePattern{outcome: Outcome{
		Repairs: []Repair{{Pattern: "P1", Detail: "repair1"}},
	}}
	p2 := &fakePattern{outcome: Outcome{
		Repairs:       []Repair{{Pattern: "P2", Detail: "repair2"}},
		Notifications: []DeferredNotification{{Kind: NotifyReFill, CommandID: "cmd1"}},
	}}

	engine := NewEngine(deps, p1, p2)
	repairs, notifications := engine.Reconcile()
	if len(repairs) != 2 {
		t.Errorf("expected 2 repairs, got %d", len(repairs))
	}
	if len(notifications) != 1 {
		t.Errorf("expected 1 notification, got %d", len(notifications))
	}
}

func TestEngine_ExecuteDeferredNotifications_NilFactory(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, setupTestDir(t))
	engine := NewEngine(deps)
	// Should not panic
	engine.ExecuteDeferredNotifications([]DeferredNotification{
		{Kind: NotifyReFill, CommandID: "cmd1"},
	})
}

func TestEngine_ExecuteDeferredNotifications_AllKinds(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, setupTestDir(t))
	exec := &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return exec, nil
	}
	engine := NewEngine(deps)
	engine.ExecuteDeferredNotifications([]DeferredNotification{
		{Kind: NotifyReFill, CommandID: "cmd1"},
		{Kind: NotifyReEvaluate, CommandID: "cmd2", Reason: "tasks not done"},
		{Kind: NotifyFillTimeout, CommandID: "cmd3", TimedOutPhases: map[string]bool{"p1": true}},
		{Kind: "unknown_kind", CommandID: "cmd4"},
	})
	if len(exec.Calls) != 3 {
		t.Errorf("expected 3 executor calls (unknown skipped), got %d", len(exec.Calls))
	}
}

func TestEngine_ExecuteDeferredNotifications_FactoryError(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, setupTestDir(t))
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return nil, fmt.Errorf("factory error")
	}
	engine := NewEngine(deps)
	// Should not panic
	engine.ExecuteDeferredNotifications([]DeferredNotification{
		{Kind: NotifyReFill, CommandID: "cmd1"},
	})
}

func TestEngine_SetCanComplete(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, setupTestDir(t))
	engine := NewEngine(deps)
	if engine.deps.CanComplete != nil {
		t.Error("expected nil CanComplete initially")
	}
	engine.SetCanComplete(func(*model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCompleted, nil
	})
	if engine.deps.CanComplete == nil {
		t.Error("expected CanComplete to be set")
	}
}

// --- R0-dispatch tests ---

func TestR0Dispatch_NoQueueFile(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)
	outcome := R0Dispatch{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR0Dispatch_StuckCommand(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	oldTime := now.Add(-20 * time.Minute).Format(time.RFC3339)
	owner := "planner"
	expiresAt := now.Add(-15 * time.Minute).Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:             "cmd_dispatch_stuck",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiresAt,
				UpdatedAt:      oldTime,
				CreatedAt:      oldTime,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)
	// No state file — dispatch never created it

	run := newRun(&deps)
	outcome := R0Dispatch{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != PatternR0Dispatch {
		t.Errorf("pattern: got %s", outcome.Repairs[0].Pattern)
	}

	// Verify queue updated
	data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "planner.yaml"))
	var updated model.CommandQueue
	yamlv3.Unmarshal(data, &updated)
	if updated.Commands[0].Status != model.StatusPending {
		t.Errorf("status: got %s, want pending", updated.Commands[0].Status)
	}
	if updated.Commands[0].LeaseOwner != nil {
		t.Error("lease_owner should be nil")
	}
	if updated.Commands[0].LastError == nil {
		t.Error("last_error should be set")
	}
}

func TestR0Dispatch_StateFileExists_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	oldTime := now.Add(-20 * time.Minute).Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd_has_state", Status: model.StatusInProgress, UpdatedAt: oldTime, CreatedAt: oldTime},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	// State file exists → not stuck in dispatch
	state := model.CommandState{CommandID: "cmd_has_state", CreatedAt: oldTime, UpdatedAt: oldTime}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_has_state.yaml"), state)

	run := newRun(&deps)
	outcome := R0Dispatch{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when state file exists, got %d", len(outcome.Repairs))
	}
}

func TestR0Dispatch_RecentCommand_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	recentTime := now.Add(-1 * time.Minute).Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd_recent", Status: model.StatusInProgress, UpdatedAt: recentTime, CreatedAt: recentTime},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	run := newRun(&deps)
	outcome := R0Dispatch{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for recent command, got %d", len(outcome.Repairs))
	}
}

func TestR0Dispatch_PendingStatus_Ignored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	oldTime := now.Add(-20 * time.Minute).Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd_pending", Status: model.StatusPending, UpdatedAt: oldTime, CreatedAt: oldTime},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	run := newRun(&deps)
	outcome := R0Dispatch{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for pending command, got %d", len(outcome.Repairs))
	}
}

// --- R0 planning stuck tests ---

func TestR0PlanningStuck_NoStateDir(t *testing.T) {
	t.Parallel()
	maestroDir := t.TempDir() // no subdirs created
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)
	outcome := R0PlanningStuck{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR0PlanningStuck_SealedState_Ignored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	oldTime := now.Add(-20 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_sealed",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  oldTime,
		UpdatedAt:  oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_sealed.yaml"), state)

	run := newRun(&deps)
	outcome := R0PlanningStuck{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for sealed state, got %d", len(outcome.Repairs))
	}
}

func TestR0PlanningStuck_StuckCommand_Repaired(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	commandID := "cmd_stuck_001"

	// Create a planning state that is well past the threshold.
	oldTime := now.Add(-20 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusPlanning,
		TaskTracking: model.TaskTracking{
			TaskDependencies: make(map[string][]string),
			TaskStates:       make(map[string]model.Status),
			CancelledReasons: make(map[string]string),
			AppliedResultIDs: make(map[string]string),
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: make(map[string]string),
		},
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", commandID+".yaml"), state)

	// Create planner queue with the stuck command.
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{ID: commandID, Content: "stuck", Status: model.StatusInProgress, CreatedAt: oldTime, UpdatedAt: oldTime},
			{ID: "cmd_other", Content: "other", Status: model.StatusPending, CreatedAt: oldTime, UpdatedAt: oldTime},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	// Create worker queue with tasks belonging to the stuck command.
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{ID: "task_1", CommandID: commandID, Status: model.StatusPending},
			{ID: "task_other", CommandID: "cmd_other", Status: model.StatusPending},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq)

	run := newRun(&deps)
	outcome := R0PlanningStuck{}.Apply(run)

	// Verify repair was reported.
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].CommandID != commandID {
		t.Errorf("repair.CommandID = %q, want %q", outcome.Repairs[0].CommandID, commandID)
	}
	if outcome.Repairs[0].Pattern != PatternR0 {
		t.Errorf("repair.Pattern = %q, want %q", outcome.Repairs[0].Pattern, PatternR0)
	}

	// Verify state file was deleted.
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("state file should have been deleted")
	}

	// Verify stuck command was removed from planner queue.
	plannerData, err := os.ReadFile(filepath.Join(maestroDir, "queue", "planner.yaml"))
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}
	var resultCQ model.CommandQueue
	if err := yamlv3.Unmarshal(plannerData, &resultCQ); err != nil {
		t.Fatalf("parse planner queue: %v", err)
	}
	if len(resultCQ.Commands) != 1 {
		t.Fatalf("planner queue should have 1 command (other), got %d", len(resultCQ.Commands))
	}
	if resultCQ.Commands[0].ID != "cmd_other" {
		t.Errorf("remaining command = %q, want %q", resultCQ.Commands[0].ID, "cmd_other")
	}

	// Verify stuck command's tasks were removed from worker queue.
	workerData, err := os.ReadFile(filepath.Join(maestroDir, "queue", "worker1.yaml"))
	if err != nil {
		t.Fatalf("read worker queue: %v", err)
	}
	var resultTQ model.TaskQueue
	if err := yamlv3.Unmarshal(workerData, &resultTQ); err != nil {
		t.Fatalf("parse worker queue: %v", err)
	}
	if len(resultTQ.Tasks) != 1 {
		t.Fatalf("worker queue should have 1 task (other), got %d", len(resultTQ.Tasks))
	}
	if resultTQ.Tasks[0].ID != "task_other" {
		t.Errorf("remaining task = %q, want %q", resultTQ.Tasks[0].ID, "task_other")
	}
}

func TestR0PlanningStuck_NotYetStuck_Ignored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	// Create a planning state that is recent (below threshold).
	recentTime := now.Add(-10 * time.Second).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_recent",
		PlanStatus:    model.PlanStatusPlanning,
		TaskTracking: model.TaskTracking{
			TaskDependencies: make(map[string][]string),
			TaskStates:       make(map[string]model.Status),
			CancelledReasons: make(map[string]string),
			AppliedResultIDs: make(map[string]string),
		},
		RetryTracking: model.RetryTracking{
			RetryLineage: make(map[string]string),
		},
		CreatedAt: recentTime,
		UpdatedAt: recentTime,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_recent.yaml"), state)

	run := newRun(&deps)
	outcome := R0PlanningStuck{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for recent planning state, got %d", len(outcome.Repairs))
	}
}

// --- R0b filling stuck tests ---

func TestR0bFillingStuck_WithFillingStartedAt(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	fillingStarted := now.Add(-10 * time.Minute).Format(time.RFC3339)
	recentUpdate := now.Add(-1 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r0b_fill_started",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates:       map[string]model.Status{"task_1": model.StatusPending},
			RequiredTaskIDs:  []string{"task_1"},
			TaskDependencies: map[string][]string{"task_1": {}},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID:          "p1",
					Name:             "phase-1",
					Status:           model.PhaseStatusFilling,
					FillingStartedAt: &fillingStarted,
					TaskIDs:          []string{"task_1"},
				},
			},
		},
		CreatedAt: recentUpdate,
		UpdatedAt: recentUpdate,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r0b_fill_started.yaml"), state)

	run := newRun(&deps)
	outcome := R0bFillingStuck{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}

	// Verify state updated
	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd_r0b_fill_started.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)

	if updated.Phases[0].Status != model.PhaseStatusAwaitingFill {
		t.Errorf("phase status: got %s", updated.Phases[0].Status)
	}
	if len(updated.TaskStates) != 0 {
		t.Errorf("task_states should be cleared, got %v", updated.TaskStates)
	}
	if len(updated.RequiredTaskIDs) != 0 {
		t.Errorf("required_task_ids should be cleared, got %v", updated.RequiredTaskIDs)
	}
}

func TestR0bFillingStuck_RecentFilling_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	fillingStarted := now.Add(-10 * time.Second).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r0b_recent",
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID:          "p1",
					Name:             "phase-1",
					Status:           model.PhaseStatusFilling,
					FillingStartedAt: &fillingStarted,
					TaskIDs:          []string{"task_1"},
				},
			},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r0b_recent.yaml"), state)

	run := newRun(&deps)
	outcome := R0bFillingStuck{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for recent filling, got %d", len(outcome.Repairs))
	}
}

func TestR0bFillingStuck_GeneratesNotification(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	// Set executor factory to trigger notification
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	}

	oldTime := now.Add(-10 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r0b_notif",
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{PhaseID: "p1", Name: "phase-1", Status: model.PhaseStatusFilling, TaskIDs: []string{"t1"}},
			},
		},
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r0b_notif.yaml"), state)

	run := newRun(&deps)
	outcome := R0bFillingStuck{}.Apply(run)
	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(outcome.Notifications))
	}
	if outcome.Notifications[0].Kind != NotifyReFill {
		t.Errorf("notification kind: got %s", outcome.Notifications[0].Kind)
	}
}

func TestR0bFillingStuck_NoExecutorFactory_NoNotification(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	oldTime := now.Add(-10 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r0b_noexec",
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{PhaseID: "p1", Name: "phase-1", Status: model.PhaseStatusFilling, TaskIDs: []string{"t1"}},
			},
		},
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r0b_noexec.yaml"), state)

	run := newRun(&deps)
	outcome := R0bFillingStuck{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected no notifications without executor factory, got %d", len(outcome.Notifications))
	}
}

func TestR0bFillingStuck_InvalidFillingStartedAt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		fillingStartedAt *string
		updatedAt        string
		wantRepairs      int
		desc             string
	}{
		{
			name:             "zero_value_time",
			fillingStartedAt: strPtr("0001-01-01T00:00:00Z"),
			wantRepairs:      1,
			desc:             "zero time is far in the past, should be detected as stuck",
		},
		{
			name:             "future_time",
			fillingStartedAt: strPtr(time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)),
			wantRepairs:      0,
			desc:             "future time should not be detected as stuck",
		},
		{
			name:             "invalid_format",
			fillingStartedAt: strPtr("not-a-valid-timestamp"),
			wantRepairs:      0,
			desc:             "invalid format should be skipped without panic",
		},
		{
			name:             "empty_string",
			fillingStartedAt: strPtr(""),
			wantRepairs:      0,
			desc:             "empty string should be skipped without panic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			maestroDir := setupTestDir(t)
			deps := newTestDeps(t, maestroDir)
			now := time.Now().UTC()
			setClock(&deps, now)

			updatedAt := now.Add(-1 * time.Minute).Format(time.RFC3339)
			state := model.CommandState{
				CommandID:  "cmd_r0b_" + tt.name,
				PlanStatus: model.PlanStatusSealed,
				TaskTracking: model.TaskTracking{
					TaskStates:       map[string]model.Status{"task_1": model.StatusPending},
					RequiredTaskIDs:  []string{"task_1"},
					TaskDependencies: map[string][]string{"task_1": {}},
				},
				PhaseTracking: model.PhaseTracking{
					Phases: []model.Phase{
						{
							PhaseID:          "p1",
							Name:             "phase-1",
							Status:           model.PhaseStatusFilling,
							FillingStartedAt: tt.fillingStartedAt,
							TaskIDs:          []string{"task_1"},
						},
					},
				},
				CreatedAt: updatedAt,
				UpdatedAt: updatedAt,
			}
			yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r0b_"+tt.name+".yaml"), state)

			run := newRun(&deps)
			outcome := R0bFillingStuck{}.Apply(run)
			if len(outcome.Repairs) != tt.wantRepairs {
				t.Errorf("%s: expected %d repairs, got %d", tt.desc, tt.wantRepairs, len(outcome.Repairs))
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// --- R1 result queue tests ---

func TestR1ResultQueue_NoResultsDir(t *testing.T) {
	t.Parallel()
	maestroDir := t.TempDir()
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR1ResultQueue_TaskNotInProgress_Ignored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	tq := model.TaskQueue{
		Tasks: []model.Task{
			{ID: "task1", CommandID: "cmd1", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq)

	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for pending task, got %d", len(outcome.Repairs))
	}
}

func TestR1ResultQueue_HappyPath_RepairsInProgressTask(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	// Result is terminal (completed)
	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	// Queue still shows in_progress
	owner := "worker1"
	expiresAt := now
	tq := model.TaskQueue{
		Tasks: []model.Task{
			{ID: "task1", CommandID: "cmd1", Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &expiresAt, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq)

	// State file for UpdateLastReconciledAt
	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != PatternR1 {
		t.Errorf("pattern: got %s, want R1", outcome.Repairs[0].Pattern)
	}

	// Verify queue updated
	data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "worker1.yaml"))
	var updated model.TaskQueue
	yamlv3.Unmarshal(data, &updated)
	if updated.Tasks[0].Status != model.StatusCompleted {
		t.Errorf("queue status: got %s, want completed", updated.Tasks[0].Status)
	}
	if updated.Tasks[0].LeaseOwner != nil {
		t.Error("lease_owner should be nil after repair")
	}
	if updated.Tasks[0].LeaseExpiresAt != nil {
		t.Error("lease_expires_at should be nil after repair")
	}
}

func TestR1ResultQueue_Idempotent(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	owner := "worker1"
	tq := model.TaskQueue{
		Tasks: []model.Task{
			{ID: "task1", CommandID: "cmd1", Status: model.StatusInProgress, LeaseOwner: &owner, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq)

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// First run — produces repair
	run1 := newRun(&deps)
	outcome1 := R1ResultQueue{}.Apply(run1)
	if len(outcome1.Repairs) != 1 {
		t.Fatalf("first run: expected 1 repair, got %d", len(outcome1.Repairs))
	}

	// Second run — queue already terminal, no repair
	run2 := newRun(&deps)
	outcome2 := R1ResultQueue{}.Apply(run2)
	if len(outcome2.Repairs) != 0 {
		t.Errorf("second run: expected 0 repairs (idempotent), got %d", len(outcome2.Repairs))
	}
}

func TestR1ResultQueue_FailedResult(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusFailed, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	tq := model.TaskQueue{
		Tasks: []model.Task{
			{ID: "task1", CommandID: "cmd1", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq)

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}

	data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "worker1.yaml"))
	var updated model.TaskQueue
	yamlv3.Unmarshal(data, &updated)
	if updated.Tasks[0].Status != model.StatusFailed {
		t.Errorf("queue status: got %s, want failed", updated.Tasks[0].Status)
	}
}

func TestR1ResultQueue_MultipleWorkers(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	// Worker1 has terminal result
	rf1 := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf1)
	tq1 := model.TaskQueue{
		Tasks: []model.Task{
			{ID: "task1", CommandID: "cmd1", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq1)

	// Worker2 has terminal result
	rf2 := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res2", TaskID: "task2", CommandID: "cmd1", Status: model.StatusFailed, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker2.yaml"), rf2)
	tq2 := model.TaskQueue{
		Tasks: []model.Task{
			{ID: "task2", CommandID: "cmd1", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker2.yaml"), tq2)

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)
	if len(outcome.Repairs) != 2 {
		t.Errorf("expected 2 repairs (one per worker), got %d", len(outcome.Repairs))
	}
}

func TestR1ResultQueue_NoQueueFile_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)
	// No queue file for worker1

	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when queue file missing, got %d", len(outcome.Repairs))
	}
}

// --- R2 result state tests ---

func TestR2ResultState_UnknownTask_Skipped(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task_unknown", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates:       map[string]model.Status{},
			AppliedResultIDs: map[string]string{},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for unknown task, got %d", len(outcome.Repairs))
	}
}

func TestR2ResultState_NilTaskStates_InitializedAndSkipped(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	// State with nil TaskStates
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	// Task not in TaskStates → skipped
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR2ResultState_NonTerminalResult_Ignored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusInProgress, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for non-terminal result, got %d", len(outcome.Repairs))
	}
}

func TestR2ResultState_HappyPath_UpdatesStateToTerminal(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates:       map[string]model.Status{"task1": model.StatusInProgress},
			AppliedResultIDs: map[string]string{},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != PatternR2 {
		t.Errorf("pattern: got %s, want R2", outcome.Repairs[0].Pattern)
	}

	// Verify state updated
	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.TaskStates["task1"] != model.StatusCompleted {
		t.Errorf("task state: got %s, want completed", updated.TaskStates["task1"])
	}
	if updated.AppliedResultIDs["task1"] != "res1" {
		t.Errorf("applied result ID: got %s, want res1", updated.AppliedResultIDs["task1"])
	}
	if updated.LastReconciledAt == nil {
		t.Error("last_reconciled_at should be set")
	}
}

func TestR2ResultState_Idempotent(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates:       map[string]model.Status{"task1": model.StatusInProgress},
			AppliedResultIDs: map[string]string{},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// First run
	run1 := newRun(&deps)
	outcome1 := R2ResultState{}.Apply(run1)
	if len(outcome1.Repairs) != 1 {
		t.Fatalf("first run: expected 1 repair, got %d", len(outcome1.Repairs))
	}

	// Second run — state already terminal, no repair
	run2 := newRun(&deps)
	outcome2 := R2ResultState{}.Apply(run2)
	if len(outcome2.Repairs) != 0 {
		t.Errorf("second run: expected 0 repairs (idempotent), got %d", len(outcome2.Repairs))
	}
}

func TestR2ResultState_AlreadyTerminal_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates:       map[string]model.Status{"task1": model.StatusCompleted},
			AppliedResultIDs: map[string]string{"task1": "res1"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for already terminal state, got %d", len(outcome.Repairs))
	}
}

func TestR2ResultState_MultipleTasks(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
			{ID: "res2", TaskID: "task2", CommandID: "cmd1", Status: model.StatusFailed, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				"task1": model.StatusInProgress,
				"task2": model.StatusInProgress,
			},
			AppliedResultIDs: map[string]string{},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	if len(outcome.Repairs) != 2 {
		t.Fatalf("expected 2 repairs, got %d", len(outcome.Repairs))
	}

	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.TaskStates["task1"] != model.StatusCompleted {
		t.Errorf("task1 state: got %s, want completed", updated.TaskStates["task1"])
	}
	if updated.TaskStates["task2"] != model.StatusFailed {
		t.Errorf("task2 state: got %s, want failed", updated.TaskStates["task2"])
	}
}

func TestR2ResultState_NoStateFile_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd_missing", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)
	// No state file for cmd_missing

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when state file missing, got %d", len(outcome.Repairs))
	}
}

// --- R0-dispatch boundary condition tests ---

func TestR0Dispatch_ThresholdBoundary_JustBelowThreshold_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	// DispatchLeaseSec=60 → threshold = max(60*2s=120s, 10m=600s) = 600s
	deps.Config.Watcher.DispatchLeaseSec = 60
	now := time.Now().UTC()
	setClock(&deps, now)

	// Age just below threshold (599s) — should NOT repair (age < threshold)
	belowTime := now.Add(-599 * time.Second).Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd_below", Status: model.StatusInProgress, UpdatedAt: belowTime, CreatedAt: belowTime},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	run := newRun(&deps)
	outcome := R0Dispatch{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs just below threshold, got %d", len(outcome.Repairs))
	}
}

func TestR0Dispatch_ThresholdBoundary_ExactlyAtThreshold_Repairs(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	// DispatchLeaseSec=60 → threshold = max(60*2s=120s, 10m=600s) = 600s
	deps.Config.Watcher.DispatchLeaseSec = 60
	now := time.Now().UTC()
	setClock(&deps, now)

	// Age exactly at threshold (600s) — should repair (age >= threshold)
	exactTime := now.Add(-600 * time.Second).Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd_exact", Status: model.StatusInProgress, UpdatedAt: exactTime, CreatedAt: exactTime},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	run := newRun(&deps)
	outcome := R0Dispatch{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair at exact threshold, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != PatternR0Dispatch {
		t.Errorf("pattern: got %s, want R0-dispatch", outcome.Repairs[0].Pattern)
	}
}

func TestR0Dispatch_MinThresholdEnforced(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	// DispatchLeaseSec=10 → 10*2=20s < minDispatchThreshold(600s) → threshold clamped to 600s
	deps.Config.Watcher.DispatchLeaseSec = 10
	now := time.Now().UTC()
	setClock(&deps, now)

	// Age = 30s > 20s but < 600s (min threshold) → should NOT repair
	oldTime := now.Add(-30 * time.Second).Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd_low_lease", Status: model.StatusInProgress, UpdatedAt: oldTime, CreatedAt: oldTime},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	run := newRun(&deps)
	outcome := R0Dispatch{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when min threshold enforced, got %d", len(outcome.Repairs))
	}
}

func TestR0Dispatch_EmptyQueue_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)

	// Empty queue with schema but no commands
	cq := model.CommandQueue{SchemaVersion: 1, Commands: []model.Command{}}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	run := newRun(&deps)
	outcome := R0Dispatch{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for empty queue, got %d", len(outcome.Repairs))
	}
}

// --- R2 result state boundary condition tests ---

func TestR2ResultState_EmptyResultsDir_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)

	// Results dir exists but is empty (no worker files)
	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for empty results dir, got %d", len(outcome.Repairs))
	}
}

func TestR2ResultState_MixedTerminalAndNonTerminal(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	// Results: task1=completed (terminal), task2=in_progress (non-terminal in result)
	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res1", TaskID: "task1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
			{ID: "res2", TaskID: "task2", CommandID: "cmd1", Status: model.StatusInProgress, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				"task1": model.StatusInProgress,
				"task2": model.StatusInProgress,
			},
			AppliedResultIDs: map[string]string{},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	// Only task1 should be repaired (terminal in result, non-terminal in state)
	// task2 is non-terminal in result, so R2 ignores it
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair (only terminal result matters), got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].TaskID != "task1" {
		t.Errorf("repaired task: got %s, want task1", outcome.Repairs[0].TaskID)
	}
}

// --- H-bug3: R0b split-brain prevention test ---

// TestR0bFillingStuck_BatchRemoveFails_StateNotUpdated verifies that when
// batchRemoveTaskIDsFromQueues fails, the state file is NOT updated to
// awaiting_fill, preventing split-brain (tasks in worker queues while state
// says awaiting_fill).
func TestR0bFillingStuck_BatchRemoveFails_StateNotUpdated(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	oldTime := now.Add(-10 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r0b_split",
		PlanStatus: model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates:       map[string]model.Status{"task_1": model.StatusPending},
			RequiredTaskIDs:  []string{"task_1"},
			TaskDependencies: map[string][]string{"task_1": {}},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "p1",
					Name:    "phase-1",
					Status:  model.PhaseStatusFilling,
					TaskIDs: []string{"task_1"},
				},
			},
		},
		CreatedAt: oldTime,
		UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r0b_split.yaml"), state)

	// Corrupted worker queue → batchRemove fails on unmarshal.
	os.WriteFile(filepath.Join(maestroDir, "queue", "worker1.yaml"), []byte("{{invalid"), 0644)

	run := newRun(&deps)
	outcome := R0bFillingStuck{}.Apply(run)

	// No repairs reported — batchRemove failure skips state update.
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when batch remove fails, got %d", len(outcome.Repairs))
	}

	// State should remain in filling (split-brain prevented).
	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd_r0b_split.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.Phases[0].Status != model.PhaseStatusFilling {
		t.Errorf("state should remain in filling, got %s", updated.Phases[0].Status)
	}
	if len(updated.TaskStates) != 1 {
		t.Errorf("task_states should be unchanged, got %v", updated.TaskStates)
	}
}

// --- H-bug4: ExecuteDeferredNotifications failure list tests ---

// TestEngine_ExecuteDeferredNotifications_ReturnsFailedOnDeliveryError verifies
// that notifications whose delivery fails are returned as a failed list.
func TestEngine_ExecuteDeferredNotifications_ReturnsFailedOnDeliveryError(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, setupTestDir(t))
	exec := &mocks.MockExecutor{Result: agent.ExecResult{Error: fmt.Errorf("delivery failed")}}
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return exec, nil
	}
	engine := NewEngine(deps)

	notifications := []DeferredNotification{
		{Kind: NotifyReFill, CommandID: "cmd1"},
		{Kind: NotifyReEvaluate, CommandID: "cmd2", Reason: "test"},
	}

	failed := engine.ExecuteDeferredNotifications(notifications)
	if len(failed) != 2 {
		t.Errorf("expected 2 failed notifications, got %d", len(failed))
	}
}

// TestEngine_ExecuteDeferredNotifications_NilFactory_ReturnsAllAsFailed verifies
// that with no executor factory, all notifications are returned as failed.
func TestEngine_ExecuteDeferredNotifications_NilFactory_ReturnsAllAsFailed(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, setupTestDir(t))
	engine := NewEngine(deps)

	notifications := []DeferredNotification{
		{Kind: NotifyReFill, CommandID: "cmd1"},
		{Kind: NotifyReEvaluate, CommandID: "cmd2", Reason: "test"},
	}

	failed := engine.ExecuteDeferredNotifications(notifications)
	if len(failed) != 2 {
		t.Errorf("expected all 2 notifications returned as failed, got %d", len(failed))
	}
}

// TestEngine_ExecuteDeferredNotifications_SuccessReturnsEmpty verifies that
// successful delivery returns no failed notifications.
func TestEngine_ExecuteDeferredNotifications_SuccessReturnsEmpty(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, setupTestDir(t))
	exec := &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return exec, nil
	}
	engine := NewEngine(deps)

	notifications := []DeferredNotification{
		{Kind: NotifyReFill, CommandID: "cmd1"},
	}

	failed := engine.ExecuteDeferredNotifications(notifications)
	if len(failed) != 0 {
		t.Errorf("expected no failures, got %d", len(failed))
	}
}

// TestEngine_ExecuteDeferredNotifications_FactoryError_ReturnsFailed verifies
// that executor creation failure causes the notification to be returned as failed.
func TestEngine_ExecuteDeferredNotifications_FactoryError_ReturnsFailed(t *testing.T) {
	t.Parallel()
	deps := newTestDeps(t, setupTestDir(t))
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return nil, fmt.Errorf("factory error")
	}
	engine := NewEngine(deps)

	notifications := []DeferredNotification{
		{Kind: NotifyReFill, CommandID: "cmd1"},
	}

	failed := engine.ExecuteDeferredNotifications(notifications)
	if len(failed) != 1 {
		t.Errorf("expected 1 failed notification, got %d", len(failed))
	}
}

// --- M48: R5 orchestrator queue lock test ---

// TestR5Notification_OrchestratorQueueLock verifies that R5 correctly reads the
// orchestrator queue (the read now runs under queue:orchestrator lock).
func TestR5Notification_OrchestratorQueueLock(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier

	// Result file: terminal + notified result
	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, NotifiableBase: model.NotifiableBase{Notified: true}, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	// State file
	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// No orchestrator queue → notification should be re-issued
	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != PatternR5 {
		t.Errorf("expected pattern R5, got %s", outcome.Repairs[0].Pattern)
	}
	if len(notifier.calls) != 1 {
		t.Errorf("expected 1 notification write, got %d", len(notifier.calls))
	}
}

// --- H-test1: R3 additional tests ---

// TestR3PlannerQueue_PendingCommand_QueueInProgress_Repaired verifies R3 repairs
// a command that is still pending in the queue when a terminal result exists.
// This is subtly different from the HappyPath test in reconcile_repair_test.go
// which uses in_progress. Here we test that any non-terminal queue status triggers repair.
func TestR3PlannerQueue_PendingCommand_QueueInProgress_Repaired(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	// Queue: pending (non-terminal, should be repaired)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{ID: "cmd1", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair for pending queue with terminal result, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != PatternR3 {
		t.Errorf("pattern: got %s, want R3", outcome.Repairs[0].Pattern)
	}

	data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "planner.yaml"))
	var updated model.CommandQueue
	yamlv3.Unmarshal(data, &updated)
	if updated.Commands[0].Status != model.StatusCompleted {
		t.Errorf("queue status: got %s, want completed", updated.Commands[0].Status)
	}
}

// TestR3PlannerQueue_CancelledResult verifies R3 handles cancelled result status correctly.
func TestR3PlannerQueue_CancelledResult(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCancelled, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd1", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}

	data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "planner.yaml"))
	var updated model.CommandQueue
	yamlv3.Unmarshal(data, &updated)
	if updated.Commands[0].Status != model.StatusCancelled {
		t.Errorf("queue status: got %s, want cancelled", updated.Commands[0].Status)
	}
}

// --- H-test1: R5 additional tests ---

// TestR5Notification_FailedStatus_CorrectNotificationType verifies that R5
// uses the correct notification type for a failed result.
func TestR5Notification_FailedStatus_CorrectNotificationType(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusFailed, NotifiableBase: model.NotifiableBase{Notified: true}, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	// Orchestrator queue has command_completed for same result but result status is now failed
	// R5 should re-issue because the dedup key (source_result_id, type) doesn't match
	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{SourceResultID: "res1", Type: model.NotificationTypeCommandCompleted},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), nq)

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair for type mismatch, got %d", len(outcome.Repairs))
	}
	if notifier.calls[0].status != model.StatusFailed {
		t.Errorf("expected failed status in notification call, got %s", notifier.calls[0].status)
	}
}

// TestR5Notification_CancelledStatus_NoExistingNotification verifies R5 re-issues
// for a cancelled result when no matching notification exists.
func TestR5Notification_CancelledStatus_NoExistingNotification(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCancelled, NotifiableBase: model.NotifiableBase{Notified: true}, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if notifier.calls[0].status != model.StatusCancelled {
		t.Errorf("expected cancelled status, got %s", notifier.calls[0].status)
	}
}

// --- H-test1: R6 additional tests ---

// TestR6FillTimeout_NoStateDir verifies R6 handles a missing state directory gracefully.
func TestR6FillTimeout_NoStateDir(t *testing.T) {
	t.Parallel()
	maestroDir := t.TempDir() // no subdirs
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

// TestR6FillTimeout_ExpiredDeadline_Repaired verifies a single phase with an
// expired fill deadline is transitioned to timed_out.
func TestR6FillTimeout_ExpiredDeadline_Repaired(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	deadline := now.Add(-5 * time.Minute).Format(time.RFC3339)
	recentTime := now.Add(-1 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r6",
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID:        "p1",
					Name:           "phase-1",
					Status:         model.PhaseStatusAwaitingFill,
					FillDeadlineAt: &deadline,
				},
			},
		},
		CreatedAt: recentTime,
		UpdatedAt: recentTime,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r6.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != PatternR6 {
		t.Errorf("pattern: got %s, want R6", outcome.Repairs[0].Pattern)
	}

	// Verify state updated
	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd_r6.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.Phases[0].Status != model.PhaseStatusTimedOut {
		t.Errorf("phase status: got %s, want timed_out", updated.Phases[0].Status)
	}
	if updated.LastReconciledAt == nil {
		t.Error("expected last_reconciled_at to be set")
	}
}

// TestR6FillTimeout_FutureDeadline_NoRepair verifies that a phase with a future
// fill deadline is not repaired.
func TestR6FillTimeout_FutureDeadline_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	deadline := now.Add(5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r6_future",
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID:        "p1",
					Name:           "phase-1",
					Status:         model.PhaseStatusAwaitingFill,
					FillDeadlineAt: &deadline,
				},
			},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r6_future.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for future deadline, got %d", len(outcome.Repairs))
	}
}

// TestR6FillTimeout_CascadeCancel verifies that a downstream pending phase
// dependent on a timed-out phase gets cascade-cancelled.
func TestR6FillTimeout_CascadeCancel(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	deadline := now.Add(-5 * time.Minute).Format(time.RFC3339)
	recentTime := now.Add(-1 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r6_cascade",
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID:        "p1",
					Name:           "phase-1",
					Status:         model.PhaseStatusAwaitingFill,
					FillDeadlineAt: &deadline,
				},
				{
					PhaseID:         "p2",
					Name:            "phase-2",
					Status:          model.PhaseStatusPending,
					DependsOnPhases: []string{"phase-1"},
				},
			},
		},
		CreatedAt: recentTime,
		UpdatedAt: recentTime,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r6_cascade.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	// Should have 2 repairs: phase-1 timed_out + phase-2 cascade cancelled
	if len(outcome.Repairs) != 2 {
		t.Fatalf("expected 2 repairs (timeout + cascade), got %d", len(outcome.Repairs))
	}

	// Verify state
	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd_r6_cascade.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.Phases[0].Status != model.PhaseStatusTimedOut {
		t.Errorf("phase-1 status: got %s, want timed_out", updated.Phases[0].Status)
	}
	if updated.Phases[1].Status != model.PhaseStatusCancelled {
		t.Errorf("phase-2 status: got %s, want cancelled", updated.Phases[1].Status)
	}
}

// TestR6FillTimeout_NonAwaitingFill_Ignored verifies that a phase in filling status
// (not awaiting_fill) with an expired deadline is ignored by R6.
func TestR6FillTimeout_NonAwaitingFill_Ignored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	deadline := now.Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r6_filling",
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID:        "p1",
					Name:           "phase-1",
					Status:         model.PhaseStatusFilling, // not awaiting_fill
					FillDeadlineAt: &deadline,
				},
			},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r6_filling.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for non-awaiting_fill phase, got %d", len(outcome.Repairs))
	}
}

// TestR6FillTimeout_GeneratesNotification verifies that R6 generates a deferred
// notification with the correct kind and timed-out phases when executor factory is set.
func TestR6FillTimeout_GeneratesNotification(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	}

	deadline := now.Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r6_notif",
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID:        "p1",
					Name:           "phase-1",
					Status:         model.PhaseStatusAwaitingFill,
					FillDeadlineAt: &deadline,
				},
			},
		},
		CreatedAt: now.Add(-1 * time.Minute).Format(time.RFC3339),
		UpdatedAt: now.Add(-1 * time.Minute).Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r6_notif.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(outcome.Notifications))
	}
	if outcome.Notifications[0].Kind != NotifyFillTimeout {
		t.Errorf("notification kind: got %s, want fill_timeout", outcome.Notifications[0].Kind)
	}
	if outcome.Notifications[0].CommandID != "cmd_r6_notif" {
		t.Errorf("notification commandID: got %s, want cmd_r6_notif", outcome.Notifications[0].CommandID)
	}
	if !outcome.Notifications[0].TimedOutPhases["phase-1"] {
		t.Errorf("expected phase-1 in timed out phases, got %v", outcome.Notifications[0].TimedOutPhases)
	}
}

// TestR6FillTimeout_NilDeadline_NoRepair verifies that a phase with nil FillDeadlineAt
// is not repaired even if it is in awaiting_fill status.
func TestR6FillTimeout_NilDeadline_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	state := model.CommandState{
		CommandID:  "cmd_r6_nil_deadline",
		PlanStatus: model.PlanStatusSealed,
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{
					PhaseID: "p1",
					Name:    "phase-1",
					Status:  model.PhaseStatusAwaitingFill,
					// FillDeadlineAt is nil
				},
			},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_r6_nil_deadline.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for nil deadline, got %d", len(outcome.Repairs))
	}
}

