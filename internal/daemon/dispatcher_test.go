package daemon

import (
	"bytes"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

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

	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

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
	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

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
	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

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
	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

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
	d := NewDispatcher("", cfg, nil, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

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
