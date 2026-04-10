package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// stallTestSetup wires a QueueHandler with worktree manager + state reader,
// seeds a single command with one terminal task and one terminal phase, and
// returns the qh + scanState ready for stepWorktreeStallDetection.
func stallTestSetup(t *testing.T, cmdUpdatedAt string, integrationStatus model.IntegrationStatus, stallSignaled bool) (*QueueHandler, *scanState) {
	t.Helper()
	maestroDir := setupScanPhaseTestDir(t)

	wtCfg := model.WorktreeConfig{
		Enabled: true,
	}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	// Seed worktree state
	writeWorktreeState(t, maestroDir, "cmd1", integrationStatus)
	if stallSignaled {
		state, err := qh.worktreeManager.GetCommandState("cmd1")
		if err != nil {
			t.Fatalf("get state: %v", err)
		}
		state.Integration.StallSignaled = true
		// Persist via the manager's helper-by-side-effect: rewrite directly.
		if err := qh.worktreeManager.MarkIntegrationStallSignaled("cmd1"); err != nil {
			t.Fatalf("mark stall signaled: %v", err)
		}
		_ = state
	}

	// Seed command state with one terminal task + one terminal phase
	taskStates := map[string]model.Status{
		"t1": model.StatusCompleted,
	}
	phases := []model.Phase{
		{
			PhaseID: "phase1",
			Name:    "phase1",
			Status:  model.PhaseStatusCompleted,
			TaskIDs: []string{"t1"},
		},
	}
	writeCommandState(t, maestroDir, "cmd1", taskStates, phases)

	// Build in-memory task queue with the terminal task
	taskQueues := makeTaskQueues(map[string][]model.Task{
		"worker1": {
			{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
		},
	})

	s := &scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{
						ID:        "cmd1",
						Status:    model.StatusInProgress,
						CreatedAt: cmdUpdatedAt,
						UpdatedAt: cmdUpdatedAt,
					},
				},
			},
		},
		tasks:       taskQueues,
		taskDirty:   make(map[string]bool),
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: make(map[signalKey]struct{}),
	}
	return qh, s
}

func TestStepWorktreeStallDetection_FiresAfterTimeout(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	qh, s := stallTestSetup(t, past, model.IntegrationStatusCreated, false)

	qh.stepWorktreeStallDetection(s)

	if len(s.signals.Data.Signals) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(s.signals.Data.Signals))
	}
	sig := s.signals.Data.Signals[0]
	if sig.Kind != "worktree_stalled" {
		t.Errorf("kind = %q, want worktree_stalled", sig.Kind)
	}
	if sig.CommandID != "cmd1" {
		t.Errorf("command_id = %q, want cmd1", sig.CommandID)
	}
	if sig.Reason != "integration_stalled:created" {
		t.Errorf("reason = %q, want integration_stalled:created", sig.Reason)
	}

	// stall_signaled flag should be persisted on the integration state
	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !state.Integration.StallSignaled {
		t.Errorf("StallSignaled flag was not persisted")
	}
	if state.Integration.Status != model.IntegrationStatusCreated {
		t.Errorf("integration status changed to %q, want unchanged created", state.Integration.Status)
	}
}

func TestStepWorktreeStallDetection_DoesNotFireWithinTimeout(t *testing.T) {
	recent := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	qh, s := stallTestSetup(t, recent, model.IntegrationStatusCreated, false)

	qh.stepWorktreeStallDetection(s)

	if len(s.signals.Data.Signals) != 0 {
		t.Fatalf("expected 0 signals (within timeout), got %d", len(s.signals.Data.Signals))
	}
	state, _ := qh.worktreeManager.GetCommandState("cmd1")
	if state.Integration.StallSignaled {
		t.Errorf("StallSignaled should not be set")
	}
}

func TestStepWorktreeStallDetection_NoReFireWhenAlreadySignaled(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	qh, s := stallTestSetup(t, past, model.IntegrationStatusCreated, true)

	qh.stepWorktreeStallDetection(s)

	if len(s.signals.Data.Signals) != 0 {
		t.Fatalf("expected 0 signals (already signaled), got %d", len(s.signals.Data.Signals))
	}
}

func TestStepWorktreeStallDetection_DeliveryFailureMarksIntegrationFailed(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	qh, s := stallTestSetup(t, past, model.IntegrationStatusCreated, false)

	// Inject a failing mark function to simulate signal persistence failure.
	qh.worktreeStallMarkFn = func(commandID string) error {
		return errors.New("inject: cannot persist stall flag")
	}

	qh.stepWorktreeStallDetection(s)

	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusFailed {
		t.Fatalf("integration status = %q, want failed", state.Integration.Status)
	}
	if state.Integration.StallSignaled {
		t.Errorf("StallSignaled should not be set when delivery failed")
	}
}
