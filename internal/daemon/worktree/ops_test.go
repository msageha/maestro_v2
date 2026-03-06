package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// fakeClock implements core.Clock for deterministic testing.
type fakeClock struct {
	now time.Time
}

func (fc *fakeClock) Now() time.Time {
	return fc.now
}

// initTestGitRepoResolved creates a test git repo and resolves symlinks.
// On macOS, t.TempDir() returns /var/folders/... but git resolves to /private/var/folders/...
// This causes filepath.Rel mismatches in orphan detection (Reconcile/GC).
func initTestGitRepoResolved(t *testing.T) string {
	t.Helper()
	raw := initTestGitRepo(t)
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		t.Fatalf("EvalSymlinks failed: %v", err)
	}
	return resolved
}

// TestWorktreeIntegration_DiscardChanges verifies that DiscardWorkerChanges
// restores tracked file modifications but preserves untracked files.
// DiscardWorkerChanges uses `git checkout -- .` which only affects tracked files.
func TestWorktreeIntegration_DiscardChanges(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_discard_int", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_discard_int", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Modify existing tracked file
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("modified content\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a new untracked file
	if err := os.WriteFile(filepath.Join(wtPath, "untracked.txt"), []byte("untracked\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Verify worktree is dirty before discard
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status failed: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatal("worktree should be dirty before discard")
	}

	// Discard changes
	if err := wm.DiscardWorkerChanges("cmd_discard_int", "worker1"); err != nil {
		t.Fatalf("DiscardWorkerChanges failed: %v", err)
	}

	// Verify tracked file is restored to original content
	content, err := os.ReadFile(filepath.Join(wtPath, "README.md"))
	if err != nil {
		t.Fatalf("read README.md failed: %v", err)
	}
	if string(content) != "# Test\n" {
		t.Errorf("README.md should be restored, got %q", string(content))
	}

	// Untracked file should still exist (git checkout -- . does not remove untracked files)
	if _, err := os.Stat(filepath.Join(wtPath, "untracked.txt")); os.IsNotExist(err) {
		t.Error("untracked.txt should still exist after DiscardWorkerChanges")
	}

	// git status should show only the untracked file
	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("git status failed: %v", err)
	}
	statusLines := strings.TrimSpace(string(out))
	if statusLines == "" {
		t.Error("expected untracked file in status")
	}
	// All remaining lines should be untracked ("?? ...")
	for _, line := range strings.Split(statusLines, "\n") {
		if line != "" && !strings.HasPrefix(line, "?? ") {
			t.Errorf("expected only untracked entries after discard, got: %q", line)
		}
	}
}

// TestWorktreeIntegration_Reconcile tests the Reconcile method in 3 scenarios.
func TestWorktreeIntegration_Reconcile(t *testing.T) {
	t.Run("StateWithoutWorktree", func(t *testing.T) {
		// State exists but worktree directory was deleted (simulates crash)
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := wm.CreateForCommand("cmd_reconcile_a", []string{"worker1"}); err != nil {
			t.Fatalf("CreateForCommand failed: %v", err)
		}

		wtPath, err := wm.GetWorkerPath("cmd_reconcile_a", "worker1")
		if err != nil {
			t.Fatal(err)
		}

		// Forcefully remove the worktree directory (simulate crash)
		rmCmd := exec.Command("git", "worktree", "remove", "--force", wtPath)
		rmCmd.Dir = projectRoot
		if out, err := rmCmd.CombinedOutput(); err != nil {
			t.Logf("git worktree remove warning: %v\n%s", err, out)
			if err := os.RemoveAll(wtPath); err != nil {
				t.Fatalf("os.RemoveAll failed: %v", err)
			}
		}

		// Prune to clean up git worktree metadata
		pruneCmd := exec.Command("git", "worktree", "prune")
		pruneCmd.Dir = projectRoot
		if err := pruneCmd.Run(); err != nil {
			t.Fatalf("git worktree prune failed: %v", err)
		}

		// Verify directory is gone
		if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
			t.Fatal("worktree directory should not exist")
		}

		// Reconcile should detect missing worktree and update state
		wm.Reconcile()

		// Verify state updated to cleanup_done
		ws, err := wm.GetState("cmd_reconcile_a", "worker1")
		if err != nil {
			t.Fatalf("GetState failed: %v", err)
		}
		if ws.Status != model.WorktreeStatusCleanupDone {
			t.Errorf("status = %q, want %q", ws.Status, model.WorktreeStatusCleanupDone)
		}
	})

	t.Run("WorktreeWithoutState", func(t *testing.T) {
		// Worktree exists in git but has no state file (orphan)
		// Use resolved path so git worktree list paths match wm.projectRoot
		projectRoot := initTestGitRepoResolved(t)
		wm := newTestWorktreeManager(t, projectRoot)

		// Create a managed worktree first (ensures state directory exists,
		// which Reconcile requires to proceed past the early return)
		if err := wm.CreateForCommand("cmd_reconcile_managed", []string{"worker1"}); err != nil {
			t.Fatalf("CreateForCommand failed: %v", err)
		}

		// Create an orphan worktree directly under the managed prefix (no state)
		orphanPath := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_orphan", "orphan_worker")
		if err := os.MkdirAll(filepath.Dir(orphanPath), 0755); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command("git", "worktree", "add", "-b", "maestro/cmd_orphan/orphan_worker", orphanPath, "HEAD")
		cmd.Dir = projectRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git worktree add failed: %v\n%s", err, out)
		}

		// Verify orphan worktree exists
		if _, err := os.Stat(orphanPath); os.IsNotExist(err) {
			t.Fatal("orphan worktree should exist before reconcile")
		}

		// Reconcile should remove the orphaned worktree
		wm.Reconcile()

		// Verify orphan worktree is removed
		if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
			t.Error("orphan worktree directory should be removed after reconcile")
		}

		// Verify it's not in git worktree list
		listCmd := exec.Command("git", "worktree", "list", "--porcelain")
		listCmd.Dir = projectRoot
		listOut, err := listCmd.Output()
		if err != nil {
			t.Fatalf("git worktree list failed: %v", err)
		}
		if strings.Contains(string(listOut), orphanPath) {
			t.Error("orphan worktree should not appear in git worktree list after reconcile")
		}

		// Managed worktree should still exist
		managedPath, err := wm.GetWorkerPath("cmd_reconcile_managed", "worker1")
		if err != nil {
			t.Fatalf("GetWorkerPath for managed worktree failed: %v", err)
		}
		if _, err := os.Stat(managedPath); os.IsNotExist(err) {
			t.Error("managed worktree should still exist after reconcile")
		}
	})

	t.Run("NormalState_Idempotent", func(t *testing.T) {
		// Normal state — reconcile should not change anything
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := wm.CreateForCommand("cmd_reconcile_c", []string{"worker1"}); err != nil {
			t.Fatalf("CreateForCommand failed: %v", err)
		}

		// Get state before reconcile
		stateBefore, err := wm.GetState("cmd_reconcile_c", "worker1")
		if err != nil {
			t.Fatal(err)
		}

		wm.Reconcile()

		// Get state after reconcile
		stateAfter, err := wm.GetState("cmd_reconcile_c", "worker1")
		if err != nil {
			t.Fatal(err)
		}

		// Status should remain unchanged
		if stateAfter.Status != stateBefore.Status {
			t.Errorf("status changed from %q to %q", stateBefore.Status, stateAfter.Status)
		}

		// Worktree directory should still exist
		if _, err := os.Stat(stateAfter.Path); os.IsNotExist(err) {
			t.Error("worktree directory should still exist after reconcile on normal state")
		}
	})
}

