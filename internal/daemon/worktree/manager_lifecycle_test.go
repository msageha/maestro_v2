package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// TestCreateForCommand tests worktree creation for a command.
func TestCreateForCommand(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workerIDs := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_test_001", workerIDs); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Verify worktrees exist
	for _, wID := range workerIDs {
		wtPath := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_test_001", wID)
		if _, err := os.Stat(wtPath); os.IsNotExist(err) {
			t.Errorf("worktree directory not created for %s at %s", wID, wtPath)
		}
	}

	// Verify state file
	state, err := wm.GetCommandState("cmd_test_001")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if state.CommandID != "cmd_test_001" {
		t.Errorf("state.CommandID = %q, want %q", state.CommandID, "cmd_test_001")
	}
	if len(state.Workers) != 2 {
		t.Errorf("len(state.Workers) = %d, want 2", len(state.Workers))
	}
	if state.Integration.Branch != "maestro/cmd_test_001/integration" {
		t.Errorf("integration branch = %q, want %q", state.Integration.Branch, "maestro/cmd_test_001/integration")
	}
}

// TestGetWorkerPath tests retrieving the worktree path for a worker.
func TestGetWorkerPath(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_test_002", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	path, err := wm.GetWorkerPath("cmd_test_002", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	expected := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_test_002", "worker1")
	if path != expected {
		t.Errorf("path = %q, want %q", path, expected)
	}

	// Test non-existent worker
	_, err = wm.GetWorkerPath("cmd_test_002", "worker99")
	if err == nil {
		t.Error("expected error for non-existent worker")
	}
}

// TestCommitWorkerChanges tests auto-commit of worker changes.
func TestCommitWorkerChanges(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_test_003", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_test_003", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// No changes — should succeed silently
	if err := wm.CommitWorkerChanges("cmd_test_003", "worker1", "test commit"); err != nil {
		t.Fatalf("CommitWorkerChanges (no changes) failed: %v", err)
	}

	// Create a file in the worktree
	testFile := filepath.Join(wtPath, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	// Commit the changes
	if err := wm.CommitWorkerChanges("cmd_test_003", "worker1", "add test.txt"); err != nil {
		t.Fatalf("CommitWorkerChanges failed: %v", err)
	}

	// Verify the state changed to committed
	state, err := getState(wm, "cmd_test_003", "worker1")
	if err != nil {
		t.Fatalf("GetState failed: %v", err)
	}
	if state.Status != model.WorktreeStatusCommitted {
		t.Errorf("status = %q, want %q", state.Status, model.WorktreeStatusCommitted)
	}

	// Verify the commit exists in the worktree
	cmd := exec.Command("git", "log", "--oneline", "-1")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	if !strings.Contains(string(out), "add test.txt") {
		t.Errorf("commit message not found, got: %s", out)
	}
}

// TestMergeToIntegration tests merging worker branches to integration.
func TestMergeToIntegration(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_test_004", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Worker1: create file1
	wt1, err := wm.GetWorkerPath("cmd_test_004", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker1) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "file1.txt"), []byte("from worker1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_test_004", "worker1", "add file1"); err != nil {
		t.Fatal(err)
	}

	// Worker2: create file2 (no conflict)
	wt2, err := wm.GetWorkerPath("cmd_test_004", "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker2) failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "file2.txt"), []byte("from worker2"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_test_004", "worker2", "add file2"); err != nil {
		t.Fatal(err)
	}

	// Merge both to integration
	conflicts, err := wm.MergeToIntegration(context.Background(), "cmd_test_004", workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration failed: %v", err)
	}
	if len(conflicts) != 0 {
		t.Errorf("expected no conflicts, got %d: %v", len(conflicts), conflicts)
	}

	// Verify integration branch has both files
	cmd := exec.Command("git", "ls-tree", "--name-only", "maestro/cmd_test_004/integration")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-tree failed: %v", err)
	}
	outStr := string(out)
	if !strings.Contains(outStr, "file1.txt") {
		t.Error("file1.txt not found in integration branch")
	}
	if !strings.Contains(outStr, "file2.txt") {
		t.Error("file2.txt not found in integration branch")
	}
}

