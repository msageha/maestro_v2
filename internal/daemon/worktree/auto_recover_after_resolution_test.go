package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// resolvingWorkerState extends baseConflictState with a single worker whose
// status is set to the given value.
func resolvingWorkerState(commandID, workerID string, integ model.IntegrationStatus, ws model.WorktreeStatus) *model.WorktreeCommandState {
	st := baseConflictState(commandID, integ)
	st.Workers = []model.WorktreeState{
		{
			CommandID: commandID,
			WorkerID:  workerID,
			Branch:    "maestro/" + commandID + "/" + workerID,
			Status:    ws,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	}
	return st
}

// TestAutoRecoverAfterResolution_Publish_BypassesBackoff verifies that a
// publish_conflict resolution completion fires RetryPublish even when the
// scan-driven backoff (NextPublishRetryAt) is far in the future. The whole
// point of the event-driven entry point is that completion is itself proof
// the previous blocker is addressed.
func TestAutoRecoverAfterResolution_Publish_BypassesBackoff(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	fc := &testutil.FakeClock{NowValue: time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)}
	wm.clock = fc

	cmdID := "cmd_event_pub_bypass"
	st := baseConflictState(cmdID, model.IntegrationStatusPublishFailed)
	st.Integration.PublishFailureCount = 3
	st.Integration.NextPublishRetryAt = "2099-01-01T00:00:00Z" // far future
	st.Integration.PublishConflictFiles = []string{"a.txt"}
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, "worker-1", true)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
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
	if got.Integration.NextPublishRetryAt != "" {
		t.Errorf("NextPublishRetryAt = %q, want empty", got.Integration.NextPublishRetryAt)
	}
	if len(got.Integration.PublishConflictFiles) != 0 {
		t.Errorf("PublishConflictFiles = %v, want empty", got.Integration.PublishConflictFiles)
	}
}

// TestAutoRecoverAfterResolution_Publish_RequiresRunOnIntegration ensures the
// publish path only fires when taskRunOnIntegration==true. A non-integration
// completion landing on a publish_failed integration must not accidentally
// trigger a publish retry — the merge-side check below would also be a no-op
// because the worker is not in resolving status.
func TestAutoRecoverAfterResolution_Publish_RequiresRunOnIntegration(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_event_pub_no_integ"
	st := baseConflictState(cmdID, model.IntegrationStatusPublishFailed)
	st.Integration.PublishConflictFiles = []string{"a.txt"}
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, "worker-1", false)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q", action, AutoRecoverNone)
	}

	got, _ := wm.GetCommandState(cmdID)
	if got.Integration.Status != model.IntegrationStatusPublishFailed {
		t.Errorf("status mutated despite no-op: %s", got.Integration.Status)
	}
}

// TestAutoRecoverAfterResolution_Publish_RequiresPublishConflictFiles ensures
// that taskRunOnIntegration alone is not enough. Without
// PublishConflictFiles we cannot identify the completion as a publish_conflict
// resolution; defer to the merge-side check.
func TestAutoRecoverAfterResolution_Publish_RequiresPublishConflictFiles(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_event_pub_no_files"
	// PublishConflictFiles intentionally empty.
	st := baseConflictState(cmdID, model.IntegrationStatusPublishFailed)
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, "worker-1", true)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q", action, AutoRecoverNone)
	}
}

// TestAutoRecoverAfterResolution_Publish_RequiresPublishFailedStatus ensures a
// completion on the integration worktree but on a non-publish_failed
// integration (e.g. failed merge) does not trigger RetryPublish.
func TestAutoRecoverAfterResolution_Publish_RequiresPublishFailedStatus(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_event_pub_wrong_status"
	st := baseConflictState(cmdID, model.IntegrationStatusFailed)
	st.Integration.PublishConflictFiles = []string{"a.txt"} // stale leftover
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, "worker-1", true)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q", action, AutoRecoverNone)
	}
}

// TestAutoRecoverAfterResolution_Merge_FromConflict verifies that a resolving
// reporter triggers ResumeMerge when integration is in conflict.
func TestAutoRecoverAfterResolution_Merge_FromConflict(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_event_merge_conflict"
	wid := "worker-1"
	st := resolvingWorkerState(cmdID, wid, model.IntegrationStatusConflict, model.WorktreeStatusResolving)
	st.Integration.MergeFailureCount = 1
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, wid, false)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverResumeMerge {
		t.Fatalf("action = %q, want %q", action, AutoRecoverResumeMerge)
	}

	got, _ := wm.GetCommandState(cmdID)
	// ResumeMerge ends in failed (no integration worktree → fallback to active reset).
	if got.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("post status = %s, want failed", got.Integration.Status)
	}
}