// TestWorktreeIntegration_GC_TTLExpiry verifies that GC removes worktrees
// whose age exceeds the configured TTL and preserves younger ones.
func TestWorktreeIntegration_GC_TTLExpiry(t *testing.T) {
	projectRoot := initTestGitRepoResolved(t)
	wm := newTestWorktreeManager(t, projectRoot)

	baseTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	fc := &fakeClock{now: baseTime}
	wm.clock = fc
	wm.config.GC.TTLHours = 1
	wm.config.GC.MaxWorktrees = 100 // high limit so max doesn't trigger

	// Create "old" command at T=0
	if err := wm.CreateForCommand("cmd_ttl_old", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand (old) failed: %v", err)
	}
	oldPath, err := wm.GetWorkerPath("cmd_ttl_old", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath (old) failed: %v", err)
	}

	// Create "new" command at T=50min
	fc.now = baseTime.Add(50 * time.Minute)
	if err := wm.CreateForCommand("cmd_ttl_new", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand (new) failed: %v", err)
	}
	newPath, err := wm.GetWorkerPath("cmd_ttl_new", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath (new) failed: %v", err)
	}

	// Advance clock to T=70min
	// Old: 70min > 60min TTL → expired
	// New: 20min < 60min TTL → survives
	fc.now = baseTime.Add(70 * time.Minute)

	if err := wm.GC(); err != nil {
		t.Fatalf("GC failed: %v", err)
	}

	// Old worktree should be removed
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old worktree should be removed after GC (TTL expired)")
	}
	if wm.HasWorktrees("cmd_ttl_old") {
		t.Error("old worktree state file should be removed after GC")
	}

	// New worktree should still exist
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		t.Error("new worktree should still exist after GC (TTL not expired)")
	}
	if !wm.HasWorktrees("cmd_ttl_new") {
		t.Error("new worktree state file should still exist")
	}
}

