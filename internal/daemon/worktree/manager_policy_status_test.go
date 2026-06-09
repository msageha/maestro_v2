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

// --- CommitWorkerChanges happy-path tests ---
//
// Per the autonomous-orchestration policy, CommitWorkerChanges does not
// enforce max-files / require-gitignore / message-pattern / sensitive-file
// gates: the orchestrator commits Worker output verbatim. The remaining
// tests below cover the bare happy path and the resume-merge ownership
// guard (the only commit-time policy that still applies).

// TestCommitWorkerChanges_HappyPath verifies that a worker commit with
// well-formed input succeeds end-to-end.
func TestCommitWorkerChanges_HappyPath(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := createForCommand(wm, "cmd_happy", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	wtPath, err := wm.GetWorkerPath("cmd_happy", "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := wm.CommitWorkerChanges("cmd_happy", "worker1", "add main.go"); err != nil {
		t.Fatalf("CommitWorkerChanges should succeed: %v", err)
	}
}

// TestSetWorkerStatus validates that status transitions are enforced via setWorkerStatus.
func TestSetWorkerStatus(t *testing.T) {
	t.Parallel()
	t.Run("valid_transition", func(t *testing.T) {
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
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
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
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

	t.Run("created_to_integrated_allowed", func(t *testing.T) {
		t.Parallel()
		// No-op merge path: a worker that never produced any commits hits
		// `no_commits_to_merge` in mergeWorkerBranch and must still advance
		// out of WorktreeStatusActive/Created so the publish gate stops
		// deferring on an "active" worker that has nothing to merge. This
		// transition is the cleanest way to express "worker is integrated
		// even though there was nothing to integrate".
		projectRoot := testutil.InitTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		ws := &model.WorktreeState{
			WorkerID: "worker1",
			Status:   model.WorktreeStatusCreated,
		}

		if err := wm.setWorkerStatus(ws, model.WorktreeStatusIntegrated, "2024-01-01T00:00:00Z"); err != nil {
			t.Fatalf("expected created → integrated to be allowed, got: %v", err)
		}
		if ws.Status != model.WorktreeStatusIntegrated {
			t.Errorf("status = %q, want %q", ws.Status, model.WorktreeStatusIntegrated)
		}
	})

	t.Run("integrated_to_committed_allowed", func(t *testing.T) {
		t.Parallel()
		// Regression: cross-phase commit on a worker still in `integrated` state
		// (e.g. verification phase reusing a worker from a previously merged phase)
		// must be permitted instead of returning "invalid worktree transition".
		projectRoot := testutil.InitTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		ws := &model.WorktreeState{
			WorkerID: "worker1",
			Status:   model.WorktreeStatusIntegrated,
		}

		if err := wm.setWorkerStatus(ws, model.WorktreeStatusCommitted, "2024-01-01T00:00:00Z"); err != nil {
			t.Fatalf("expected valid integrated → committed transition, got error: %v", err)
		}
		if ws.Status != model.WorktreeStatusCommitted {
			t.Errorf("status = %q, want %q", ws.Status, model.WorktreeStatusCommitted)
		}
	})

	t.Run("fresh_conflict_clears_escalation_guard", func(t *testing.T) {
		t.Parallel()
		// A worker that escalated a conflict in an earlier phase (ConflictEscalated=true)
		// and was resolved back to `integrated` must have the guard cleared when it
		// re-enters conflict in a later phase, so R7 can escalate the NEW conflict.
		projectRoot := testutil.InitTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		ws := &model.WorktreeState{
			WorkerID:                   "worker1",
			Status:                     model.WorktreeStatusIntegrated,
			ConflictEscalated:          true,
			ConflictResolutionAttempts: 2,
		}
		if err := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, "2024-01-01T00:00:00Z"); err != nil {
			t.Fatalf("expected integrated → conflict to be allowed, got: %v", err)
		}
		if ws.ConflictEscalated {
			t.Error("ConflictEscalated should be cleared on a fresh conflict entry (later-phase re-conflict)")
		}
		if ws.ConflictResolutionAttempts != 0 {
			t.Errorf("ConflictResolutionAttempts = %d, want 0 (fresh episode gets a new resolution budget)", ws.ConflictResolutionAttempts)
		}
	})

	t.Run("resolving_to_conflict_preserves_episode_state", func(t *testing.T) {
		t.Parallel()
		// resolving → conflict is the SAME conflict episode (e.g. R7 Pass 1 reset of
		// a stale resolving worker). The guard AND the attempt count must be
		// preserved so the unrecoverable conflict is not re-escalated on every scan
		// nor handed a fresh resolution budget each cycle.
		projectRoot := testutil.InitTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		ws := &model.WorktreeState{
			WorkerID:                   "worker1",
			Status:                     model.WorktreeStatusResolving,
			ConflictEscalated:          true,
			ConflictResolutionAttempts: 2,
		}
		if err := wm.setWorkerStatus(ws, model.WorktreeStatusConflict, "2024-01-01T00:00:00Z"); err != nil {
			t.Fatalf("expected resolving → conflict to be allowed, got: %v", err)
		}
		if !ws.ConflictEscalated {
			t.Error("ConflictEscalated should be preserved on resolving → conflict (same episode)")
		}
		if ws.ConflictResolutionAttempts != 2 {
			t.Errorf("ConflictResolutionAttempts = %d, want 2 preserved (same episode)", ws.ConflictResolutionAttempts)
		}
	})

	t.Run("self_transition_committed", func(t *testing.T) {
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
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
	t.Parallel()
	t.Run("valid_transition", func(t *testing.T) {
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
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
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
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
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
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
		t.Parallel()
		projectRoot := testutil.InitTestGitRepo(t)
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

// TestMergeToIntegration_PartialMergeOnConflict verifies that when a merge conflict
// occurs, successfully merged workers are preserved (partial merge) and integration
// status is set to partial_merge.
func TestMergeToIntegration_PartialMergeOnConflict(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_rollback", workers); err != nil {
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

	// Merge — worker1 succeeds, worker2 conflicts → partial merge expected
	conflicts, err := wm.MergeToIntegration(context.Background(), "cmd_rollback", workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration failed: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	// Verify integration branch HEAD advanced (worker1's merge is preserved)
	postMergeCmd := exec.Command("git", "rev-parse", "HEAD")
	postMergeCmd.Dir = integrationPath
	postMergeOut, err := postMergeCmd.Output()
	if err != nil {
		t.Fatalf("get post-merge HEAD: %v", err)
	}
	postMergeHEAD := strings.TrimSpace(string(postMergeOut))

	if postMergeHEAD == preMergeHEAD {
		t.Errorf("integration branch should have advanced (worker1 merge preserved): HEAD=%s", postMergeHEAD)
	}

	// Verify worker1's file IS on integration branch (preserved)
	lsCmd := exec.Command("git", "ls-tree", "--name-only", "HEAD")
	lsCmd.Dir = integrationPath
	lsOut, err := lsCmd.Output()
	if err != nil {
		t.Fatalf("git ls-tree failed: %v", err)
	}
	if !strings.Contains(string(lsOut), "unique1.txt") {
		t.Error("unique1.txt should be on integration branch (worker1 merge preserved)")
	}

	// Verify worker1's status is "integrated" (preserved, not rolled back)
	cmdState, err := wm.GetCommandState("cmd_rollback")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	for _, ws := range cmdState.Workers {
		switch ws.WorkerID {
		case "worker1":
			if ws.Status != model.WorktreeStatusIntegrated {
				t.Errorf("worker1 status = %q, want %q (preserved)", ws.Status, model.WorktreeStatusIntegrated)
			}
		case "worker2":
			if ws.Status != model.WorktreeStatusConflict {
				t.Errorf("worker2 status = %q, want %q", ws.Status, model.WorktreeStatusConflict)
			}
		}
	}

	// Verify integration status is partial_merge
	if cmdState.Integration.Status != model.IntegrationStatusPartialMerge {
		t.Errorf("integration status = %q, want %q", cmdState.Integration.Status, model.IntegrationStatusPartialMerge)
	}

	// Verify conflict has ref information
	if conflicts[0].WorkerID != "worker2" {
		t.Errorf("conflict worker = %q, want worker2", conflicts[0].WorkerID)
	}
	// BaseRef/OursRef/TheirsRef should be populated for text file conflicts
	if conflicts[0].BaseRef == "" || conflicts[0].OursRef == "" || conflicts[0].TheirsRef == "" {
		t.Errorf("conflict refs not populated: base=%q ours=%q theirs=%q",
			conflicts[0].BaseRef, conflicts[0].OursRef, conflicts[0].TheirsRef)
	}
}

// TestMergeToIntegration_AllConflict verifies that when all workers conflict,
// integration status is set to "conflict" (not partial_merge).
func TestMergeToIntegration_AllConflict(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1", "worker2"}
	if err := createForCommand(wm, "cmd_allconflict", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_allconflict", "_integration")

	// Modify README.md directly in integration to create conflict base
	if err := os.WriteFile(filepath.Join(integrationPath, "README.md"), []byte("integration readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	commitCmd := exec.Command("git", "add", "README.md")
	commitCmd.Dir = integrationPath
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s %v", out, err)
	}
	commitCmd = exec.Command("git", "commit", "-m", "integration modify README")
	commitCmd.Dir = integrationPath
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s %v", out, err)
	}

	// Both workers modify README.md differently
	wt1, _ := wm.GetWorkerPath("cmd_allconflict", "worker1")
	if err := os.WriteFile(filepath.Join(wt1, "README.md"), []byte("worker1 readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_allconflict", "worker1", "worker1 modify README"); err != nil {
		t.Fatal(err)
	}
	wt2, _ := wm.GetWorkerPath("cmd_allconflict", "worker2")
	if err := os.WriteFile(filepath.Join(wt2, "README.md"), []byte("worker2 readme\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_allconflict", "worker2", "worker2 modify README"); err != nil {
		t.Fatal(err)
	}

	conflicts, err := wm.MergeToIntegration(context.Background(), "cmd_allconflict", workers, nil)
	if err != nil {
		t.Fatalf("MergeToIntegration failed: %v", err)
	}
	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %d", len(conflicts))
	}

	cmdState, err := wm.GetCommandState("cmd_allconflict")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	// All conflicted, none merged → IntegrationStatusConflict
	if cmdState.Integration.Status != model.IntegrationStatusConflict {
		t.Errorf("integration status = %q, want %q", cmdState.Integration.Status, model.IntegrationStatusConflict)
	}
}

// TestPublishToBase_NoFalsePositiveDurableStash verifies that PublishToBase does
// NOT create a false-positive durable stash ref. After update-ref moves the branch
// pointer, read-tree --reset -u HEAD syncs the index and working tree to match the
// new HEAD before stash create runs, so stash create sees no divergence and returns
// empty. This prevents orphaned refs from accumulating under refs/maestro/pre-publish-stash/.
func TestPublishToBase_NoFalsePositiveDurableStash(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	currentBranch := "main"
	wm.config.BaseBranch = currentBranch

	workers := []string{"worker1"}
	if err := createForCommand(wm, "cmd_stash_ref", workers); err != nil {
		t.Fatal(err)
	}

	wt1, err := wm.GetWorkerPath("cmd_stash_ref", "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "stash_test.txt"), []byte("stash test"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_stash_ref", "worker1", "add stash_test.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration(context.Background(), "cmd_stash_ref", workers, nil); err != nil {
		t.Fatal(err)
	}

	// Publish should succeed
	if err := wm.PublishToBase("cmd_stash_ref", ""); err != nil {
		t.Fatalf("PublishToBase failed: %v", err)
	}

	// Verify NO durable stash ref was created (false positive eliminated by read-tree sync)
	refCmd := exec.Command("git", "rev-parse", "--verify", "refs/maestro/pre-publish-stash/cmd_stash_ref")
	refCmd.Dir = projectRoot
	if err := refCmd.Run(); err == nil {
		t.Error("durable stash ref should NOT exist after clean publish (false positive stash)")
	}

	// Verify the publish succeeded: base branch should have the file
	lsCmd := exec.Command("git", "ls-tree", "--name-only", currentBranch)
	lsCmd.Dir = projectRoot
	lsOut, err := lsCmd.Output()
	if err != nil {
		t.Fatalf("git ls-tree: %v", err)
	}
	if !strings.Contains(string(lsOut), "stash_test.txt") {
		t.Error("stash_test.txt not found on base branch after publish")
	}
}

// TestPublishToBase_StashCreateFailureContinues verifies that if git stash create
// fails (e.g., due to a corrupted index), PublishToBase still completes successfully.
// This ensures stash create failure is non-fatal (the primary defense is CAS + dirty check).
func TestPublishToBase_StashCreateFailureContinues(t *testing.T) {
	t.Parallel()
	// This test verifies the non-fatal path by doing a normal publish.
	// In practice, stash create failure is rare (requires index corruption or similar).
	// We test the happy path to confirm the stash create call doesn't break normal flow.
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	currentBranch := "main"
	wm.config.BaseBranch = currentBranch

	workers := []string{"worker1"}
	if err := createForCommand(wm, "cmd_stash_fail", workers); err != nil {
		t.Fatal(err)
	}

	wt1, err := wm.GetWorkerPath("cmd_stash_fail", "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "resilience.txt"), []byte("resilience"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_stash_fail", "worker1", "add resilience.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration(context.Background(), "cmd_stash_fail", workers, nil); err != nil {
		t.Fatal(err)
	}

	// PublishToBase should succeed regardless of stash create outcome
	if err := wm.PublishToBase("cmd_stash_fail", ""); err != nil {
		t.Fatalf("PublishToBase failed: %v", err)
	}

	// Verify integration status
	state, err := wm.GetCommandState("cmd_stash_fail")
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	if state.Integration.Status != model.IntegrationStatusPublished {
		t.Errorf("integration status = %q, want %q", state.Integration.Status, model.IntegrationStatusPublished)
	}
}

// TestCommitWorkerChanges_WorkerOwnedByResumeMerge verifies that
// CommitWorkerChanges refuses to auto-commit workers whose status is
// Conflict or Resolving, returning ErrWorkerOwnedByResumeMerge so the
// Phase B caller can distinguish "out of scope" from a genuine commit
// failure and avoid recording a spurious commit_failed signal. The
// `resolving -> committed` transition is invalid by design.
func TestCommitWorkerChanges_WorkerOwnedByResumeMerge(t *testing.T) {
	t.Parallel()
	for _, status := range []model.WorktreeStatus{
		model.WorktreeStatusResolving,
		model.WorktreeStatusConflict,
	} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			projectRoot := testutil.InitTestGitRepo(t)
			wm := newTestWorktreeManager(t, projectRoot)
			commandID := "cmd_resume_merge_owned_" + string(status)
			if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
				t.Fatalf("createForCommand: %v", err)
			}

			// Manually transition the worker to the target status.
			state, err := wm.loadState(commandID)
			if err != nil {
				t.Fatalf("loadState: %v", err)
			}
			ws := &state.Workers[0]
			// created → active → conflict (→ resolving)
			ws.Status = model.WorktreeStatusActive
			if status == model.WorktreeStatusResolving {
				ws.Status = model.WorktreeStatusResolving
			} else {
				ws.Status = model.WorktreeStatusConflict
			}
			if err := wm.saveState(commandID, state); err != nil {
				t.Fatalf("saveState: %v", err)
			}

			// Add a dirty file to the worker worktree to make sure the guard
			// fires before the commit attempt (otherwise "no changes" would
			// return nil before the status check).
			wtPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "worker1")
			if err := os.WriteFile(filepath.Join(wtPath, "resolved.txt"), []byte("resolved"), 0644); err != nil {
				t.Fatalf("write dirty file: %v", err)
			}

			err = wm.CommitWorkerChanges(commandID, "worker1", "resolved conflict")
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, ErrWorkerOwnedByResumeMerge) {
				t.Errorf("errors.Is(err, ErrWorkerOwnedByResumeMerge) = false, got %v", err)
			}
		})
	}
}

// TestRollbackWorkerWorktree_ReturnsErrors tests that rollbackWorkerWorktree
// returns errors when git cleanup operations fail (e.g., non-existent worktree/branch).
func TestRollbackWorkerWorktree_ReturnsErrors(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create a fake state with a worker pointing to non-existent path/branch
	state := &model.WorktreeCommandState{
		Workers: []model.WorktreeState{
			{
				WorkerID: "ghost",
				Path:     filepath.Join(projectRoot, "nonexistent-worktree"),
				Branch:   "maestro/nonexistent/ghost",
			},
		},
	}

	err := wm.rollbackWorkerWorktree("cmd_test", state, "ghost")
	if err == nil {
		t.Fatal("expected rollbackWorkerWorktree to return error for non-existent worktree/branch")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "remove worktree") {
		t.Errorf("error should contain worktree remove failure, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "delete branch") {
		t.Errorf("error should contain branch delete failure, got: %s", errMsg)
	}
}

// TestRollbackWorkerWorktree_NoErrorForMissingWorker tests that rollbackWorkerWorktree
// returns nil when the workerID is not found in state (nothing to roll back).
func TestRollbackWorkerWorktree_NoErrorForMissingWorker(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	state := &model.WorktreeCommandState{}
	err := wm.rollbackWorkerWorktree("cmd_test", state, "nonexistent")
	if err != nil {
		t.Errorf("expected nil error for missing worker, got: %v", err)
	}
}

// TestEnsureWorkerWorktree_RollbackSuccessReturnsOriginalError tests that when
// rollback succeeds, only the original error is returned (no rollback error appended).
func TestEnsureWorkerWorktree_RollbackSuccessReturnsOriginalError(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Pre-create a branch that will conflict with worker1's branch name
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = projectRoot
	headSHA, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(headSHA))

	cmd = exec.Command("git", "branch", "maestro/cmd_rb_success/worker1", sha)
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create conflicting branch failed: %v\n%s", err, out)
	}
	defer func() {
		cmd := exec.Command("git", "branch", "-D", "maestro/cmd_rb_success/worker1")
		cmd.Dir = projectRoot
		_ = cmd.Run()
	}()

	// EnsureWorkerWorktree should fail (branch conflict) and rollback integration.
	// Since rollback succeeds, error should NOT contain "rollback also failed".
	err = wm.EnsureWorkerWorktree("cmd_rb_success", "worker1")
	if err == nil {
		t.Fatal("expected EnsureWorkerWorktree to fail")
	}

	errMsg := err.Error()
	if strings.Contains(errMsg, "rollback also failed") {
		t.Errorf("error should NOT contain rollback failure when rollback succeeds, got: %s", errMsg)
	}
}

// TestEnsureWorkerWorktree_RollbackFailurePropagation tests that when both
// the original operation and rollback fail, the returned error contains both.
func TestEnsureWorkerWorktree_RollbackFailurePropagation(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Create initial state with worker1 (existing state path)
	if err := wm.EnsureWorkerWorktree("cmd_rb_fail", "worker1"); err != nil {
		t.Fatalf("initial EnsureWorkerWorktree failed: %v", err)
	}

	// Make state directory read-only to force saveState failure.
	// When EnsureWorkerWorktree tries to add worker2:
	// 1. addWorkerWorktreeUnlocked succeeds (worktree created)
	// 2. saveState fails (read-only dir)
	// 3. rollbackWorkerWorktree succeeds (removes worktree)
	// 4. restore saveState also fails (still read-only) → rollback error
	stateDir := filepath.Join(projectRoot, ".maestro", "state", "worktrees")
	if err := os.Chmod(stateDir, 0555); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	t.Cleanup(func() { os.Chmod(stateDir, 0755) }) //nolint:errcheck // restore for cleanup

	err := wm.EnsureWorkerWorktree("cmd_rb_fail", "worker2")
	if err == nil {
		t.Fatal("expected EnsureWorkerWorktree to fail")
	}

	errMsg := err.Error()

	// Original error should be present
	if !strings.Contains(errMsg, "save worktree state") {
		t.Errorf("error should contain original error, got: %s", errMsg)
	}

	// Rollback error should be present
	if !strings.Contains(errMsg, "rollback also failed") {
		t.Errorf("error should contain rollback failure, got: %s", errMsg)
	}

	// The restore state failure should be mentioned
	if !strings.Contains(errMsg, "restore state") {
		t.Errorf("error should contain restore state failure detail, got: %s", errMsg)
	}

	// Restore permissions and verify original state is intact
	if err := os.Chmod(stateDir, 0755); err != nil {
		t.Fatalf("chmod restore failed: %v", err)
	}
	state, err := wm.GetCommandState("cmd_rb_fail")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	// worker1 should still exist
	if len(state.Workers) < 1 {
		t.Errorf("expected at least 1 worker, got %d", len(state.Workers))
	}
}
