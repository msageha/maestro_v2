package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// TestSyncFromIntegration tests syncing integration to worker worktrees.
func TestSyncFromIntegration(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_test_sync", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Worker1 creates a file and commits
	wt1, err := wm.GetWorkerPath("cmd_test_sync", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker1) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "sync_test.txt"), []byte("sync"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_test_sync", "worker1", "add sync_test.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge worker1 to integration
	if _, err := wm.MergeToIntegration(context.Background(), "cmd_test_sync", []string{"worker1"}, nil); err != nil {
		t.Fatal(err)
	}

	// Sync integration to worker2
	if err := wm.SyncFromIntegration("cmd_test_sync", []string{"worker2"}); err != nil {
		t.Fatalf("SyncFromIntegration failed: %v", err)
	}

	// Verify worker2 now has the file from worker1
	wt2, err := wm.GetWorkerPath("cmd_test_sync", "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker2) failed: %v", err)
	}
	syncFile := filepath.Join(wt2, "sync_test.txt")
	if _, err := os.Stat(syncFile); os.IsNotExist(err) {
		t.Error("sync_test.txt not found in worker2 worktree after sync")
	}
}

// --- H3 Tests: projectRoot HEAD preservation ---

func gitSymbolicRef(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "symbolic-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git symbolic-ref HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func gitRevParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

// TestMergeToIntegration_PreservesProjectRootHEAD verifies that
// MergeToIntegration does not change projectRoot's HEAD.
func TestMergeToIntegration_PreservesProjectRootHEAD(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	headRefBefore := gitSymbolicRef(t, projectRoot)
	headSHABefore := gitRevParse(t, projectRoot, "HEAD")

	workers := []string{"worker1"}
	if err := createForCommand(wm, "cmd_h3_merge", workers); err != nil {
		t.Fatal(err)
	}

	wt1, err := wm.GetWorkerPath("cmd_h3_merge", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "h3_test.txt"), []byte("h3"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_h3_merge", "worker1", "add h3_test.txt"); err != nil {
		t.Fatal(err)
	}

	if _, err := wm.MergeToIntegration(context.Background(), "cmd_h3_merge", workers, nil); err != nil {
		t.Fatal(err)
	}

	headRefAfter := gitSymbolicRef(t, projectRoot)
	headSHAAfter := gitRevParse(t, projectRoot, "HEAD")

	if headRefBefore != headRefAfter {
		t.Errorf("HEAD ref changed: %s → %s", headRefBefore, headRefAfter)
	}
	if headSHABefore != headSHAAfter {
		t.Errorf("HEAD SHA changed: %s → %s", headSHABefore, headSHAAfter)
	}
}

// TestPublishToBase_PreservesProjectRootHEAD verifies that
// PublishToBase does not change projectRoot's symbolic ref.
func TestPublishToBase_PreservesProjectRootHEAD(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = projectRoot
	branchOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("get current branch: %v", err)
	}
	currentBranch := strings.TrimSpace(string(branchOut))
	wm.config.BaseBranch = currentBranch

	headRefBefore := gitSymbolicRef(t, projectRoot)

	workers := []string{"worker1"}
	if err := createForCommand(wm, "cmd_h3_pub", workers); err != nil {
		t.Fatal(err)
	}

	wt1, err := wm.GetWorkerPath("cmd_h3_pub", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "pub_test.txt"), []byte("pub"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_h3_pub", "worker1", "add pub_test.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration(context.Background(), "cmd_h3_pub", workers, nil); err != nil {
		t.Fatal(err)
	}

	if err := wm.PublishToBase("cmd_h3_pub", ""); err != nil {
		t.Fatalf("PublishToBase failed: %v", err)
	}

	headRefAfter := gitSymbolicRef(t, projectRoot)
	if headRefBefore != headRefAfter {
		t.Errorf("HEAD ref changed: %s → %s", headRefBefore, headRefAfter)
	}

	// Verify base branch now contains the published file
	lsCmd := exec.Command("git", "ls-tree", "--name-only", currentBranch)
	lsCmd.Dir = projectRoot
	out, err := lsCmd.Output()
	if err != nil {
		t.Fatalf("git ls-tree: %v", err)
	}
	if !strings.Contains(string(out), "pub_test.txt") {
		t.Error("pub_test.txt not found on base branch after publish")
	}
}

// --- C1 Test: PublishToBase rejects when projectRoot has uncommitted changes ---

func TestPublishToBase_RejectsUncommittedChanges(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	currentBranch := "main"
	wm.config.BaseBranch = currentBranch

	workers := []string{"worker1"}
	if err := createForCommand(wm, "cmd_dirty", workers); err != nil {
		t.Fatal(err)
	}

	// Worker1: create a file and commit
	wt1, err := wm.GetWorkerPath("cmd_dirty", "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "feature.txt"), []byte("feature"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_dirty", "worker1", "add feature.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge to integration
	if _, err := wm.MergeToIntegration(context.Background(), "cmd_dirty", workers, nil); err != nil {
		t.Fatal(err)
	}

	// Create an uncommitted change in projectRoot by modifying a tracked file.
	// Note: untracked files are intentionally ignored by the dirty check
	// (--untracked-files=no) because git reset --hard does not remove them.
	readmePath := filepath.Join(projectRoot, "README.md")
	if err := os.WriteFile(readmePath, []byte("modified content"), 0644); err != nil {
		t.Fatal(err)
	}

	// PublishToBase should fail because projectRoot has uncommitted changes
	err = wm.PublishToBase("cmd_dirty", "")
	if err == nil {
		t.Fatal("PublishToBase should have failed with uncommitted changes, but succeeded")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Dirty-root is a deterministic, non-retryable failure: the integration
	// should be quarantined immediately (bypassing the retry-with-backoff
	// cycle) so R8 can notify the Planner on the next reconcile pass.
	state, err := wm.loadState("cmd_dirty")
	if err != nil {
		t.Fatalf("loadState after dirty-root publish failed: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusQuarantined {
		t.Errorf("expected integration status=quarantined after dirty-root abort, got %q",
			state.Integration.Status)
	}
	if state.Integration.QuarantineSource != model.QuarantineSourcePublish {
		t.Errorf("expected quarantine source=publish, got %q", state.Integration.QuarantineSource)
	}
	if !strings.Contains(state.Integration.QuarantineReason, "publish_dirty_root") {
		t.Errorf("expected quarantine reason to mention publish_dirty_root, got %q",
			state.Integration.QuarantineReason)
	}
	if state.Integration.PublishFailureCount != 1 {
		t.Errorf("expected PublishFailureCount=1 (no retry budget consumed), got %d",
			state.Integration.PublishFailureCount)
	}
}

// --- M2 Test: SyncFromIntegration skips conflict workers ---

func TestSyncFromIntegration_SkipsConflictWorker(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_m2", workers); err != nil {
		t.Fatal(err)
	}

	// Worker1 and worker2 both modify README.md to create a conflict
	wt1, err := wm.GetWorkerPath("cmd_m2", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker1) failed: %v", err)
	}
	wt2, err := wm.GetWorkerPath("cmd_m2", "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker2) failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_m2", "worker1", "w1 edit"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_m2", "worker2", "w2 edit"); err != nil {
		t.Fatal(err)
	}

	// Merge — worker2 will conflict
	conflicts, err := wm.MergeToIntegration(context.Background(), "cmd_m2", workers, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].WorkerID != "worker2" {
		t.Fatalf("expected conflict on worker2, got %v", conflicts)
	}

	// Verify worker2 is in conflict state
	ws2, err := getState(wm, "cmd_m2", "worker2")
	if err != nil {
		t.Fatalf("GetState(worker2) failed: %v", err)
	}
	if ws2.Status != model.WorktreeStatusConflict {
		t.Fatalf("worker2 status = %q, want conflict", ws2.Status)
	}

	// Sync — worker2 should be skipped (no error, no change)
	if err := wm.SyncFromIntegration("cmd_m2", workers); err != nil {
		t.Fatalf("SyncFromIntegration failed: %v", err)
	}

	// worker2 should still be in conflict state (not changed to active)
	ws2After, err := getState(wm, "cmd_m2", "worker2")
	if err != nil {
		t.Fatalf("GetState(worker2) after sync failed: %v", err)
	}
	if ws2After.Status != model.WorktreeStatusConflict {
		t.Errorf("worker2 status after sync = %q, want conflict (should be skipped)", ws2After.Status)
	}
}

// --- M3 Test: SyncFromIntegration skips dirty worktrees ---

func TestSyncFromIntegration_SkipsDirtyWorktree(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_m3", workers); err != nil {
		t.Fatal(err)
	}

	// Worker1 creates and commits a file
	wt1, err := wm.GetWorkerPath("cmd_m3", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker1) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "m3_file.txt"), []byte("m3"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_m3", "worker1", "add m3_file"); err != nil {
		t.Fatal(err)
	}

	// Merge worker1 to integration
	if _, err := wm.MergeToIntegration(context.Background(), "cmd_m3", []string{"worker1"}, nil); err != nil {
		t.Fatal(err)
	}

	// Make worker2 dirty (uncommitted changes)
	wt2, err := wm.GetWorkerPath("cmd_m3", "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker2) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "dirty.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}

	// Sync — worker2 should be skipped because it's dirty
	if err := wm.SyncFromIntegration("cmd_m3", []string{"worker2"}); err != nil {
		t.Fatalf("SyncFromIntegration failed: %v", err)
	}

	// Verify worker2 did NOT get the synced file (skipped)
	syncFile := filepath.Join(wt2, "m3_file.txt")
	if _, err := os.Stat(syncFile); err == nil {
		t.Error("worker2 should NOT have m3_file.txt (should have been skipped due to dirty worktree)")
	}

	// Verify dirty file is still there
	if _, err := os.Stat(filepath.Join(wt2, "dirty.txt")); os.IsNotExist(err) {
		t.Error("dirty.txt should still exist in worker2 worktree")
	}
}