// TestMergeConflict tests merge conflict detection.
func TestMergeConflict(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_test_005", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Both workers modify the same file
	wt1, err := wm.GetWorkerPath("cmd_test_005", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker1) failed: %v", err)
	}
	wt2, err := wm.GetWorkerPath("cmd_test_005", "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker2) failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 changes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_test_005", "worker1", "worker1 edit README"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2 changes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_test_005", "worker2", "worker2 edit README"); err != nil {
		t.Fatal(err)
	}

	// Merge — should detect conflict on worker2 (worker1 merges first)
	conflicts, err := wm.MergeToIntegration(context.Background(), "cmd_test_005", workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration failed: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	if conflicts[0].WorkerID != "worker2" {
		t.Errorf("conflict worker = %q, want worker2", conflicts[0].WorkerID)
	}
}

// TestPublishToBase tests publishing integration to base branch.
func TestPublishToBase(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Check current branch name (could be main or master)
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = projectRoot
	branchOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("get current branch: %v", err)
	}
	currentBranch := strings.TrimSpace(string(branchOut))
	wm.config.BaseBranch = currentBranch

	workers := []string{"worker1"}
	if err := createForCommand(wm, "cmd_test_006", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Worker1: create a file
	wt1, err := wm.GetWorkerPath("cmd_test_006", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "published.txt"), []byte("published"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_test_006", "worker1", "add published.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge to integration
	if _, err := wm.MergeToIntegration(context.Background(), "cmd_test_006", workers, nil); err != nil {
		t.Fatal(err)
	}

	// Publish to base
	if err := wm.PublishToBase("cmd_test_006", ""); err != nil {
		t.Fatalf("PublishToBase failed: %v", err)
	}

	// Verify the file exists on base branch (via git ls-tree, not filesystem)
	cmd = exec.Command("git", "ls-tree", "--name-only", currentBranch)
	cmd.Dir = projectRoot
	lsOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-tree failed: %v", err)
	}
	if !strings.Contains(string(lsOut), "published.txt") {
		t.Error("published.txt not found on base branch after publish")
	}

	// Verify state
	state, err := wm.GetCommandState("cmd_test_006")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusPublished {
		t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusPublished)
	}
}

// TestCleanupCommand tests worktree cleanup.
func TestCleanupCommand(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_test_007", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Verify worktrees exist
	if !wm.HasWorktrees("cmd_test_007") {
		t.Error("HasWorktrees should be true before cleanup")
	}

	if err := wm.CleanupCommand("cmd_test_007"); err != nil {
		t.Fatalf("CleanupCommand failed: %v", err)
	}

	// Verify worktree directories are removed
	wtDir := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_test_007")
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Error("worktree directory should be removed after cleanup")
	}

	// Verify branches are removed
	cmd := exec.Command("git", "branch", "--list", "maestro/cmd_test_007/*")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("branches should be removed, got: %s", out)
	}
}

