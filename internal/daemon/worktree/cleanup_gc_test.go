package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// TestGCBakFiles_OrphanRemoved verifies that a .bak file with no matching
// .yaml is removed by gcBakFiles.
func TestGCBakFiles_OrphanRemoved(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	stateDir := filepath.Join(wm.maestroDir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(stateDir, "orphan.yaml.bak")
	if err := os.WriteFile(orphan, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	wm.gcBakFiles()

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan .bak should have been removed, stat err=%v", err)
	}
}

// TestGCBakFiles_ExpiredRemoved verifies that a .bak file older than bakTTL
// is removed even when its companion .yaml still exists.
func TestGCBakFiles_ExpiredRemoved(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	queuesDir := filepath.Join(wm.maestroDir, "queues")
	if err := os.MkdirAll(queuesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(queuesDir, "q.yaml")
	bakPath := yamlPath + ".bak"
	if err := os.WriteFile(yamlPath, []byte("k: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bakPath, []byte("k: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-2 * bakTTL)
	if err := os.Chtimes(bakPath, old, old); err != nil {
		t.Fatal(err)
	}

	wm.gcBakFiles()

	if _, err := os.Stat(bakPath); !os.IsNotExist(err) {
		t.Fatalf("expired .bak should have been removed, stat err=%v", err)
	}
	if _, err := os.Stat(yamlPath); err != nil {
		t.Fatalf(".yaml companion must remain: %v", err)
	}
}

// TestGCBakFiles_FreshRetained verifies that a recent .bak with a matching
// .yaml is preserved.
func TestGCBakFiles_FreshRetained(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	resultsDir := filepath.Join(wm.maestroDir, "results")
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(resultsDir, "r.yaml")
	bakPath := yamlPath + ".bak"
	if err := os.WriteFile(yamlPath, []byte("k: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bakPath, []byte("k: prev\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wm.gcBakFiles()

	if _, err := os.Stat(bakPath); err != nil {
		t.Fatalf("fresh .bak with matching .yaml must be retained: %v", err)
	}
}

// --- C1 Tests: GC skips active (non-terminal) worktrees ---

// TestGC_SkipsActiveWorktree_TTL verifies that GC does not delete a worktree
// whose integration status is non-terminal even when the TTL has expired.
func TestGC_SkipsActiveWorktree_TTL(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Set very short TTL so our worktree will exceed it
	wm.config.GC.TTLHours = ptr.Int(0)

	commandID := "cmd_gc_active"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Verify integration status is "created" (non-terminal)
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if model.IsIntegrationTerminal(state.Integration.Status) {
		t.Fatalf("expected non-terminal status, got %s", state.Integration.Status)
	}

	// Run GC — should skip the active worktree despite TTL=0
	if err := wm.GC(); err != nil {
		t.Fatalf("GC: %v", err)
	}

	// Verify worktree was NOT cleaned up
	if _, err := wm.GetCommandState(commandID); err != nil {
		t.Errorf("active worktree should still exist after GC, got error: %v", err)
	}
}

// TestGC_CleansTerminalWorktree_TTL verifies that GC does delete a worktree
// whose integration status is terminal (published) when TTL has expired.
func TestGC_CleansTerminalWorktree_TTL(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	wm.config.GC.TTLHours = ptr.Int(0)

	commandID := "cmd_gc_terminal"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Commit worker changes and merge+publish to reach terminal state
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "gc_test.txt"), []byte("gc"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add gc_test.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := wm.PublishToBase(commandID, ""); err != nil {
		t.Fatalf("PublishToBase: %v", err)
	}

	// Verify integration is now terminal (published)
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if !model.IsIntegrationTerminal(state.Integration.Status) {
		t.Fatalf("expected terminal status, got %s", state.Integration.Status)
	}

	// Run GC — should clean up the terminal worktree
	if err := wm.GC(); err != nil {
		t.Fatalf("GC: %v", err)
	}

	// Verify worktree was cleaned up (state file removed)
	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("terminal worktree state file should be removed after GC")
	}
}

// TestGC_SkipsActiveWorktree_MaxWorktrees verifies that max_worktrees limit
// does not evict active (non-terminal) worktrees.
func TestGC_SkipsActiveWorktree_MaxWorktrees(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Set max_worktrees to 1 so that having 2 triggers eviction
	wm.config.GC.MaxWorktrees = ptr.Int(1)
	wm.config.GC.TTLHours = ptr.Int(999) // high TTL so no TTL-based cleanup

	// Create two commands — both active
	for _, cmdID := range []string{"cmd_gc_max1", "cmd_gc_max2"} {
		if err := createForCommand(wm, cmdID, []string{"worker1"}); err != nil {
			t.Fatalf("CreateForCommand(%s): %v", cmdID, err)
		}
	}

	// Both are active (created status), run GC
	if err := wm.GC(); err != nil {
		t.Fatalf("GC: %v", err)
	}

	// Both should still exist (neither should be evicted since both are active)
	for _, cmdID := range []string{"cmd_gc_max1", "cmd_gc_max2"} {
		if _, err := wm.GetCommandState(cmdID); err != nil {
			t.Errorf("active worktree %s should still exist after GC, got error: %v", cmdID, err)
		}
	}
}

// --- Failed worktree TTL-based cleanup ---

// TestGC_CleansFailedWorktree_TTLExpired verifies that GC cleans up a worktree
// whose integration status is "failed" (non-terminal) when TTL has expired.
func TestGC_CleansFailedWorktree_TTLExpired(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	wm.config.GC.TTLHours = ptr.Int(0) // TTL=0 so everything is expired

	commandID := "cmd_gc_failed_expired"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Set integration status to "failed" by directly modifying state
	wm.mu.Lock()
	state, err := wm.loadState(commandID)
	if err != nil {
		wm.mu.Unlock()
		t.Fatalf("loadState: %v", err)
	}
	state.Integration.Status = model.IntegrationStatusFailed
	state.Integration.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.saveState(commandID, state); err != nil {
		wm.mu.Unlock()
		t.Fatalf("saveState: %v", err)
	}
	wm.mu.Unlock()

	// Verify failed is NOT terminal (precondition)
	if model.IsIntegrationTerminal(model.IntegrationStatusFailed) {
		t.Fatal("IntegrationStatusFailed should not be terminal")
	}

	// Run GC — should clean up the failed worktree since TTL expired
	if err := wm.GC(); err != nil {
		t.Fatalf("GC: %v", err)
	}

	// Verify worktree was cleaned up (state file removed)
	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("failed worktree state file should be removed after GC, stat err=%v", err)
	}
}

// TestGC_RetainsFailedWorktree_TTLNotExpired verifies that GC preserves a
// worktree whose integration status is "failed" when TTL has NOT expired,
// allowing retry (failed → merging) to proceed.
func TestGC_RetainsFailedWorktree_TTLNotExpired(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	wm.config.GC.TTLHours = ptr.Int(999) // high TTL so nothing expires

	commandID := "cmd_gc_failed_retained"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Set integration status to "failed"
	wm.mu.Lock()
	state, err := wm.loadState(commandID)
	if err != nil {
		wm.mu.Unlock()
		t.Fatalf("loadState: %v", err)
	}
	state.Integration.Status = model.IntegrationStatusFailed
	state.Integration.UpdatedAt = wm.clock.Now().UTC().Format(time.RFC3339)
	if err := wm.saveState(commandID, state); err != nil {
		wm.mu.Unlock()
		t.Fatalf("saveState: %v", err)
	}
	wm.mu.Unlock()

	// Run GC — should NOT clean up (TTL not expired)
	if err := wm.GC(); err != nil {
		t.Fatalf("GC: %v", err)
	}

	// Verify worktree was NOT cleaned up
	if _, err := wm.GetCommandState(commandID); err != nil {
		t.Errorf("failed worktree with unexpired TTL should still exist after GC, got error: %v", err)
	}
}

// TestGC_TerminalStatusUnaffectedByFailedChange verifies that the existing
// terminal status (published) cleanup is not impacted by the failed worktree
// TTL cleanup logic.
func TestGC_TerminalStatusUnaffectedByFailedChange(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	wm.config.GC.TTLHours = ptr.Int(0)

	commandID := "cmd_gc_terminal_unaffected"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Drive to published (terminal) state
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "gc_test.txt"), []byte("gc"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add gc_test.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := wm.PublishToBase(commandID, ""); err != nil {
		t.Fatalf("PublishToBase: %v", err)
	}

	// Verify terminal
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusPublished {
		t.Fatalf("expected published status, got %s", state.Integration.Status)
	}

	// Run GC — should still clean up terminal worktrees as before
	if err := wm.GC(); err != nil {
		t.Fatalf("GC: %v", err)
	}

	statePath := filepath.Join(wm.maestroDir, "state", "worktrees", commandID+".yaml")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("terminal worktree state file should be removed after GC")
	}
}

// --- _publish branch leak prevention ---

// TestCleanupCommand_DeletesPublishBranch verifies that CleanupCommand deletes
// the maestro/{commandID}/_publish temporary branch if it exists (leaked from
// a crash during PublishToBase).
func TestCleanupCommand_DeletesPublishBranch(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_publish_leak"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Simulate a leaked _publish branch
	baseSHA := gitRevParse(t, projectRoot, "HEAD")
	publishBranch := fmt.Sprintf("maestro/%s/_publish", commandID)
	if err := wm.gitRun("branch", publishBranch, baseSHA); err != nil {
		t.Fatalf("create publish branch: %v", err)
	}

	// Verify the branch exists
	branchOut, err := wm.gitOutput("branch", "--list", publishBranch)
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	if !strings.Contains(branchOut, "_publish") {
		t.Fatalf("publish branch should exist before cleanup, got: %q", branchOut)
	}

	// Cleanup should delete it
	if err := wm.CleanupCommand(commandID); err != nil {
		t.Fatalf("CleanupCommand: %v", err)
	}

	// Verify the branch is gone
	branchOut, err = wm.gitOutput("branch", "--list", publishBranch)
	if err != nil {
		t.Fatalf("list branches after cleanup: %v", err)
	}
	if strings.Contains(branchOut, "_publish") {
		t.Errorf("publish branch should be deleted after cleanup, got: %q", branchOut)
	}
}

// --- M2 Tests: ensureWithinProjectRoot in CleanupCommand/cleanupCommandUnlocked ---

// TestCleanupCommand_PathGuardRejectsEscape verifies that CleanupCommand refuses
// to remove worktrees whose path escapes the project root (e.g. via symlink).
func TestCleanupCommand_PathGuardRejectsEscape(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_pathguard_cleanup"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Get the real worker path and replace it with a symlink to outside
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	workerPath := state.Workers[0].Path

	// Remove the real worktree first
	removeCmd := exec.Command("git", "worktree", "remove", "--force", workerPath)
	removeCmd.Dir = projectRoot
	_ = removeCmd.Run()
	_ = os.RemoveAll(workerPath)

	// Create an outside directory and symlink to it
	outside := t.TempDir()
	if err := os.Symlink(outside, workerPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// CleanupCommand should report a path guard error
	err = wm.CleanupCommand(commandID)
	if err == nil {
		t.Fatal("expected path guard error, got nil")
	}
	if !strings.Contains(err.Error(), "path guard") {
		t.Errorf("expected path guard error, got: %v", err)
	}
}

// TestCleanupCommandUnlocked_PathGuardRejectsEscape verifies path guard in the
// unlocked variant used by GC.
func TestCleanupCommandUnlocked_PathGuardRejectsEscape(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_pathguard_unlocked"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Load state and tamper the worker path
	wm.mu.Lock()
	state, err := wm.loadState(commandID)
	wm.mu.Unlock()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	workerPath := state.Workers[0].Path

	// Remove the real worktree
	removeCmd := exec.Command("git", "worktree", "remove", "--force", workerPath)
	removeCmd.Dir = projectRoot
	_ = removeCmd.Run()
	_ = os.RemoveAll(workerPath)

	// Create symlink to outside
	outside := t.TempDir()
	if err := os.Symlink(outside, workerPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// cleanupCommandUnlocked should report a path guard error
	wm.mu.Lock()
	err = wm.cleanupCommandUnlocked(commandID, state)
	wm.mu.Unlock()
	if err == nil {
		t.Fatal("expected path guard error, got nil")
	}
	if !strings.Contains(err.Error(), "path guard") {
		t.Errorf("expected path guard error, got: %v", err)
	}
}

// --- CleanupAll (shutdown cleanup) tests ---

// TestCleanupAll_RemovesAllWorktrees verifies that CleanupAll removes all
// worktrees regardless of their integration status (both active and terminal).
func TestCleanupAll_RemovesAllWorktrees(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create two commands
	for _, cmdID := range []string{"cmd_cleanup_all_1", "cmd_cleanup_all_2"} {
		if err := createForCommand(wm, cmdID, []string{"worker1"}); err != nil {
			t.Fatalf("CreateForCommand(%s): %v", cmdID, err)
		}
	}

	// Verify both exist
	for _, cmdID := range []string{"cmd_cleanup_all_1", "cmd_cleanup_all_2"} {
		if _, err := wm.GetCommandState(cmdID); err != nil {
			t.Fatalf("GetCommandState(%s): %v", cmdID, err)
		}
	}

	// CleanupAll with generous timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := wm.CleanupAll(ctx); err != nil {
		t.Fatalf("CleanupAll: %v", err)
	}

	// Verify both state files are removed
	stateDir := filepath.Join(wm.maestroDir, "state", "worktrees")
	for _, cmdID := range []string{"cmd_cleanup_all_1", "cmd_cleanup_all_2"} {
		statePath := filepath.Join(stateDir, cmdID+".yaml")
		if _, err := os.Stat(statePath); !os.IsNotExist(err) {
			t.Errorf("state file for %s should be removed after CleanupAll", cmdID)
		}
	}
}

// TestCleanupAll_EmptyStateDir is a no-op when no worktrees exist.
func TestCleanupAll_EmptyStateDir(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wm.CleanupAll(ctx); err != nil {
		t.Fatalf("CleanupAll on empty state: %v", err)
	}
}

// TestCleanupAll_RespectsContextCancellation verifies that CleanupAll stops
// processing when the context is cancelled.
func TestCleanupAll_RespectsContextCancellation(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create multiple commands
	for i := 0; i < 3; i++ {
		cmdID := fmt.Sprintf("cmd_cancel_%d", i)
		if err := createForCommand(wm, cmdID, []string{"worker1"}); err != nil {
			t.Fatalf("CreateForCommand(%s): %v", cmdID, err)
		}
	}

	// Use an already-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := wm.CleanupAll(ctx)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected context canceled error, got: %v", err)
	}
}
