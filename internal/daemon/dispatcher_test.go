package daemon

import (
	"bytes"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// testClock is a controllable clock for testing.
type testClock struct {
	now time.Time
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestGetExecutor_ErrorTTL(t *testing.T) {
	cfg := model.Config{}
	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)

	callCount := 0
	d.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (core.AgentExecutor, error) {
		callCount++
		return nil, fmt.Errorf("factory error %d", callCount)
	})

	clk := &testClock{now: time.Now()}
	d.clock = clk

	// First call: factory invoked
	_, err := d.getExecutor()
	if err == nil {
		t.Fatal("expected error")
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call, got %d", callCount)
	}

	// Second call within TTL: cached error, no factory call
	clk.Advance(10 * time.Second)
	_, err = d.getExecutor()
	if err == nil {
		t.Fatal("expected error")
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call (cached), got %d", callCount)
	}

	// Third call after TTL: factory retried
	clk.Advance(25 * time.Second) // total 35s > 30s TTL
	_, err = d.getExecutor()
	if err == nil {
		t.Fatal("expected error")
	}
	if callCount != 2 {
		t.Fatalf("expected 2 calls (retried), got %d", callCount)
	}
}

func TestGetExecutor_ErrorTTL_RecoveryOnRetry(t *testing.T) {
	cfg := model.Config{}
	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)

	callCount := 0
	d.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (core.AgentExecutor, error) {
		callCount++
		if callCount == 1 {
			return nil, fmt.Errorf("transient error")
		}
		return &stubExecutor{}, nil
	})

	clk := &testClock{now: time.Now()}
	d.clock = clk

	// First call: error
	_, err := d.getExecutor()
	if err == nil {
		t.Fatal("expected error")
	}

	// Advance past TTL and retry: should succeed
	clk.Advance(31 * time.Second)
	exec, err := d.getExecutor()
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if exec == nil {
		t.Fatal("expected non-nil executor")
	}
	if callCount != 2 {
		t.Fatalf("expected 2 calls, got %d", callCount)
	}

	// Subsequent call: should use cached successful executor
	exec2, err := d.getExecutor()
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if exec2 != exec {
		t.Fatal("expected same cached executor")
	}
	if callCount != 2 {
		t.Fatalf("expected 2 calls (cached success), got %d", callCount)
	}
}

// stubExecutor is a minimal core.AgentExecutor for testing.
type stubExecutor struct{}

func (s *stubExecutor) Execute(req agent.ExecRequest) agent.ExecResult { return agent.ExecResult{} }
func (s *stubExecutor) Close() error                                   { return nil }

func TestEffectivePriority(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name             string
		priority         int
		createdAt        string
		priorityAgingSec int
		expected         int
	}{
		{
			"no aging",
			5, now.Format(time.RFC3339), 0,
			5,
		},
		{
			"fresh entry",
			5, now.Format(time.RFC3339), 60,
			5,
		},
		{
			"aged 2 intervals",
			5, now.Add(-120 * time.Second).Format(time.RFC3339), 60,
			3,
		},
		{
			"aged beyond zero clamps to 0",
			2, now.Add(-300 * time.Second).Format(time.RFC3339), 60,
			0,
		},
		{
			"invalid time",
			5, "invalid", 60,
			5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectivePriority(tt.priority, tt.createdAt, tt.priorityAgingSec)
			if got != tt.expected {
				t.Errorf("got %d, want %d", got, tt.expected)
			}
		})
	}
}

func TestSortPendingTasks(t *testing.T) {
	now := time.Now().UTC()

	cfg := model.Config{
		Queue: model.QueueConfig{PriorityAgingSec: 60},
	}

	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)

	tasks := []model.Task{
		{ID: "t_high", Priority: 5, Status: model.StatusPending, CreatedAt: now.Format(time.RFC3339)},
		{ID: "t_low", Priority: 1, Status: model.StatusPending, CreatedAt: now.Format(time.RFC3339)},
		{ID: "t_medium", Priority: 3, Status: model.StatusPending, CreatedAt: now.Format(time.RFC3339)},
		{ID: "t_skip", Priority: 0, Status: model.StatusInProgress, CreatedAt: now.Format(time.RFC3339)},
	}

	sorted := d.SortPendingTasks(tasks)

	// Should only include pending entries
	if len(sorted) != 3 {
		t.Fatalf("expected 3 pending, got %d", len(sorted))
	}

	// Priority order: 1(t_low) < 3(t_medium) < 5(t_high)
	if tasks[sorted[0]].ID != "t_low" {
		t.Errorf("first: got %s, want t_low", tasks[sorted[0]].ID)
	}
	if tasks[sorted[1]].ID != "t_medium" {
		t.Errorf("second: got %s, want t_medium", tasks[sorted[1]].ID)
	}
	if tasks[sorted[2]].ID != "t_high" {
		t.Errorf("third: got %s, want t_high", tasks[sorted[2]].ID)
	}
}