// TestCleanupAll tests cleanup of all worktrees.
func TestCleanupAll(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create worktrees for two commands
	if err := createForCommand(wm, "cmd_all_001", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}
	if err := createForCommand(wm, "cmd_all_002", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	if err := cleanupAll(wm); err != nil {
		t.Fatalf("CleanupAll failed: %v", err)
	}

	// Verify all cleaned up
	stateDir := filepath.Join(projectRoot, ".maestro", "state", "worktrees")
	entries, err := os.ReadDir(stateDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Errorf("expected 0 state files after CleanupAll, got %d", len(entries))
	}
}

// TestHasWorktrees tests the HasWorktrees check.
func TestHasWorktrees(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if wm.HasWorktrees("nonexistent") {
		t.Error("HasWorktrees should be false for non-existent command")
	}

	if err := createForCommand(wm, "cmd_test_exists", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	if !wm.HasWorktrees("cmd_test_exists") {
		t.Error("HasWorktrees should be true after creation")
	}
}

// TestGC tests garbage collection of old worktrees.
func TestGC(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Set max_worktrees to 1 so the second one triggers GC
	wm.config.GC.MaxWorktrees = ptr.Int(1)

	if err := createForCommand(wm, "cmd_gc_001", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}
	if err := createForCommand(wm, "cmd_gc_002", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	// Mark both as terminal (published) so GC is allowed to clean them up.
	// GC skips active (non-terminal) worktrees to prevent data loss.
	for _, cmdID := range []string{"cmd_gc_001", "cmd_gc_002"} {
		state, err := wm.GetCommandState(cmdID)
		if err != nil {
			t.Fatalf("GetCommandState(%s): %v", cmdID, err)
		}
		state.Integration.Status = model.IntegrationStatusPublished
		wm.mu.Lock()
		if err := wm.saveState(cmdID, state); err != nil {
			wm.mu.Unlock()
			t.Fatalf("saveState(%s): %v", cmdID, err)
		}
		wm.mu.Unlock()
	}

	// GC should remove the oldest (cmd_gc_001)
	if err := wm.GC(); err != nil {
		t.Fatalf("GC failed: %v", err)
	}

	// The oldest should be cleaned up
	stateDir := filepath.Join(projectRoot, ".maestro", "state", "worktrees")
	entries, err := os.ReadDir(stateDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadDir failed: %v", err)
	}
	// Count only .yaml files (ignore .bak backups from AtomicWrite)
	var yamlCount int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			yamlCount++
		}
	}
	if yamlCount > 1 {
		t.Errorf("expected at most 1 state file after GC (max_worktrees=1), got %d", yamlCount)
	}
}

// TestWorktreeConfigDefaults tests default values for WorktreeConfig.
func TestWorktreeConfigDefaults(t *testing.T) {
	t.Parallel()
	cfg := model.WorktreeConfig{}

	if cfg.EffectiveBaseBranch() != "main" {
		t.Errorf("default base_branch = %q, want 'main'", cfg.EffectiveBaseBranch())
	}
	if cfg.EffectivePathPrefix() != ".maestro/worktrees" {
		t.Errorf("default path_prefix = %q, want '.maestro/worktrees'", cfg.EffectivePathPrefix())
	}
	if cfg.EffectiveMergeStrategy() != "ort" {
		t.Errorf("default merge_strategy = %q, want 'ort'", cfg.EffectiveMergeStrategy())
	}

	gcCfg := model.WorktreeGCConfig{}
	if gcCfg.EffectiveTTLHours() != 24 {
		t.Errorf("default ttl_hours = %d, want 24", gcCfg.EffectiveTTLHours())
	}
	if gcCfg.EffectiveMaxWorktrees() != 32 {
		t.Errorf("default max_worktrees = %d, want 32", gcCfg.EffectiveMaxWorktrees())
	}
}

// TestCreateForCommand_RollbackOnWorktreeFailure tests that CreateForCommand
// cleans up already-created worktrees when a subsequent worktree creation fails.
func TestCreateForCommand_RollbackOnWorktreeFailure(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// First, create worktrees for worker1 successfully
	if err := createForCommand(wm, "cmd_rollback_001", []string{"worker1"}); err != nil {
		t.Fatalf("initial CreateForCommand failed: %v", err)
	}

	// Clean up worker1 but leave the branch name "maestro/cmd_rollback_002/worker1" free
	if err := wm.CleanupCommand("cmd_rollback_001"); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	// Pre-create a branch that will conflict with worker2's branch name,
	// causing CreateForCommand to fail on worker2 after worker1 succeeds.
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = projectRoot
	headSHA, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(headSHA))

	// Create the conflicting branch
	cmd = exec.Command("git", "branch", "maestro/cmd_rollback_002/worker2", sha)
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create conflicting branch failed: %v\n%s", err, out)
	}

	// Now CreateForCommand should fail on worker2 (branch already exists)
	err = createForCommand(wm, "cmd_rollback_002", []string{"worker1", "worker2"})
	if err == nil {
		t.Fatal("expected CreateForCommand to fail due to conflicting branch")
	}

	// Verify rollback: worker1's worktree should have been cleaned up
	wt1Path := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_rollback_002", "worker1")
	if _, err := os.Stat(wt1Path); !os.IsNotExist(err) {
		t.Error("worker1 worktree should have been cleaned up on rollback")
	}

	// Verify rollback: integration branch should have been cleaned up
	cmd = exec.Command("git", "branch", "--list", "maestro/cmd_rollback_002/integration")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Error("integration branch should have been cleaned up on rollback")
	}

	// Verify no state file was left behind
	if wm.HasWorktrees("cmd_rollback_002") {
		t.Error("state file should not exist after rollback")
	}

	// Clean up the conflicting branch
	cmd = exec.Command("git", "branch", "-D", "maestro/cmd_rollback_002/worker2")
	cmd.Dir = projectRoot
	_ = cmd.Run()
}

