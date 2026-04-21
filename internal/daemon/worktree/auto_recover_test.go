package worktree

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// baseConflictState builds a minimal command state in the given integration
// status with a single placeholder worker.
func baseConflictState(commandID string, status model.IntegrationStatus) *model.WorktreeCommandState {
	return &model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID: commandID,
			Branch:    "maestro/" + commandID + "/integration",
			BaseSHA:   "0000000000000000000000000000000000000000",
			Status:    status,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
}

func TestAutoRecover_Conflict_DispatchesResumeMerge(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_auto_conflict"
	st := baseConflictState(cmdID, model.IntegrationStatusConflict)
	st.Integration.MergeFailureCount = 1
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverResumeMerge {
		t.Fatalf("action = %q, want %q", action, AutoRecoverResumeMerge)
	}

	got, _ := wm.GetCommandState(cmdID)
	if got.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("post status = %s, want failed", got.Integration.Status)
	}
}

func TestAutoRecover_PartialMerge_DispatchesResumeMerge(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_auto_partial"
	st := baseConflictState(cmdID, model.IntegrationStatusPartialMerge)
	st.Integration.MergeFailureCount = 1
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverResumeMerge {
		t.Fatalf("action = %q, want %q", action, AutoRecoverResumeMerge)
	}
}

func TestAutoRecover_FailedWithCount_DispatchesResumeMerge(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_auto_failed"
	st := baseConflictState(cmdID, model.IntegrationStatusFailed)
	st.Integration.MergeFailureCount = 2
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverResumeMerge {
		t.Fatalf("action = %q, want %q", action, AutoRecoverResumeMerge)
	}
}

func TestAutoRecover_FailedNoWork_NoOp(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_auto_failed_noop"
	st := baseConflictState(cmdID, model.IntegrationStatusFailed)
	// MergeFailureCount == 0 and no workers → ResumeMerge would return
	// ErrAlreadyResolved, so AutoRecover must avoid calling it.
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q", action, AutoRecoverNone)
	}
}

func TestAutoRecover_FailedWithConflictWorker_DispatchesResumeMerge(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_auto_failed_worker"
	st := baseConflictState(cmdID, model.IntegrationStatusFailed)
	st.Workers = []model.WorktreeState{
		{
			CommandID: cmdID,
			WorkerID:  "worker-1",
			Branch:    "maestro/" + cmdID + "/worker-1",
			Status:    model.WorktreeStatusConflict,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	}
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverResumeMerge {
		t.Fatalf("action = %q, want %q", action, AutoRecoverResumeMerge)
	}
}

func TestAutoRecover_PublishFailed_ElapsedBackoff_DispatchesRetryPublish(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	fc := &testutil.FakeClock{NowValue: time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)}
	wm.clock = fc

	cmdID := "cmd_auto_pub_elapsed"
	st := baseConflictState(cmdID, model.IntegrationStatusPublishFailed)
	st.Integration.PublishFailureCount = 2
	st.Integration.NextPublishRetryAt = "2026-04-21T11:00:00Z" // in the past
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverRetryPublish {
		t.Fatalf("action = %q, want %q", action, AutoRecoverRetryPublish)
	}

	got, _ := wm.GetCommandState(cmdID)
	if got.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("post status = %s, want merged", got.Integration.Status)
	}
	if got.Integration.PublishFailureCount != 0 {
		t.Errorf("PublishFailureCount = %d, want 0", got.Integration.PublishFailureCount)
	}
}

func TestAutoRecover_PublishFailed_BackoffActive_NoOp(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	fc := &testutil.FakeClock{NowValue: time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)}
	wm.clock = fc

	cmdID := "cmd_auto_pub_backoff"
	st := baseConflictState(cmdID, model.IntegrationStatusPublishFailed)
	st.Integration.NextPublishRetryAt = "2026-04-21T13:00:00Z" // in the future
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q (backoff not yet elapsed)", action, AutoRecoverNone)
	}

	got, _ := wm.GetCommandState(cmdID)
	if got.Integration.Status != model.IntegrationStatusPublishFailed {
		t.Errorf("status changed while backoff active: %s", got.Integration.Status)
	}
}

func TestAutoRecover_PublishFailed_EmptyBackoff_DispatchesRetry(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_auto_pub_empty"
	st := baseConflictState(cmdID, model.IntegrationStatusPublishFailed)
	// NextPublishRetryAt empty → treat as elapsed.
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverRetryPublish {
		t.Fatalf("action = %q, want %q", action, AutoRecoverRetryPublish)
	}
}

func TestAutoRecover_PublishFailed_UnparseableBackoff_TreatedAsElapsed(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_auto_pub_bad"
	st := baseConflictState(cmdID, model.IntegrationStatusPublishFailed)
	st.Integration.NextPublishRetryAt = "not-a-timestamp"
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverRetryPublish {
		t.Fatalf("action = %q, want %q (corrupted backoff must not block recovery)",
			action, AutoRecoverRetryPublish)
	}
}

func TestAutoRecover_Quarantined_NeverAutoRecovers(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_auto_quarantined"
	writeWorktreeState(t, wm, quarantinedState(cmdID))
	before := readStateFile(t, maestroDir, cmdID)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q (quarantined must require operator)",
			action, AutoRecoverNone)
	}

	after := readStateFile(t, maestroDir, cmdID)
	if string(before) != string(after) {
		t.Errorf("state file mutated on quarantined AutoRecover")
	}
}

func TestAutoRecover_Merged_NoOp(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_auto_merged"
	st := baseConflictState(cmdID, model.IntegrationStatusMerged)
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("AutoRecover: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q", action, AutoRecoverNone)
	}
}

func TestAutoRecover_NoWorktreeState(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	action, err := wm.AutoRecover(context.Background(), "cmd_auto_missing")
	if !errors.Is(err, ErrNoWorktreeState) {
		t.Fatalf("err = %v, want ErrNoWorktreeState", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q on missing state", action, AutoRecoverNone)
	}
}

func TestAutoRecover_InvalidCommandID(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	_, err := wm.AutoRecover(context.Background(), "")
	if err == nil {
		t.Fatalf("expected validation error for empty commandID")
	}
}

func TestAutoRecover_Idempotent(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_auto_idempotent"
	st := baseConflictState(cmdID, model.IntegrationStatusConflict)
	st.Integration.MergeFailureCount = 1
	writeWorktreeState(t, wm, st)

	// First call: dispatches ResumeMerge.
	if _, err := wm.AutoRecover(context.Background(), cmdID); err != nil {
		t.Fatalf("first AutoRecover: %v", err)
	}
	first := readStateFile(t, maestroDir, cmdID)

	// Second call: state is now failed with count=0 and no workers, so the
	// selector must report AutoRecoverNone — not re-enter ResumeMerge.
	action, err := wm.AutoRecover(context.Background(), cmdID)
	if err != nil {
		t.Fatalf("second AutoRecover: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("second action = %q, want %q", action, AutoRecoverNone)
	}
	second := readStateFile(t, maestroDir, cmdID)
	if string(first) != string(second) {
		t.Errorf("state mutated on idempotent second call")
	}
}
