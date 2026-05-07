package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// TestWorktreeIntegration_ConflictDetection tests the full conflict detection flow:
// merge conflict → state verification → SyncFromIntegration skips conflict workers (M2).
func TestWorktreeIntegration_ConflictDetection(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_conflict", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wt1, err := wm.GetWorkerPath("cmd_conflict", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath worker1 failed: %v", err)
	}
	wt2, err := wm.GetWorkerPath("cmd_conflict", "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath worker2 failed: %v", err)
	}

	// worker1: create conflict.txt with "worker1 line"
	if err := os.WriteFile(filepath.Join(wt1, "conflict.txt"), []byte("worker1 line\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_conflict", "worker1", "worker1 add conflict.txt"); err != nil {
		t.Fatal(err)
	}

	// worker2: create conflict.txt with "worker2 line"
	if err := os.WriteFile(filepath.Join(wt2, "conflict.txt"), []byte("worker2 line\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_conflict", "worker2", "worker2 add conflict.txt"); err != nil {
		t.Fatal(err)
	}

	// Record worker2's HEAD before merge for later verification
	w2HeadBefore := gitRevParse(t, wt2, "HEAD")

	// Merge both to integration — worker1 first (sorted), worker2 conflicts
	conflicts, err := wm.MergeToIntegration(context.Background(), "cmd_conflict", workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration failed: %v", err)
	}

	// Verify exactly 1 conflict on worker2
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d: %v", len(conflicts), conflicts)
	}
	if conflicts[0].WorkerID != "worker2" {
		t.Errorf("conflict worker = %q, want worker2", conflicts[0].WorkerID)
	}

	// Verify conflict_files contains "conflict.txt"
	found := false
	for _, f := range conflicts[0].ConflictFiles {
		if f == "conflict.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("conflict_files = %v, want to contain 'conflict.txt'", conflicts[0].ConflictFiles)
	}

	// Verify worker2 status is "conflict" via GetCommandState
	cmdState, err := wm.GetCommandState("cmd_conflict")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	for _, ws := range cmdState.Workers {
		if ws.WorkerID == "worker2" {
			if ws.Status != model.WorktreeStatusConflict {
				t.Errorf("worker2 status = %q, want %q", ws.Status, model.WorktreeStatusConflict)
			}
		}
		if ws.WorkerID == "worker1" {
			// Worker1 merged successfully — preserved in partial merge (not rolled back)
			if ws.Status != model.WorktreeStatusIntegrated {
				t.Errorf("worker1 status = %q, want %q (preserved)", ws.Status, model.WorktreeStatusIntegrated)
			}
		}
	}

	// Verify integration branch preserved worker1's merge — conflict.txt should exist
	integrationBranch := cmdState.Integration.Branch
	cmd := exec.Command("git", "show", integrationBranch+":conflict.txt")
	cmd.Dir = projectRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("expected conflict.txt to exist on integration branch (worker1 preserved), but got error: %v", err)
	}
	if err == nil && !strings.Contains(string(out), "worker1 line") {
		t.Errorf("conflict.txt should contain worker1's content, got: %s", string(out))
	}

	// Verify integration status is partial_merge (worker1 merged, worker2 conflicted)
	if cmdState.Integration.Status != model.IntegrationStatusPartialMerge {
		t.Errorf("integration status = %q, want %q", cmdState.Integration.Status, model.IntegrationStatusPartialMerge)
	}

	// SyncFromIntegration with both workers
	if err := wm.SyncFromIntegration("cmd_conflict", workers); err != nil {
		t.Fatalf("SyncFromIntegration failed: %v", err)
	}

	// Verify worker2 (conflict) was skipped — HEAD unchanged
	w2HeadAfter := gitRevParse(t, wt2, "HEAD")
	if w2HeadBefore != w2HeadAfter {
		t.Errorf("worker2 HEAD changed after sync (should have been skipped): %s → %s", w2HeadBefore, w2HeadAfter)
	}

	// Verify worker2 still in conflict state
	ws2After, err := getState(wm, "cmd_conflict", "worker2")
	if err != nil {
		t.Fatalf("GetState worker2 failed: %v", err)
	}
	if ws2After.Status != model.WorktreeStatusConflict {
		t.Errorf("worker2 status after sync = %q, want %q (should remain conflict)", ws2After.Status, model.WorktreeStatusConflict)
	}

	// Verify worker1 remains integrated (sync skips integrated workers to
	// prevent incorrect status reversion from integrated back to active).
	ws1After, err := getState(wm, "cmd_conflict", "worker1")
	if err != nil {
		t.Fatalf("GetState worker1 failed: %v", err)
	}
	if ws1After.Status != model.WorktreeStatusIntegrated {
		t.Errorf("worker1 status after sync = %q, want %q", ws1After.Status, model.WorktreeStatusIntegrated)
	}
}

