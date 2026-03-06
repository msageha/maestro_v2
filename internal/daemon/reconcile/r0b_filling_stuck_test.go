package reconcile

import (
	"os"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

func TestR0bFillingStuck_RevertToAwaitingFill(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R0bFillingStuck{})

	fillingStarted := h.ts(-5 * time.Minute)
	state := model.CommandState{
		CommandID:        "cmd_r0b_01",
		PlanStatus:       model.PlanStatusSealed,
		RequiredTaskIDs:  []string{"task_1", "task_2"},
		ExpectedTaskCount: 2,
		TaskStates: map[string]model.Status{
			"task_1": model.StatusPending,
			"task_2": model.StatusPending,
		},
		Phases: []model.Phase{
			{
				PhaseID:          "ph1",
				Name:             "phase_1",
				Status:           model.PhaseStatusFilling,
				TaskIDs:          []string{"task_1", "task_2"},
				FillingStartedAt: &fillingStarted,
			},
		},
		CreatedAt: h.ts(-10 * time.Minute),
		UpdatedAt: h.ts(-5 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r0b_01"), state)

	// Create a worker queue with those tasks.
	tq := model.TaskQueue{
		Tasks: []model.Task{
			{ID: "task_1", CommandID: "cmd_r0b_01", Status: model.StatusPending},
			{ID: "task_2", CommandID: "cmd_r0b_01", Status: model.StatusPending},
		},
	}
	h.mustWriteYAML(h.workerQueuePath("worker1"), tq)

	repairs, notifications := h.engine.Reconcile()

	if len(repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(repairs))
	}
	if repairs[0].Pattern != "R0b" {
		t.Errorf("expected pattern R0b, got %s", repairs[0].Pattern)
	}

	// Verify state was reverted.
	data, err := os.ReadFile(h.statePath("cmd_r0b_01"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var updated model.CommandState
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if updated.Phases[0].Status != model.PhaseStatusAwaitingFill {
		t.Errorf("expected phase status awaiting_fill, got %s", updated.Phases[0].Status)
	}
	if len(updated.Phases[0].TaskIDs) != 0 {
		t.Errorf("expected 0 task IDs in phase, got %d", len(updated.Phases[0].TaskIDs))
	}
	if len(updated.TaskStates) != 0 {
		t.Errorf("expected 0 task states, got %d", len(updated.TaskStates))
	}
	if updated.ExpectedTaskCount != 0 {
		t.Errorf("expected expected_task_count 0, got %d", updated.ExpectedTaskCount)
	}

	// Tasks should be removed from worker queue.
	qData, err := os.ReadFile(h.workerQueuePath("worker1"))
	if err != nil {
		t.Fatalf("read worker queue: %v", err)
	}
	var updatedQ model.TaskQueue
	if err := yamlv3.Unmarshal(qData, &updatedQ); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	if len(updatedQ.Tasks) != 0 {
		t.Errorf("expected 0 tasks in worker queue, got %d", len(updatedQ.Tasks))
	}

	// No notifications without ExecutorFactory.
	if len(notifications) != 0 {
		t.Errorf("expected 0 notifications, got %d", len(notifications))
	}

	// Idempotency.
	repairs2, _ := h.engine.Reconcile()
	if len(repairs2) != 0 {
		t.Fatalf("expected idempotent second pass, got %d repairs", len(repairs2))
	}
}

func TestR0bFillingStuck_NotStuckYet(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R0bFillingStuck{})

	// filling_started_at only 30 seconds ago (threshold is 120s).
	fillingStarted := h.ts(-30 * time.Second)
	state := model.CommandState{
		CommandID:  "cmd_r0b_02",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID:          "ph1",
				Name:             "phase_1",
				Status:           model.PhaseStatusFilling,
				FillingStartedAt: &fillingStarted,
			},
		},
		CreatedAt: h.ts(-10 * time.Minute),
		UpdatedAt: h.ts(-30 * time.Second),
	}
	h.mustWriteYAML(h.statePath("cmd_r0b_02"), state)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 0 {
		t.Fatalf("expected 0 repairs for not-stuck-yet phase, got %d", len(repairs))
	}
}

func TestR0bFillingStuck_WithNotification(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R0bFillingStuck{})

	// Set ExecutorFactory to enable notifications.
	h.engine.SetExecutorFactory(func(string, model.WatcherConfig, string) (core.AgentExecutor, error) {
		return nil, nil
	})

	fillingStarted := h.ts(-5 * time.Minute)
	state := model.CommandState{
		CommandID:  "cmd_r0b_03",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID:          "ph1",
				Name:             "phase_1",
				Status:           model.PhaseStatusFilling,
				FillingStartedAt: &fillingStarted,
			},
		},
		CreatedAt: h.ts(-10 * time.Minute),
		UpdatedAt: h.ts(-5 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r0b_03"), state)

	_, notifications := h.engine.Reconcile()

	if len(notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifications))
	}
	if notifications[0].Kind != "re_fill" {
		t.Errorf("expected notification kind re_fill, got %s", notifications[0].Kind)
	}
	if notifications[0].CommandID != "cmd_r0b_03" {
		t.Errorf("expected command cmd_r0b_03, got %s", notifications[0].CommandID)
	}
}