// TestAutoRecoverAfterResolution_Merge_FromPartialMerge verifies the
// partial_merge path triggers ResumeMerge.
func TestAutoRecoverAfterResolution_Merge_FromPartialMerge(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_event_merge_partial"
	wid := "worker-1"
	st := resolvingWorkerState(cmdID, wid, model.IntegrationStatusPartialMerge, model.WorktreeStatusResolving)
	st.Integration.MergeFailureCount = 1
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, wid, false)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverResumeMerge {
		t.Fatalf("action = %q, want %q", action, AutoRecoverResumeMerge)
	}
}

// TestAutoRecoverAfterResolution_Merge_FromFailed verifies the failed-with-
// resolving-worker path triggers ResumeMerge even when MergeFailureCount==0.
func TestAutoRecoverAfterResolution_Merge_FromFailed(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_event_merge_failed"
	wid := "worker-1"
	st := resolvingWorkerState(cmdID, wid, model.IntegrationStatusFailed, model.WorktreeStatusResolving)
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, wid, false)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverResumeMerge {
		t.Fatalf("action = %q, want %q", action, AutoRecoverResumeMerge)
	}
}

// TestAutoRecoverAfterResolution_Merge_ReporterNotResolving_NoOp verifies
// that a non-resolving reporter is treated as a no-op even when the
// integration is in a recoverable state. A completion from an unrelated
// worker must not advance an in-flight resolver against the wrong branch.
func TestAutoRecoverAfterResolution_Merge_ReporterNotResolving_NoOp(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_event_merge_not_resolving"
	wid := "worker-1"
	st := resolvingWorkerState(cmdID, wid, model.IntegrationStatusConflict, model.WorktreeStatusActive)
	st.Integration.MergeFailureCount = 1
	writeWorktreeState(t, wm, st)
	before := readStateFile(t, maestroDir, cmdID)

	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, wid, false)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q", action, AutoRecoverNone)
	}

	after := readStateFile(t, maestroDir, cmdID)
	if string(before) != string(after) {
		t.Errorf("state file mutated despite no-op")
	}
}

// TestAutoRecoverAfterResolution_Merge_DifferentResolvingReporter_NoOp ensures
// that a completion from a worker that is *not* the resolving worker (when
// some other worker is resolving) is a no-op.
func TestAutoRecoverAfterResolution_Merge_DifferentResolvingReporter_NoOp(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_event_merge_wrong_reporter"
	st := baseConflictState(cmdID, model.IntegrationStatusConflict)
	st.Integration.MergeFailureCount = 1
	st.Workers = []model.WorktreeState{
		{
			CommandID: cmdID,
			WorkerID:  "worker-resolving",
			Branch:    "maestro/" + cmdID + "/worker-resolving",
			Status:    model.WorktreeStatusResolving,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
		{
			CommandID: cmdID,
			WorkerID:  "worker-other",
			Branch:    "maestro/" + cmdID + "/worker-other",
			Status:    model.WorktreeStatusActive,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		},
	}
	writeWorktreeState(t, wm, st)
	before := readStateFile(t, maestroDir, cmdID)

	// Reporter is the *active* worker, not the resolving one.
	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, "worker-other", false)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q", action, AutoRecoverNone)
	}
	after := readStateFile(t, maestroDir, cmdID)
	if string(before) != string(after) {
		t.Errorf("state file mutated despite no-op")
	}
}

// TestAutoRecoverAfterResolution_Merge_TerminalIntegration_NoOp ensures that a
// resolving reporter on an already-merged integration does not call ResumeMerge.
func TestAutoRecoverAfterResolution_Merge_TerminalIntegration_NoOp(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_event_merge_already_merged"
	wid := "worker-1"
	st := resolvingWorkerState(cmdID, wid, model.IntegrationStatusMerged, model.WorktreeStatusResolving)
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, wid, false)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q (terminal integration must not auto-recover)", action, AutoRecoverNone)
	}
}

