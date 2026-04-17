package reconcile

import (
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// writeWorktreeState is a helper that writes a WorktreeCommandState to the
// expected path under maestroDir/state/worktrees/{commandID}.yaml.
func writeWorktreeState(t *testing.T, maestroDir, commandID string, state model.WorktreeCommandState) {
	t.Helper()
	path := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write worktree state: %v", err)
	}
}

// newWorktreeCommandState builds a minimal WorktreeCommandState for testing.
func newWorktreeCommandState(commandID string, integrationStatus model.IntegrationStatus, workers []model.WorktreeState) model.WorktreeCommandState {
	return model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID: commandID,
			Branch:    "maestro/integrate/" + commandID,
			BaseSHA:   "abc123",
			Status:    integrationStatus,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		Workers:   workers,
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
}

func newWorkerState(commandID, workerID string, status model.WorktreeStatus, attempts int) model.WorktreeState {
	return model.WorktreeState{
		CommandID:                  commandID,
		WorkerID:                   workerID,
		Path:                       "/tmp/worktrees/" + workerID,
		Branch:                     "maestro/" + workerID + "/" + commandID,
		BaseSHA:                    "abc123",
		Status:                     status,
		ConflictResolutionAttempts: attempts,
		CreatedAt:                  "2026-01-01T00:00:00Z",
		UpdatedAt:                  "2026-01-01T00:00:00Z",
	}
}

// --- R7 Tests ---

func TestR7MergeConflict_ConflictDetected_DispatchesResolution(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000001_r7test01"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusConflict, []model.WorktreeState{
		newWorkerState(commandID, "worker1", model.WorktreeStatusConflict, 0),
	})
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	r := outcome.Repairs[0]
	if r.Pattern != PatternR7 {
		t.Errorf("repair pattern = %s, want %s", r.Pattern, PatternR7)
	}
	if r.CommandID != commandID {
		t.Errorf("repair commandID = %s, want %s", r.CommandID, commandID)
	}

	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}
	n := outcome.Notifications[0]
	if n.Kind != NotifyConflictResolution {
		t.Errorf("notification kind = %s, want %s", n.Kind, NotifyConflictResolution)
	}
	if n.CommandID != commandID {
		t.Errorf("notification commandID = %s, want %s", n.CommandID, commandID)
	}
	if n.WorkerID != "worker1" {
		t.Errorf("notification workerID = %s, want worker1", n.WorkerID)
	}

	// Verify state file was updated: worker status -> resolving, attempts incremented.
	statePath := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	reloaded, err := run.loadWorktreeState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	ws := reloaded.Workers[0]
	if ws.Status != model.WorktreeStatusResolving {
		t.Errorf("worker status = %s, want %s", ws.Status, model.WorktreeStatusResolving)
	}
	if ws.ConflictResolutionAttempts != 1 {
		t.Errorf("conflict resolution attempts = %d, want 1", ws.ConflictResolutionAttempts)
	}
}

func TestR7MergeConflict_Escalation_WhenAttemptsExceeded(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000002_r7test02"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusConflict, []model.WorktreeState{
		newWorkerState(commandID, "worker1", model.WorktreeStatusConflict, maxConflictResolutionAttempts),
	})
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	r := outcome.Repairs[0]
	if r.Pattern != PatternR7 {
		t.Errorf("repair pattern = %s, want %s", r.Pattern, PatternR7)
	}

	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}
	n := outcome.Notifications[0]
	if n.Kind != NotifyConflictEscalation {
		t.Errorf("notification kind = %s, want %s", n.Kind, NotifyConflictEscalation)
	}
	if n.CommandID != commandID {
		t.Errorf("notification commandID = %s, want %s", n.CommandID, commandID)
	}
	if n.WorkerID != "worker1" {
		t.Errorf("notification workerID = %s, want worker1", n.WorkerID)
	}
	if n.Reason == "" {
		t.Error("notification reason should not be empty for escalation")
	}

	// State file should NOT be modified for escalation (no status transition).
	statePath := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	reloaded, err := run.loadWorktreeState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	ws := reloaded.Workers[0]
	if ws.Status != model.WorktreeStatusConflict {
		t.Errorf("worker status should remain %s after escalation, got %s", model.WorktreeStatusConflict, ws.Status)
	}
	if ws.ConflictResolutionAttempts != maxConflictResolutionAttempts {
		t.Errorf("attempts should remain %d, got %d", maxConflictResolutionAttempts, ws.ConflictResolutionAttempts)
	}
}

func TestR7MergeConflict_NoConflict_NoAction(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000003_r7test03"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusMerged, []model.WorktreeState{
		newWorkerState(commandID, "worker1", model.WorktreeStatusActive, 0),
	})
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Errorf("expected 0 repairs, got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected 0 notifications, got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}
}

func TestR7MergeConflict_IntegrationConflict_WorkerNotConflict_NoAction(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000004_r7test04"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusConflict, []model.WorktreeState{
		newWorkerState(commandID, "worker1", model.WorktreeStatusActive, 0),
		newWorkerState(commandID, "worker2", model.WorktreeStatusCommitted, 0),
	})
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Errorf("expected 0 repairs when workers are not in conflict, got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected 0 notifications, got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}
}

