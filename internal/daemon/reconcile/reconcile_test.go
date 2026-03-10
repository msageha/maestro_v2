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
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// --- Test Helpers ---

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

type mockExecutor struct {
	calls  []agent.ExecRequest
	result agent.ExecResult
}

func (m *mockExecutor) Execute(req agent.ExecRequest) agent.ExecResult {
	m.calls = append(m.calls, req)
	return m.result
}
func (m *mockExecutor) Close() error { return nil }

func setupTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	for _, sub := range []string{"queue", "results", "state/commands", "logs"} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return maestroDir
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

func TestExtractWorkerID(t *testing.T) {
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
		got := ExtractWorkerID(tt.input)
		if got != tt.want {
			t.Errorf("ExtractWorkerID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRemoveFromSlice(t *testing.T) {
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
		got := RemoveFromSlice(tt.s, tt.target)
		if len(got) != tt.want {
			t.Errorf("RemoveFromSlice(%v, %q): len=%d, want %d", tt.s, tt.target, len(got), tt.want)
		}
	}
}

func TestStuckThresholdSec(t *testing.T) {
	deps := Deps{Config: model.Config{Watcher: model.WatcherConfig{DispatchLeaseSec: 100}}}
	run := newRun(&deps)
	if got := run.StuckThresholdSec(); got != 200 {
		t.Errorf("got %d, want 200", got)
	}

	// Zero defaults to 300
	deps.Config.Watcher.DispatchLeaseSec = 0
	run2 := newRun(&deps)
	if got := run2.StuckThresholdSec(); got != 600 {
		t.Errorf("got %d, want 600 (300*2)", got)
	}
}

func TestCachedReadDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(""), 0644)

	deps := Deps{}
	run := newRun(&deps)

	entries1, err := run.CachedReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries1) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries1))
	}

	// Create another file — cached result should still return 1
	os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(""), 0644)
	entries2, err := run.CachedReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries2) != 1 {
		t.Errorf("cached dir should still return 1 entry, got %d", len(entries2))
	}
}

func TestCachedReadDir_NonExistent(t *testing.T) {
	deps := Deps{}
	run := newRun(&deps)
	_, err := run.CachedReadDir("/nonexistent/path")
	if err == nil {
		t.Error("expected error for non-existent directory")
	}
}

// --- Engine tests ---

func TestEngine_Reconcile_EmptyPatterns(t *testing.T) {
	deps := newTestDeps(t, setupTestDir(t))
	engine := NewEngine(deps)
	repairs, notifications := engine.Reconcile()
	if len(repairs) != 0 || len(notifications) != 0 {
		t.Errorf("expected empty results, got repairs=%d notifications=%d", len(repairs), len(notifications))
	}
}

type fakePattern struct {
	name    string
	outcome Outcome
}

func (f *fakePattern) Name() string      { return f.name }
func (f *fakePattern) Apply(*Run) Outcome { return f.outcome }

func TestEngine_Reconcile_AggregatesPatterns(t *testing.T) {
	deps := newTestDeps(t, setupTestDir(t))
	p1 := &fakePattern{name: "P1", outcome: Outcome{
		Repairs: []Repair{{Pattern: "P1", Detail: "repair1"}},
	}}
	p2 := &fakePattern{name: "P2", outcome: Outcome{
		Repairs:       []Repair{{Pattern: "P2", Detail: "repair2"}},
		Notifications: []DeferredNotification{{Kind: "re_fill", CommandID: "cmd1"}},
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
	deps := newTestDeps(t, setupTestDir(t))
	engine := NewEngine(deps)
	// Should not panic
	engine.ExecuteDeferredNotifications([]DeferredNotification{
		{Kind: "re_fill", CommandID: "cmd1"},
	})
}

func TestEngine_ExecuteDeferredNotifications_AllKinds(t *testing.T) {
	deps := newTestDeps(t, setupTestDir(t))
	exec := &mockExecutor{result: agent.ExecResult{Success: true}}
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return exec, nil
	}
	engine := NewEngine(deps)
	engine.ExecuteDeferredNotifications([]DeferredNotification{
		{Kind: "re_fill", CommandID: "cmd1"},
		{Kind: "re_evaluate", CommandID: "cmd2", Reason: "tasks not done"},
		{Kind: "fill_timeout", CommandID: "cmd3", TimedOutPhases: map[string]bool{"p1": true}},
		{Kind: "unknown_kind", CommandID: "cmd4"},
	})
	if len(exec.calls) != 3 {
		t.Errorf("expected 3 executor calls (unknown skipped), got %d", len(exec.calls))
	}
}

func TestEngine_ExecuteDeferredNotifications_FactoryError(t *testing.T) {
	deps := newTestDeps(t, setupTestDir(t))
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return nil, fmt.Errorf("factory error")
	}
	engine := NewEngine(deps)
	// Should not panic
	engine.ExecuteDeferredNotifications([]DeferredNotification{
		{Kind: "re_fill", CommandID: "cmd1"},
	})
}