// TestWorktreeIntegration_GC_Disabled verifies that GC does nothing when disabled.
func TestWorktreeIntegration_GC_Disabled(t *testing.T) {
	projectRoot := initTestGitRepoResolved(t)
	wm := newTestWorktreeManager(t, projectRoot)

	baseTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	fc := &fakeClock{now: baseTime}
	wm.clock = fc
	wm.config.GC.Enabled = false
	wm.config.GC.TTLHours = 1
	wm.config.GC.MaxWorktrees = 1

	// Create two commands (MaxWorktrees=1 would GC one if enabled)
	if err := wm.CreateForCommand("cmd_gc_dis_1", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand (1) failed: %v", err)
	}
	if err := wm.CreateForCommand("cmd_gc_dis_2", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand (2) failed: %v", err)
	}

	path1, err := wm.GetWorkerPath("cmd_gc_dis_1", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath (1) failed: %v", err)
	}
	path2, err := wm.GetWorkerPath("cmd_gc_dis_2", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath (2) failed: %v", err)
	}

	// Advance clock past TTL
	fc.now = baseTime.Add(2 * time.Hour)

	// GC should do nothing (disabled)
	if err := wm.GC(); err != nil {
		t.Fatalf("GC failed: %v", err)
	}

	// Both worktrees should still exist
	if _, err := os.Stat(path1); os.IsNotExist(err) {
		t.Error("worktree 1 should still exist (GC disabled)")
	}
	if _, err := os.Stat(path2); os.IsNotExist(err) {
		t.Error("worktree 2 should still exist (GC disabled)")
	}
	if !wm.HasWorktrees("cmd_gc_dis_1") {
		t.Error("state file 1 should still exist (GC disabled)")
	}
	if !wm.HasWorktrees("cmd_gc_dis_2") {
		t.Error("state file 2 should still exist (GC disabled)")
	}
}

// TestWorktreeIntegration_DiscardStagedChanges verifies that DiscardWorkerChanges
// correctly resets staged (git add) but uncommitted changes.
func TestWorktreeIntegration_DiscardStagedChanges(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_staged", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_staged", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	// Modify tracked file and stage it
	if err := os.WriteFile(filepath.Join(wtPath, "README.md"), []byte("staged modification\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", "README.md")
	cmd.Dir = wtPath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add failed: %v\n%s", err, out)
	}

	// Verify the change is staged (porcelain: "M " means staged modification)
	cmd = exec.Command("git", "diff", "--cached", "--name-only")
	cmd.Dir = wtPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff --cached failed: %v", err)
	}
	if !strings.Contains(string(out), "README.md") {
		t.Fatal("expected README.md to be staged")
	}

	// Discard changes (should reset staged + working tree)
	if err := wm.DiscardWorkerChanges("cmd_staged", "worker1"); err != nil {
		t.Fatalf("DiscardWorkerChanges failed: %v", err)
	}

	// Verify worktree is clean (no staged or unstaged changes)
	cmd = exec.Command("git", "status", "--porcelain")
	cmd.Dir = wtPath
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("git status after discard failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("worktree should be clean after discard of staged changes, got: %q", string(out))
	}

	// Verify content is restored to original
	content, err := os.ReadFile(filepath.Join(wtPath, "README.md"))
	if err != nil {
		t.Fatalf("read README.md failed: %v", err)
	}
	if string(content) != "# Test\n" {
		t.Errorf("README.md should be restored to original, got %q", string(content))
	}
}

// TestWorktreeIntegration_GCHealthCheck verifies that GC removes orphaned
// worktrees but preserves normally managed ones.
func TestWorktreeIntegration_GCHealthCheck(t *testing.T) {
	// Use resolved path so git worktree list paths match wm.projectRoot
	projectRoot := initTestGitRepoResolved(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create a normal managed worktree
	if err := wm.CreateForCommand("cmd_gc_health", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	normalPath, err := wm.GetWorkerPath("cmd_gc_health", "worker1")
	if err != nil {
		t.Fatal(err)
	}

	// Create an orphan worktree (no state file) under the managed prefix
	orphanPath := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_gc_orphan", "orphan_worker")
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("git", "worktree", "add", "-b", "maestro/cmd_gc_orphan/orphan_worker", orphanPath, "HEAD")
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add failed: %v\n%s", err, out)
	}

	// Verify both worktrees exist
	if _, err := os.Stat(normalPath); os.IsNotExist(err) {
		t.Fatal("normal worktree should exist before GC")
	}
	if _, err := os.Stat(orphanPath); os.IsNotExist(err) {
		t.Fatal("orphan worktree should exist before GC")
	}

	// Run GC
	if err := wm.GC(); err != nil {
		t.Fatalf("GC failed: %v", err)
	}

	// Orphaned worktree should be removed
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Error("orphan worktree should be removed after GC")
	}

	// Normal worktree should still exist
	if _, err := os.Stat(normalPath); os.IsNotExist(err) {
		t.Error("normal worktree should still exist after GC")
	}

	// Verify orphan is not in git worktree list
	listCmd := exec.Command("git", "worktree", "list", "--porcelain")
	listCmd.Dir = projectRoot
	listOut, err := listCmd.Output()
	if err != nil {
		t.Fatalf("git worktree list failed: %v", err)
	}
	if strings.Contains(string(listOut), orphanPath) {
		t.Error("orphan worktree should not appear in git worktree list after GC")
	}
	if !strings.Contains(string(listOut), normalPath) {
		t.Error("normal worktree should still appear in git worktree list after GC")
	}

	// Verify state is intact
	state, err := wm.GetCommandState("cmd_gc_health")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if len(state.Workers) != 1 || state.Workers[0].WorkerID != "worker1" {
		t.Error("managed worktree state should be intact after GC")
	}
}