// TestEnsureWorkerWorktree_RollbackOnFailure tests that EnsureWorkerWorktree
// cleans up the integration branch when worker worktree creation fails.
func TestEnsureWorkerWorktree_RollbackOnFailure(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Pre-create a branch that will conflict with the worker's branch name
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = projectRoot
	headSHA, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(headSHA))

	cmd = exec.Command("git", "branch", "maestro/cmd_ensure_rollback/worker1", sha)
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create conflicting branch failed: %v\n%s", err, out)
	}

	// EnsureWorkerWorktree should fail (branch conflict)
	err = wm.EnsureWorkerWorktree("cmd_ensure_rollback", "worker1")
	if err == nil {
		t.Fatal("expected EnsureWorkerWorktree to fail due to conflicting branch")
	}

	// Verify rollback: integration branch should have been cleaned up
	cmd = exec.Command("git", "branch", "--list", "maestro/cmd_ensure_rollback/integration")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Error("integration branch should have been cleaned up on rollback")
	}

	// No state file should exist
	if wm.HasWorktrees("cmd_ensure_rollback") {
		t.Error("state file should not exist after rollback")
	}

	// Clean up the conflicting branch
	cmd = exec.Command("git", "branch", "-D", "maestro/cmd_ensure_rollback/worker1")
	cmd.Dir = projectRoot
	_ = cmd.Run()
}

// TestEnsureWorkerWorktree_RollbackOnAddWorker tests rollback when adding a
// worker to an existing command state fails.
func TestEnsureWorkerWorktree_RollbackOnAddWorker(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create initial state with worker1
	if err := wm.EnsureWorkerWorktree("cmd_ensure_add", "worker1"); err != nil {
		t.Fatalf("initial EnsureWorkerWorktree failed: %v", err)
	}

	// Pre-create a branch that will conflict with worker2's branch name
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = projectRoot
	headSHA, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(headSHA))

	cmd = exec.Command("git", "branch", "maestro/cmd_ensure_add/worker2", sha)
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create conflicting branch failed: %v\n%s", err, out)
	}

	// Adding worker2 should fail
	err = wm.EnsureWorkerWorktree("cmd_ensure_add", "worker2")
	if err == nil {
		t.Fatal("expected EnsureWorkerWorktree to fail for worker2")
	}

	// Verify worker1 still works (state is not corrupted)
	state, err := wm.GetCommandState("cmd_ensure_add")
	if err != nil {
		t.Fatalf("GetCommandState failed after rollback: %v", err)
	}
	if len(state.Workers) != 1 {
		t.Errorf("expected 1 worker after failed add, got %d", len(state.Workers))
	}
	if state.Workers[0].WorkerID != "worker1" {
		t.Errorf("expected worker1, got %s", state.Workers[0].WorkerID)
	}

	// Clean up
	cmd = exec.Command("git", "branch", "-D", "maestro/cmd_ensure_add/worker2")
	cmd.Dir = projectRoot
	_ = cmd.Run()
}