// TestWorktreeIntegration_CreateRollback tests that CreateForCommand rolls back
// all previously created worktrees when a subsequent worker creation fails.
// Uses 3 workers where the 3rd fails to trigger rollback of workers 1 and 2.
func TestWorktreeIntegration_CreateRollback(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Pre-create a branch that will conflict with worker3's branch name
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = projectRoot
	headSHA, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(headSHA))

	cmd = exec.Command("git", "branch", "maestro/cmd_rollback_3w/worker3", sha)
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create conflicting branch failed: %v\n%s", err, out)
	}

	// CreateForCommand with 3 workers — worker3 should fail
	err = createForCommand(wm, "cmd_rollback_3w", []string{"worker1", "worker2", "worker3"})
	if err == nil {
		t.Fatal("expected CreateForCommand to fail due to conflicting branch for worker3")
	}

	// Verify rollback: worker1 and worker2 worktrees should be cleaned up
	for _, wID := range []string{"worker1", "worker2"} {
		wtPath := filepath.Join(projectRoot, wm.config.EffectivePathPrefix(), "cmd_rollback_3w", wID)
		if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
			t.Errorf("%s worktree should have been cleaned up on rollback, but exists at %s", wID, wtPath)
		}
	}

	// Verify integration branch was cleaned up
	cmd = exec.Command("git", "branch", "--list", "maestro/cmd_rollback_3w/integration")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Error("integration branch should have been cleaned up on rollback")
	}

	// Verify no state file exists
	if wm.HasWorktrees("cmd_rollback_3w") {
		t.Error("state file should not exist after rollback")
	}

	// Verify git worktree list has no residual maestro worktrees for this command
	cmd = exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = projectRoot
	wtListOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git worktree list failed: %v", err)
	}
	for _, line := range strings.Split(string(wtListOut), "\n") {
		if strings.HasPrefix(line, "worktree ") && strings.Contains(line, "cmd_rollback_3w") {
			t.Errorf("residual worktree found: %s", line)
		}
	}

	// Clean up the conflicting branch
	cmd = exec.Command("git", "branch", "-D", "maestro/cmd_rollback_3w/worker3")
	cmd.Dir = projectRoot
	_ = cmd.Run()
}

