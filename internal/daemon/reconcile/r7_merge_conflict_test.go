package reconcile

import (
	"os"
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

	if len(outcome.Notifications) != 0 {
		t.Fatalf("escalation must use the durable signal queue, not notifications; got %+v", outcome.Notifications)
	}

	// WAL: the durable planner signal is queued.
	sq := readPlannerSignalQueue(t, maestroDir)
	if len(sq.Signals) != 1 {
		t.Fatalf("planner signals = %d, want 1", len(sq.Signals))
	}
	sig := sq.Signals[0]
	if sig.Kind != "conflict_escalation" || sig.CommandID != commandID || sig.WorkerID != "worker1" {
		t.Fatalf("unexpected planner signal: %+v", sig)
	}
	if sig.Message == "" {
		t.Error("signal message should not be empty")
	}

	// Escalation does not change status or attempts, but it now persists the
	// ConflictEscalated one-shot guard so the notification is not re-emitted on
	// every scan.
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
	if !ws.ConflictEscalated {
		t.Error("ConflictEscalated should be set true after escalation (one-shot guard)")
	}
}

// TestR7MergeConflict_Escalation_OnceGuard verifies that a persistent
// unrecoverable conflict escalates exactly once across repeated reconcile
// scans, instead of re-emitting the notification + repair on every scan.
func TestR7MergeConflict_Escalation_OnceGuard(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000099_r7once99"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusConflict, []model.WorktreeState{
		newWorkerState(commandID, "worker1", model.WorktreeStatusConflict, maxConflictResolutionAttempts),
	})
	writeWorktreeState(t, maestroDir, commandID, state)

	// First scan: escalates once (durable signal + guard).
	first := R7MergeConflict{}.Apply(newRun(&deps))
	if len(first.Notifications) != 0 {
		t.Fatalf("first scan: escalation must not use notifications, got %+v", first.Notifications)
	}
	if len(first.Repairs) != 1 {
		t.Fatalf("first scan: expected 1 repair, got %d", len(first.Repairs))
	}
	if sq := readPlannerSignalQueue(t, maestroDir); len(sq.Signals) != 1 {
		t.Fatalf("first scan: planner signals = %d, want 1", len(sq.Signals))
	}

	// Subsequent scans on the unchanged conflict must NOT re-fire.
	for i := 0; i < 3; i++ {
		again := R7MergeConflict{}.Apply(newRun(&deps))
		if len(again.Repairs) != 0 {
			t.Fatalf("scan %d: expected no repair on re-scan, got %+v", i+2, again.Repairs)
		}
		if sq := readPlannerSignalQueue(t, maestroDir); len(sq.Signals) != 1 {
			t.Fatalf("scan %d: planner signals = %d, want 1 (no duplicates)", i+2, len(sq.Signals))
		}
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
		newWorkerState(commandID, "worker1", model.WorktreeStatusConflict, 0),                             // should resolve
		newWorkerState(commandID, "worker2", model.WorktreeStatusConflict, maxConflictResolutionAttempts), // should escalate
		newWorkerState(commandID, "worker3", model.WorktreeStatusActive, 0),                               // not in conflict
	})
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	if len(outcome.Repairs) != 2 {
		t.Fatalf("expected 2 repairs (resolution + escalation), got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification (resolution only; escalation uses the signal queue), got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}
	if outcome.Notifications[0].Kind != NotifyConflictResolution || outcome.Notifications[0].WorkerID != "worker1" {
		t.Fatalf("expected resolution notification for worker1, got %+v", outcome.Notifications[0])
	}
	sq := readPlannerSignalQueue(t, maestroDir)
	if len(sq.Signals) != 1 || sq.Signals[0].Kind != "conflict_escalation" || sq.Signals[0].WorkerID != "worker2" {
		t.Fatalf("expected conflict_escalation signal for worker2, got %+v", sq.Signals)
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

func TestR7MergeConflict_StateWriteFailure_SuppressesNotifications(t *testing.T) {
	if os.Getuid() == 0 {
		// root bypasses directory write-permission bits entirely, so the
		// chmod-based failure injection below is a no-op and the write
		// would succeed instead of failing as this test requires.
		t.Skip("test requires non-root user")
	}
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000010_r7writefail"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusConflict, []model.WorktreeState{
		newWorkerState(commandID, "worker1", model.WorktreeStatusConflict, 0),
	})
	writeWorktreeState(t, maestroDir, commandID, state)

	// Make the worktrees directory read-only so AtomicWrite fails.
	worktreeDir := filepath.Join(maestroDir, "state", "worktrees")
	if err := os.Chmod(worktreeDir, 0555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		os.Chmod(worktreeDir, 0755)
	})

	run := newRun(&deps)
	outcome := R7MergeConflict{}.Apply(run)

	// Suppressed: the unsaved attempt counters / resolving transition mean
	// the next scan re-derives the SAME decision and re-emits. Delivering
	// now AND on the next scan would double-dispatch resolution tasks to
	// the Planner (no downstream dedupe exists for these pane messages).
	if len(outcome.Repairs) != 0 {
		t.Fatalf("expected 0 repairs on state write failure (re-derived next scan), got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if len(outcome.Notifications) != 0 {
		t.Fatalf("expected 0 notifications on state write failure (re-derived next scan), got %d: %+v", len(outcome.Notifications), outcome.Notifications)
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
	if len(outcome.Notifications) != 1 {
		t.Fatalf("expected 1 notification (resolution only; escalation uses the signal queue), got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}
	if outcome.Notifications[0].Kind != NotifyConflictResolution {
		t.Errorf("expected a ConflictResolution notification, got %s", outcome.Notifications[0].Kind)
	}
	sq := readPlannerSignalQueue(t, maestroDir)
	if len(sq.Signals) != 1 || sq.Signals[0].Kind != "conflict_escalation" || sq.Signals[0].CommandID != cmd2 {
		t.Fatalf("expected conflict_escalation signal for %s, got %+v", cmd2, sq.Signals)
	}
}
