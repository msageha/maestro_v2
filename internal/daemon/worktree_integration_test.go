package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// TestWorktreeIntegration_BasicLifecycle tests the full worktree lifecycle:
// create → commit → merge → publish → cleanup.
func TestWorktreeIntegration_BasicLifecycle(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Detect actual branch name (main or master)
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = projectRoot
	branchOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("get current branch: %v", err)
	}
	baseBranch := strings.TrimSpace(string(branchOut))
	wm.config.BaseBranch = baseBranch

	commandID := "cmd_integ_basic"
	workerIDs := []string{"worker1", "worker2"}

	// Step 1: Create worktrees
	if err := wm.CreateForCommand(commandID, workerIDs); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Step 2: Verify worktree paths are different
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker1): %v", err)
	}
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker2): %v", err)
	}
	if wt1 == wt2 {
		t.Fatal("worker1 and worker2 should have different worktree paths")
	}

	// Step 3: Worker1 creates file1.txt and commits
	if err := os.WriteFile(filepath.Join(wt1, "file1.txt"), []byte("content from worker1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add file1.txt"); err != nil {
		t.Fatalf("CommitWorkerChanges(worker1): %v", err)
	}

	// Step 4: Worker2 creates file2.txt and commits
	if err := os.WriteFile(filepath.Join(wt2, "file2.txt"), []byte("content from worker2"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "add file2.txt"); err != nil {
		t.Fatalf("CommitWorkerChanges(worker2): %v", err)
	}

	// Step 5: Merge to integration
	conflicts, err := wm.MergeToIntegration(commandID, workerIDs)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d: %v", len(conflicts), conflicts)
	}

	// Step 6: Verify integration branch has both files
	integrationBranch := "maestro/" + commandID + "/integration"
	lsOut := gitLsTreeNames(t, projectRoot, integrationBranch)
	if !strings.Contains(lsOut, "file1.txt") {
		t.Error("file1.txt not found in integration branch")
	}
	if !strings.Contains(lsOut, "file2.txt") {
		t.Error("file2.txt not found in integration branch")
	}

	// Step 7: Publish to base
	if err := wm.PublishToBase(commandID); err != nil {
		t.Fatalf("PublishToBase: %v", err)
	}

	// Step 8: Verify base branch has both files
	baseLsOut := gitLsTreeNames(t, projectRoot, baseBranch)
	if !strings.Contains(baseLsOut, "file1.txt") {
		t.Error("file1.txt not found on base branch after publish")
	}
	if !strings.Contains(baseLsOut, "file2.txt") {
		t.Error("file2.txt not found on base branch after publish")
	}

	// Step 9: Verify integration status is published
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusPublished {
		t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusPublished)
	}

	// Step 10: Cleanup
	if err := wm.CleanupCommand(commandID); err != nil {
		t.Fatalf("CleanupCommand: %v", err)
	}

	// Step 11: Verify worktree directories are removed
	wtDir := filepath.Join(projectRoot, ".maestro", "worktrees", commandID)
	if _, statErr := os.Stat(wtDir); statErr == nil {
		t.Error("worktree directory should be removed after cleanup")
	} else if !os.IsNotExist(statErr) {
		t.Errorf("unexpected error checking worktree dir: %v", statErr)
	}

	// Step 12: Verify branches are removed
	cmd = exec.Command("git", "branch", "--list", "maestro/"+commandID+"/*")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("branches should be removed after cleanup, got: %s", out)
	}

	// Step 13: Verify state file is removed (HasWorktrees returns false)
	if wm.HasWorktrees(commandID) {
		t.Error("HasWorktrees should be false after cleanup")
	}
}