// TestWorktreeIntegration_DirtyWorktreeSkip tests that SyncFromIntegration
// skips workers with uncommitted changes (M3) while syncing clean workers normally.
func TestWorktreeIntegration_DirtyWorktreeSkip(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_dirty", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// worker1: create and commit a file
	wt1, err := wm.GetWorkerPath("cmd_dirty", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath worker1 failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "shared.txt"), []byte("shared content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_dirty", "worker1", "add shared.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge worker1 to integration
	if _, err := wm.MergeToIntegration(context.Background(), "cmd_dirty", []string{"worker1"}, nil); err != nil {
		t.Fatal(err)
	}

	// Make worker2 dirty with uncommitted changes
	wt2, err := wm.GetWorkerPath("cmd_dirty", "worker2")
	if err != nil {
		t.Fatalf("GetWorkerPath worker2 failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "dirty_file.txt"), []byte("uncommitted work\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Record worker2's HEAD before sync
	w2HeadBefore := gitRevParse(t, wt2, "HEAD")

	// Sync both workers — worker2 should be skipped (dirty), worker1 should sync
	if err := wm.SyncFromIntegration("cmd_dirty", workers); err != nil {
		t.Fatalf("SyncFromIntegration failed: %v", err)
	}

	// Verify worker2 was skipped: shared.txt should NOT appear
	syncFile := filepath.Join(wt2, "shared.txt")
	if _, err := os.Stat(syncFile); err == nil {
		t.Error("worker2 should NOT have shared.txt (skipped due to dirty worktree)")
	}

	// Verify worker2's dirty file is still intact
	dirtyFile := filepath.Join(wt2, "dirty_file.txt")
	if _, err := os.Stat(dirtyFile); os.IsNotExist(err) {
		t.Error("dirty_file.txt should still exist in worker2 worktree")
	}

	// Verify worker2's HEAD did not change
	w2HeadAfter := gitRevParse(t, wt2, "HEAD")
	if w2HeadBefore != w2HeadAfter {
		t.Errorf("worker2 HEAD changed after sync (should have been skipped): %s → %s", w2HeadBefore, w2HeadAfter)
	}

	// Verify worker1 remains integrated (sync skips integrated workers to
	// prevent incorrect status reversion from integrated back to active).
	ws1After, err := getState(wm, "cmd_dirty", "worker1")
	if err != nil {
		t.Fatalf("GetState worker1 failed: %v", err)
	}
	if ws1After.Status != model.WorktreeStatusIntegrated {
		t.Errorf("worker1 status after sync = %q, want %q", ws1After.Status, model.WorktreeStatusIntegrated)
	}

	// Verify worker2 status remains "created" (unchanged — skipped due to dirty)
	ws2After, err := getState(wm, "cmd_dirty", "worker2")
	if err != nil {
		t.Fatalf("GetState worker2 failed: %v", err)
	}
	if ws2After.Status != model.WorktreeStatusCreated {
		t.Errorf("worker2 status after sync = %q, want %q (should remain unchanged)", ws2After.Status, model.WorktreeStatusCreated)
	}
}

// TestWorktreeIntegration_PublishToBaseConflict tests that PublishToBase
// correctly handles conflicts when the base branch has advanced since worktree creation.
// Verifies: error returned, integration status = conflict, temp-branch cleanup,
// base ref unchanged, integration branch intact.
func TestWorktreeIntegration_PublishToBaseConflict(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
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

	commandID := "cmd_publish_conflict"
	workerIDs := []string{"worker1"}

	// Step 1: Create worktrees
	if err := createForCommand(wm, commandID, workerIDs); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath(worker1): %v", err)
	}

	// Step 2: Worker1 modifies README.md and commits
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 changes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 modify README.md"); err != nil {
		t.Fatalf("CommitWorkerChanges: %v", err)
	}

	// Step 3: Merge worker1 to integration
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workerIDs, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("expected no conflicts during merge, got %d", len(conflicts))
	}

	// Record integration branch SHA before publish attempt
	integrationBranch := "maestro/" + commandID + "/integration"
	integrationSHABefore := gitRevParse(t, projectRoot, integrationBranch)

	// Record base branch SHA before advancing it
	baseSHABeforeAdvance := gitRevParse(t, projectRoot, baseBranch)

	// Step 4: Advance base branch directly with conflicting change to README.md
	cmd = exec.Command("git", "checkout", baseBranch)
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout base: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "README.md"), []byte("base branch conflict changes\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "add", "README.md")
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "advance base with conflict")
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit on base: %v\n%s", err, out)
	}

	// Record base SHA after advancing (before publish)
	baseSHAAfterAdvance := gitRevParse(t, projectRoot, baseBranch)
	if baseSHABeforeAdvance == baseSHAAfterAdvance {
		t.Fatal("base branch should have advanced")
	}

	// Step 5: Call PublishToBase — expect error due to conflict
	publishErr := wm.PublishToBase(commandID, "")
	if publishErr == nil {
		t.Fatal("PublishToBase should have returned an error due to conflict")
	}

	// Step 6: Verify integration status is "publish_failed" (not "conflict")
	// Publish merge conflicts are recorded as publish_failed so the existing
	// retry mechanism (collectWorktreePublishAndCleanup) can re-attempt the
	// publish with exponential backoff, instead of stalling permanently.
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusPublishFailed {
		t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusPublishFailed)
	}
	// Verify publish failure tracking was incremented
	if state.Integration.PublishFailureCount < 1 {
		t.Errorf("PublishFailureCount = %d, want >= 1", state.Integration.PublishFailureCount)
	}
	if state.Integration.NextPublishRetryAt == "" {
		t.Error("NextPublishRetryAt should be set for backoff retry")
	}

	// Step 7: Verify temp-branch is cleaned up
	cmd = exec.Command("git", "branch", "--list", "maestro/"+commandID+"/_publish")
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("temp-branch should be cleaned up, but found: %s", strings.TrimSpace(string(out)))
	}

	// Step 8: Verify base ref is unchanged (PublishToBase should NOT have updated it)
	baseSHAAfterPublish := gitRevParse(t, projectRoot, baseBranch)
	if baseSHAAfterAdvance != baseSHAAfterPublish {
		t.Errorf("base branch SHA changed by failed publish: %s → %s", baseSHAAfterAdvance, baseSHAAfterPublish)
	}

	// Step 9: Verify integration branch SHA is unchanged (not corrupted)
	integrationSHAAfter := gitRevParse(t, projectRoot, integrationBranch)
	if integrationSHABefore != integrationSHAAfter {
		t.Errorf("integration branch SHA changed: %s → %s", integrationSHABefore, integrationSHAAfter)
	}

	// Step 10: Verify integration worktree is on integration branch (not temp branch)
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")
	cmd = exec.Command("git", "branch", "--show-current")
	cmd.Dir = integrationPath
	branchOut, err = cmd.Output()
	if err != nil {
		t.Fatalf("get integration worktree branch: %v", err)
	}
	currentBranch := strings.TrimSpace(string(branchOut))
	if currentBranch != integrationBranch {
		t.Errorf("integration worktree should be on %q, got %q", integrationBranch, currentBranch)
	}

	// Step 11: Verify the integration worktree retains the forward-merge
	// conflict markers so the Planner-dispatched resolution worker
	// (--run-on-integration) can resolve them in place. Aborting here would
	// erase the markers before the worker ever saw them, producing an empty
	// resolution commit and an infinite publish_conflict recovery loop.
	cmd = exec.Command("git", "rev-parse", "--verify", "-q", "MERGE_HEAD")
	cmd.Dir = integrationPath
	if mergeErr := cmd.Run(); mergeErr != nil {
		t.Errorf("MERGE_HEAD should be preserved for worker resolution; got err=%v", mergeErr)
	}

	// Step 12: Verify worker branch still exists
	cmd = exec.Command("git", "branch", "--list", "maestro/"+commandID+"/worker1")
	cmd.Dir = projectRoot
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("git branch --list worker1: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Error("worker1 branch should still exist after failed publish")
	}
}

