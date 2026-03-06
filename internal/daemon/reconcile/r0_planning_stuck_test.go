package reconcile

import (
	"os"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestR0PlanningStuck_DeletesStateAndCleansQueue(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R0PlanningStuck{})

	// State stuck in planning for 5 minutes (threshold = 60*2 = 120s).
	state := model.CommandState{
		CommandID:  "cmd_r0_01",
		PlanStatus: model.PlanStatusPlanning,
		CreatedAt:  h.ts(-5 * time.Minute),
		UpdatedAt:  h.ts(-5 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r0_01"), state)

	// Planner queue with the stuck command + an unrelated command.
	cq := model.CommandQueue{
		Commands: []model.Command{
			{ID: "cmd_r0_01", Status: model.StatusInProgress, UpdatedAt: h.ts(-5 * time.Minute)},
			{ID: "cmd_other", Status: model.StatusPending, UpdatedAt: h.ts(-1 * time.Minute)},
		},
	}
	h.mustWriteYAML(h.plannerQueuePath(), cq)

	// Worker queue with tasks for this command + an unrelated task.
	tq := model.TaskQueue{
		Tasks: []model.Task{
			{ID: "task_1", CommandID: "cmd_r0_01", Status: model.StatusPending},
			{ID: "task_other", CommandID: "cmd_other", Status: model.StatusPending},
		},
	}
	h.mustWriteYAML(h.workerQueuePath("worker1"), tq)

	repairs, _ := h.engine.Reconcile()

	if len(repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(repairs))
	}
	if repairs[0].Pattern != "R0" {
		t.Errorf("expected pattern R0, got %s", repairs[0].Pattern)
	}

	// State file should be deleted.
	h.mustNotExist(h.statePath("cmd_r0_01"))

	// Command should be removed from planner queue; unrelated command should remain.
	data, err := os.ReadFile(h.plannerQueuePath())
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}
	var updatedCQ model.CommandQueue
	if err := yamlv3.Unmarshal(data, &updatedCQ); err != nil {
		t.Fatalf("unmarshal planner queue: %v", err)
	}
	if len(updatedCQ.Commands) != 1 {
		t.Fatalf("expected 1 command remaining in planner queue, got %d", len(updatedCQ.Commands))
	}
	if updatedCQ.Commands[0].ID != "cmd_other" {
		t.Errorf("expected remaining command cmd_other, got %s", updatedCQ.Commands[0].ID)
	}

	// Worker queue: stuck command's task removed, unrelated task remains.
	wqData, err := os.ReadFile(h.workerQueuePath("worker1"))
	if err != nil {
		t.Fatalf("read worker queue: %v", err)
	}
	var updatedTQ model.TaskQueue
	if err := yamlv3.Unmarshal(wqData, &updatedTQ); err != nil {
		t.Fatalf("unmarshal worker queue: %v", err)
	}
	if len(updatedTQ.Tasks) != 1 {
		t.Fatalf("expected 1 task remaining in worker queue, got %d", len(updatedTQ.Tasks))
	}
	if updatedTQ.Tasks[0].ID != "task_other" {
		t.Errorf("expected remaining task task_other, got %s", updatedTQ.Tasks[0].ID)
	}

	// Idempotency.
	repairs2, _ := h.engine.Reconcile()
	if len(repairs2) != 0 {
		t.Fatalf("expected idempotent second pass, got %d repairs", len(repairs2))
	}
}

func TestR0PlanningStuck_NotStuckYet(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R0PlanningStuck{})

	// Only 30 seconds old.
	state := model.CommandState{
		CommandID:  "cmd_r0_02",
		PlanStatus: model.PlanStatusPlanning,
		CreatedAt:  h.ts(-30 * time.Second),
		UpdatedAt:  h.ts(-30 * time.Second),
	}
	h.mustWriteYAML(h.statePath("cmd_r0_02"), state)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 0 {
		t.Fatalf("expected 0 repairs for not-stuck-yet state, got %d", len(repairs))
	}
}

func TestR0PlanningStuck_NonPlanningStatusSkipped(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R0PlanningStuck{})

	state := model.CommandState{
		CommandID:  "cmd_r0_03",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  h.ts(-10 * time.Minute),
		UpdatedAt:  h.ts(-10 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r0_03"), state)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 0 {
		t.Fatalf("expected 0 repairs for non-planning state, got %d", len(repairs))
	}
}