// TestWorktreeIntegration_CrossPhaseSync tests the cross-phase synchronization flow:
// Phase1 workers commit and merge → sync to Phase2 workers → Phase2 commits and publishes.
func TestWorktreeIntegration_CrossPhaseSync(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Detect actual branch name
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = projectRoot
	branchOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("get current branch: %v", err)
	}
	baseBranch := strings.TrimSpace(string(branchOut))
	wm.config.BaseBranch = baseBranch

	commandID := "cmd_integ_crossphase"
	workerIDs := []string{"worker1", "worker2", "worker3"}

	// Step 1: Create worktrees for all workers
	if err := wm.CreateForCommand(commandID, workerIDs); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker1): %v", err)
	}
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker2): %v", err)
	}
	wt3, err := wm.GetWorkerPath(commandID, "worker3")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker3): %v", err)
	}

	// --- Phase 1 ---

	// Step 2: Worker1 creates phase1_file1.txt
	if err := os.WriteFile(filepath.Join(wt1, "phase1_file1.txt"), []byte("phase1 from worker1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add phase1_file1.txt"); err != nil {
		t.Fatalf("CommitWorkerChanges(worker1): %v", err)
	}

	// Step 3: Worker2 creates phase1_file2.txt
	if err := os.WriteFile(filepath.Join(wt2, "phase1_file2.txt"), []byte("phase1 from worker2"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "add phase1_file2.txt"); err != nil {
		t.Fatalf("CommitWorkerChanges(worker2): %v", err)
	}

	// Step 4: Merge Phase1 workers to integration
	conflicts, err := wm.MergeToIntegration(commandID, []string{"worker1", "worker2"})
	if err != nil {
		t.Fatalf("MergeToIntegration(phase1): %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("phase1: expected no conflicts, got %d", len(conflicts))
	}

	// Step 5: Mark phase1 as merged
	if err := wm.MarkPhaseMerged(commandID, "phase1"); err != nil {
		t.Fatalf("MarkPhaseMerged(phase1): %v", err)
	}

	// Step 6: Sync integration to worker3
	if err := wm.SyncFromIntegration(commandID, []string{"worker3"}); err != nil {
		t.Fatalf("SyncFromIntegration(worker3): %v", err)
	}

	// Step 7: Verify worker3 has Phase1 files
	if _, err := os.Stat(filepath.Join(wt3, "phase1_file1.txt")); err != nil {
		t.Errorf("phase1_file1.txt not accessible in worker3 after sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt3, "phase1_file2.txt")); err != nil {
		t.Errorf("phase1_file2.txt not accessible in worker3 after sync: %v", err)
	}

	// --- Phase 2 ---

	// Step 8: Worker3 creates phase2_file3.txt referencing Phase1 files
	phase2Content := "phase2 from worker3\nuses phase1_file1.txt and phase1_file2.txt"
	if err := os.WriteFile(filepath.Join(wt3, "phase2_file3.txt"), []byte(phase2Content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker3", "add phase2_file3.txt"); err != nil {
		t.Fatalf("CommitWorkerChanges(worker3): %v", err)
	}

	// Step 9: Merge Phase2 to integration
	conflicts, err = wm.MergeToIntegration(commandID, []string{"worker3"})
	if err != nil {
		t.Fatalf("MergeToIntegration(phase2): %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("phase2: expected no conflicts, got %d", len(conflicts))
	}

	// Step 10: Publish to base
	if err := wm.PublishToBase(commandID); err != nil {
		t.Fatalf("PublishToBase: %v", err)
	}

	// Step 11: Verify base branch has all three files
	baseLsOut := gitLsTreeNames(t, projectRoot, baseBranch)
	for _, fname := range []string{"phase1_file1.txt", "phase1_file2.txt", "phase2_file3.txt"} {
		if !strings.Contains(baseLsOut, fname) {
			t.Errorf("%s not found on base branch after publish", fname)
		}
	}

	// Step 12: Verify merged phases are recorded
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if _, ok := state.MergedPhases["phase1"]; !ok {
		t.Error("phase1 should be recorded in MergedPhases")
	}

	// Step 13: Verify integration status is published
	if state.Integration.Status != model.IntegrationStatusPublished {
		t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusPublished)
	}
}

// gitLsTreeNames is a test helper that returns `git ls-tree --name-only` output for a ref.
func gitLsTreeNames(t *testing.T, projectRoot, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "ls-tree", "-r", "--name-only", ref)
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-tree %s: %v", ref, err)
	}
	return string(out)
}