// TestEnsureWorkerWorktree_CorruptedStateReturnsError tests that a corrupted
// YAML state file causes EnsureWorkerWorktree to return an error instead of
// silently proceeding with the creation flow.
func TestEnsureWorkerWorktree_CorruptedStateReturnsError(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create the state directory and write a corrupted YAML file
	stateDir := filepath.Join(projectRoot, ".maestro", "state", "worktrees")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(stateDir, "cmd_corrupted.yaml")
	if err := os.WriteFile(statePath, []byte("{{invalid yaml content\n\t:::"), 0644); err != nil {
		t.Fatal(err)
	}

	err := wm.EnsureWorkerWorktree("cmd_corrupted", "worker1")
	if err == nil {
		t.Fatal("expected error for corrupted YAML state, got nil")
	}
	if !strings.Contains(err.Error(), "load worktree state") {
		t.Errorf("error should mention 'load worktree state', got: %v", err)
	}

	// Verify no integration branch was created (creation flow was NOT entered)
	cmd := exec.Command("git", "branch", "--list", "maestro/cmd_corrupted/integration")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Error("integration branch should NOT be created when state file is corrupted")
	}
}

// TestEnsureWorkerWorktree_NotExistCreatesNew tests that when no state file
// exists (os.ErrNotExist), EnsureWorkerWorktree proceeds with the creation flow.
func TestEnsureWorkerWorktree_NotExistCreatesNew(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Ensure no state file exists for this command
	stateDir := filepath.Join(projectRoot, ".maestro", "state", "worktrees")
	statePath := filepath.Join(stateDir, "cmd_notexist.yaml")
	if _, err := os.Stat(statePath); err == nil {
		t.Fatal("precondition failed: state file should not exist")
	}

	// Should succeed — creates everything from scratch
	if err := wm.EnsureWorkerWorktree("cmd_notexist", "worker1"); err != nil {
		t.Fatalf("EnsureWorkerWorktree failed: %v", err)
	}

	// Verify state was created
	state, err := wm.GetCommandState("cmd_notexist")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if len(state.Workers) != 1 || state.Workers[0].WorkerID != "worker1" {
		t.Errorf("unexpected state: workers=%v", state.Workers)
	}
}

// --- M5 Tests: EnsureWorkerWorktree ---

// TestEnsureWorkerWorktree_LazyCreation tests the initial creation path
// when no state exists for the command.
func TestEnsureWorkerWorktree_LazyCreation(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.EnsureWorkerWorktree("cmd_lazy", "worker1"); err != nil {
		t.Fatalf("EnsureWorkerWorktree failed: %v", err)
	}

	// Verify state file was created
	state, err := wm.GetCommandState("cmd_lazy")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if state.CommandID != "cmd_lazy" {
		t.Errorf("CommandID = %q, want %q", state.CommandID, "cmd_lazy")
	}
	if len(state.Workers) != 1 {
		t.Fatalf("len(Workers) = %d, want 1", len(state.Workers))
	}
	if state.Workers[0].WorkerID != "worker1" {
		t.Errorf("WorkerID = %q, want %q", state.Workers[0].WorkerID, "worker1")
	}
	if state.Workers[0].Status != model.WorktreeStatusCreated {
		t.Errorf("Status = %q, want %q", state.Workers[0].Status, model.WorktreeStatusCreated)
	}

	// Verify integration branch exists
	if state.Integration.Branch != "maestro/cmd_lazy/integration" {
		t.Errorf("Integration.Branch = %q, want %q", state.Integration.Branch, "maestro/cmd_lazy/integration")
	}

	// Verify worktree directory exists
	wtPath := state.Workers[0].Path
	if _, err := os.Stat(wtPath); os.IsNotExist(err) {
		t.Errorf("worktree directory not created at %s", wtPath)
	}
}

