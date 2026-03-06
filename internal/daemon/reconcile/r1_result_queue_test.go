package reconcile

import (
	"os"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestR1ResultQueue_UpdateQueueToTerminal(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R1ResultQueue{})

	// Worker queue has task in_progress.
	tq := model.TaskQueue{
		Tasks: []model.Task{
			{ID: "task_1", CommandID: "cmd_r1_01", Status: model.StatusInProgress, UpdatedAt: h.ts(-5 * time.Minute)},
		},
	}
	h.mustWriteYAML(h.workerQueuePath("worker1"), tq)

	// Result shows task completed.
	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res_1", TaskID: "task_1", CommandID: "cmd_r1_01", Status: model.StatusCompleted},
		},
	}
	h.mustWriteYAML(h.workerResultPath("worker1"), rf)

	// Create state file so UpdateLastReconciledAt doesn't fail silently.
	state := model.CommandState{
		CommandID:  "cmd_r1_01",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  h.ts(-10 * time.Minute),
		UpdatedAt:  h.ts(-5 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r1_01"), state)

	repairs, _ := h.engine.Reconcile()

	if len(repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(repairs))
	}
	if repairs[0].Pattern != "R1" {
		t.Errorf("expected pattern R1, got %s", repairs[0].Pattern)
	}

	// Verify queue was updated.
	data, err := os.ReadFile(h.workerQueuePath("worker1"))
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var updated model.TaskQueue
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if updated.Tasks[0].Status != model.StatusCompleted {
		t.Errorf("expected task status completed, got %s", updated.Tasks[0].Status)
	}
	if updated.Tasks[0].LeaseOwner != nil {
		t.Errorf("expected lease_owner nil, got %v", updated.Tasks[0].LeaseOwner)
	}

	// Idempotency.
	repairs2, _ := h.engine.Reconcile()
	if len(repairs2) != 0 {
		t.Fatalf("expected idempotent second pass, got %d repairs", len(repairs2))
	}
}

func TestR1ResultQueue_NoMismatch(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R1ResultQueue{})

	// Queue already at terminal.
	tq := model.TaskQueue{
		Tasks: []model.Task{
			{ID: "task_1", CommandID: "cmd_r1_02", Status: model.StatusCompleted, UpdatedAt: h.ts(-5 * time.Minute)},
		},
	}
	h.mustWriteYAML(h.workerQueuePath("worker1"), tq)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res_1", TaskID: "task_1", CommandID: "cmd_r1_02", Status: model.StatusCompleted},
		},
	}
	h.mustWriteYAML(h.workerResultPath("worker1"), rf)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 0 {
		t.Fatalf("expected 0 repairs for no mismatch, got %d", len(repairs))
	}
}