// --- DiscardWorkerChanges Test ---

func TestDiscardWorkerChanges(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_discard", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_discard", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Modify an existing tracked file
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("modified\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Verify it's dirty
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status failed: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatal("worktree should be dirty")
	}

	// Discard changes
	if err := wm.DiscardWorkerChanges("cmd_discard", "worker1"); err != nil {
		t.Fatalf("DiscardWorkerChanges failed: %v", err)
	}

	// Verify it's clean
	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("git status failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("worktree should be clean after discard, got: %s", out)
	}
}

// --- Integration Worktree Creation Test ---

func TestCreateForCommand_CreatesIntegrationWorktree(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_int_wt", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	// Verify integration worktree exists
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_int_wt", "_integration")
	if _, err := os.Stat(integrationPath); os.IsNotExist(err) {
		t.Error("integration worktree directory not created")
	}

	// Verify integration branch is checked out in the worktree
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = integrationPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --show-current in integration worktree: %v", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch != "maestro/cmd_int_wt/integration" {
		t.Errorf("integration worktree branch = %q, want %q", branch, "maestro/cmd_int_wt/integration")
	}
}

// TestCommitWorkerChanges_ErrorPaths tests error handling in CommitWorkerChanges.
func TestCommitWorkerChanges_ErrorPaths(t *testing.T) {
	t.Parallel()
	t.Run("NonExistentCommand", func(t *testing.T) {
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		err := wm.CommitWorkerChanges("nonexistent_cmd", "worker1", "msg")
		if err == nil {
			t.Fatal("expected error for non-existent command")
		}
		if !strings.Contains(err.Error(), "load state") {
			t.Errorf("error should mention 'load state', got: %v", err)
		}
	})

	t.Run("NonExistentWorker", func(t *testing.T) {
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := createForCommand(wm, "cmd_err_worker", []string{"worker1"}); err != nil {
			t.Fatalf("CreateForCommand failed: %v", err)
		}

		err := wm.CommitWorkerChanges("cmd_err_worker", "nonexistent_worker", "msg")
		if err == nil {
			t.Fatal("expected error for non-existent worker")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should mention 'not found', got: %v", err)
		}
	})

	t.Run("InvalidWorktreePath", func(t *testing.T) {
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := createForCommand(wm, "cmd_err_path", []string{"worker1"}); err != nil {
			t.Fatalf("CreateForCommand failed: %v", err)
		}

		// Corrupt the worker path in state to point to a non-existent directory
		state, err := wm.GetCommandState("cmd_err_path")
		if err != nil {
			t.Fatalf("GetCommandState failed: %v", err)
		}
		state.Workers[0].Path = filepath.Join(t.TempDir(), "nonexistent_subdir", "bogus")
		if err := wm.saveState("cmd_err_path", state); err != nil {
			t.Fatalf("saveState failed: %v", err)
		}

		err = wm.CommitWorkerChanges("cmd_err_path", "worker1", "msg")
		if err == nil {
			t.Fatal("expected error for invalid worktree path")
		}
		if !strings.Contains(err.Error(), "git status") {
			t.Errorf("error should mention 'git status', got: %v", err)
		}
	})

	t.Run("EmptyCommitMessage", func(t *testing.T) {
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := createForCommand(wm, "cmd_err_commit", []string{"worker1"}); err != nil {
			t.Fatalf("CreateForCommand failed: %v", err)
		}

		wtPath, err := wm.GetWorkerPath("cmd_err_commit", "worker1")
		if err != nil {
			t.Fatalf("GetWorkerPath failed: %v", err)
		}

		// Create a file so there are changes to commit
		if err := os.WriteFile(filepath.Join(wtPath, "test.txt"), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}

		// Empty commit message should cause git commit to fail
		err = wm.CommitWorkerChanges("cmd_err_commit", "worker1", "")
		if err == nil {
			t.Fatal("expected error for empty commit message")
		}
		if !strings.Contains(err.Error(), "git commit") {
			t.Errorf("error should mention 'git commit', got: %v", err)
		}
	})

	t.Run("UnwritableStateDir", func(t *testing.T) {
		t.Parallel()
		if os.Getuid() == 0 {
			t.Skip("skipping: running as root")
		}

		projectRoot := testutil.InitTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := createForCommand(wm, "cmd_err_save", []string{"worker1"}); err != nil {
			t.Fatalf("CreateForCommand failed: %v", err)
		}

		wtPath, err := wm.GetWorkerPath("cmd_err_save", "worker1")
		if err != nil {
			t.Fatalf("GetWorkerPath failed: %v", err)
		}

		// Create a file so there are changes to commit
		if err := os.WriteFile(filepath.Join(wtPath, "save_test.txt"), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}

		// Make state dir unwritable to cause saveState failure
		stateDir := filepath.Join(projectRoot, ".maestro", "state", "worktrees")
		if err := os.Chmod(stateDir, 0555); err != nil {
			t.Fatalf("chmod failed: %v", err)
		}
		t.Cleanup(func() {
			os.Chmod(stateDir, 0755)
		})

		err = wm.CommitWorkerChanges("cmd_err_save", "worker1", "save fail test")
		if err == nil {
			t.Fatal("expected error for unwritable state dir")
		}
		if !strings.Contains(err.Error(), "save state") {
			t.Errorf("error should mention 'save state', got: %v", err)
		}
	})
}

