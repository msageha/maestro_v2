package reconcile

import (
	"os"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestR0Dispatch_StuckReverted(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R0Dispatch{})

	// Command in_progress but no state file, updated 15 minutes ago (threshold = max(120s, 10min) = 10min).
	cq := model.CommandQueue{
		Commands: []model.Command{
			{
				ID:        "cmd_rd_01",
				Status:    model.StatusInProgress,
				UpdatedAt: h.ts(-15 * time.Minute),
			},
		},
	}
	h.mustWriteYAML(h.plannerQueuePath(), cq)

	repairs, _ := h.engine.Reconcile()

	if len(repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(repairs))
	}
	if repairs[0].Pattern != "R0-dispatch" {
		t.Errorf("expected pattern R0-dispatch, got %s", repairs[0].Pattern)
	}

	// Verify command was reverted to pending.
	data, err := os.ReadFile(h.plannerQueuePath())
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var updated model.CommandQueue
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if updated.Commands[0].Status != model.StatusPending {
		t.Errorf("expected status pending, got %s", updated.Commands[0].Status)
	}
	if updated.Commands[0].LeaseOwner != nil {
		t.Errorf("expected nil lease_owner")
	}
	if updated.Commands[0].LastError == nil {
		t.Errorf("expected last_error to be set")
	}

	// Idempotency: second pass should not repair (now it's pending).
	repairs2, _ := h.engine.Reconcile()
	if len(repairs2) != 0 {
		t.Fatalf("expected idempotent second pass, got %d repairs", len(repairs2))
	}
}

func TestR0Dispatch_StateFileExists(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R0Dispatch{})

	cq := model.CommandQueue{
		Commands: []model.Command{
			{
				ID:        "cmd_rd_02",
				Status:    model.StatusInProgress,
				UpdatedAt: h.ts(-15 * time.Minute),
			},
		},
	}
	h.mustWriteYAML(h.plannerQueuePath(), cq)

	// State file exists, so no repair.
	state := model.CommandState{
		CommandID:  "cmd_rd_02",
		PlanStatus: model.PlanStatusPlanning,
		CreatedAt:  h.ts(-15 * time.Minute),
		UpdatedAt:  h.ts(-15 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_rd_02"), state)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 0 {
		t.Fatalf("expected 0 repairs when state file exists, got %d", len(repairs))
	}
}

func TestR0Dispatch_NotOldEnough(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R0Dispatch{})

	cq := model.CommandQueue{
		Commands: []model.Command{
			{
				ID:        "cmd_rd_03",
				Status:    model.StatusInProgress,
				UpdatedAt: h.ts(-5 * time.Minute), // 5 min < 10 min threshold.
			},
		},
	}
	h.mustWriteYAML(h.plannerQueuePath(), cq)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 0 {
		t.Fatalf("expected 0 repairs for not-old-enough command, got %d", len(repairs))
	}
}
