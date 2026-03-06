package reconcile

import (
	"os"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestR6FillTimeout_DeadlineExpired(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R6FillTimeout{})

	deadline := now.Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r6_01",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID:        "ph1",
				Name:           "phase_1",
				Status:         model.PhaseStatusAwaitingFill,
				FillDeadlineAt: &deadline,
			},
		},
		CreatedAt: h.ts(-10 * time.Minute),
		UpdatedAt: h.ts(-10 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r6_01"), state)

	repairs, notifications := h.engine.Reconcile()

	if len(repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d", len(repairs))
	}
	if repairs[0].Pattern != "R6" {
		t.Errorf("expected pattern R6, got %s", repairs[0].Pattern)
	}
	if repairs[0].CommandID != "cmd_r6_01" {
		t.Errorf("expected command cmd_r6_01, got %s", repairs[0].CommandID)
	}

	// Verify state was updated to timed_out.
	data, err := os.ReadFile(h.statePath("cmd_r6_01"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var updated model.CommandState
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if updated.Phases[0].Status != model.PhaseStatusTimedOut {
		t.Errorf("expected phase status timed_out, got %s", updated.Phases[0].Status)
	}

	// Notifications should be empty (no ExecutorFactory).
	if len(notifications) != 0 {
		t.Errorf("expected 0 notifications without ExecutorFactory, got %d", len(notifications))
	}

	// Idempotency: second pass should produce no repairs.
	repairs2, _ := h.engine.Reconcile()
	if len(repairs2) != 0 {
		t.Fatalf("expected idempotent second pass, got %d repairs", len(repairs2))
	}
}

func TestR6FillTimeout_NotExpired(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R6FillTimeout{})

	deadline := now.Add(10 * time.Minute).UTC().Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r6_02",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID:        "ph1",
				Name:           "phase_1",
				Status:         model.PhaseStatusAwaitingFill,
				FillDeadlineAt: &deadline,
			},
		},
		CreatedAt: h.ts(-10 * time.Minute),
		UpdatedAt: h.ts(-10 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r6_02"), state)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 0 {
		t.Fatalf("expected 0 repairs for non-expired deadline, got %d", len(repairs))
	}
}

func TestR6FillTimeout_CascadeCancel(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R6FillTimeout{})

	deadline := now.Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	state := model.CommandState{
		CommandID:  "cmd_r6_03",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID:        "ph1",
				Name:           "phase_1",
				Status:         model.PhaseStatusAwaitingFill,
				FillDeadlineAt: &deadline,
			},
			{
				PhaseID:         "ph2",
				Name:            "phase_2",
				Status:          model.PhaseStatusPending,
				DependsOnPhases: []string{"phase_1"},
			},
			{
				PhaseID:         "ph3",
				Name:            "phase_3",
				Status:          model.PhaseStatusPending,
				DependsOnPhases: []string{"phase_2"},
			},
		},
		CreatedAt: h.ts(-10 * time.Minute),
		UpdatedAt: h.ts(-10 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r6_03"), state)

	repairs, _ := h.engine.Reconcile()

	// phase_1 timed out + phase_2 cancelled + phase_3 cancelled = 3 repairs.
	if len(repairs) != 3 {
		t.Fatalf("expected 3 repairs (timeout + 2 cascade), got %d", len(repairs))
	}

	data, err := os.ReadFile(h.statePath("cmd_r6_03"))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var updated model.CommandState
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}

	expectedStatuses := map[string]model.PhaseStatus{
		"phase_1": model.PhaseStatusTimedOut,
		"phase_2": model.PhaseStatusCancelled,
		"phase_3": model.PhaseStatusCancelled,
	}
	for _, phase := range updated.Phases {
		want, ok := expectedStatuses[phase.Name]
		if !ok {
			continue
		}
		if phase.Status != want {
			t.Errorf("phase %s: expected status %s, got %s", phase.Name, want, phase.Status)
		}
	}

	// Idempotency.
	repairs2, _ := h.engine.Reconcile()
	if len(repairs2) != 0 {
		t.Fatalf("expected idempotent second pass, got %d repairs", len(repairs2))
	}
}

func TestR6FillTimeout_NoDeadline(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, now, R6FillTimeout{})

	state := model.CommandState{
		CommandID:  "cmd_r6_04",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID: "ph1",
				Name:    "phase_1",
				Status:  model.PhaseStatusAwaitingFill,
				// No FillDeadlineAt set.
			},
		},
		CreatedAt: h.ts(-10 * time.Minute),
		UpdatedAt: h.ts(-10 * time.Minute),
	}
	h.mustWriteYAML(h.statePath("cmd_r6_04"), state)

	repairs, _ := h.engine.Reconcile()
	if len(repairs) != 0 {
		t.Fatalf("expected 0 repairs for phase without deadline, got %d", len(repairs))
	}
}
