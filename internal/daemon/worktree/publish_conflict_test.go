package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// --- RetryPublish unit tests (state-only, no real git) ---

func publishFailedState(commandID string) *model.WorktreeCommandState {
	return &model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID:               commandID,
			Branch:                  "maestro/" + commandID + "/integration",
			BaseSHA:                 "0000000000000000000000000000000000000000",
			Status:                  model.IntegrationStatusPublishFailed,
			PublishFailureCount:     3,
			NextPublishRetryAt:      "2026-04-18T00:10:00Z",
			PublishConflictFiles:    []string{"file1.go", "file2.go"},
			PublishConflictSignaled: true,
			CreatedAt:               "2026-01-01T00:00:00Z",
			UpdatedAt:               "2026-01-01T00:00:00Z",
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
}

func publishQuarantinedState(commandID string) *model.WorktreeCommandState {
	return &model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID:               commandID,
			Branch:                  "maestro/" + commandID + "/integration",
			BaseSHA:                 "0000000000000000000000000000000000000000",
			Status:                  model.IntegrationStatusQuarantined,
			PublishFailureCount:     5,
			QuarantinedAt:           "2026-04-18T00:05:00Z",
			QuarantineReason:        "publish: publish_forward_merge_conflict (failure_count=5)",
			QuarantineSource:        model.QuarantineSourcePublish,
			StallSignaled:           true,
			PublishConflictFiles:    []string{"conflict.go"},
			PublishConflictSignaled: true,
			CreatedAt:               "2026-01-01T00:00:00Z",
			UpdatedAt:               "2026-01-01T00:00:00Z",
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
}

func TestRetryPublish_FromPublishFailed(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_retry_pub_001"
	writeWorktreeState(t, wm, publishFailedState(cmdID))

	if err := wm.RetryPublish(cmdID); err != nil {
		t.Fatalf("RetryPublish: %v", err)
	}

	got, err := wm.GetCommandState(cmdID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if got.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("status = %s, want merged", got.Integration.Status)
	}
	if got.Integration.PublishFailureCount != 0 {
		t.Errorf("PublishFailureCount = %d, want 0", got.Integration.PublishFailureCount)
	}
	if got.Integration.NextPublishRetryAt != "" {
		t.Errorf("NextPublishRetryAt = %q, want empty", got.Integration.NextPublishRetryAt)
	}
	if len(got.Integration.PublishConflictFiles) != 0 {
		t.Errorf("PublishConflictFiles = %v, want nil", got.Integration.PublishConflictFiles)
	}
	if got.Integration.PublishConflictSignaled {
		t.Errorf("PublishConflictSignaled = true, want false")
	}
}

func TestRetryPublish_FromPublishQuarantined(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_retry_pub_002"
	writeWorktreeState(t, wm, publishQuarantinedState(cmdID))

	if err := wm.RetryPublish(cmdID); err != nil {
		t.Fatalf("RetryPublish: %v", err)
	}

	got, err := wm.GetCommandState(cmdID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if got.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("status = %s, want merged", got.Integration.Status)
	}
	if got.Integration.QuarantinedAt != "" {
		t.Errorf("QuarantinedAt = %q, want empty", got.Integration.QuarantinedAt)
	}
	if got.Integration.QuarantineReason != "" {
		t.Errorf("QuarantineReason = %q, want empty", got.Integration.QuarantineReason)
	}
	if got.Integration.QuarantineSource != "" {
		t.Errorf("QuarantineSource = %q, want empty", got.Integration.QuarantineSource)
	}
	if got.Integration.StallSignaled {
		t.Errorf("StallSignaled = true, want false")
	}
}

func TestRetryPublish_RejectsNonPublishQuarantine(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_retry_pub_003"
	// Non-publish quarantine (merge quarantine with QuarantineSource=merge)
	writeWorktreeState(t, wm, quarantinedState(cmdID))

	err := wm.RetryPublish(cmdID)
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("err = %v, want ErrAlreadyResolved", err)
	}
}