// TestEnsureWorkerWorktree_AddWorker tests adding a worker to an existing command state.
func TestEnsureWorkerWorktree_AddWorker(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create initial worker
	if err := wm.EnsureWorkerWorktree("cmd_add_wt", "worker1"); err != nil {
		t.Fatalf("EnsureWorkerWorktree(worker1) failed: %v", err)
	}

	// Add second worker
	if err := wm.EnsureWorkerWorktree("cmd_add_wt", "worker2"); err != nil {
		t.Fatalf("EnsureWorkerWorktree(worker2) failed: %v", err)
	}

	state, err := wm.GetCommandState("cmd_add_wt")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if len(state.Workers) != 2 {
		t.Fatalf("len(Workers) = %d, want 2", len(state.Workers))
	}

	// Verify both workers exist
	workerIDs := map[string]bool{}
	for _, ws := range state.Workers {
		workerIDs[ws.WorkerID] = true
	}
	if !workerIDs["worker1"] || !workerIDs["worker2"] {
		t.Errorf("expected worker1 and worker2, got %v", workerIDs)
	}

	// Verify worker2's worktree directory exists
	wt2Path := state.Workers[1].Path
	if _, err := os.Stat(wt2Path); os.IsNotExist(err) {
		t.Errorf("worker2 worktree directory not created at %s", wt2Path)
	}
}

// TestEnsureWorkerWorktree_Idempotent tests that calling EnsureWorkerWorktree
// for an already-existing worker is a no-op.
func TestEnsureWorkerWorktree_Idempotent(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.EnsureWorkerWorktree("cmd_idem", "worker1"); err != nil {
		t.Fatalf("first EnsureWorkerWorktree failed: %v", err)
	}

	// Call again — should be a no-op
	if err := wm.EnsureWorkerWorktree("cmd_idem", "worker1"); err != nil {
		t.Fatalf("second EnsureWorkerWorktree failed: %v", err)
	}

	state, err := wm.GetCommandState("cmd_idem")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if len(state.Workers) != 1 {
		t.Errorf("len(Workers) = %d, want 1 (idempotent)", len(state.Workers))
	}
}

// --- M5 Tests: MarkPhaseMerged ---

// TestMarkPhaseMerged_Basic tests that a phase merge is recorded correctly.
func TestMarkPhaseMerged_Basic(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_phase", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	if err := wm.MarkPhaseMerged("cmd_phase", "phase_001"); err != nil {
		t.Fatalf("MarkPhaseMerged failed: %v", err)
	}

	state, err := wm.GetCommandState("cmd_phase")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if state.MergedPhases == nil {
		t.Fatal("MergedPhases should not be nil")
	}
	if _, ok := state.MergedPhases["phase_001"]; !ok {
		t.Error("phase_001 should be in MergedPhases")
	}
}

// TestMarkPhaseMerged_ImplicitPhase tests that __implicit_phase is accepted
// by MarkPhaseMerged via the new PhaseID validation.
func TestMarkPhaseMerged_ImplicitPhase(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_implicit", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	if err := wm.MarkPhaseMerged("cmd_implicit", "__implicit_phase"); err != nil {
		t.Fatalf("MarkPhaseMerged with __implicit_phase failed: %v", err)
	}

	state, err := wm.GetCommandState("cmd_implicit")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if state.MergedPhases == nil {
		t.Fatal("MergedPhases should not be nil")
	}
	if _, ok := state.MergedPhases["__implicit_phase"]; !ok {
		t.Error("__implicit_phase should be in MergedPhases")
	}
}

// TestMarkPhaseMerged_DuplicatePhase tests that marking the same phase twice
// overwrites the timestamp without creating duplicate entries.
func TestMarkPhaseMerged_DuplicatePhase(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_phase_dup", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	if err := wm.MarkPhaseMerged("cmd_phase_dup", "phase_001"); err != nil {
		t.Fatalf("first MarkPhaseMerged failed: %v", err)
	}

	state1, err := wm.GetCommandState("cmd_phase_dup")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	ts1 := state1.MergedPhases["phase_001"]

	// Mark same phase again — should succeed (overwrite timestamp)
	if err := wm.MarkPhaseMerged("cmd_phase_dup", "phase_001"); err != nil {
		t.Fatalf("second MarkPhaseMerged failed: %v", err)
	}

	state2, err := wm.GetCommandState("cmd_phase_dup")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if len(state2.MergedPhases) != 1 {
		t.Errorf("MergedPhases should still have 1 entry, got %d", len(state2.MergedPhases))
	}
	ts2 := state2.MergedPhases["phase_001"]
	if ts2 < ts1 {
		t.Errorf("timestamp should not go backwards: %s < %s", ts2, ts1)
	}
}