func TestR7MergeConflict_MultipleWorkers_MixedResolutionAndEscalation(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000005_r7test05"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusConflict, []model.WorktreeState{
		newWorkerState(commandID, "worker1", model.WorktreeStatusConflict, 0),                          // should resolve
		newWorkerState(commandID, "worker2", model.WorktreeStatusConflict, maxConflictResolutionAttempts), // should escalate
		newWorkerState(commandID, "worker3", model.WorktreeStatusActive, 0),                            // not in conflict
	})
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	if len(outcome.Repairs) != 2 {
		t.Fatalf("expected 2 repairs (resolution + escalation), got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if len(outcome.Notifications) != 2 {
		t.Fatalf("expected 2 notifications, got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}

	// Find resolution and escalation notifications by kind.
	var resolutionN, escalationN *DeferredNotification
	for i := range outcome.Notifications {
		switch outcome.Notifications[i].Kind {
		case NotifyConflictResolution:
			resolutionN = &outcome.Notifications[i]
		case NotifyConflictEscalation:
			escalationN = &outcome.Notifications[i]
		}
	}
	if resolutionN == nil {
		t.Fatal("expected a ConflictResolution notification")
	}
	if resolutionN.WorkerID != "worker1" {
		t.Errorf("resolution workerID = %s, want worker1", resolutionN.WorkerID)
	}
	if escalationN == nil {
		t.Fatal("expected a ConflictEscalation notification")
	}
	if escalationN.WorkerID != "worker2" {
		t.Errorf("escalation workerID = %s, want worker2", escalationN.WorkerID)
	}

	// Verify state: worker1 should be resolving, worker2 unchanged.
	statePath := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	reloaded, err := run.loadWorktreeState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	for _, ws := range reloaded.Workers {
		switch ws.WorkerID {
		case "worker1":
			if ws.Status != model.WorktreeStatusResolving {
				t.Errorf("worker1 status = %s, want %s", ws.Status, model.WorktreeStatusResolving)
			}
			if ws.ConflictResolutionAttempts != 1 {
				t.Errorf("worker1 attempts = %d, want 1", ws.ConflictResolutionAttempts)
			}
		case "worker2":
			if ws.Status != model.WorktreeStatusConflict {
				t.Errorf("worker2 status = %s, want %s (unchanged)", ws.Status, model.WorktreeStatusConflict)
			}
		case "worker3":
			if ws.Status != model.WorktreeStatusActive {
				t.Errorf("worker3 status = %s, want %s (unchanged)", ws.Status, model.WorktreeStatusActive)
			}
		}
	}
}

func TestR7MergeConflict_NoWorktreeStateFiles_NoAction(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	// Don't create any worktree state files.
	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Errorf("expected 0 repairs, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected 0 notifications, got %d", len(outcome.Notifications))
	}
}

func TestR7MergeConflict_ResolutionAttempt1_StillBelowThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000006_r7test06"
	// attempts=1: still below maxConflictResolutionAttempts (2), so should resolve not escalate.
	state := newWorktreeCommandState(commandID, model.IntegrationStatusConflict, []model.WorktreeState{
		newWorkerState(commandID, "worker1", model.WorktreeStatusConflict, 1),
	})
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(outcome.Notifications))
	}
	if outcome.Notifications[0].Kind != NotifyConflictResolution {
		t.Errorf("expected resolution (attempt 2 of max %d), got %s", maxConflictResolutionAttempts, outcome.Notifications[0].Kind)
	}

	statePath := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	reloaded, err := run.loadWorktreeState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	ws := reloaded.Workers[0]
	if ws.ConflictResolutionAttempts != 2 {
		t.Errorf("attempts = %d, want 2", ws.ConflictResolutionAttempts)
	}
	if ws.Status != model.WorktreeStatusResolving {
		t.Errorf("status = %s, want %s", ws.Status, model.WorktreeStatusResolving)
	}
}

func TestR7MergeConflict_EmptyWorkers_NoAction(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000009_r7empty"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusConflict, []model.WorktreeState{})
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Errorf("expected 0 repairs for empty workers, got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected 0 notifications for empty workers, got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}
}

func TestR7MergeConflict_MultipleCommands(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	cmd1 := "cmd_0000000007_r7test07"
	cmd2 := "cmd_0000000008_r7test08"

	state1 := newWorktreeCommandState(cmd1, model.IntegrationStatusConflict, []model.WorktreeState{
		newWorkerState(cmd1, "worker1", model.WorktreeStatusConflict, 0),
	})
	state2 := newWorktreeCommandState(cmd2, model.IntegrationStatusConflict, []model.WorktreeState{
		newWorkerState(cmd2, "worker2", model.WorktreeStatusConflict, maxConflictResolutionAttempts),
	})
	writeWorktreeState(t, maestroDir, cmd1, state1)
	writeWorktreeState(t, maestroDir, cmd2, state2)

	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	if len(outcome.Repairs) != 2 {
		t.Fatalf("expected 2 repairs across 2 commands, got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if len(outcome.Notifications) != 2 {
		t.Fatalf("expected 2 notifications, got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}

	// Check that we got one resolution and one escalation.
	kinds := map[NotificationKind]bool{}
	for _, n := range outcome.Notifications {
		kinds[n.Kind] = true
	}
	if !kinds[NotifyConflictResolution] {
		t.Error("expected a ConflictResolution notification")
	}
	if !kinds[NotifyConflictEscalation] {
		t.Error("expected a ConflictEscalation notification")
	}
}