func TestSortPendingTasks_TieBreakers(t *testing.T) {
	now := time.Now().UTC()

	cfg := model.Config{
		Queue: model.QueueConfig{PriorityAgingSec: 0},
	}
	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)

	// Same priority, different created_at
	tasks := []model.Task{
		{ID: "t_b", Priority: 1, Status: model.StatusPending, CreatedAt: now.Add(time.Second).Format(time.RFC3339)},
		{ID: "t_a", Priority: 1, Status: model.StatusPending, CreatedAt: now.Format(time.RFC3339)},
	}

	sorted := d.SortPendingTasks(tasks)
	if tasks[sorted[0]].ID != "t_a" {
		t.Errorf("first: got %s, want t_a (earlier created_at)", tasks[sorted[0]].ID)
	}
}

func TestSortPendingTasks_IDTieBreaker(t *testing.T) {
	now := time.Now().UTC()
	created := now.Format(time.RFC3339)

	cfg := model.Config{}
	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)

	tasks := []model.Task{
		{ID: "t_z", Priority: 1, Status: model.StatusPending, CreatedAt: created},
		{ID: "t_a", Priority: 1, Status: model.StatusPending, CreatedAt: created},
	}

	sorted := d.SortPendingTasks(tasks)
	if tasks[sorted[0]].ID != "t_a" {
		t.Errorf("first: got %s, want t_a (lexicographic)", tasks[sorted[0]].ID)
	}
}

func TestSortPendingCommands(t *testing.T) {
	now := time.Now().UTC()

	cfg := model.Config{}
	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)

	commands := []model.Command{
		{ID: "c2", Priority: 3, Status: model.StatusPending, CreatedAt: now.Format(time.RFC3339)},
		{ID: "c1", Priority: 1, Status: model.StatusPending, CreatedAt: now.Format(time.RFC3339)},
	}

	sorted := d.SortPendingCommands(commands)
	if len(sorted) != 2 {
		t.Fatalf("expected 2, got %d", len(sorted))
	}
	if commands[sorted[0]].ID != "c1" {
		t.Errorf("first: got %s, want c1", commands[sorted[0]].ID)
	}
}

func TestSortPendingNotifications(t *testing.T) {
	now := time.Now().UTC()

	cfg := model.Config{}
	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)

	notifications := []model.Notification{
		{ID: "n2", Priority: 5, Status: model.StatusPending, CreatedAt: now.Format(time.RFC3339)},
		{ID: "n1", Priority: 1, Status: model.StatusPending, CreatedAt: now.Format(time.RFC3339)},
		{ID: "n3", Priority: 3, Status: model.StatusCompleted, CreatedAt: now.Format(time.RFC3339)},
	}

	sorted := d.SortPendingNotifications(notifications)
	if len(sorted) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(sorted))
	}
	if notifications[sorted[0]].ID != "n1" {
		t.Errorf("first: got %s, want n1", notifications[sorted[0]].ID)
	}
}

func TestEffectivePriority_Aging(t *testing.T) {
	// 5 minutes old, aging every 60 seconds → effective = max(0, 3 - 5) = 0
	created := time.Now().Add(-5 * time.Minute).Format(time.RFC3339)
	ep := EffectivePriority(3, created, 60)
	if ep != 0 {
		t.Errorf("expected 0, got %d", ep)
	}
}

func TestSortPendingIndices_EmptySlice(t *testing.T) {
	result := sortPendingIndices([]model.Task{}, func(t model.Task) sortKey {
		return sortKey{Status: t.Status, Priority: t.Priority, CreatedAt: t.CreatedAt, ID: t.ID}
	}, 0)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d", len(result))
	}
}

func TestSortPendingIndices_AllNonPending(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	items := []model.Task{
		{ID: "t1", Status: model.StatusInProgress, Priority: 1, CreatedAt: now},
		{ID: "t2", Status: model.StatusCompleted, Priority: 2, CreatedAt: now},
		{ID: "t3", Status: model.StatusFailed, Priority: 3, CreatedAt: now},
	}
	result := sortPendingIndices(items, func(t model.Task) sortKey {
		return sortKey{Status: t.Status, Priority: t.Priority, CreatedAt: t.CreatedAt, ID: t.ID}
	}, 0)
	if len(result) != 0 {
		t.Errorf("expected no pending items, got %d", len(result))
	}
}