// TestMarkPhaseMerged_NonExistentCommand tests that MarkPhaseMerged returns nil
// for a non-existent command (state file absent = already cleaned up).
func TestMarkPhaseMerged_NonExistentCommand(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	err := wm.MarkPhaseMerged("nonexistent_command", "phase_001")
	if err != nil {
		t.Errorf("expected nil for non-existent command (idempotent), got: %v", err)
	}
}

// TestMarkPhaseMerged_AfterCleanup tests that MarkPhaseMerged returns nil
// when the state file has been removed by CleanupCommand.
func TestMarkPhaseMerged_AfterCleanup(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_cleanup_then_mark"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Verify state file exists
	if err := wm.MarkPhaseMerged(commandID, "phase_001"); err != nil {
		t.Fatalf("MarkPhaseMerged before cleanup failed: %v", err)
	}

	// Cleanup removes the state file
	if err := wm.CleanupCommand(commandID); err != nil {
		t.Fatalf("CleanupCommand failed: %v", err)
	}

	// MarkPhaseMerged after cleanup should succeed (idempotent)
	if err := wm.MarkPhaseMerged(commandID, "phase_002"); err != nil {
		t.Errorf("MarkPhaseMerged after cleanup should return nil, got: %v", err)
	}
}

// TestCleanupCommand_DeletesCmdLocks verifies that CleanupCommand removes the
// per-command mutex entry from cmdLocks (M5 memory leak fix).
func TestCleanupCommand_DeletesCmdLocks(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_cmdlock_cleanup"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Populate cmdLocks by calling commandLock (simulates resolver usage)
	_ = wm.commandLock(commandID)

	// Verify cmdLocks has the entry
	if _, ok := wm.cmdLocks.Load(commandID); !ok {
		t.Fatal("cmdLocks should have an entry before cleanup")
	}

	if err := wm.CleanupCommand(commandID); err != nil {
		t.Fatalf("CleanupCommand failed: %v", err)
	}

	// Verify cmdLocks entry was removed
	if _, ok := wm.cmdLocks.Load(commandID); ok {
		t.Error("cmdLocks entry should be deleted after CleanupCommand")
	}
}

// TestCleanupCommand_CmdLocksReusable verifies that after CleanupCommand deletes
// a cmdLocks entry, a new entry can be created for the same commandID.
func TestCleanupCommand_CmdLocksReusable(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_cmdlock_reuse"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Populate and verify cmdLocks
	lock1 := wm.commandLock(commandID)
	if lock1 == nil {
		t.Fatal("commandLock should return non-nil mutex")
	}

	if err := wm.CleanupCommand(commandID); err != nil {
		t.Fatalf("CleanupCommand failed: %v", err)
	}

	// After cleanup, getting a commandLock should return a new instance
	lock2 := wm.commandLock(commandID)
	if lock2 == nil {
		t.Fatal("commandLock should return non-nil mutex after cleanup")
	}
	if lock1 == lock2 {
		t.Error("commandLock after cleanup should return a new mutex instance")
	}
}