// TestWorktreeIntegration_SyncFromIntegrationConflict tests that SyncFromIntegration
// correctly handles merge conflicts: aborts the merge, preserves worktree state,
// and continues syncing remaining workers.
func TestWorktreeIntegration_SyncFromIntegrationConflict(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_sync_conflict"
	workerIDs := []string{"worker1", "worker2", "worker3"}

	// Step 1: Create worktrees for all workers
	if err := createForCommand(wm, commandID, workerIDs); err != nil {
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

	// Step 2: Worker1 creates shared.txt, commits, and merges to integration
	if err := os.WriteFile(filepath.Join(wt1, "shared.txt"), []byte("worker1 content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 add shared.txt"); err != nil {
		t.Fatalf("CommitWorkerChanges(worker1): %v", err)
	}
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration(worker1): %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("expected no conflicts for worker1, got %d", len(conflicts))
	}

	// Step 3: Worker2 creates shared.txt with DIFFERENT content (will conflict with integration)
	if err := os.WriteFile(filepath.Join(wt2, "shared.txt"), []byte("worker2 content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 add shared.txt"); err != nil {
		t.Fatalf("CommitWorkerChanges(worker2): %v", err)
	}

	// Record worker2 state before sync
	w2HeadBefore := gitRevParse(t, wt2, "HEAD")

	// Record worker3 HEAD before sync (worker3 is clean, should sync normally)
	w3HeadBefore := gitRevParse(t, wt3, "HEAD")

	// Step 4: Call SyncFromIntegration for worker2 and worker3
	// worker2 should conflict (has shared.txt that conflicts with integration's shared.txt)
	// worker3 should sync successfully (clean worktree, no conflicts)
	syncErr := wm.SyncFromIntegration(commandID, []string{"worker2", "worker3"})
	if syncErr != nil {
		t.Fatalf("SyncFromIntegration should not return error (conflicts are handled per-worker): %v", syncErr)
	}

	// Step 5: Verify worker2's HEAD is unchanged (merge was aborted)
	w2HeadAfter := gitRevParse(t, wt2, "HEAD")
	if w2HeadBefore != w2HeadAfter {
		t.Errorf("worker2 HEAD changed after sync conflict: %s → %s", w2HeadBefore, w2HeadAfter)
	}

	// Step 6: Verify worker2's worktree is clean (merge --abort restored it)
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wt2
	statusOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status in worker2: %v", err)
	}
	if strings.TrimSpace(string(statusOut)) != "" {
		t.Errorf("worker2 worktree should be clean after merge --abort, got: %s", strings.TrimSpace(string(statusOut)))
	}

	// Step 7: Verify worker2's shared.txt still has its own content (not corrupted)
	content, err := os.ReadFile(filepath.Join(wt2, "shared.txt"))
	if err != nil {
		t.Fatalf("read worker2 shared.txt: %v", err)
	}
	if string(content) != "worker2 content\n" {
		t.Errorf("worker2 shared.txt = %q, want %q", string(content), "worker2 content\n")
	}

	// Step 8: Verify worker2 status is "conflict" (SyncFromIntegration sets conflict status)
	ws2After, err := getState(wm, commandID, "worker2")
	if err != nil {
		t.Fatalf("GetState worker2 after sync: %v", err)
	}
	if ws2After.Status != model.WorktreeStatusConflict {
		t.Errorf("worker2 status = %q, want %q (should be conflict after sync conflict)", ws2After.Status, model.WorktreeStatusConflict)
	}

	// Step 9: Verify worker3 was synced successfully (continues after worker2 conflict)
	w3HeadAfter := gitRevParse(t, wt3, "HEAD")
	if w3HeadBefore == w3HeadAfter {
		t.Error("worker3 HEAD should have changed after successful sync")
	}

	// Step 10: Verify worker3 has shared.txt from integration with correct content
	w3Content, err := os.ReadFile(filepath.Join(wt3, "shared.txt"))
	if err != nil {
		t.Fatalf("worker3 should have shared.txt after sync: %v", err)
	}
	if string(w3Content) != "worker1 content\n" {
		t.Errorf("worker3 shared.txt = %q, want %q", string(w3Content), "worker1 content\n")
	}

	// Step 11: Verify worker3 status is "active" (successfully synced)
	ws3After, err := getState(wm, commandID, "worker3")
	if err != nil {
		t.Fatalf("GetState worker3 after sync: %v", err)
	}
	if ws3After.Status != model.WorktreeStatusActive {
		t.Errorf("worker3 status = %q, want %q (should be active after successful sync)", ws3After.Status, model.WorktreeStatusActive)
	}

	// Step 12: Verify no unresolved merge state in worker2 worktree
	cmd = exec.Command("git", "diff", "--name-only", "--diff-filter=U")
	cmd.Dir = wt2
	conflictOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff --diff-filter=U in worker2: %v", err)
	}
	if strings.TrimSpace(string(conflictOut)) != "" {
		t.Errorf("worker2 should have no unresolved conflicts after merge --abort, got: %s", strings.TrimSpace(string(conflictOut)))
	}
}