func TestEngine_SetCanComplete(t *testing.T) {
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
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)
	outcome := R0Dispatch{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR0Dispatch_StuckCommand(t *testing.T) {
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
	if outcome.Repairs[0].Pattern != "R0-dispatch" {
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
	maestroDir := t.TempDir() // no subdirs created
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)
	outcome := R0PlanningStuck{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR0PlanningStuck_SealedState_Ignored(t *testing.T) {
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

// --- R0b filling stuck tests ---

func TestR0bFillingStuck_WithFillingStartedAt(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	fillingStarted := now.Add(-10 * time.Minute).Format(time.RFC3339)
	recentUpdate := now.Add(-1 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r0b_fill_started",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID:          "p1",
				Name:             "phase-1",
				Status:           model.PhaseStatusFilling,
				FillingStartedAt: &fillingStarted,
				TaskIDs:          []string{"task_1"},
			},
		},
		TaskStates:       map[string]model.Status{"task_1": model.StatusPending},
		RequiredTaskIDs:  []string{"task_1"},
		TaskDependencies: map[string][]string{"task_1": {}},
		CreatedAt:        recentUpdate,
		UpdatedAt:        recentUpdate,
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
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	fillingStarted := now.Add(-10 * time.Second).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r0b_recent",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID:          "p1",
				Name:             "phase-1",
				Status:           model.PhaseStatusFilling,
				FillingStartedAt: &fillingStarted,
				TaskIDs:          []string{"task_1"},
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
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	// Set executor factory to trigger notification
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	}

	oldTime := now.Add(-10 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r0b_notif",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase-1", Status: model.PhaseStatusFilling, TaskIDs: []string{"t1"}},
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
	if outcome.Notifications[0].Kind != "re_fill" {
		t.Errorf("notification kind: got %s", outcome.Notifications[0].Kind)
	}
}

func TestR0bFillingStuck_NoExecutorFactory_NoNotification(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	oldTime := now.Add(-10 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r0b_noexec",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase-1", Status: model.PhaseStatusFilling, TaskIDs: []string{"t1"}},
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

// --- R1 result queue tests ---

func TestR1ResultQueue_NoResultsDir(t *testing.T) {
	maestroDir := t.TempDir()
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)
	outcome := R1ResultQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR1ResultQueue_TaskNotInProgress_Ignored(t *testing.T) {
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
	if outcome.Repairs[0].Pattern != "R1" {
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
		CommandID:        "cmd1",
		PlanStatus:       model.PlanStatusSealed,
		TaskStates:       map[string]model.Status{},
		AppliedResultIDs: map[string]string{},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for unknown task, got %d", len(outcome.Repairs))
	}
}

func TestR2ResultState_NilTaskStates_InitializedAndSkipped(t *testing.T) {
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
		CommandID:        "cmd1",
		PlanStatus:       model.PlanStatusSealed,
		TaskStates:       map[string]model.Status{"task1": model.StatusInProgress},
		AppliedResultIDs: map[string]string{},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != "R2" {
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
		CommandID:        "cmd1",
		PlanStatus:       model.PlanStatusSealed,
		TaskStates:       map[string]model.Status{"task1": model.StatusInProgress},
		AppliedResultIDs: map[string]string{},
		CreatedAt:        now,
		UpdatedAt:        now,
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
		CommandID:        "cmd1",
		PlanStatus:       model.PlanStatusSealed,
		TaskStates:       map[string]model.Status{"task1": model.StatusCompleted},
		AppliedResultIDs: map[string]string{"task1": "res1"},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R2ResultState{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for already terminal state, got %d", len(outcome.Repairs))
	}
}

func TestR2ResultState_MultipleTasks(t *testing.T) {
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
		TaskStates: map[string]model.Status{
			"task1": model.StatusInProgress,
			"task2": model.StatusInProgress,
		},
		AppliedResultIDs: map[string]string{},
		CreatedAt:        now,
		UpdatedAt:        now,
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

// --- R3 planner queue tests ---

func TestR3PlannerQueue_NoResultFile(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR3PlannerQueue_NoTerminalResults(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusInProgress, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for non-terminal results, got %d", len(outcome.Repairs))
	}
}

func TestR3PlannerQueue_HappyPath_RepairsNonTerminalCommand(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	// Planner result is terminal
	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	// Queue still shows in_progress
	owner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd1", Status: model.StatusInProgress, LeaseOwner: &owner, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	// State file for UpdateLastReconciledAt
	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != "R3" {
		t.Errorf("pattern: got %s, want R3", outcome.Repairs[0].Pattern)
	}

	// Verify queue updated
	data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "planner.yaml"))
	var updated model.CommandQueue
	yamlv3.Unmarshal(data, &updated)
	if updated.Commands[0].Status != model.StatusCompleted {
		t.Errorf("queue status: got %s, want completed", updated.Commands[0].Status)
	}
	if updated.Commands[0].LeaseOwner != nil {
		t.Error("lease_owner should be nil after repair")
	}
	if updated.Commands[0].LeaseExpiresAt != nil {
		t.Error("lease_expires_at should be nil after repair")
	}
}

func TestR3PlannerQueue_Idempotent(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
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

	// First run
	run1 := newRun(&deps)
	outcome1 := R3PlannerQueue{}.Apply(run1)
	if len(outcome1.Repairs) != 1 {
		t.Fatalf("first run: expected 1 repair, got %d", len(outcome1.Repairs))
	}

	// Second run — queue already terminal
	run2 := newRun(&deps)
	outcome2 := R3PlannerQueue{}.Apply(run2)
	if len(outcome2.Repairs) != 0 {
		t.Errorf("second run: expected 0 repairs (idempotent), got %d", len(outcome2.Repairs))
	}
}

func TestR3PlannerQueue_AlreadyTerminalQueue_NoRepair(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd1", Status: model.StatusCompleted, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for already terminal queue, got %d", len(outcome.Repairs))
	}
}

func TestR3PlannerQueue_FailedResult(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusFailed, CreatedAt: now},
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
	if updated.Commands[0].Status != model.StatusFailed {
		t.Errorf("queue status: got %s, want failed", updated.Commands[0].Status)
	}
}