// TestAutoRecoverAfterResolution_Quarantined_NoOp ensures that a quarantined
// integration is never auto-recovered, even with a resolving reporter.
func TestAutoRecoverAfterResolution_Quarantined_NoOp(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_event_quarantined"
	wid := "worker-1"
	st := resolvingWorkerState(cmdID, wid, model.IntegrationStatusQuarantined, model.WorktreeStatusResolving)
	writeWorktreeState(t, wm, st)

	action, err := wm.AutoRecoverAfterResolution(context.Background(), cmdID, wid, false)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q (quarantined must require operator)", action, AutoRecoverNone)
	}
}

// TestAutoRecoverAfterResolution_Merge_DeferredWhenResolutionInFlight
// pins the AutoRecoverResumeMergeDeferred outcome: when the resolving
// worker has not yet committed the resolution edits to its worktree,
// ResumeMerge takes the resume_merge_deferred_resolution_in_flight
// branch and returns nil without flipping the worker out of Resolving.
// AutoRecoverAfterResolution must surface that distinct outcome so
// result_write_handler can log "auto_recover_after_resolution_deferred"
// rather than the misleading "_completed action=resume_merge".
func TestAutoRecoverAfterResolution_Merge_DeferredWhenResolutionInFlight(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_auto_recover_deferred"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Build the same conflict shape used by TestResumeMerge_SkipsResolvingWithoutEdits:
	// both workers commit divergent edits to the same file, MergeToIntegration
	// turns worker2 into conflict, and we then flip worker2 to resolving
	// without committing the resolution. ResumeMerge from this state must
	// take the deferred branch.
	wt1, _ := wm.GetWorkerPath(commandID, "worker1")
	wt2, _ := wm.GetWorkerPath(commandID, "worker2")

	if err := os.WriteFile(filepath.Join(wt1, "DEFER.txt"), []byte("worker1 wins\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 add DEFER.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "DEFER.txt"), []byte("worker2 wins\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 add DEFER.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil); err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}

	func() {
		wm.mu.Lock()
		defer wm.mu.Unlock()
		st, _ := wm.loadState(commandID)
		ws := wm.findWorker(st, "worker2")
		now := wm.clock.Now().UTC().Format("2006-01-02T15:04:05Z")
		_ = wm.setWorkerStatus(ws, model.WorktreeStatusResolving, now)
		st.UpdatedAt = now
		_ = wm.saveState(commandID, st)
	}()

	action, err := wm.AutoRecoverAfterResolution(context.Background(), commandID, "worker2", false)
	if err != nil {
		t.Fatalf("AutoRecoverAfterResolution: %v", err)
	}
	if action != AutoRecoverResumeMergeDeferred {
		t.Fatalf("action = %q, want %q (worker still resolving with no committed edits → deferred)",
			action, AutoRecoverResumeMergeDeferred)
	}

	// Sanity: the deferred outcome implies the worker is still in resolving;
	// the resolution task report (next AutoRecoverAfterResolution call) will
	// drive the merge once edits land.
	got, _ := wm.GetCommandState(commandID)
	for _, ws := range got.Workers {
		if ws.WorkerID == "worker2" && ws.Status != model.WorktreeStatusResolving {
			t.Errorf("worker2 status = %q, want resolving (deferred branch must not flip status)", ws.Status)
		}
	}
}

// TestAutoRecoverAfterResolution_NoWorktreeState verifies the missing-state
// error path.
func TestAutoRecoverAfterResolution_NoWorktreeState(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	action, err := wm.AutoRecoverAfterResolution(context.Background(), "cmd_event_missing", "worker-1", false)
	if !errors.Is(err, ErrNoWorktreeState) {
		t.Fatalf("err = %v, want ErrNoWorktreeState", err)
	}
	if action != AutoRecoverNone {
		t.Fatalf("action = %q, want %q on missing state", action, AutoRecoverNone)
	}
}

// TestAutoRecoverAfterResolution_InvalidIDs covers validation failures for
// both commandID and workerID.
func TestAutoRecoverAfterResolution_InvalidIDs(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	if _, err := wm.AutoRecoverAfterResolution(context.Background(), "", "worker-1", false); err == nil {
		t.Errorf("expected error for empty commandID")
	}
	if _, err := wm.AutoRecoverAfterResolution(context.Background(), "cmd_x", "", false); err == nil {
		t.Errorf("expected error for empty reporterWorkerID")
	}
}

