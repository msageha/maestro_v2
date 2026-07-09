package reconcile

import (
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

func TestR8PublishFailed_Quarantined_QueuesDurableSignalAndSetsGuard(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000001_r8test01"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusQuarantined, nil)
	state.Integration.PublishFailureCount = 5
	state.Integration.QuarantinedAt = "2026-01-01T01:00:00Z"
	state.Integration.QuarantineReason = "publish: push to base failed (failure_count=5)"
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R8PublishFailed{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	r := outcome.Repairs[0]
	if r.Pattern != PatternR8 {
		t.Errorf("repair pattern = %s, want %s", r.Pattern, PatternR8)
	}
	if r.CommandID != commandID {
		t.Errorf("repair commandID = %s, want %s", r.CommandID, commandID)
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
	if sig.Kind != "publish_quarantined" || sig.CommandID != commandID {
		t.Fatalf("unexpected planner signal: %+v", sig)
	}
	if sig.Message == "" {
		t.Error("signal message should not be empty")
	}

	// Verify StallSignaled was set to prevent re-emission.
	statePath := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	reloaded, err := run.loadWorktreeState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if !reloaded.Integration.StallSignaled {
		t.Error("StallSignaled should be true after notification")
	}
}

func TestR8PublishFailed_AlreadySignaled_NoAction(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000002_r8test02"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusQuarantined, nil)
	state.Integration.PublishFailureCount = 5
	state.Integration.QuarantinedAt = "2026-01-01T01:00:00Z"
	state.Integration.QuarantineReason = "publish: push to base failed (failure_count=5)"
	state.Integration.StallSignaled = true
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R8PublishFailed{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Errorf("expected 0 repairs for already-signaled quarantine, got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected 0 notifications for already-signaled quarantine, got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}
}

func TestR8PublishFailed_NotQuarantined_NoAction(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000003_r8test03"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusPublishFailed, nil)
	state.Integration.PublishFailureCount = 2
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R8PublishFailed{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Errorf("expected 0 repairs for non-quarantined state, got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected 0 notifications, got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}
}

func TestR8PublishFailed_MergeQuarantined_NoAction(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000004_r8test04"
	state := newWorktreeCommandState(commandID, model.IntegrationStatusQuarantined, nil)
	state.Integration.MergeFailureCount = 3
	state.Integration.QuarantinedAt = "2026-01-01T01:00:00Z"
	state.Integration.QuarantineReason = "merge: abort+recover failed (failure_count=3)"
	writeWorktreeState(t, maestroDir, commandID, state)

	run := newRun(&deps)
	outcome := R8PublishFailed{}.Apply(run)

	// Merge-related quarantine should NOT trigger R8.
	if len(outcome.Repairs) != 0 {
		t.Errorf("expected 0 repairs for merge-related quarantine, got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected 0 notifications, got %d: %+v", len(outcome.Notifications), outcome.Notifications)
	}
}

func TestR8PublishFailed_NoWorktreeStateFiles_NoAction(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	run := newRun(&deps)
	outcome := R8PublishFailed{}.Apply(run)

	if len(outcome.Repairs) != 0 {
		t.Errorf("expected 0 repairs, got %d", len(outcome.Repairs))
	}
	if len(outcome.Notifications) != 0 {
		t.Errorf("expected 0 notifications, got %d", len(outcome.Notifications))
	}
}

func TestR8PublishFailed_MultipleCommands_MixedStates(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	// cmd1: quarantined due to publish — should trigger.
	cmd1 := "cmd_0000000005_r8test05"
	state1 := newWorktreeCommandState(cmd1, model.IntegrationStatusQuarantined, nil)
	state1.Integration.PublishFailureCount = 5
	state1.Integration.QuarantinedAt = "2026-01-01T01:00:00Z"
	state1.Integration.QuarantineReason = "publish: push to base failed (failure_count=5)"
	writeWorktreeState(t, maestroDir, cmd1, state1)

	// cmd2: quarantined due to publish, already signaled — should NOT trigger.
	cmd2 := "cmd_0000000006_r8test06"
	state2 := newWorktreeCommandState(cmd2, model.IntegrationStatusQuarantined, nil)
	state2.Integration.PublishFailureCount = 5
	state2.Integration.QuarantinedAt = "2026-01-01T01:00:00Z"
	state2.Integration.QuarantineReason = "publish: push to base failed (failure_count=5)"
	state2.Integration.StallSignaled = true
	writeWorktreeState(t, maestroDir, cmd2, state2)

	// cmd3: still in publish_failed, not yet quarantined — should NOT trigger.
	cmd3 := "cmd_0000000007_r8test07"
	state3 := newWorktreeCommandState(cmd3, model.IntegrationStatusPublishFailed, nil)
	state3.Integration.PublishFailureCount = 3
	writeWorktreeState(t, maestroDir, cmd3, state3)

	run := newRun(&deps)
	outcome := R8PublishFailed{}.Apply(run)

	if len(outcome.Repairs) != 1 {
		t.Fatalf("expected 1 repair (only cmd1), got %d: %+v", len(outcome.Repairs), outcome.Repairs)
	}
	if outcome.Repairs[0].CommandID != cmd1 {
		t.Errorf("repair commandID = %s, want %s", outcome.Repairs[0].CommandID, cmd1)
	}
	if len(outcome.Notifications) != 0 {
		t.Fatalf("escalation must use the durable signal queue, not notifications; got %+v", outcome.Notifications)
	}
	sq := readPlannerSignalQueue(t, maestroDir)
	if len(sq.Signals) != 1 || sq.Signals[0].CommandID != cmd1 || sq.Signals[0].Kind != "publish_quarantined" {
		t.Fatalf("expected exactly one publish_quarantined signal for cmd1, got %+v", sq.Signals)
	}

	// Verify cmd1 StallSignaled was set.
	statePath := filepath.Join(maestroDir, "state", "worktrees", cmd1+".yaml")
	reloaded, err := run.loadWorktreeState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if !reloaded.Integration.StallSignaled {
		t.Error("cmd1 StallSignaled should be true after notification")
	}
}

func TestR8PublishFailed_DoesNotBreakR7(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	// Set up a conflict scenario for R7.
	conflictCmd := "cmd_0000000008_r8r7coexist"
	conflictState := newWorktreeCommandState(conflictCmd, model.IntegrationStatusConflict, []model.WorktreeState{
		newWorkerState(conflictCmd, "worker1", model.WorktreeStatusConflict, 0),
	})
	writeWorktreeState(t, maestroDir, conflictCmd, conflictState)

	// Set up a publish quarantine scenario for R8.
	publishCmd := "cmd_0000000009_r8r7coexist"
	publishState := newWorktreeCommandState(publishCmd, model.IntegrationStatusQuarantined, nil)
	publishState.Integration.PublishFailureCount = 5
	publishState.Integration.QuarantinedAt = "2026-01-01T01:00:00Z"
	publishState.Integration.QuarantineReason = "publish: push to base failed (failure_count=5)"
	writeWorktreeState(t, maestroDir, publishCmd, publishState)

	// Run R7 — should only handle conflict.
	run := newRun(&deps)
	r7Outcome := R7MergeConflict{}.Apply(run)
	if len(r7Outcome.Repairs) != 1 {
		t.Fatalf("R7 expected 1 repair, got %d: %+v", len(r7Outcome.Repairs), r7Outcome.Repairs)
	}
	if r7Outcome.Repairs[0].CommandID != conflictCmd {
		t.Errorf("R7 repair commandID = %s, want %s", r7Outcome.Repairs[0].CommandID, conflictCmd)
	}

	// Run R8 — should only handle publish quarantine.
	r8Outcome := R8PublishFailed{}.Apply(run)
	if len(r8Outcome.Repairs) != 1 {
		t.Fatalf("R8 expected 1 repair, got %d: %+v", len(r8Outcome.Repairs), r8Outcome.Repairs)
	}
	if r8Outcome.Repairs[0].CommandID != publishCmd {
		t.Errorf("R8 repair commandID = %s, want %s", r8Outcome.Repairs[0].CommandID, publishCmd)
	}
}