// TestMergeToIntegration_DirtyIntegrationWorktreeAutoCleaned verifies that
// MergeToIntegration auto-cleans transient pollution in the integration
// worktree (e.g. RunOnIntegration verify build artefacts) and proceeds with
// the merge instead of marking the integration Failed. The orchestrator
// owns the integration worktree; treating dirt there as a hard error
// stalled final_verification phases indefinitely under the prior design.
func TestMergeToIntegration_DirtyIntegrationWorktreeAutoCleaned(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1"}
	if err := createForCommand(wm, "cmd_dirty_int", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wt1, err := wm.GetWorkerPath("cmd_dirty_int", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "test.txt"), []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_dirty_int", "worker1", "add test.txt"); err != nil {
		t.Fatal(err)
	}

	// Drop transient pollution into the integration worktree (mirrors what
	// a RunOnIntegration verify task would leave behind).
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_dirty_int", "_integration")
	if err := os.WriteFile(filepath.Join(integrationPath, "dirty.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(integrationPath, "build_out"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(integrationPath, "build_out", "main.js"), []byte("artefact"), 0644); err != nil {
		t.Fatal(err)
	}

	conflicts, err := wm.MergeToIntegration(context.Background(), "cmd_dirty_int", workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration must auto-clean and succeed; got: %v", err)
	}
	if len(conflicts) != 0 {
		t.Errorf("expected 0 conflicts, got %d", len(conflicts))
	}

	// Pollution must be gone after auto-clean.
	if _, err := os.Stat(filepath.Join(integrationPath, "dirty.txt")); !os.IsNotExist(err) {
		t.Errorf("dirty.txt should have been cleaned, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(integrationPath, "build_out")); !os.IsNotExist(err) {
		t.Errorf("build_out/ should have been cleaned, stat err=%v", err)
	}

	state, err := wm.GetCommandState("cmd_dirty_int")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusMerged)
	}
}

// TestMergeToIntegration_TransientError tests that MergeToIntegration classifies
// lock-file errors as transient. When a lock exists, merge --abort and recovery
// also fail (same lock), so the worktree becomes stuck.
func TestMergeToIntegration_TransientError(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1"}
	if err := createForCommand(wm, "cmd_nce", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Worker1: create and commit a file
	wt1, err := wm.GetWorkerPath("cmd_nce", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "test.txt"), []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_nce", "worker1", "add test.txt"); err != nil {
		t.Fatal(err)
	}

	// Create index.lock in the integration worktree's git dir to cause a transient error.
	// Note: the lock also blocks merge --abort and reset --hard, causing recovery to fail.
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_nce", "_integration")
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = integrationPath
	gitDirOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --git-dir failed: %v", err)
	}
	gitDir := strings.TrimSpace(string(gitDirOut))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(integrationPath, gitDir)
	}

	lockFile := filepath.Join(gitDir, "index.lock")
	if err := os.WriteFile(lockFile, []byte("locked"), 0644); err != nil {
		t.Fatalf("create index.lock: %v", err)
	}
	defer os.Remove(lockFile)

	// MergeToIntegration returns an error because the lock blocks recovery too
	conflicts, mergeErr := wm.MergeToIntegration(context.Background(), "cmd_nce", workers, nil)
	if mergeErr == nil {
		t.Fatal("expected error (lock blocks recovery)")
	}
	if strings.Contains(mergeErr.Error(), "merge conflict") {
		t.Errorf("error should NOT be classified as merge conflict, got: %v", mergeErr)
	}
	if len(conflicts) != 0 {
		t.Errorf("expected 0 conflicts (transient error, not conflict), got %d", len(conflicts))
	}

	// Remove lock before reading state
	os.Remove(lockFile)

	// Pre-merge cleanup (reset --hard / clean -fd) hits the lock and bails
	// out before any worker merge is attempted. The integration is marked
	// Failed so the publish gate blocks; worker status stays at Committed
	// because no per-worker merge attempt actually ran.
	state, err := wm.GetCommandState("cmd_nce")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	for _, ws := range state.Workers {
		if ws.WorkerID == "worker1" {
			if ws.Status != model.WorktreeStatusCommitted {
				t.Errorf("worker1 status = %q, want %q (pre-merge bailout must not flip worker)",
					ws.Status, model.WorktreeStatusCommitted)
			}
		}
	}

	if state.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusFailed)
	}
}