// TestResetResolvingWorkerToConflict_Success verifies the happy path: the
// resolving worker is reverted to conflict so the next R7 / dispatch cycle can
// re-attempt resolution without waiting for the 20-minute stall sweep.
func TestResetResolvingWorkerToConflict_Success(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_reset_success"
	wid := "worker-1"
	st := resolvingWorkerState(cmdID, wid, model.IntegrationStatusFailed, model.WorktreeStatusResolving)
	writeWorktreeState(t, wm, st)

	if err := wm.ResetResolvingWorkerToConflict(cmdID, wid); err != nil {
		t.Fatalf("ResetResolvingWorkerToConflict: %v", err)
	}

	got, _ := wm.GetCommandState(cmdID)
	var found bool
	for _, ws := range got.Workers {
		if ws.WorkerID == wid {
			found = true
			if ws.Status != model.WorktreeStatusConflict {
				t.Errorf("worker status = %s, want conflict", ws.Status)
			}
		}
	}
	if !found {
		t.Errorf("worker %s missing from state", wid)
	}
}

// TestResetResolvingWorkerToConflict_NotResolving_NoOp verifies that calling
// reset on a non-resolving worker is a silent no-op and does not mutate the
// state file. This makes the call site safe to invoke unconditionally.
func TestResetResolvingWorkerToConflict_NotResolving_NoOp(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_reset_no_op"
	wid := "worker-1"
	st := resolvingWorkerState(cmdID, wid, model.IntegrationStatusFailed, model.WorktreeStatusActive)
	writeWorktreeState(t, wm, st)
	before := readStateFile(t, maestroDir, cmdID)

	if err := wm.ResetResolvingWorkerToConflict(cmdID, wid); err != nil {
		t.Fatalf("ResetResolvingWorkerToConflict: %v", err)
	}

	after := readStateFile(t, maestroDir, cmdID)
	if string(before) != string(after) {
		t.Errorf("state file mutated despite no-op")
	}
}

// TestResetResolvingWorkerToConflict_MissingWorker_NoOp verifies that calling
// reset for a worker that does not appear in the state file is a silent no-op.
func TestResetResolvingWorkerToConflict_MissingWorker_NoOp(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_reset_missing_worker"
	st := baseConflictState(cmdID, model.IntegrationStatusFailed)
	writeWorktreeState(t, wm, st)
	before := readStateFile(t, maestroDir, cmdID)

	if err := wm.ResetResolvingWorkerToConflict(cmdID, "worker-ghost"); err != nil {
		t.Fatalf("ResetResolvingWorkerToConflict: %v", err)
	}

	after := readStateFile(t, maestroDir, cmdID)
	if string(before) != string(after) {
		t.Errorf("state file mutated despite no-op")
	}
}

// TestResetResolvingWorkerToConflict_NoWorktreeState verifies the missing-
// state error path.
func TestResetResolvingWorkerToConflict_NoWorktreeState(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	err := wm.ResetResolvingWorkerToConflict("cmd_reset_missing", "worker-1")
	if !errors.Is(err, ErrNoWorktreeState) {
		t.Errorf("err = %v, want ErrNoWorktreeState", err)
	}
}

// TestResetResolvingWorkerToConflict_InvalidIDs covers validation failures.
func TestResetResolvingWorkerToConflict_InvalidIDs(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	if err := wm.ResetResolvingWorkerToConflict("", "worker-1"); err == nil {
		t.Errorf("expected error for empty commandID")
	}
	if err := wm.ResetResolvingWorkerToConflict("cmd_x", ""); err == nil {
		t.Errorf("expected error for empty workerID")
	}
}

// TestResetResolvingWorkerToConflict_Idempotent verifies that calling reset
// twice returns nil (the second call hits the not-resolving no-op branch).
func TestResetResolvingWorkerToConflict_Idempotent(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_reset_idem"
	wid := "worker-1"
	st := resolvingWorkerState(cmdID, wid, model.IntegrationStatusFailed, model.WorktreeStatusResolving)
	writeWorktreeState(t, wm, st)

	if err := wm.ResetResolvingWorkerToConflict(cmdID, wid); err != nil {
		t.Fatalf("first reset: %v", err)
	}
	first := readStateFile(t, maestroDir, cmdID)

	if err := wm.ResetResolvingWorkerToConflict(cmdID, wid); err != nil {
		t.Fatalf("second reset: %v", err)
	}
	second := readStateFile(t, maestroDir, cmdID)

	if string(first) != string(second) {
		t.Errorf("state file mutated on idempotent second call")
	}
}