func TestR3PlannerQueue_MultipleCommands(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
			{ID: "res2", CommandID: "cmd2", Status: model.StatusFailed, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		Commands: []model.Command{
			{ID: "cmd1", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
			{ID: "cmd2", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
			{ID: "cmd3", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now}, // no result
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	state1 := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state1)
	state2 := model.CommandState{CommandID: "cmd2", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd2.yaml"), state2)

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 2 {
		t.Errorf("expected 2 repairs, got %d", len(outcome.Repairs))
	}
}

func TestR3PlannerQueue_NoQueueFile_NoRepair(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	os.Remove(filepath.Join(maestroDir, "queue", "planner.yaml"))

	run := newRun(&deps)
	outcome := R3PlannerQueue{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when queue file missing, got %d", len(outcome.Repairs))
	}
}

// --- R4 plan status tests ---

func TestR4PlanStatus_CanCompleteNil_Skipped(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R4PlanStatus{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when CanComplete is nil, got %d", len(outcome.Repairs))
	}
}

func TestR4PlanStatus_AlreadyTerminal_Skipped(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.CanComplete = func(*model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCompleted, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusCompleted, // Already terminal
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R4PlanStatus{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for already terminal state, got %d", len(outcome.Repairs))
	}
}

func TestR4PlanStatus_CanCompleteSuccess(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.CanComplete = func(*model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCompleted, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R4PlanStatus{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected no notifications on success, got %d", len(outcome.Notifications))
	}

	// Verify state updated
	data, _ := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("plan_status: got %s, want completed", updated.PlanStatus)
	}
}

func TestR4PlanStatus_CanCompleteFails_QuarantineAndNotify(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.CanComplete = func(*model.CommandState) (model.PlanStatus, error) {
		return "", fmt.Errorf("tasks incomplete")
	}
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R4PlanStatus{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(outcome.Notifications))
	}
	if outcome.Notifications[0].Kind != "re_evaluate" {
		t.Errorf("notification kind: got %s", outcome.Notifications[0].Kind)
	}

	// Verify quarantine file created
	entries, _ := os.ReadDir(filepath.Join(maestroDir, "quarantine"))
	if len(entries) != 1 {
		t.Errorf("expected 1 quarantine file, got %d", len(entries))
	}

	// Verify result removed from planner.yaml
	data, _ := os.ReadFile(filepath.Join(maestroDir, "results", "planner.yaml"))
	var updatedRF model.CommandResultFile
	yamlv3.Unmarshal(data, &updatedRF)
	if len(updatedRF.Results) != 0 {
		t.Errorf("expected 0 results after quarantine, got %d", len(updatedRF.Results))
	}
}

func TestR4PlanStatus_StateNotFound_NoRepair(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.CanComplete = func(*model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCompleted, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd_no_state", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	run := newRun(&deps)
	outcome := R4PlanStatus{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when state file missing, got %d", len(outcome.Repairs))
	}
}

// --- R5 notification tests ---

func TestR5Notification_NilResultHandler(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	// ResultHandler is nil
	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when ResultHandler is nil, got %d", len(outcome.Repairs))
	}
}

func TestR5Notification_NotNotified_Ignored(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	deps.ResultHandler = &mockResultNotifier{}
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, Notified: false, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for non-notified result, got %d", len(outcome.Repairs))
	}
}

func TestR5Notification_WriteError(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{err: fmt.Errorf("write failed")}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, Notified: true, NotifiedAt: &now, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), model.NotificationQueue{})

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	// Should not produce repairs on write error
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs on write error, got %d", len(outcome.Repairs))
	}
	if len(notifier.calls) != 1 {
		t.Errorf("expected 1 write attempt, got %d", len(notifier.calls))
	}
}

