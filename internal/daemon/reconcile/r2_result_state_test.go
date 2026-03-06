package reconcile

import (
	"os"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestR2ResultState_RepairNonTerminal(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R2ResultState{})

	// State has task in non-terminal status.
	state := model.CommandState{
		CommandID:  "cmd_r2_01",
		PlanStatus: model.PlanStatusSealed,
		TaskStates: map[string]model.Status{
			"task_1": model.StatusInProgress,
		},
		AppliedResultIDs: map[string]string{},
		CreatedAt:        h.ts(-10 * time.Minute),
		UpdatedAt:        h.ts(-5 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r2_01"), state)

	// Result file shows task completed.
	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{
				ID:        "res_1",
				TaskID:    "task_1",
				CommandID: "cmd_r2_01",
				Status:    model.StatusCompleted,
			},
		},
	}
	h.mustWriteYAML(h.workerResultPath("worker1"), rf)

	repairs, _ := h.engine.Reconcile()

	if len(repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(repairs))
	}
	if repairs[0].Pattern != "R2" {
		t.Errorf("expected pattern R2, got %s", repairs[0].Pattern)
	}

	// Verify state was updated.
	data, err := os.ReadFile(h.statePath("cmd_r2_01"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var updated model.CommandState
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if updated.TaskStates["task_1"] != model.StatusCompleted {
		t.Errorf("expected task_1 status completed, got %s", updated.TaskStates["task_1"])
	}
	if updated.AppliedResultIDs["task_1"] != "res_1" {
		t.Errorf("expected applied_result_id res_1, got %s", updated.AppliedResultIDs["task_1"])
	}

	// Idempotency.
	repairs2, _ := h.engine.Reconcile()
	if len(repairs2) != 0 {
		t.Fatalf("expected idempotent second pass, got %d repairs", len(repairs2))
	}
}

func TestR2ResultState_AlreadyTerminal(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R2ResultState{})

	state := model.CommandState{
		CommandID:  "cmd_r2_02",
		PlanStatus: model.PlanStatusSealed,
		TaskStates: map[string]model.Status{
			"task_1": model.StatusCompleted,
		},
		AppliedResultIDs: map[string]string{
			"task_1": "res_1",
		},
		CreatedAt: h.ts(-10 * time.Minute),
		UpdatedAt: h.ts(-5 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r2_02"), state)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{
				ID:        "res_1",
				TaskID:    "task_1",
				CommandID: "cmd_r2_02",
				Status:    model.StatusCompleted,
			},
		},
	}
	h.mustWriteYAML(h.workerResultPath("worker1"), rf)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 0 {
		t.Fatalf("expected 0 repairs for already-terminal state, got %d", len(repairs))
	}
}

func TestR2ResultState_MultipleTasksMultipleWorkers(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R2ResultState{})

	state := model.CommandState{
		CommandID:  "cmd_r2_03",
		PlanStatus: model.PlanStatusSealed,
		TaskStates: map[string]model.Status{
			"task_1": model.StatusInProgress,
			"task_2": model.StatusPending,
		},
		AppliedResultIDs: map[string]string{},
		CreatedAt:        h.ts(-10 * time.Minute),
		UpdatedAt:        h.ts(-5 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r2_03"), state)

	rf1 := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res_1", TaskID: "task_1", CommandID: "cmd_r2_03", Status: model.StatusCompleted},
		},
	}
	h.mustWriteYAML(h.workerResultPath("worker1"), rf1)

	rf2 := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res_2", TaskID: "task_2", CommandID: "cmd_r2_03", Status: model.StatusFailed},
		},
	}
	h.mustWriteYAML(h.workerResultPath("worker2"), rf2)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 2 {
		t.Fatalf("expected 2 repairs, got %d", len(repairs))
	}

	data, err := os.ReadFile(h.statePath("cmd_r2_03"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var updated model.CommandState
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if updated.TaskStates["task_1"] != model.StatusCompleted {
		t.Errorf("expected task_1 completed, got %s", updated.TaskStates["task_1"])
	}
	if updated.TaskStates["task_2"] != model.StatusFailed {
		t.Errorf("expected task_2 failed, got %s", updated.TaskStates["task_2"])
	}
}

func TestR2ResultState_NilMapsInitialized(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R2ResultState{})

	// State with nil TaskStates and AppliedResultIDs (covers nil-map init branch).
	state := model.CommandState{
		CommandID:  "cmd_r2_04",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  h.ts(-10 * time.Minute),
		UpdatedAt:  h.ts(-5 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r2_04"), state)

	rf := model.TaskResultFile{
		Results: []model.TaskResult{
			{ID: "res_1", TaskID: "task_1", CommandID: "cmd_r2_04", Status: model.StatusCompleted},
		},
	}
	h.mustWriteYAML(h.workerResultPath("worker1"), rf)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(repairs))
	}

	data, err := os.ReadFile(h.statePath("cmd_r2_04"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var updated model.CommandState
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if updated.TaskStates["task_1"] != model.StatusCompleted {
		t.Errorf("expected task_1 completed, got %s", updated.TaskStates["task_1"])
	}
	if updated.AppliedResultIDs["task_1"] != "res_1" {
		t.Errorf("expected applied_result_id res_1, got %s", updated.AppliedResultIDs["task_1"])
	}
}