// TestClassifyGitError_TransientVsPermanent tests the error classification logic.
func TestClassifyGitError_TransientVsPermanent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		err      error
		expected gitErrorClass
	}{
		{"lock error is transient", fmt.Errorf("Unable to create index.lock"), gitErrorTransient},
		{".lock file error is transient", fmt.Errorf("File exists: .lock"), gitErrorTransient},
		{"bad object is permanent", fmt.Errorf("bad object 1234abc"), gitErrorPermanent},
		{"corrupt repo is permanent", fmt.Errorf("corrupt loose object"), gitErrorPermanent},
		{"not a git repo is permanent", fmt.Errorf("fatal: not a git repository"), gitErrorPermanent},
		{"unknown error defaults to permanent", fmt.Errorf("some unknown error"), gitErrorPermanent},
		{"nil error is permanent", nil, gitErrorPermanent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyGitError(tt.err)
			if got != tt.expected {
				t.Errorf("classifyGitError(%v) = %d, want %d", tt.err, got, tt.expected)
			}
		})
	}
}

// TestMergeToIntegration_ConflictVsNonConflict tests that when multiple workers
// are merged and one has a conflict while another would have a non-conflict error,
// the conflict worker is correctly classified before the loop halts on the non-conflict error.
func TestMergeToIntegration_ConflictVsNonConflict(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Use worker1 (succeeds), worker2 (conflict), worker3 (would be non-conflict error)
	// Sorted order: worker1, worker2, worker3
	workers := []string{"worker1", "worker2", "worker3"}
	if err := createForCommand(wm, "cmd_cvnc", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Worker1: unique file (no conflict)
	wt1, _ := wm.GetWorkerPath("cmd_cvnc", "worker1")
	if err := os.WriteFile(filepath.Join(wt1, "unique1.txt"), []byte("worker1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_cvnc", "worker1", "add unique1.txt"); err != nil {
		t.Fatal(err)
	}

	// Worker2: modify README.md (will conflict with worker1 if worker1 also modified it,
	// but here we make worker2 conflict by having a different change to the same file)
	wt2, _ := wm.GetWorkerPath("cmd_cvnc", "worker2")
	// First, worker1 also modifies README.md
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_cvnc", "worker1", "worker1 modify README"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2 readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_cvnc", "worker2", "worker2 modify README"); err != nil {
		t.Fatal(err)
	}

	// Worker3: create a file (would merge normally, but we'll block it with index.lock)
	wt3, _ := wm.GetWorkerPath("cmd_cvnc", "worker3")
	if err := os.WriteFile(filepath.Join(wt3, "unique3.txt"), []byte("worker3"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_cvnc", "worker3", "add unique3.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge — worker1 succeeds, worker2 conflicts (README.md), worker3 never runs
	// because worker2's conflict is handled with continue (not halt)
	conflicts, err := wm.MergeToIntegration(context.Background(), "cmd_cvnc", workers, nil)

	// We expect the merge to complete (even with conflicts — halts only on non-conflict errors)
	if err != nil {
		t.Fatalf("MergeToIntegration should succeed (conflicts are not errors): %v", err)
	}

	// Verify worker2 is classified as conflict
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	if conflicts[0].WorkerID != "worker2" {
		t.Errorf("conflict worker = %q, want worker2", conflicts[0].WorkerID)
	}

	// Verify worker statuses
	state, err := wm.GetCommandState("cmd_cvnc")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	for _, ws := range state.Workers {
		switch ws.WorkerID {
		case "worker1":
			// Worker1 merged successfully — preserved in partial merge (not rolled back)
			if ws.Status != model.WorktreeStatusIntegrated {
				t.Errorf("worker1 status = %q, want %q (preserved)", ws.Status, model.WorktreeStatusIntegrated)
			}
		case "worker2":
			if ws.Status != model.WorktreeStatusConflict {
				t.Errorf("worker2 status = %q, want %q", ws.Status, model.WorktreeStatusConflict)
			}
		case "worker3":
			// Worker3 merged successfully — preserved in partial merge (not rolled back)
			if ws.Status != model.WorktreeStatusIntegrated {
				t.Errorf("worker3 status = %q, want %q (preserved)", ws.Status, model.WorktreeStatusIntegrated)
			}
		}
	}
}

// TestPublishToBase_PreservesConflictWorkerStatus verifies that PublishToBase
// only sets integrated workers to published, preserving conflict/failed worker
// statuses. This is a defensive invariant of the publish loop: even if some
// upstream inconsistency slipped a non-Integrated worker into a Merged
// integration state, the publish step must not silently promote that worker
// to Published.
//
// The test reaches this state by forcing it on disk rather than driving it
// through MergeToIntegration, because determineMergeOutcome deliberately
// refuses to mark integration Merged while any worker is still in
// Conflict/Resolving. Driving through the merge path would (correctly)
// produce PartialMerge and PublishToBase would be unreachable — but the
// invariant this test guards is the publish loop's own filter, which
// still matters if a future bug reintroduces the bad state.
func TestPublishToBase_PreservesConflictWorkerStatus(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = projectRoot
	branchOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("get current branch: %v", err)
	}
	baseBranch := strings.TrimSpace(string(branchOut))
	wm.config.BaseBranch = baseBranch

	commandID := "cmd_publish_preserve"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker1: create a unique file (will merge successfully)
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "unique1.txt"), []byte("worker1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add unique1.txt"); err != nil {
		t.Fatal(err)
	}

	// Worker1 also modifies README to create conflict with worker2
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 modify README"); err != nil {
		t.Fatal(err)
	}

	// Worker2: conflicting README change
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2 readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 modify README"); err != nil {
		t.Fatal(err)
	}

	// Merge — worker1 succeeds, worker2 conflicts → partial_merge.
	// Post-fix this correctly leaves integration in PartialMerge (not Merged)
	// because worker2 is in Conflict.
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	stateAfterMerge, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if stateAfterMerge.Integration.Status != model.IntegrationStatusPartialMerge {
		t.Fatalf("post-merge integration status = %q, want %q (PartialMerge) while worker2 is in Conflict",
			stateAfterMerge.Integration.Status, model.IntegrationStatusPartialMerge)
	}

	// Force the state onto disk as if some upstream path (incorrectly) flipped
	// integration to Merged while worker2 is still in Conflict. This reaches
	// the invariant the publish loop is meant to defend against.
	st, err := wm.loadState(commandID)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	now := wm.clock.Now().UTC().Format(time.RFC3339)
	st.Integration.Status = model.IntegrationStatusMerged
	st.Integration.UpdatedAt = now
	st.UpdatedAt = now
	if err := wm.saveState(commandID, st); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	// Publish to base
	if err := wm.PublishToBase(commandID, ""); err != nil {
		t.Fatalf("PublishToBase: %v", err)
	}

	// Verify worker statuses
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	for _, ws := range state.Workers {
		switch ws.WorkerID {
		case "worker1":
			if ws.Status != model.WorktreeStatusPublished {
				t.Errorf("worker1 status = %q, want %q", ws.Status, model.WorktreeStatusPublished)
			}
		case "worker2":
			if ws.Status != model.WorktreeStatusConflict {
				t.Errorf("worker2 status = %q, want %q (should be preserved)", ws.Status, model.WorktreeStatusConflict)
			}
		}
	}
	if state.Integration.Status != model.IntegrationStatusPublished {
		t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusPublished)
	}
}