// TestCommitWorkerChanges_SensitiveFilesNotStaged verifies that sensitive files
// (e.g., .env, *.key, *.pem) are not staged by CommitWorkerChanges even when
// they are not covered by .gitignore.
func TestCommitWorkerChanges_SensitiveFilesNotStaged(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_sensitive", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_sensitive", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Create a legitimate file and several sensitive files
	if err := os.WriteFile(filepath.Join(wtPath, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sensitiveFiles := []string{".env", ".env.local", "server.key", "cert.pem", "credentials.json", "token.secret"}
	for _, f := range sensitiveFiles {
		if err := os.WriteFile(filepath.Join(wtPath, f), []byte("sensitive data"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Commit
	if err := wm.CommitWorkerChanges("cmd_sensitive", "worker1", "add main.go"); err != nil {
		t.Fatalf("CommitWorkerChanges failed: %v", err)
	}

	// Verify committed files: main.go should be committed, sensitive files should NOT
	cmd := exec.Command("git", "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff-tree failed: %v", err)
	}

	committedFiles := string(out)
	if !strings.Contains(committedFiles, "main.go") {
		t.Error("main.go should have been committed")
	}
	for _, f := range sensitiveFiles {
		if strings.Contains(committedFiles, f) {
			t.Errorf("sensitive file %q should NOT have been committed", f)
		}
	}

	// Verify sensitive files still exist on disk (not deleted, just not committed)
	for _, f := range sensitiveFiles {
		if _, statErr := os.Stat(filepath.Join(wtPath, f)); os.IsNotExist(statErr) {
			t.Errorf("sensitive file %q should still exist on disk", f)
		}
	}
}

// TestCommitWorkerChanges_TrackedModificationsStaged verifies that modifications
// to already-tracked files are still properly staged and committed.
func TestCommitWorkerChanges_TrackedModificationsStaged(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_tracked", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_tracked", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Modify the existing tracked file (README.md from initial commit)
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("# Updated\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Commit
	if err := wm.CommitWorkerChanges("cmd_tracked", "worker1", "update README"); err != nil {
		t.Fatalf("CommitWorkerChanges failed: %v", err)
	}

	// Verify README.md was committed
	cmd := exec.Command("git", "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff-tree failed: %v", err)
	}
	if !strings.Contains(string(out), "README.md") {
		t.Error("README.md modification should have been committed")
	}
}

// TestCommitWorkerChanges_NewFileStagedWhenSafe verifies that new non-sensitive
// files are staged and committed.
func TestCommitWorkerChanges_NewFileStagedWhenSafe(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_newfile", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_newfile", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Create new safe files
	newFiles := []string{"feature.go", "feature_test.go", "docs/guide.md"}
	for _, f := range newFiles {
		fullPath := filepath.Join(wtPath, f)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Commit
	if err := wm.CommitWorkerChanges("cmd_newfile", "worker1", "add features"); err != nil {
		t.Fatalf("CommitWorkerChanges failed: %v", err)
	}

	// Verify new files were committed
	cmd := exec.Command("git", "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff-tree failed: %v", err)
	}
	committedFiles := string(out)
	for _, f := range newFiles {
		if !strings.Contains(committedFiles, filepath.Base(f)) {
			t.Errorf("new file %q should have been committed", f)
		}
	}
}

// --- Git Timeout Tests ---

func TestGitTimeout_Default(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Default should be 120 seconds (from EffectiveGitTimeout)
	timeout := wm.gitTimeout()
	if timeout != 120*time.Second {
		t.Errorf("default timeout = %v, want 120s", timeout)
	}
}

func TestGitTimeout_Custom(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	wm.config.GitTimeoutSec = ptr.Int(30)

	timeout := wm.gitTimeout()
	if timeout != 30*time.Second {
		t.Errorf("custom timeout = %v, want 30s", timeout)
	}
}

func TestGitRunUsesContext(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Normal git command should succeed within timeout
	if err := wm.gitRun("status"); err != nil {
		t.Fatalf("gitRun(status) failed: %v", err)
	}
}

func TestGitOutputUsesContext(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Normal git output should succeed
	output, err := wm.gitOutput("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("gitOutput(rev-parse HEAD) failed: %v", err)
	}
	if strings.TrimSpace(output) == "" {
		t.Error("expected non-empty output from rev-parse HEAD")
	}
}

func TestGitRunInDirUsesContext(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.gitRunInDir(projectRoot, "status"); err != nil {
		t.Fatalf("gitRunInDir(status) failed: %v", err)
	}
}

func TestGitOutputInDirUsesContext(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	output, err := wm.gitOutputInDir(projectRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("gitOutputInDir(rev-parse HEAD) failed: %v", err)
	}
	if strings.TrimSpace(output) == "" {
		t.Error("expected non-empty output from rev-parse HEAD")
	}
}

// TestCommitWorkerChanges_RejectsInvalidTransition verifies that CommitWorkerChanges
// pre-checks the state transition to Committed and rejects it if invalid,
// preventing a git commit that cannot be rolled back.
func TestCommitWorkerChanges_RejectsInvalidTransition(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_reject_transition"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wtPath, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}

	// Create a file so there are changes
	if err := os.WriteFile(filepath.Join(wtPath, "test.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Commit successfully first
	if err := wm.CommitWorkerChanges(commandID, "worker1", "first commit"); err != nil {
		t.Fatalf("first CommitWorkerChanges: %v", err)
	}

	// Merge to integration to transition worker to "integrated"
	if _, err := wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil); err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}

	// Publish to transition worker to "published"
	if err := wm.PublishToBase(commandID, ""); err != nil {
		t.Fatalf("PublishToBase: %v", err)
	}

	// Worker is now "published". Transition to "committed" is NOT valid from "published".
	// Create another file to trigger a dirty worktree
	if err := os.WriteFile(filepath.Join(wtPath, "test2.txt"), []byte("data2"), 0644); err != nil {
		t.Fatal(err)
	}

	err = wm.CommitWorkerChanges(commandID, "worker1", "should fail")
	if err == nil {
		t.Fatal("expected error for invalid transition from published to committed")
	}
	if !strings.Contains(err.Error(), "invalid transition") {
		t.Errorf("expected 'invalid transition' error, got: %v", err)
	}
}

func TestEffectiveGitTimeout_Default(t *testing.T) {
	t.Parallel()
	cfg := model.WorktreeConfig{}
	if cfg.EffectiveGitTimeout() != 120 {
		t.Errorf("default EffectiveGitTimeout = %d, want 120", cfg.EffectiveGitTimeout())
	}
}

func TestEffectiveGitTimeout_Custom(t *testing.T) {
	t.Parallel()
	cfg := model.WorktreeConfig{GitTimeoutSec: ptr.Int(60)}
	if cfg.EffectiveGitTimeout() != 60 {
		t.Errorf("custom EffectiveGitTimeout = %d, want 60", cfg.EffectiveGitTimeout())
	}
}