func TestSortPendingIndices_MixedStatuses(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	items := []model.Command{
		{ID: "c1", Status: model.StatusInProgress, Priority: 1, CreatedAt: now},
		{ID: "c2", Status: model.StatusPending, Priority: 3, CreatedAt: now},
		{ID: "c3", Status: model.StatusPending, Priority: 1, CreatedAt: now},
		{ID: "c4", Status: model.StatusCompleted, Priority: 0, CreatedAt: now},
	}
	result := sortPendingIndices(items, func(c model.Command) sortKey {
		return sortKey{Status: c.Status, Priority: c.Priority, CreatedAt: c.CreatedAt, ID: c.ID}
	}, 0)
	if len(result) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(result))
	}
	// c3 (priority 1) before c2 (priority 3)
	if items[result[0]].ID != "c3" {
		t.Errorf("first: got %s, want c3", items[result[0]].ID)
	}
	if items[result[1]].ID != "c2" {
		t.Errorf("second: got %s, want c2", items[result[1]].ID)
	}
}

func TestSortPendingIndices_PreservesOriginalIndices(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	items := []model.Notification{
		{ID: "n_skip", Status: model.StatusCompleted, Priority: 0, CreatedAt: now},
		{ID: "n_high", Status: model.StatusPending, Priority: 5, CreatedAt: now},
		{ID: "n_skip2", Status: model.StatusInProgress, Priority: 0, CreatedAt: now},
		{ID: "n_low", Status: model.StatusPending, Priority: 1, CreatedAt: now},
	}
	result := sortPendingIndices(items, func(n model.Notification) sortKey {
		return sortKey{Status: n.Status, Priority: n.Priority, CreatedAt: n.CreatedAt, ID: n.ID}
	}, 0)
	if len(result) != 2 {
		t.Fatalf("expected 2, got %d", len(result))
	}
	// Verify original indices are preserved correctly
	if result[0] != 3 { // n_low is at index 3
		t.Errorf("first index: got %d, want 3", result[0])
	}
	if result[1] != 1 { // n_high is at index 1
		t.Errorf("second index: got %d, want 1", result[1])
	}
}

func TestSortPendingIndices_WithAging(t *testing.T) {
	now := time.Now().UTC()
	items := []model.Task{
		{ID: "t_new_high", Status: model.StatusPending, Priority: 5, CreatedAt: now.Format(time.RFC3339)},
		{ID: "t_old_high", Status: model.StatusPending, Priority: 5, CreatedAt: now.Add(-3 * time.Minute).Format(time.RFC3339)},
	}
	// With aging every 60s: t_old_high effective = max(0, 5-3) = 2, t_new_high effective = 5
	result := sortPendingIndices(items, func(t model.Task) sortKey {
		return sortKey{Status: t.Status, Priority: t.Priority, CreatedAt: t.CreatedAt, ID: t.ID}
	}, 60)
	if len(result) != 2 {
		t.Fatalf("expected 2, got %d", len(result))
	}
	if items[result[0]].ID != "t_old_high" {
		t.Errorf("first: got %s, want t_old_high (aged priority should be lower)", items[result[0]].ID)
	}
}

func TestSortPendingIndices_ConsistentAcrossTypes(t *testing.T) {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	ts2 := now.Add(time.Second).Format(time.RFC3339)

	// Verify that tasks, commands, and notifications produce identical ordering
	// for the same priority/created_at/id combinations
	taskResult := sortPendingIndices([]model.Task{
		{ID: "b", Status: model.StatusPending, Priority: 1, CreatedAt: ts2},
		{ID: "a", Status: model.StatusPending, Priority: 1, CreatedAt: ts},
	}, func(t model.Task) sortKey {
		return sortKey{Status: t.Status, Priority: t.Priority, CreatedAt: t.CreatedAt, ID: t.ID}
	}, 0)

	cmdResult := sortPendingIndices([]model.Command{
		{ID: "b", Status: model.StatusPending, Priority: 1, CreatedAt: ts2},
		{ID: "a", Status: model.StatusPending, Priority: 1, CreatedAt: ts},
	}, func(c model.Command) sortKey {
		return sortKey{Status: c.Status, Priority: c.Priority, CreatedAt: c.CreatedAt, ID: c.ID}
	}, 0)

	ntfResult := sortPendingIndices([]model.Notification{
		{ID: "b", Status: model.StatusPending, Priority: 1, CreatedAt: ts2},
		{ID: "a", Status: model.StatusPending, Priority: 1, CreatedAt: ts},
	}, func(n model.Notification) sortKey {
		return sortKey{Status: n.Status, Priority: n.Priority, CreatedAt: n.CreatedAt, ID: n.ID}
	}, 0)

	// All should sort "a" (earlier created_at) first → index 1
	for name, result := range map[string][]int{"task": taskResult, "command": cmdResult, "notification": ntfResult} {
		if len(result) != 2 {
			t.Fatalf("%s: expected 2, got %d", name, len(result))
		}
		if result[0] != 1 {
			t.Errorf("%s: first should be index 1 (id=a), got %d", name, result[0])
		}
	}
}
