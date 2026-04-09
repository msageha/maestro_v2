package worktree

import (
	"errors"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// TestRecordMergeFailure_TransitionsToQuarantine validates that consecutive
// unrecoverable failures escalate from Failed to Quarantined at the threshold.
func TestRecordMergeFailure_TransitionsToQuarantine(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	state := &model.WorktreeCommandState{
		CommandID: "cmd_test",
		Integration: model.IntegrationState{
			CommandID: "cmd_test",
			Status:    model.IntegrationStatusMerging,
		},
	}

	now := "2026-04-07T00:00:00Z"

	// First failure: Merging → Failed (count=1)
	if err := wm.recordMergeFailure(state, "test_reason", now); err != nil {
		t.Fatalf("first recordMergeFailure: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("after 1st failure: status=%s want=%s", state.Integration.Status, model.IntegrationStatusFailed)
	}
	if state.Integration.MergeFailureCount != 1 {
		t.Errorf("count=%d want=1", state.Integration.MergeFailureCount)
	}

	// Second failure: Failed → Failed (count=2)
	if err := wm.recordMergeFailure(state, "test_reason", now); err != nil {
		t.Fatalf("second recordMergeFailure: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("after 2nd failure: status=%s want=%s", state.Integration.Status, model.IntegrationStatusFailed)
	}
	if state.Integration.MergeFailureCount != 2 {
		t.Errorf("count=%d want=2", state.Integration.MergeFailureCount)
	}

	// Third failure: Failed → Quarantined (count=3, threshold)
	if err := wm.recordMergeFailure(state, "abort_recover_failed", now); err != nil {
		t.Fatalf("third recordMergeFailure: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusQuarantined {
		t.Errorf("after 3rd failure: status=%s want=%s", state.Integration.Status, model.IntegrationStatusQuarantined)
	}
	if state.Integration.MergeFailureCount != 3 {
		t.Errorf("count=%d want=3", state.Integration.MergeFailureCount)
	}
	if state.Integration.QuarantinedAt != now {
		t.Errorf("quarantined_at=%q want=%q", state.Integration.QuarantinedAt, now)
	}
	if !strings.Contains(state.Integration.QuarantineReason, "abort_recover_failed") {
		t.Errorf("quarantine_reason=%q does not contain reason", state.Integration.QuarantineReason)
	}
}

// TestMergeToIntegration_QuarantinedShortCircuits validates that calling
// MergeToIntegration on a quarantined integration returns errIntegrationQuarantined
// immediately without performing any git operations or state mutations.
// This is the core fix for H10: prevents the infinite reconcile loop.
func TestMergeToIntegration_QuarantinedShortCircuits(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_quarantined_test"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	// Force the integration to quarantined state directly via state mutation,
	// bypassing the normal failure path so the test is independent of failure
	// classification details.
	state, err := wm.loadState(commandID)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	state.Integration.Status = model.IntegrationStatusQuarantined
	state.Integration.MergeFailureCount = mergeFailureQuarantineThreshold
	state.Integration.QuarantinedAt = "2026-04-07T00:00:00Z"
	state.Integration.QuarantineReason = "test_setup (failure_count=3)"
	if err := wm.saveState(commandID, state); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	// Call MergeToIntegration; it must return errIntegrationQuarantined
	// without modifying the persisted state.
	conflicts, mergeErr := wm.MergeToIntegration(commandID, []string{"worker1"}, nil)
	if mergeErr == nil {
		t.Fatalf("MergeToIntegration returned nil err, want errIntegrationQuarantined")
	}
	if !errors.Is(mergeErr, errIntegrationQuarantined) {
		t.Errorf("err=%v does not wrap errIntegrationQuarantined", mergeErr)
	}
	if conflicts != nil {
		t.Errorf("conflicts=%v want nil", conflicts)
	}

	// Verify state is unchanged: still Quarantined, count unchanged.
	state2, err := wm.loadState(commandID)
	if err != nil {
		t.Fatalf("loadState after: %v", err)
	}
	if state2.Integration.Status != model.IntegrationStatusQuarantined {
		t.Errorf("post-call status=%s want=%s", state2.Integration.Status, model.IntegrationStatusQuarantined)
	}
	if state2.Integration.MergeFailureCount != mergeFailureQuarantineThreshold {
		t.Errorf("post-call count=%d want=%d", state2.Integration.MergeFailureCount, mergeFailureQuarantineThreshold)
	}
}

// TestQuarantinedIsTerminalIntegrationStatus validates the terminal-state
// guarantee that prevents any transition out of Quarantined.
func TestQuarantinedIsTerminalIntegrationStatus(t *testing.T) {
	if !model.IsIntegrationTerminal(model.IntegrationStatusQuarantined) {
		t.Errorf("IntegrationStatusQuarantined is not terminal")
	}
	if err := model.ValidateIntegrationTransition(model.IntegrationStatusQuarantined, model.IntegrationStatusMerging); err == nil {
		t.Errorf("transition Quarantined→Merging unexpectedly allowed")
	}
}