func TestR5Notification_HappyPath_ReissuesNotification(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	// Result is terminal, notified, but no orchestrator notification exists
	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, Notified: true, NotifiedAt: &now, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	// Empty orchestrator queue — no matching source_result_id
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), model.NotificationQueue{})

	// State file for UpdateLastReconciledAt
	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if outcome.Repairs[0].Pattern != "R5" {
		t.Errorf("pattern: got %s, want R5", outcome.Repairs[0].Pattern)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notification call, got %d", len(notifier.calls))
	}
	if notifier.calls[0].resultID != "res1" {
		t.Errorf("resultID: got %s, want res1", notifier.calls[0].resultID)
	}
	if notifier.calls[0].commandID != "cmd1" {
		t.Errorf("commandID: got %s, want cmd1", notifier.calls[0].commandID)
	}
	if notifier.calls[0].status != model.StatusCompleted {
		t.Errorf("status: got %s, want completed", notifier.calls[0].status)
	}
}

func TestR5Notification_AlreadyInOrchestratorQueue_NoRepair(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, Notified: true, NotifiedAt: &now, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	// Orchestrator queue already has matching notification
	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{ID: "ntf1", CommandID: "cmd1", SourceResultID: "res1", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), nq)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs when notification already exists, got %d", len(outcome.Repairs))
	}
	if len(notifier.calls) != 0 {
		t.Errorf("expected no notification calls, got %d", len(notifier.calls))
	}
}

func TestR5Notification_Idempotent(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, Notified: true, NotifiedAt: &now, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), model.NotificationQueue{})

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	// First run — produces repair
	run1 := newRun(&deps)
	outcome1 := R5Notification{}.Apply(run1)
	if len(outcome1.Repairs) != 1 {
		t.Fatalf("first run: expected 1 repair, got %d", len(outcome1.Repairs))
	}

	// The mockResultNotifier doesn't actually write to orchestrator.yaml,
	// so calling again would still produce a repair (which is correct behavior —
	// R5 re-issues until the notification actually appears in the queue).
	// This verifies the mock was called twice.
	run2 := newRun(&deps)
	outcome2 := R5Notification{}.Apply(run2)
	if len(outcome2.Repairs) != 1 {
		t.Errorf("second run: expected 1 repair (mock doesn't persist), got %d", len(outcome2.Repairs))
	}
	if len(notifier.calls) != 2 {
		t.Errorf("expected 2 total notification calls, got %d", len(notifier.calls))
	}
}

func TestR5Notification_NonTerminalResult_Ignored(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusInProgress, Notified: true, NotifiedAt: &now, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), model.NotificationQueue{})

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for non-terminal result, got %d", len(outcome.Repairs))
	}
}

func TestR5Notification_NoOrchestratorQueue_StillRepairs(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, Notified: true, NotifiedAt: &now, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	// No orchestrator.yaml file

	state := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Errorf("expected 1 repair when orchestrator queue missing, got %d", len(outcome.Repairs))
	}
}

