package worktree

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// initTestGitRepo creates a temporary git repo with an initial commit.
func initTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init failed: %v\n%s", err, out)
		}
	}

	// Create initial file and commit
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cmds = [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "initial commit"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git initial commit failed: %v\n%s", err, out)
		}
	}

	return dir
}

func newTestWorktreeManager(t *testing.T, projectRoot string) *Manager {
	t.Helper()
	maestroDir := filepath.Join(projectRoot, ".maestro")
	if err := os.MkdirAll(maestroDir, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := model.WorktreeConfig{
		Enabled:          true,
		BaseBranch:       "main",
		PathPrefix:       ".maestro/worktrees",
		AutoCommit:       true,
		AutoMerge:        true,
		MergeStrategy:    "ort",
		CleanupOnSuccess: true,
		CleanupOnFailure: false,
		GC: model.WorktreeGCConfig{
			Enabled:      true,
			TTLHours:     model.IntPtr(24),
			MaxWorktrees: model.IntPtr(32),
		},
		CommitPolicy: model.CommitPolicyConfig{
			// Zero-valued: no enforcement (MaxFiles=0 means unlimited,
			// RequireGitignore=false, MessagePattern="" means no check)
		},
	}

	logger := log.New(os.Stderr, "", 0)
	return NewManager(maestroDir, cfg, logger, core.LogLevelError)
}

// TestCreateForCommand tests worktree creation for a command.
func TestCreateForCommand(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workerIDs := []string{"worker1", "worker2"}
	if err := wm.CreateForCommand("cmd_test_001", workerIDs); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_test_002", []string{"worker1"}); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_test_003", []string{"worker1"}); err != nil {
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
	state, err := wm.GetState("cmd_test_003", "worker1")
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := wm.CreateForCommand("cmd_test_004", workers); err != nil {
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
	conflicts, err := wm.MergeToIntegration("cmd_test_004", workers)
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := wm.CreateForCommand("cmd_test_005", workers); err != nil {
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
	conflicts, err := wm.MergeToIntegration("cmd_test_005", workers)
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
	projectRoot := initTestGitRepo(t)
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
	if err := wm.CreateForCommand("cmd_test_006", workers); err != nil {
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
	if _, err := wm.MergeToIntegration("cmd_test_006", workers); err != nil {
		t.Fatal(err)
	}

	// Publish to base
	if err := wm.PublishToBase("cmd_test_006"); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := wm.CreateForCommand("cmd_test_007", workers); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create worktrees for two commands
	if err := wm.CreateForCommand("cmd_all_001", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}
	if err := wm.CreateForCommand("cmd_all_002", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	if err := wm.CleanupAll(); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if wm.HasWorktrees("nonexistent") {
		t.Error("HasWorktrees should be false for non-existent command")
	}

	if err := wm.CreateForCommand("cmd_test_exists", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	if !wm.HasWorktrees("cmd_test_exists") {
		t.Error("HasWorktrees should be true after creation")
	}
}

// TestGC tests garbage collection of old worktrees.
func TestGC(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Set max_worktrees to 1 so the second one triggers GC
	wm.config.GC.MaxWorktrees = model.IntPtr(1)

	if err := wm.CreateForCommand("cmd_gc_001", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}
	if err := wm.CreateForCommand("cmd_gc_002", []string{"worker1"}); err != nil {
		t.Fatal(err)
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
	if len(entries) > 1 {
		t.Errorf("expected at most 1 state file after GC (max_worktrees=1), got %d", len(entries))
	}
}

// TestWorktreeConfigDefaults tests default values for WorktreeConfig.
func TestWorktreeConfigDefaults(t *testing.T) {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// First, create worktrees for worker1 successfully
	if err := wm.CreateForCommand("cmd_rollback_001", []string{"worker1"}); err != nil {
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
	err = wm.CreateForCommand("cmd_rollback_002", []string{"worker1", "worker2"})
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
	projectRoot := initTestGitRepo(t)
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
	projectRoot := initTestGitRepo(t)
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

// --- M5 Tests: EnsureWorkerWorktree ---

// TestEnsureWorkerWorktree_LazyCreation tests the initial creation path
// when no state exists for the command.
func TestEnsureWorkerWorktree_LazyCreation(t *testing.T) {
	projectRoot := initTestGitRepo(t)
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
	projectRoot := initTestGitRepo(t)
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
	projectRoot := initTestGitRepo(t)
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_phase", []string{"worker1"}); err != nil {
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

// TestMarkPhaseMerged_DuplicatePhase tests that marking the same phase twice
// overwrites the timestamp without creating duplicate entries.
func TestMarkPhaseMerged_DuplicatePhase(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_phase_dup", []string{"worker1"}); err != nil {
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

// TestMarkPhaseMerged_NonExistentCommand tests that MarkPhaseMerged returns
// an error for a non-existent command.
func TestMarkPhaseMerged_NonExistentCommand(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	err := wm.MarkPhaseMerged("nonexistent_command", "phase_001")
	if err == nil {
		t.Error("expected error for non-existent command")
	}
}

// TestSyncFromIntegration tests syncing integration to worker worktrees.
func TestSyncFromIntegration(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := wm.CreateForCommand("cmd_test_sync", workers); err != nil {
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
	if _, err := wm.MergeToIntegration("cmd_test_sync", []string{"worker1"}); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	headRefBefore := gitSymbolicRef(t, projectRoot)
	headSHABefore := gitRevParse(t, projectRoot, "HEAD")

	workers := []string{"worker1"}
	if err := wm.CreateForCommand("cmd_h3_merge", workers); err != nil {
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

	if _, err := wm.MergeToIntegration("cmd_h3_merge", workers); err != nil {
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
	projectRoot := initTestGitRepo(t)
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
	if err := wm.CreateForCommand("cmd_h3_pub", workers); err != nil {
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
	if _, err := wm.MergeToIntegration("cmd_h3_pub", workers); err != nil {
		t.Fatal(err)
	}

	if err := wm.PublishToBase("cmd_h3_pub"); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	currentBranch := "main"
	wm.config.BaseBranch = currentBranch

	workers := []string{"worker1"}
	if err := wm.CreateForCommand("cmd_dirty", workers); err != nil {
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
	if _, err := wm.MergeToIntegration("cmd_dirty", workers); err != nil {
		t.Fatal(err)
	}

	// Create an uncommitted change in projectRoot
	if err := os.WriteFile(filepath.Join(projectRoot, "dirty.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}

	// PublishToBase should fail because projectRoot has uncommitted changes
	err = wm.PublishToBase("cmd_dirty")
	if err == nil {
		t.Fatal("PublishToBase should have failed with uncommitted changes, but succeeded")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- M2 Test: SyncFromIntegration skips conflict workers ---

func TestSyncFromIntegration_SkipsConflictWorker(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := wm.CreateForCommand("cmd_m2", workers); err != nil {
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
	conflicts, err := wm.MergeToIntegration("cmd_m2", workers)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].WorkerID != "worker2" {
		t.Fatalf("expected conflict on worker2, got %v", conflicts)
	}

	// Verify worker2 is in conflict state
	ws2, err := wm.GetState("cmd_m2", "worker2")
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
	ws2After, err := wm.GetState("cmd_m2", "worker2")
	if err != nil {
		t.Fatalf("GetState(worker2) after sync failed: %v", err)
	}
	if ws2After.Status != model.WorktreeStatusConflict {
		t.Errorf("worker2 status after sync = %q, want conflict (should be skipped)", ws2After.Status)
	}
}

// --- M3 Test: SyncFromIntegration skips dirty worktrees ---

func TestSyncFromIntegration_SkipsDirtyWorktree(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := wm.CreateForCommand("cmd_m3", workers); err != nil {
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
	if _, err := wm.MergeToIntegration("cmd_m3", []string{"worker1"}); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_discard", []string{"worker1"}); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_int_wt", []string{"worker1"}); err != nil {
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
	t.Run("NonExistentCommand", func(t *testing.T) {
		projectRoot := initTestGitRepo(t)
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
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := wm.CreateForCommand("cmd_err_worker", []string{"worker1"}); err != nil {
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
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := wm.CreateForCommand("cmd_err_path", []string{"worker1"}); err != nil {
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
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := wm.CreateForCommand("cmd_err_commit", []string{"worker1"}); err != nil {
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
		if os.Getuid() == 0 {
			t.Skip("skipping: running as root")
		}

		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := wm.CreateForCommand("cmd_err_save", []string{"worker1"}); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_sensitive", []string{"worker1"}); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_tracked", []string{"worker1"}); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_newfile", []string{"worker1"}); err != nil {
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Default should be 120 seconds (from EffectiveGitTimeout)
	timeout := wm.gitTimeout()
	if timeout != 120*time.Second {
		t.Errorf("default timeout = %v, want 120s", timeout)
	}
}

func TestGitTimeout_Custom(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	wm.config.GitTimeoutSec = model.IntPtr(30)

	timeout := wm.gitTimeout()
	if timeout != 30*time.Second {
		t.Errorf("custom timeout = %v, want 30s", timeout)
	}
}

func TestGitRunUsesContext(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Normal git command should succeed within timeout
	if err := wm.gitRun("status"); err != nil {
		t.Fatalf("gitRun(status) failed: %v", err)
	}
}

func TestGitOutputUsesContext(t *testing.T) {
	projectRoot := initTestGitRepo(t)
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
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.gitRunInDir(projectRoot, "status"); err != nil {
		t.Fatalf("gitRunInDir(status) failed: %v", err)
	}
}

func TestGitOutputInDirUsesContext(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	output, err := wm.gitOutputInDir(projectRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("gitOutputInDir(rev-parse HEAD) failed: %v", err)
	}
	if strings.TrimSpace(output) == "" {
		t.Error("expected non-empty output from rev-parse HEAD")
	}
}

func TestEffectiveGitTimeout_Default(t *testing.T) {
	cfg := model.WorktreeConfig{}
	if cfg.EffectiveGitTimeout() != 120 {
		t.Errorf("default EffectiveGitTimeout = %d, want 120", cfg.EffectiveGitTimeout())
	}
}

func TestEffectiveGitTimeout_Custom(t *testing.T) {
	cfg := model.WorktreeConfig{GitTimeoutSec: model.IntPtr(60)}
	if cfg.EffectiveGitTimeout() != 60 {
		t.Errorf("custom EffectiveGitTimeout = %d, want 60", cfg.EffectiveGitTimeout())
	}
}

// --- CommitPolicy Tests ---

// TestCommitWorkerChanges_MaxFilesExceeded verifies that CommitWorkerChanges
// rejects commits that exceed the configured max files limit.
func TestCommitWorkerChanges_MaxFilesExceeded(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	wm.config.CommitPolicy.MaxFiles = model.IntPtr(3) // Set a low limit for testing

	if err := wm.CreateForCommand("cmd_maxfiles", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_maxfiles", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Create 5 files (exceeds limit of 3)
	for i := 0; i < 5; i++ {
		f := filepath.Join(wtPath, fmt.Sprintf("file%d.go", i))
		if err := os.WriteFile(f, []byte(fmt.Sprintf("package f%d\n", i)), 0644); err != nil {
			t.Fatal(err)
		}
	}

	err = wm.CommitWorkerChanges("cmd_maxfiles", "worker1", "[maestro] add files")
	if err == nil {
		t.Fatal("expected error for exceeding max files limit")
	}
	if !strings.Contains(err.Error(), "max_files_exceeded") {
		t.Errorf("error should mention 'max_files_exceeded', got: %v", err)
	}
}

// TestCommitWorkerChanges_MaxFilesWithinLimit verifies that commits within the
// file limit succeed.
func TestCommitWorkerChanges_MaxFilesWithinLimit(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	wm.config.CommitPolicy.MaxFiles = model.IntPtr(5)

	if err := wm.CreateForCommand("cmd_maxfiles_ok", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_maxfiles_ok", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Create 3 files (within limit of 5)
	for i := 0; i < 3; i++ {
		f := filepath.Join(wtPath, fmt.Sprintf("ok%d.go", i))
		if err := os.WriteFile(f, []byte(fmt.Sprintf("package ok%d\n", i)), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if err := wm.CommitWorkerChanges("cmd_maxfiles_ok", "worker1", "[maestro] add ok files"); err != nil {
		t.Fatalf("CommitWorkerChanges should succeed within limit: %v", err)
	}
}

// TestCommitWorkerChanges_MissingGitignore verifies that CommitWorkerChanges
// rejects commits when .gitignore is missing and RequireGitignore is true.
func TestCommitWorkerChanges_MissingGitignore(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	wm.config.CommitPolicy.RequireGitignore = true

	if err := wm.CreateForCommand("cmd_gitignore", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_gitignore", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Ensure no .gitignore exists
	os.Remove(filepath.Join(wtPath, ".gitignore"))

	// Create a file to commit
	if err := os.WriteFile(filepath.Join(wtPath, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	err = wm.CommitWorkerChanges("cmd_gitignore", "worker1", "[maestro] add main.go")
	if err == nil {
		t.Fatal("expected error for missing .gitignore")
	}
	if !strings.Contains(err.Error(), "missing_gitignore") {
		t.Errorf("error should mention 'missing_gitignore', got: %v", err)
	}
}

// TestCommitWorkerChanges_GitignorePresent verifies that commits succeed when
// .gitignore is present.
func TestCommitWorkerChanges_GitignorePresent(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	wm.config.CommitPolicy.RequireGitignore = true

	if err := wm.CreateForCommand("cmd_gitignore_ok", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_gitignore_ok", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Create .gitignore
	if err := os.WriteFile(filepath.Join(wtPath, ".gitignore"), []byte(".env\n*.key\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create a file to commit
	if err := os.WriteFile(filepath.Join(wtPath, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := wm.CommitWorkerChanges("cmd_gitignore_ok", "worker1", "[maestro] add files"); err != nil {
		t.Fatalf("CommitWorkerChanges should succeed with .gitignore present: %v", err)
	}
}

// TestCommitWorkerChanges_MessageFormatInvalid verifies that commits with
// invalid message format are rejected.
func TestCommitWorkerChanges_MessageFormatInvalid(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	wm.config.CommitPolicy.MessagePattern = `^\[maestro\]\s`

	if err := wm.CreateForCommand("cmd_msgfmt", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_msgfmt", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Create .gitignore so that check passes
	if err := os.WriteFile(filepath.Join(wtPath, ".gitignore"), []byte(".env\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Message does not match default pattern "^\[maestro\]\s"
	err = wm.CommitWorkerChanges("cmd_msgfmt", "worker1", "bad commit message")
	if err == nil {
		t.Fatal("expected error for invalid commit message format")
	}
	if !strings.Contains(err.Error(), "message_format_invalid") {
		t.Errorf("error should mention 'message_format_invalid', got: %v", err)
	}
}

// TestCommitWorkerChanges_MessageFormatValid verifies that commits with valid
// message format succeed.
func TestCommitWorkerChanges_MessageFormatValid(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	wm.config.CommitPolicy.MessagePattern = `^\[maestro\]\s`

	if err := wm.CreateForCommand("cmd_msgfmt_ok", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_msgfmt_ok", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Create .gitignore
	if err := os.WriteFile(filepath.Join(wtPath, ".gitignore"), []byte(".env\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtPath, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := wm.CommitWorkerChanges("cmd_msgfmt_ok", "worker1", "[maestro] add main.go"); err != nil {
		t.Fatalf("CommitWorkerChanges should succeed with valid message format: %v", err)
	}
}

// TestCommitWorkerChanges_RequireGitignoreDisabled verifies that commits succeed
// without .gitignore when RequireGitignore is explicitly set to false.
func TestCommitWorkerChanges_RequireGitignoreDisabled(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	// Explicitly set a non-zero policy with RequireGitignore=false
	wm.config.CommitPolicy = model.CommitPolicyConfig{
		MaxFiles:         model.IntPtr(30),
		RequireGitignore: false,
		MessagePattern:   `^\[maestro\]\s`,
	}

	if err := wm.CreateForCommand("cmd_nogitig", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_nogitig", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Ensure no .gitignore exists
	os.Remove(filepath.Join(wtPath, ".gitignore"))

	if err := os.WriteFile(filepath.Join(wtPath, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := wm.CommitWorkerChanges("cmd_nogitig", "worker1", "[maestro] add main.go"); err != nil {
		t.Fatalf("CommitWorkerChanges should succeed with RequireGitignore disabled: %v", err)
	}
}

// TestCheckCommitPolicy_Unit tests the checkCommitPolicy method directly.
func TestCheckCommitPolicy_Unit(t *testing.T) {
	projectRoot := initTestGitRepo(t)

	t.Run("all_checks_pass", func(t *testing.T) {
		wm := newTestWorktreeManager(t, projectRoot)
		wm.config.CommitPolicy.MaxFiles = model.IntPtr(30)
		wm.config.CommitPolicy.MessagePattern = `^\[maestro\]\s`

		stagedNul := "file1.go\x00file2.go\x00"
		violations := wm.checkCommitPolicy(projectRoot, "[maestro] test", stagedNul)
		if len(violations) != 0 {
			t.Errorf("expected no violations, got %d: %v", len(violations), violations)
		}
	})

	t.Run("max_files_exceeded", func(t *testing.T) {
		wm := newTestWorktreeManager(t, projectRoot)
		wm.config.CommitPolicy.MaxFiles = model.IntPtr(2)
		wm.config.CommitPolicy.MessagePattern = `^\[maestro\]\s`

		stagedNul := "a.go\x00b.go\x00c.go\x00"
		violations := wm.checkCommitPolicy(projectRoot, "[maestro] test", stagedNul)
		if len(violations) != 1 || violations[0].Code != "max_files_exceeded" {
			t.Errorf("expected max_files_exceeded violation, got %v", violations)
		}
	})

	t.Run("message_format_invalid", func(t *testing.T) {
		wm := newTestWorktreeManager(t, projectRoot)
		wm.config.CommitPolicy.MaxFiles = model.IntPtr(30)
		wm.config.CommitPolicy.MessagePattern = `^\[maestro\]\s`

		stagedNul := "file.go\x00"
		violations := wm.checkCommitPolicy(projectRoot, "no prefix", stagedNul)
		if len(violations) != 1 || violations[0].Code != "message_format_invalid" {
			t.Errorf("expected message_format_invalid violation, got %v", violations)
		}
	})

	t.Run("multiple_violations", func(t *testing.T) {
		wm := newTestWorktreeManager(t, projectRoot)
		wm.config.CommitPolicy.MaxFiles = model.IntPtr(1)
		wm.config.CommitPolicy.RequireGitignore = true
		wm.config.CommitPolicy.MessagePattern = `^\[maestro\]\s`

		// Use a temp dir without .gitignore
		tmpDir := t.TempDir()
		stagedNul := "a.go\x00b.go\x00"
		violations := wm.checkCommitPolicy(tmpDir, "bad msg", stagedNul)
		if len(violations) < 2 {
			t.Errorf("expected at least 2 violations, got %d: %v", len(violations), violations)
		}
	})
}

// TestSetWorkerStatus validates that status transitions are enforced via setWorkerStatus.
func TestSetWorkerStatus(t *testing.T) {
	t.Run("valid_transition", func(t *testing.T) {
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		ws := &model.WorktreeState{
			WorkerID: "worker1",
			Status:   model.WorktreeStatusCreated,
		}
		now := "2024-01-01T00:00:00Z"

		if err := wm.setWorkerStatus(ws, model.WorktreeStatusActive, now); err != nil {
			t.Fatalf("expected valid transition, got error: %v", err)
		}
		if ws.Status != model.WorktreeStatusActive {
			t.Errorf("status = %q, want %q", ws.Status, model.WorktreeStatusActive)
		}
		if ws.UpdatedAt != now {
			t.Errorf("updated_at = %q, want %q", ws.UpdatedAt, now)
		}
	})

	t.Run("invalid_transition_rejected", func(t *testing.T) {
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		ws := &model.WorktreeState{
			WorkerID: "worker1",
			Status:   model.WorktreeStatusCleanupDone,
		}

		err := wm.setWorkerStatus(ws, model.WorktreeStatusActive, "2024-01-01T00:00:00Z")
		if err == nil {
			t.Fatal("expected error for terminal → active transition")
		}
		// Status should not change
		if ws.Status != model.WorktreeStatusCleanupDone {
			t.Errorf("status changed to %q, should remain %q", ws.Status, model.WorktreeStatusCleanupDone)
		}
	})

	t.Run("created_to_integrated_rejected", func(t *testing.T) {
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		ws := &model.WorktreeState{
			WorkerID: "worker1",
			Status:   model.WorktreeStatusCreated,
		}

		err := wm.setWorkerStatus(ws, model.WorktreeStatusIntegrated, "2024-01-01T00:00:00Z")
		if err == nil {
			t.Fatal("expected error for created → integrated transition")
		}
	})

	t.Run("self_transition_committed", func(t *testing.T) {
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		ws := &model.WorktreeState{
			WorkerID: "worker1",
			Status:   model.WorktreeStatusCommitted,
		}

		if err := wm.setWorkerStatus(ws, model.WorktreeStatusCommitted, "2024-01-01T00:00:00Z"); err != nil {
			t.Fatalf("expected valid self-transition committed → committed, got error: %v", err)
		}
	})
}

// TestSetIntegrationStatus validates that integration status transitions are enforced.
func TestSetIntegrationStatus(t *testing.T) {
	t.Run("valid_transition", func(t *testing.T) {
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		state := &model.WorktreeCommandState{
			CommandID: "cmd1",
			Integration: model.IntegrationState{
				Status: model.IntegrationStatusCreated,
			},
		}
		now := "2024-01-01T00:00:00Z"

		if err := wm.setIntegrationStatus(state, model.IntegrationStatusMerging, now); err != nil {
			t.Fatalf("expected valid transition, got error: %v", err)
		}
		if state.Integration.Status != model.IntegrationStatusMerging {
			t.Errorf("status = %q, want %q", state.Integration.Status, model.IntegrationStatusMerging)
		}
	})

	t.Run("terminal_rejected", func(t *testing.T) {
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		state := &model.WorktreeCommandState{
			CommandID: "cmd1",
			Integration: model.IntegrationState{
				Status: model.IntegrationStatusPublished,
			},
		}

		err := wm.setIntegrationStatus(state, model.IntegrationStatusMerging, "2024-01-01T00:00:00Z")
		if err == nil {
			t.Fatal("expected error for published → merging transition")
		}
		if state.Integration.Status != model.IntegrationStatusPublished {
			t.Errorf("status changed to %q, should remain %q", state.Integration.Status, model.IntegrationStatusPublished)
		}
	})

	t.Run("failed_to_merging_allowed", func(t *testing.T) {
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		state := &model.WorktreeCommandState{
			CommandID: "cmd1",
			Integration: model.IntegrationState{
				Status: model.IntegrationStatusFailed,
			},
		}

		if err := wm.setIntegrationStatus(state, model.IntegrationStatusMerging, "2024-01-01T00:00:00Z"); err != nil {
			t.Fatalf("expected valid transition failed → merging, got error: %v", err)
		}
	})

	t.Run("merged_to_merging_allowed", func(t *testing.T) {
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		state := &model.WorktreeCommandState{
			CommandID: "cmd1",
			Integration: model.IntegrationState{
				Status: model.IntegrationStatusMerged,
			},
		}

		if err := wm.setIntegrationStatus(state, model.IntegrationStatusMerging, "2024-01-01T00:00:00Z"); err != nil {
			t.Fatalf("expected valid transition merged → merging (re-merge), got error: %v", err)
		}
	})
}

// TestIsSensitiveFile tests the sensitive file pattern matching.
func TestIsSensitiveFile(t *testing.T) {
	tests := []struct {
		name      string
		sensitive bool
	}{
		{".env", true},
		{".env.local", true},
		{".env.production", true},
		{"server.key", true},
		{"cert.pem", true},
		{"credentials.json", true},
		{"credentials.yaml", true},
		{"api.secret", true},
		{"keystore.p12", true},
		{"cert.pfx", true},
		{"main.go", false},
		{"README.md", false},
		{"config.yaml", false},
		{"Makefile", false},
		{".gitignore", false},
		{"keys.go", false},       // .go, not .key
		{"env_test.go", false},   // not .env
		{"secret_handler.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSensitiveFile(tt.name)
			if got != tt.sensitive {
				t.Errorf("isSensitiveFile(%q) = %v, want %v", tt.name, got, tt.sensitive)
			}
		})
	}
}

// TestMergeToIntegration_RollbackOnConflict verifies that when a merge conflict
// occurs, the integration branch is rolled back to its pre-merge HEAD.
func TestMergeToIntegration_RollbackOnConflict(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := wm.CreateForCommand("cmd_rollback", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Get integration worktree path and save pre-merge HEAD
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_rollback", "_integration")
	preMergeCmd := exec.Command("git", "rev-parse", "HEAD")
	preMergeCmd.Dir = integrationPath
	preMergeOut, err := preMergeCmd.Output()
	if err != nil {
		t.Fatalf("get pre-merge HEAD: %v", err)
	}
	preMergeHEAD := strings.TrimSpace(string(preMergeOut))

	// Worker1: create a unique file (will merge successfully)
	wt1, err := wm.GetWorkerPath("cmd_rollback", "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "unique1.txt"), []byte("worker1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_rollback", "worker1", "add unique1.txt"); err != nil {
		t.Fatal(err)
	}

	// Both workers modify README.md to create a conflict on worker2
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_rollback", "worker1", "worker1 modify README"); err != nil {
		t.Fatal(err)
	}

	wt2, err := wm.GetWorkerPath("cmd_rollback", "worker2")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2 readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_rollback", "worker2", "worker2 modify README"); err != nil {
		t.Fatal(err)
	}

	// Merge — worker1 succeeds, worker2 conflicts → rollback expected
	conflicts, err := wm.MergeToIntegration("cmd_rollback", workers)
	if err != nil {
		t.Fatalf("MergeToIntegration failed: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	// Verify integration branch HEAD was rolled back to pre-merge HEAD
	postMergeCmd := exec.Command("git", "rev-parse", "HEAD")
	postMergeCmd.Dir = integrationPath
	postMergeOut, err := postMergeCmd.Output()
	if err != nil {
		t.Fatalf("get post-merge HEAD: %v", err)
	}
	postMergeHEAD := strings.TrimSpace(string(postMergeOut))

	if postMergeHEAD != preMergeHEAD {
		t.Errorf("integration branch was not rolled back: pre-merge HEAD=%s, post-merge HEAD=%s",
			preMergeHEAD, postMergeHEAD)
	}

	// Verify worker1's file is NOT on integration branch (was rolled back)
	lsCmd := exec.Command("git", "ls-tree", "--name-only", "HEAD")
	lsCmd.Dir = integrationPath
	lsOut, err := lsCmd.Output()
	if err != nil {
		t.Fatalf("git ls-tree failed: %v", err)
	}
	if strings.Contains(string(lsOut), "unique1.txt") {
		t.Error("unique1.txt should not be on integration branch after rollback")
	}

	// Verify worker1's status was rolled back from "integrated" to "committed"
	cmdState, err := wm.GetCommandState("cmd_rollback")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	for _, ws := range cmdState.Workers {
		if ws.WorkerID == "worker1" {
			if ws.Status != model.WorktreeStatusCommitted {
				t.Errorf("worker1 status = %q, want %q (rolled back)", ws.Status, model.WorktreeStatusCommitted)
			}
		}
	}
}