// TestGC_DeletesCmdLocks verifies that GC (via cleanupCommandUnlocked) also
// removes cmdLocks entries for cleaned-up commands.
func TestGC_DeletesCmdLocks(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Set max_worktrees to 1 so the second triggers GC
	wm.config.GC.MaxWorktrees = ptr.Int(1)

	if err := createForCommand(wm, "cmd_gc_lock_1", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}
	// Populate cmdLocks for cmd_gc_lock_1
	_ = wm.commandLock("cmd_gc_lock_1")

	if err := createForCommand(wm, "cmd_gc_lock_2", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	// Mark both as terminal so GC is allowed to clean them up
	for _, cmdID := range []string{"cmd_gc_lock_1", "cmd_gc_lock_2"} {
		state, err := wm.GetCommandState(cmdID)
		if err != nil {
			t.Fatalf("GetCommandState(%s): %v", cmdID, err)
		}
		state.Integration.Status = model.IntegrationStatusPublished
		wm.mu.Lock()
		if err := wm.saveState(cmdID, state); err != nil {
			wm.mu.Unlock()
			t.Fatalf("saveState(%s): %v", cmdID, err)
		}
		wm.mu.Unlock()
	}

	// GC should remove cmd_gc_lock_1 (oldest)
	if err := wm.GC(); err != nil {
		t.Fatalf("GC failed: %v", err)
	}

	// Verify cmdLocks entry was removed for the GC'd command
	if _, ok := wm.cmdLocks.Load("cmd_gc_lock_1"); ok {
		t.Error("cmdLocks entry should be deleted after GC cleanup")
	}
}

// TestEnsureIntegrationBranchCheckedOut_AlreadyAttached is the fast path:
// integration worktree already has the integration branch checked out.
func TestEnsureIntegrationBranchCheckedOut_AlreadyAttached(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_ensure_attached"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	if err := wm.EnsureIntegrationBranchCheckedOut(commandID); err != nil {
		t.Fatalf("expected no error for already-attached worktree, got %v", err)
	}

	// State unchanged: still on integration branch.
	integrationPath := wm.integrationWorktreePath(commandID)
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	headRef, err := wm.gitOutputInDir(integrationPath, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatalf("symbolic-ref: %v", err)
	}
	if got, want := strings.TrimSpace(headRef), state.Integration.Branch; got != want {
		t.Errorf("HEAD = %q, want %q", got, want)
	}
}

// TestEnsureIntegrationBranchCheckedOut_DetachedCleanReattaches is the
// defense-in-depth path: a detached (but clean) integration worktree gets
// reattached to state.Integration.Branch so RunOnIntegration dispatch never
// lands in an orphaned-HEAD state. This directly prevents the publish loop
// that triggered the investigation.
func TestEnsureIntegrationBranchCheckedOut_DetachedCleanReattaches(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_ensure_reattach"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	integrationPath := wm.integrationWorktreePath(commandID)
	if err := wm.gitRunInDir(integrationPath, "checkout", "--detach"); err != nil {
		t.Fatalf("pre-detach checkout: %v", err)
	}
	// Sanity: symbolic-ref must fail while detached.
	if _, err := wm.gitOutputInDir(integrationPath, "symbolic-ref", "--short", "HEAD"); err == nil {
		t.Fatalf("pre-condition failed: expected detached HEAD")
	}

	if err := wm.EnsureIntegrationBranchCheckedOut(commandID); err != nil {
		t.Fatalf("EnsureIntegrationBranchCheckedOut: %v", err)
	}

	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	headRef, err := wm.gitOutputInDir(integrationPath, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatalf("symbolic-ref after reattach: %v", err)
	}
	if got, want := strings.TrimSpace(headRef), state.Integration.Branch; got != want {
		t.Errorf("HEAD after reattach = %q, want %q", got, want)
	}
}

// TestEnsureIntegrationBranchCheckedOut_DirtyWorktreeFails verifies that a
// detached worktree with uncommitted changes is refused rather than silently
// discarding the edits. The error surfaces to the dispatcher, which aborts
// the RunOnIntegration task so an operator can intervene.
func TestEnsureIntegrationBranchCheckedOut_DirtyWorktreeFails(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_ensure_dirty"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	integrationPath := wm.integrationWorktreePath(commandID)
	if err := wm.gitRunInDir(integrationPath, "checkout", "--detach"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := os.WriteFile(filepath.Join(integrationPath, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	err := wm.EnsureIntegrationBranchCheckedOut(commandID)
	if err == nil {
		t.Fatal("expected error for dirty detached worktree, got nil")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error should mention uncommitted changes, got: %v", err)
	}
}
