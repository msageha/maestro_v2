package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// setWorkerResolvingForTest mutates the on-disk worktree state so the named
// worker is in the resolving status, with UpdatedAt back-dated to updatedAt.
// Used by stepResolvingWorkerStallDetection tests to drive the stall path.
func setWorkerResolvingForTest(t *testing.T, maestroDir, commandID, workerID, updatedAt string) {
	t.Helper()
	path := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read worktree state: %v", err)
	}
	var state model.WorktreeCommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse worktree state: %v", err)
	}
	found := false
	for i := range state.Workers {
		if state.Workers[i].WorkerID == workerID {
			state.Workers[i].Status = model.WorktreeStatusResolving
			state.Workers[i].UpdatedAt = updatedAt
			found = true
		}
	}
	if !found {
		t.Fatalf("worker %s not found in worktree state", workerID)
	}
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write worktree state: %v", err)
	}
}

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	qh, s := stallTestSetup(t, past, model.IntegrationStatusCreated, true)

	qh.stepWorktreeStallDetection(s)

	if len(s.signals.Data.Signals) != 0 {
		t.Fatalf("expected 0 signals (already signaled), got %d", len(s.signals.Data.Signals))
	}
}

// stallTestSetupNoPhases wires a QueueHandler similar to stallTestSetup but
// does NOT write any command state file, so GetCommandPhases returns
// ErrStateNotFound and the noPhases fast-path is triggered.
func stallTestSetupNoPhases(t *testing.T, cmdUpdatedAt string, integrationStatus model.IntegrationStatus) (*QueueHandler, *scanState) {
	t.Helper()
	maestroDir := setupScanPhaseTestDir(t)

	wtCfg := model.WorktreeConfig{
		Enabled: true,
	}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	// Seed worktree state
	writeWorktreeState(t, maestroDir, "cmd1", integrationStatus)

	// Do NOT write command state — GetCommandPhases will return ErrStateNotFound,
	// triggering noPhases=true in the stall detection logic.

	// Build in-memory task queue with a terminal task
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

func TestStepWorktreeStallDetection_NoPhasesDoesNotFireWithinTimeout(t *testing.T) {
	t.Parallel()
	recent := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	qh, s := stallTestSetupNoPhases(t, recent, model.IntegrationStatusCreated)

	qh.stepWorktreeStallDetection(s)

	if len(s.signals.Data.Signals) != 0 {
		t.Fatalf("expected 0 signals for noPhases within timeout, got %d", len(s.signals.Data.Signals))
	}
	state, _ := qh.worktreeManager.GetCommandState("cmd1")
	if state.Integration.StallSignaled {
		t.Errorf("StallSignaled should not be set within timeout")
	}
}

func TestStepWorktreeStallDetection_NoPhasesFiresAfterTimeout(t *testing.T) {
	t.Parallel()
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	qh, s := stallTestSetupNoPhases(t, past, model.IntegrationStatusCreated)

	qh.stepWorktreeStallDetection(s)

	if len(s.signals.Data.Signals) != 1 {
		t.Fatalf("expected 1 signal for noPhases after timeout, got %d", len(s.signals.Data.Signals))
	}
	sig := s.signals.Data.Signals[0]
	if sig.Kind != "worktree_stalled" {
		t.Errorf("kind = %q, want worktree_stalled", sig.Kind)
	}
	if sig.CommandID != "cmd1" {
		t.Errorf("command_id = %q, want cmd1", sig.CommandID)
	}
	if sig.Reason != "integration_stalled_no_phases:created" {
		t.Errorf("reason = %q, want integration_stalled_no_phases:created", sig.Reason)
	}

	state, err := qh.worktreeManager.GetCommandState("cmd1")
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if !state.Integration.StallSignaled {
		t.Errorf("StallSignaled flag was not persisted")
	}
}

// resolvingStallTestSetup wires a QueueHandler + scanState with one command
// whose worker1 is in resolving status, back-dated to workerUpdatedAt. The
// integration status is left at active because resolving stall is independent
// of integration-branch stall.
func resolvingStallTestSetup(t *testing.T, workerUpdatedAt string) (*QueueHandler, *scanState) {
	t.Helper()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: true}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)
	setWorkerResolvingForTest(t, maestroDir, "cmd1", "worker1", workerUpdatedAt)

	now := time.Now().UTC().Format(time.RFC3339)
	s := &scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{
						ID:        "cmd1",
						Status:    model.StatusInProgress,
						CreatedAt: now,
						UpdatedAt: now,
					},
				},
			},
		},
		tasks:       makeTaskQueues(nil),
		taskDirty:   make(map[string]bool),
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: make(map[signalKey]struct{}),
	}
	return qh, s
}