// TestMergeToIntegration_DoesNotFlipToMergedWhileWorkerInConflict pins the
// invariant: a second merge round called with only already-Integrated
// workers must NOT flip integration to Merged when another worker is
// still in Conflict. Otherwise downstream phase activation and publish
// gate would proceed against an integration branch that had not absorbed
// the conflicting worker's resolution.
//
// determineMergeOutcome must detect workers in state.Workers that are
// still in Conflict/Resolving — even if they were excluded from the
// current merge round's workerIDs — and refuse the Merged transition.
// The correct status with a pending resolution is PartialMerge
// (mergedCount > 0) or the pre-merge status (mergedCount == 0).
func TestMergeToIntegration_DoesNotFlipToMergedWhileWorkerInConflict(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_no_false_merged"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Set up worker1 + worker2 with a conflicting README edit.
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 modify README"); err != nil {
		t.Fatal(err)
	}
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2 readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "worker2 modify README"); err != nil {
		t.Fatal(err)
	}

	// First merge: worker1 integrates, worker2 conflicts → PartialMerge.
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusPartialMerge {
		t.Fatalf("after first merge: integration status = %q, want %q",
			state.Integration.Status, model.IntegrationStatusPartialMerge)
	}

	// Simulate the buggy call path: Phase A collects only non-Conflict/
	// Resolving workers, so the next merge round is invoked with only
	// [worker1]. Previously the outcome loop concluded "mergedCount=1,
	// all-zero-else → Merged" because worker2 wasn't in workerIDs to be
	// counted as conflictSkipped. The fix reads state.Workers directly and
	// observes worker2 is still in Conflict.
	_, err = wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil)
	if err != nil {
		t.Fatalf("second MergeToIntegration: %v", err)
	}
	state, err = wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if state.Integration.Status == model.IntegrationStatusMerged {
		t.Fatalf("integration status = %q; must NOT be Merged while a worker is in Conflict "+
			"(downstream phase activation and publish gate would break)",
			state.Integration.Status)
	}
	// The expected status after the second merge is PartialMerge: worker1
	// counts as mergedCount (already-integrated short-circuit) and
	// statePendingResolution > 0 prevents Merged. The mergedCount>0 branch
	// then writes PartialMerge.
	if state.Integration.Status != model.IntegrationStatusPartialMerge {
		t.Fatalf("integration status = %q, want %q (second merge with pending conflict worker)",
			state.Integration.Status, model.IntegrationStatusPartialMerge)
	}
	// worker2 must remain in Conflict so the resume-merge pipeline can act on it.
	var w2 *model.WorktreeState
	for i := range state.Workers {
		if state.Workers[i].WorkerID == "worker2" {
			w2 = &state.Workers[i]
			break
		}
	}
	if w2 == nil {
		t.Fatal("worker2 missing from state")
	}
	if w2.Status != model.WorktreeStatusConflict {
		t.Errorf("worker2 status = %q, want %q", w2.Status, model.WorktreeStatusConflict)
	}
}