func TestR5Notification_MultipleResults(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	notifier := &mockResultNotifier{}
	deps.ResultHandler = notifier
	now := time.Now().UTC().Format(time.RFC3339)

	rf := model.CommandResultFile{
		Results: []model.CommandResult{
			{ID: "res1", CommandID: "cmd1", Status: model.StatusCompleted, Notified: true, NotifiedAt: &now, CreatedAt: now},
			{ID: "res2", CommandID: "cmd2", Status: model.StatusFailed, Notified: true, NotifiedAt: &now, CreatedAt: now},
			{ID: "res3", CommandID: "cmd3", Status: model.StatusCompleted, Notified: false, CreatedAt: now}, // not notified
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), model.NotificationQueue{})

	state1 := model.CommandState{CommandID: "cmd1", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state1)
	state2 := model.CommandState{CommandID: "cmd2", PlanStatus: model.PlanStatusSealed, CreatedAt: now, UpdatedAt: now}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd2.yaml"), state2)

	run := newRun(&deps)
	outcome := R5Notification{}.Apply(run)
	// Only res1 and res2 should be repaired (res3 is not notified)
	if len(outcome.Repairs) != 2 {
		t.Errorf("expected 2 repairs, got %d", len(outcome.Repairs))
	}
	if len(notifier.calls) != 2 {
		t.Errorf("expected 2 notification calls, got %d", len(notifier.calls))
	}
}

// --- R6 fill timeout tests ---

func TestR6FillTimeout_NoPhases_NoRepair(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs, got %d", len(outcome.Repairs))
	}
}

func TestR6FillTimeout_NoDeadline_NoRepair(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusAwaitingFill},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs without deadline, got %d", len(outcome.Repairs))
	}
}

func TestR6FillTimeout_ActivePhase_Ignored(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	pastDeadline := now.Add(-1 * time.Hour).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusActive, FillDeadlineAt: &pastDeadline},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected no repairs for active phase, got %d", len(outcome.Repairs))
	}
}

func TestR6FillTimeout_MultipleTimedOutPhases(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)
	deps.ExecutorFactory = func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	}

	pastDeadline := now.Add(-1 * time.Hour).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 2 {
		t.Fatalf("expected 2 repairs, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(outcome.Notifications))
	}
}

func TestR6FillTimeout_NoExecutorFactory_NoNotification(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC()
	setClock(&deps, now)

	pastDeadline := now.Add(-1 * time.Hour).Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd1",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
		},
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), state)

	run := newRun(&deps)
	outcome := R6FillTimeout{}.Apply(run)
	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected no notifications without executor factory, got %d", len(outcome.Notifications))
	}
}

// --- Run helper method tests ---

func TestLoadState_CorruptedYAML(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)

	path := filepath.Join(maestroDir, "state", "commands", "corrupt.yaml")
	os.WriteFile(path, []byte("plan_status: [unterminated"), 0644)

	_, err := run.LoadState(path)
	if err == nil {
		t.Error("expected error for corrupted YAML")
	}
}

func TestLoadState_NonExistent(t *testing.T) {
	deps := Deps{}
	run := newRun(&deps)
	_, err := run.LoadState("/nonexistent/path.yaml")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestLoadCommandResultFile_NonExistent(t *testing.T) {
	deps := Deps{}
	run := newRun(&deps)
	rf, err := run.LoadCommandResultFile("/nonexistent/path.yaml")
	if err != nil {
		t.Fatalf("expected no error for non-existent file, got %v", err)
	}
	if len(rf.Results) != 0 {
		t.Errorf("expected empty results, got %d", len(rf.Results))
	}
}

func TestRemoveCommandFromPlannerQueue_NoQueueFile(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	run := newRun(&deps)

	// No planner.yaml exists — should remove queue dir to test missing file
	os.Remove(filepath.Join(maestroDir, "queue", "planner.yaml"))

	err := run.RemoveCommandFromPlannerQueue("cmd_nonexistent")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestRemoveCommandFromPlannerQueue_CommandNotFound(t *testing.T) {
	maestroDir := setupTestDir(t)
	deps := newTestDeps(t, maestroDir)
	now := time.Now().UTC().Format(time.RFC3339)

	cq := model.CommandQueue{
		Commands: []model.Command{
			{ID: "cmd_other", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), cq)

	run := newRun(&deps)
	err := run.RemoveCommandFromPlannerQueue("cmd_nonexistent")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}

	// Original command should still be there
	data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "planner.yaml"))
	var updated model.CommandQueue
	yamlv3.Unmarshal(data, &updated)
	if len(updated.Commands) != 1 {
		t.Errorf("expected 1 command, got %d", len(updated.Commands))
	}
}