func TestStepResolvingWorkerStallDetection_FiresAfterTimeout(t *testing.T) {
	t.Parallel()
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	qh, s := resolvingStallTestSetup(t, past)

	qh.stepResolvingWorkerStallDetection(s)

	if len(s.signals.Data.Signals) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(s.signals.Data.Signals))
	}
	sig := s.signals.Data.Signals[0]
	if sig.Kind != "worker_resolving_stalled" {
		t.Errorf("kind = %q, want worker_resolving_stalled", sig.Kind)
	}
	if sig.CommandID != "cmd1" {
		t.Errorf("command_id = %q, want cmd1", sig.CommandID)
	}
	if sig.WorkerID != "worker1" {
		t.Errorf("worker_id = %q, want worker1", sig.WorkerID)
	}
	if sig.Reason != "resolving_status_held_too_long" {
		t.Errorf("reason = %q, want resolving_status_held_too_long", sig.Reason)
	}
}

func TestStepResolvingWorkerStallDetection_DoesNotFireWithinTimeout(t *testing.T) {
	t.Parallel()
	recent := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	qh, s := resolvingStallTestSetup(t, recent)

	qh.stepResolvingWorkerStallDetection(s)

	if len(s.signals.Data.Signals) != 0 {
		t.Fatalf("expected 0 signals (within timeout), got %d", len(s.signals.Data.Signals))
	}
}

func TestStepResolvingWorkerStallDetection_DedupsRepeatedScans(t *testing.T) {
	t.Parallel()
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	qh, s := resolvingStallTestSetup(t, past)

	qh.stepResolvingWorkerStallDetection(s)
	qh.stepResolvingWorkerStallDetection(s) // second pass — must dedup via signalIndex

	if len(s.signals.Data.Signals) != 1 {
		t.Fatalf("expected 1 signal after dedup, got %d", len(s.signals.Data.Signals))
	}
}

func TestStepResolvingWorkerStallDetection_IgnoresNonResolvingWorkers(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{Enabled: true}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)
	// Default writeWorktreeState leaves worker1 in WorktreeStatusActive.

	now := time.Now().UTC().Format(time.RFC3339)
	s := &scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{
						ID:        "cmd1",
						Status:    model.StatusInProgress,
						CreatedAt: now,
						UpdatedAt: now,
					},
				},
			},
		},
		tasks:       makeTaskQueues(nil),
		taskDirty:   make(map[string]bool),
		signals:     fileState[model.PlannerSignalQueue]{Data: model.PlannerSignalQueue{}},
		signalIndex: make(map[signalKey]struct{}),
	}

	qh.stepResolvingWorkerStallDetection(s)

	if len(s.signals.Data.Signals) != 0 {
		t.Fatalf("expected 0 signals for non-resolving workers, got %d", len(s.signals.Data.Signals))
	}
}

func TestStepWorktreeStallDetection_DeliveryFailureMarksIntegrationFailed(t *testing.T) {
	t.Parallel()
	past := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	qh, s := stallTestSetup(t, past, model.IntegrationStatusCreated, false)

	// Inject a failing mark function to simulate signal persistence failure.
	qh.scanExecutor.worktreeStallMarkFn = func(string) error {
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