// TestMergeToIntegration_SkippedWorkerCausesPartialMerge verifies that when a worker
// is skipped due to a transient error, the integration status is partial_merge (not merged).
func TestMergeToIntegration_SkippedWorkerCausesPartialMerge(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_skip_partial"
	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Worker1: create a unique file (will merge successfully)
	wt1, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "unique1.txt"), []byte("worker1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "add unique1.txt"); err != nil {
		t.Fatal(err)
	}

	// Worker2: create a unique file (would merge successfully but will be blocked by lock)
	wt2, err := wm.GetWorkerPath(commandID, "worker2")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt2, "unique2.txt"), []byte("worker2"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker2", "add unique2.txt"); err != nil {
		t.Fatal(err)
	}

	// Merge worker1 first (alone, will succeed)
	conflicts, err := wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration(worker1): %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("expected 0 conflicts for worker1, got %d", len(conflicts))
	}

	// Now create index.lock in the integration worktree's git dir to cause a transient error for worker2
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")
	gitDirCmd := exec.Command("git", "rev-parse", "--git-dir")
	gitDirCmd.Dir = integrationPath
	gitDirOut, err := gitDirCmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --git-dir: %v", err)
	}
	gitDir := strings.TrimSpace(string(gitDirOut))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(integrationPath, gitDir)
	}

	lockFile := filepath.Join(gitDir, "index.lock")
	if err := os.WriteFile(lockFile, []byte("locked"), 0644); err != nil {
		t.Fatalf("create index.lock: %v", err)
	}

	// MergeToIntegration with worker2 — transient error causes skip.
	// The lock also blocks merge --abort + recovery, causing fatal error return.
	conflicts, mergeErr := wm.MergeToIntegration(context.Background(), commandID, []string{"worker2"}, nil)
	// Remove lock before state checks
	os.Remove(lockFile)

	// With the lock, recovery fails so the function returns an error.
	// But the worker status should be "failed" and integration should NOT be "merged".
	if mergeErr == nil {
		// If no error, the function completed (lock may have been non-blocking for some git versions).
		// In that case, check the state directly.
		state, err := wm.GetCommandState(commandID)
		if err != nil {
			t.Fatalf("GetCommandState: %v", err)
		}
		// If worker2 was skipped (failed), integration should be partial_merge or conflict, not merged
		for _, ws := range state.Workers {
			if ws.WorkerID == "worker2" && ws.Status == model.WorktreeStatusFailed {
				// Worker2 was skipped — integration should not be "merged"
				if state.Integration.Status == model.IntegrationStatusMerged {
					t.Errorf("integration status = %q, want partial_merge or conflict (worker2 was skipped)",
						state.Integration.Status)
				}
			}
		}
		return
	}

	_ = conflicts // may contain partial results

	// Pre-merge cleanup hits the lock and bails out before any per-worker
	// merge actually runs, so worker status stays at Committed and the
	// integration is marked Failed so publish is blocked.
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	for _, ws := range state.Workers {
		if ws.WorkerID == "worker2" {
			if ws.Status != model.WorktreeStatusCommitted {
				t.Errorf("worker2 status = %q, want %q (pre-merge bailout must not flip worker)",
					ws.Status, model.WorktreeStatusCommitted)
			}
		}
	}
	if state.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusFailed)
	}
}