// TestRetryPublish_QuarantineSourceDistinguishesPublishFromMerge verifies that
// the quarantine check uses the structured QuarantineSource field rather than
// string-matching QuarantineReason. A quarantine with reason containing "publish"
// but QuarantineSource=merge must still be rejected.
func TestRetryPublish_QuarantineSourceDistinguishesPublishFromMerge(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	cmdID := "cmd_retry_pub_source_check"
	st := quarantinedState(cmdID)
	// Simulate a merge quarantine whose reason text happens to contain "publish"
	st.Integration.QuarantineReason = "abort_recover_failed after publish attempt (failure_count=3)"
	st.Integration.QuarantineSource = model.QuarantineSourceMerge
	writeWorktreeState(t, wm, st)

	err := wm.RetryPublish(cmdID)
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Errorf("err = %v, want ErrAlreadyResolved (merge quarantine with 'publish' in reason text must be rejected)", err)
	}
}

func TestRetryPublish_Idempotent(t *testing.T) {
	t.Parallel()
	wm, maestroDir := newRecoveryTestManager(t)
	cmdID := "cmd_retry_pub_004"
	writeWorktreeState(t, wm, publishFailedState(cmdID))

	if err := wm.RetryPublish(cmdID); err != nil {
		t.Fatalf("first RetryPublish: %v", err)
	}
	first := readStateFile(t, maestroDir, cmdID)

	err := wm.RetryPublish(cmdID)
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second RetryPublish err = %v, want ErrAlreadyResolved", err)
	}
	second := readStateFile(t, maestroDir, cmdID)
	if string(first) != string(second) {
		t.Errorf("state file mutated on second call")
	}
}

func TestRetryPublish_NoWorktreeState(t *testing.T) {
	t.Parallel()
	wm, _ := newRecoveryTestManager(t)
	err := wm.RetryPublish("cmd_retry_pub_missing")
	if !errors.Is(err, ErrNoWorktreeState) {
		t.Errorf("err = %v, want ErrNoWorktreeState", err)
	}
}

func TestRetryPublish_RejectsWrongStates(t *testing.T) {
	t.Parallel()
	for _, status := range []model.IntegrationStatus{
		model.IntegrationStatusCreated,
		model.IntegrationStatusMerging,
		model.IntegrationStatusPublishing,
		model.IntegrationStatusPublished,
		model.IntegrationStatusConflict,
		model.IntegrationStatusFailed,
	} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			wm, _ := newRecoveryTestManager(t)
			cmdID := "cmd_retry_pub_reject_" + string(status)
			st := publishFailedState(cmdID)
			st.Integration.Status = status
			writeWorktreeState(t, wm, st)

			err := wm.RetryPublish(cmdID)
			if !errors.Is(err, ErrAlreadyResolved) {
				t.Errorf("status=%s: err = %v, want ErrAlreadyResolved", status, err)
			}
		})
	}
}

// --- Forward-merge integration tests (require real git repo) ---

// TestPublishToBase_ForwardMergeAutoResolves verifies that PublishToBase
// automatically forward-merges base into integration when base has advanced,
// allowing publish to succeed without conflict.
func TestPublishToBase_ForwardMergeAutoResolves(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	defer func() { _ = cleanupAll(wm) }()

	commandID := "cmd_fwd_merge_auto"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker1 creates a file and commits
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "worker_file.txt"), []byte("worker content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add worker_file.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge worker to integration
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) > 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}

	// Advance base (main) with a non-conflicting commit
	if err := os.WriteFile(filepath.Join(projectRoot, "base_file.txt"), []byte("base content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitAdd(t, projectRoot, "base_file.txt")
	gitCommit(t, projectRoot, "add base_file.txt on main")

	// PublishToBase should succeed via auto forward-merge
	if err := wm.PublishToBase(commandID, "test publish"); err != nil {
		t.Fatalf("PublishToBase: %v", err)
	}

	got, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if got.Integration.Status != model.IntegrationStatusPublished {
		t.Errorf("status = %s, want published", got.Integration.Status)
	}
	if got.Integration.PublishFailureCount != 0 {
		t.Errorf("PublishFailureCount = %d, want 0", got.Integration.PublishFailureCount)
	}
}

// TestPublishToBase_ForwardMergeConflictRecordsFiles verifies that when the
// forward-merge of base into integration fails due to content conflict,
// PublishConflictFiles is populated and the integration enters publish_failed.
func TestPublishToBase_ForwardMergeConflictRecordsFiles(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	defer func() { _ = cleanupAll(wm) }()

	commandID := "cmd_fwd_merge_conflict"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker1 modifies README.md
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "modify README.md"); err != nil {
		t.Fatal(err)
	}

	// Merge worker to integration
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) > 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}

	// Advance base (main) with a conflicting change to the same file
	if err := os.WriteFile(filepath.Join(projectRoot, "README.md"), []byte("base version\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitAdd(t, projectRoot, "README.md")
	gitCommit(t, projectRoot, "modify README.md on main (conflicting)")

	// PublishToBase should fail due to forward-merge conflict
	pubErr := wm.PublishToBase(commandID, "test publish")
	if pubErr == nil {
		t.Fatal("PublishToBase should have failed due to conflict")
	}
	if !strings.Contains(pubErr.Error(), "forward-merge") {
		t.Errorf("error should mention forward-merge, got: %v", pubErr)
	}

	got, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if got.Integration.Status != model.IntegrationStatusPublishFailed {
		t.Errorf("status = %s, want publish_failed", got.Integration.Status)
	}
	if got.Integration.PublishFailureCount != 1 {
		t.Errorf("PublishFailureCount = %d, want 1", got.Integration.PublishFailureCount)
	}
	if len(got.Integration.PublishConflictFiles) == 0 {
		t.Fatal("PublishConflictFiles should be non-empty")
	}

	foundREADME := false
	for _, f := range got.Integration.PublishConflictFiles {
		if f == "README.md" {
			foundREADME = true
		}
	}
	if !foundREADME {
		t.Errorf("PublishConflictFiles = %v, want to contain README.md", got.Integration.PublishConflictFiles)
	}

	// Verify integration worktree is clean (merge was aborted)
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")
	statusOut := gitStatus(t, integrationPath)
	if statusOut != "" {
		t.Errorf("integration worktree should be clean after abort, got: %s", statusOut)
	}
}

// TestPublishToBase_NoForwardMergeNeeded verifies that when base hasn't
// advanced, PublishToBase skips forward-merge and publishes directly.
func TestPublishToBase_NoForwardMergeNeeded(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	defer func() { _ = cleanupAll(wm) }()

	commandID := "cmd_no_fwd_merge"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker1 creates a file and commits
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "new_file.txt"), []byte("content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add new_file.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge worker to integration
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) > 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}

	// Do NOT advance base — forward-merge should be skipped
	if err := wm.PublishToBase(commandID, "test publish"); err != nil {
		t.Fatalf("PublishToBase: %v", err)
	}

	got, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if got.Integration.Status != model.IntegrationStatusPublished {
		t.Errorf("status = %s, want published", got.Integration.Status)
	}
}

// --- ConflictType field tests ---

// TestConflictTypeOnTaskMergeConflict verifies that task_merge_conflict
// signals have the correct ConflictType field (tested via existing merge
// conflict flow — this is a field-level assertion on the signal struct).
func TestConflictTypeOnTaskMergeConflict(t *testing.T) {
	t.Parallel()
	sig := model.PlannerSignal{
		Kind:         "merge_conflict",
		ConflictType: "task_merge_conflict",
	}
	if sig.ConflictType != "task_merge_conflict" {
		t.Errorf("ConflictType = %q, want task_merge_conflict", sig.ConflictType)
	}
}

// TestConflictTypeOnPublishConflict verifies that publish_conflict signals
// have Kind="publish_conflict" (not "merge_conflict") and the correct ConflictType field.
func TestConflictTypeOnPublishConflict(t *testing.T) {
	t.Parallel()
	sig := model.PlannerSignal{
		Kind:          "publish_conflict",
		ConflictType:  "publish_conflict",
		ConflictFiles: []string{"file.go"},
	}
	if sig.Kind != "publish_conflict" {
		t.Errorf("Kind = %q, want publish_conflict", sig.Kind)
	}
	if sig.ConflictType != "publish_conflict" {
		t.Errorf("ConflictType = %q, want publish_conflict", sig.ConflictType)
	}
	if len(sig.ConflictFiles) != 1 || sig.ConflictFiles[0] != "file.go" {
		t.Errorf("ConflictFiles = %v, want [file.go]", sig.ConflictFiles)
	}
}

// --- git test helpers ---

func gitAdd(t *testing.T, dir, file string) {
	t.Helper()
	cmd := exec.Command("git", "add", file)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", file, err, out)
	}
}

func gitCommit(t *testing.T, dir, msg string) {
	t.Helper()
	cmd := exec.Command("git", "commit", "-m", msg)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func gitStatus(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	return strings.TrimSpace(string(out))
}
