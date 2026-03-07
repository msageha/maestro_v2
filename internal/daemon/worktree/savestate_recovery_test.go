package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

// writeStateFile writes a WorktreeCommandState as YAML to the state directory.
func writeStateFile(t *testing.T, maestroDir, commandID string, state *model.WorktreeCommandState) {
	t.Helper()
	stateDir := filepath.Join(maestroDir, "state", "worktrees")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("MkdirAll state dir: %v", err)
	}
	data, err := yamlv3.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, commandID+".yaml"), data, 0644); err != nil {
		t.Fatalf("write state file: %v", err)
	}
}

// TestReconcile_StuckMergingRecovery verifies that Reconcile detects integration
// stuck in "merging" status (simulating saveState failure after merge) and
// transitions it to "failed", restoring worker statuses.
func TestReconcile_StuckMergingRecovery(t *testing.T) {
	t.Run("IntegrationMerging_WorkersIntegrated", func(t *testing.T) {
		// Simulate: merge succeeded, workers marked "integrated" in memory,
		// but final saveState failed. Disk retains "merging" + "integrated" workers.
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		// Create real worktrees so paths/branches exist
		if err := wm.CreateForCommand("cmd_stuck_1", []string{"worker1", "worker2"}); err != nil {
			t.Fatalf("CreateForCommand failed: %v", err)
		}

		// Load the real state, then corrupt it to simulate stuck merging
		state, err := wm.GetCommandState("cmd_stuck_1")
		if err != nil {
			t.Fatalf("GetCommandState failed: %v", err)
		}

		// Set integration to "merging" and workers to "integrated"
		state.Integration.Status = model.IntegrationStatusMerging
		for i := range state.Workers {
			state.Workers[i].Status = model.WorktreeStatusIntegrated
		}
		writeStateFile(t, filepath.Join(projectRoot, ".maestro"), "cmd_stuck_1", state)

		// Verify disk state before Reconcile
		diskState, err := wm.GetCommandState("cmd_stuck_1")
		if err != nil {
			t.Fatalf("GetCommandState (pre-reconcile) failed: %v", err)
		}
		if diskState.Integration.Status != model.IntegrationStatusMerging {
			t.Fatalf("pre-reconcile integration status = %q, want %q",
				diskState.Integration.Status, model.IntegrationStatusMerging)
		}

		// Run Reconcile — should detect stuck "merging" and recover
		wm.Reconcile()

		// Verify integration transitioned to "failed"
		recovered, err := wm.GetCommandState("cmd_stuck_1")
		if err != nil {
			t.Fatalf("GetCommandState (post-reconcile) failed: %v", err)
		}
		if recovered.Integration.Status != model.IntegrationStatusFailed {
			t.Errorf("integration status = %q, want %q",
				recovered.Integration.Status, model.IntegrationStatusFailed)
		}

		// Verify workers restored to "active"
		for _, ws := range recovered.Workers {
			if ws.Status != model.WorktreeStatusActive {
				t.Errorf("worker %s status = %q, want %q",
					ws.WorkerID, ws.Status, model.WorktreeStatusActive)
			}
		}
	})

	t.Run("IntegrationMerging_WorkersMixed", func(t *testing.T) {
		// Simulate: partial merge — worker1 merged (integrated), worker2 still created.
		// saveState failed. Reconcile should only restore "integrated" workers.
		projectRoot := initTestGitRepo(t)
		wm := newTestWorktreeManager(t, projectRoot)

		if err := wm.CreateForCommand("cmd_stuck_2", []string{"worker1", "worker2"}); err != nil {
			t.Fatalf("CreateForCommand failed: %v", err)
		}

		state, err := wm.GetCommandState("cmd_stuck_2")
		if err != nil {
			t.Fatalf("GetCommandState failed: %v", err)
		}

		// Commit worker1 so it has a valid "committed" → "integrated" path
		wt1, err := wm.GetWorkerPath("cmd_stuck_2", "worker1")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt1, "f1.txt"), []byte("w1"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := wm.CommitWorkerChanges("cmd_stuck_2", "worker1", "add f1"); err != nil {
			t.Fatal(err)
		}

		// Reload state after commit
		state, err = wm.GetCommandState("cmd_stuck_2")
		if err != nil {
			t.Fatalf("GetCommandState (after commit) failed: %v", err)
		}

		// Simulate stuck: integration=merging, worker1=integrated, worker2=created
		state.Integration.Status = model.IntegrationStatusMerging
		for i := range state.Workers {
			if state.Workers[i].WorkerID == "worker1" {
				state.Workers[i].Status = model.WorktreeStatusIntegrated
			}
			// worker2 stays "created" — should NOT be changed by Reconcile
		}
		writeStateFile(t, filepath.Join(projectRoot, ".maestro"), "cmd_stuck_2", state)

		wm.Reconcile()

		recovered, err := wm.GetCommandState("cmd_stuck_2")
		if err != nil {
			t.Fatalf("GetCommandState (post-reconcile) failed: %v", err)
		}

		if recovered.Integration.Status != model.IntegrationStatusFailed {
			t.Errorf("integration status = %q, want %q",
				recovered.Integration.Status, model.IntegrationStatusFailed)
		}

		for _, ws := range recovered.Workers {
			switch ws.WorkerID {
			case "worker1":
				// Was "integrated" → should be restored to "active"
				if ws.Status != model.WorktreeStatusActive {
					t.Errorf("worker1 status = %q, want %q", ws.Status, model.WorktreeStatusActive)
				}
			case "worker2":
				// Was "created" → should remain "created"
				if ws.Status != model.WorktreeStatusCreated {
					t.Errorf("worker2 status = %q, want %q", ws.Status, model.WorktreeStatusCreated)
				}
			}
		}
	})
}

// TestReconcile_StuckMergingRetryMerge verifies that after Reconcile recovers
// from stuck "merging", a subsequent MergeToIntegration succeeds without
// creating duplicate merge commits.
func TestReconcile_StuckMergingRetryMerge(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1"}
	if err := wm.CreateForCommand("cmd_retry", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Worker1: create a file and commit
	wt1, err := wm.GetWorkerPath("cmd_retry", "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "retry.txt"), []byte("retry data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_retry", "worker1", "add retry.txt"); err != nil {
		t.Fatal(err)
	}

	// Do a real merge first
	conflicts, err := wm.MergeToIntegration("cmd_retry", workers)
	if err != nil {
		t.Fatalf("first MergeToIntegration failed: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}

	// Record integration HEAD SHA after successful merge
	integrationBranch := "maestro/cmd_retry/integration"
	cmd := exec.Command("git", "rev-parse", integrationBranch)
	cmd.Dir = projectRoot
	headBefore, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse failed: %v", err)
	}
	headBeforeSHA := strings.TrimSpace(string(headBefore))

	// Simulate stuck merging: overwrite state to "merging"
	state, err := wm.GetCommandState("cmd_retry")
	if err != nil {
		t.Fatal(err)
	}
	state.Integration.Status = model.IntegrationStatusMerging
	writeStateFile(t, filepath.Join(projectRoot, ".maestro"), "cmd_retry", state)

	// Reconcile should transition to "failed"
	wm.Reconcile()

	recovered, err := wm.GetCommandState("cmd_retry")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Integration.Status != model.IntegrationStatusFailed {
		t.Fatalf("expected integration 'failed' after reconcile, got %q", recovered.Integration.Status)
	}

	// Retry merge — should succeed (failed → merging → merged)
	conflicts, err = wm.MergeToIntegration("cmd_retry", workers)
	if err != nil {
		t.Fatalf("retry MergeToIntegration failed: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts on retry: %v", conflicts)
	}

	// Verify no duplicate merge commits: HEAD should not change because
	// worker1 is already merged into integration
	cmd = exec.Command("git", "rev-parse", integrationBranch)
	cmd.Dir = projectRoot
	headAfter, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse after retry failed: %v", err)
	}
	headAfterSHA := strings.TrimSpace(string(headAfter))

	if headBeforeSHA != headAfterSHA {
		t.Errorf("integration HEAD changed after retry merge (duplicate commit created)\nbefore: %s\nafter:  %s",
			headBeforeSHA, headAfterSHA)
	}
}

// TestMergeToIntegration_PreMergePersistFailure verifies that when the
// pre-merge saveState("merging") fails (e.g., unwritable state dir),
// MergeToIntegration returns an error without performing any git mutations,
// and disk retains the original pre-merge status.
func TestMergeToIntegration_PreMergePersistFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping: running as root")
	}

	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1"}
	if err := wm.CreateForCommand("cmd_save_fail", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Worker1: create a file and commit
	wt1, err := wm.GetWorkerPath("cmd_save_fail", "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "data.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_save_fail", "worker1", "add data.txt"); err != nil {
		t.Fatal(err)
	}

	// Make state dir read-only AFTER the "merging" status is persisted.
	// We can't precisely inject between the pre-merge and post-merge saveState
	// calls, so instead we:
	// 1. Make state dir unwritable
	// 2. Call MergeToIntegration — the pre-merge saveState("merging") will also fail
	// 3. Verify the error is returned
	stateDir := filepath.Join(projectRoot, ".maestro", "state", "worktrees")
	if err := os.Chmod(stateDir, 0555); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	t.Cleanup(func() {
		os.Chmod(stateDir, 0755)
	})

	_, err = wm.MergeToIntegration("cmd_save_fail", workers)
	if err == nil {
		t.Fatal("expected MergeToIntegration to fail when state dir is unwritable")
	}
	if !strings.Contains(err.Error(), "persist merging status") {
		t.Errorf("expected 'persist merging status' error, got: %v", err)
	}

	// Restore write permission and verify disk state
	if err := os.Chmod(stateDir, 0755); err != nil {
		t.Fatalf("restore chmod failed: %v", err)
	}

	// State on disk should still show the pre-merge status (created or committed)
	// because the "merging" saveState itself failed
	diskState, err := wm.GetCommandState("cmd_save_fail")
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	// Integration should NOT be "merging" since saveState failed
	if diskState.Integration.Status == model.IntegrationStatusMerging {
		t.Error("disk should NOT show 'merging' since the persist call failed")
	}
}

// TestMergeToIntegration_FinalSaveFailure_ReconcileRecovers verifies the full
// recovery path: merge succeeds → final saveState fails → disk retains "merging"
// → Reconcile detects and transitions to "failed" → retry merge succeeds.
func TestMergeToIntegration_FinalSaveFailure_ReconcileRecovers(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	workers := []string{"worker1"}
	if err := wm.CreateForCommand("cmd_full_recovery", workers); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Worker1: create file and commit
	wt1, err := wm.GetWorkerPath("cmd_full_recovery", "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "recover.txt"), []byte("recover"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_full_recovery", "worker1", "add recover.txt"); err != nil {
		t.Fatal(err)
	}

	// Simulate: the merge succeeded and final saveState failed, leaving
	// disk with "merging" + worker as "committed" (pre-merge state).
	// Do a real merge first to get the git state right.
	conflicts, err := wm.MergeToIntegration("cmd_full_recovery", workers)
	if err != nil {
		t.Fatalf("MergeToIntegration failed: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", conflicts)
	}

	// Now overwrite state file to simulate final saveState failure:
	// disk shows "merging", worker shows "committed" (as if the final save never happened)
	state, err := wm.GetCommandState("cmd_full_recovery")
	if err != nil {
		t.Fatal(err)
	}
	state.Integration.Status = model.IntegrationStatusMerging
	for i := range state.Workers {
		state.Workers[i].Status = model.WorktreeStatusCommitted
	}
	writeStateFile(t, filepath.Join(projectRoot, ".maestro"), "cmd_full_recovery", state)

	// Reconcile should detect stuck "merging"
	wm.Reconcile()

	recovered, err := wm.GetCommandState("cmd_full_recovery")
	if err != nil {
		t.Fatal(err)
	}

	// Integration should be "failed"
	if recovered.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("integration status = %q, want %q",
			recovered.Integration.Status, model.IntegrationStatusFailed)
	}

	// Workers should remain "committed" (Reconcile only restores "integrated" → "active")
	for _, ws := range recovered.Workers {
		if ws.Status != model.WorktreeStatusCommitted {
			t.Errorf("worker %s status = %q, want %q",
				ws.WorkerID, ws.Status, model.WorktreeStatusCommitted)
		}
	}

	// Retry merge should succeed
	conflicts, err = wm.MergeToIntegration("cmd_full_recovery", workers)
	if err != nil {
		t.Fatalf("retry MergeToIntegration failed: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts on retry: %v", conflicts)
	}

	// Final state should be "merged"
	finalState, err := wm.GetCommandState("cmd_full_recovery")
	if err != nil {
		t.Fatal(err)
	}
	if finalState.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("final integration status = %q, want %q",
			finalState.Integration.Status, model.IntegrationStatusMerged)
	}
}

// TestReconcile_StuckMergingNoIntegratedWorkers verifies Reconcile handles the
// crash window right after the pre-merge "merging" marker is persisted but before
// any workers are marked "integrated". Workers should remain in their original status.
func TestReconcile_StuckMergingNoIntegratedWorkers(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_stuck_zero", []string{"worker1", "worker2"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Commit worker1 so it's in "committed" status
	wt1, err := wm.GetWorkerPath("cmd_stuck_zero", "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "f.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_stuck_zero", "worker1", "add f"); err != nil {
		t.Fatal(err)
	}

	state, err := wm.GetCommandState("cmd_stuck_zero")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate crash right after pre-merge persist: integration=merging,
	// but no workers have been marked "integrated" yet
	state.Integration.Status = model.IntegrationStatusMerging
	// Workers remain: worker1=committed, worker2=created
	writeStateFile(t, filepath.Join(projectRoot, ".maestro"), "cmd_stuck_zero", state)

	wm.Reconcile()

	recovered, err := wm.GetCommandState("cmd_stuck_zero")
	if err != nil {
		t.Fatal(err)
	}

	if recovered.Integration.Status != model.IntegrationStatusFailed {
		t.Errorf("integration status = %q, want %q",
			recovered.Integration.Status, model.IntegrationStatusFailed)
	}

	// Workers should retain their original statuses (not touched by Reconcile)
	for _, ws := range recovered.Workers {
		switch ws.WorkerID {
		case "worker1":
			if ws.Status != model.WorktreeStatusCommitted {
				t.Errorf("worker1 status = %q, want %q", ws.Status, model.WorktreeStatusCommitted)
			}
		case "worker2":
			if ws.Status != model.WorktreeStatusCreated {
				t.Errorf("worker2 status = %q, want %q", ws.Status, model.WorktreeStatusCreated)
			}
		}
	}

	// Verify merge can be retried after recovery
	conflicts, err := wm.MergeToIntegration("cmd_stuck_zero", []string{"worker1", "worker2"})
	if err != nil {
		t.Fatalf("retry MergeToIntegration failed: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts on retry: %v", conflicts)
	}

	finalState, err := wm.GetCommandState("cmd_stuck_zero")
	if err != nil {
		t.Fatal(err)
	}
	if finalState.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("final integration status = %q, want %q",
			finalState.Integration.Status, model.IntegrationStatusMerged)
	}
}

// TestReconcile_NormalMergedState_NoChange verifies that Reconcile does not
// alter a normally "merged" integration state (no false positives).
func TestReconcile_NormalMergedState_NoChange(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	if err := wm.CreateForCommand("cmd_normal_merged", []string{"worker1"}); err != nil {
		t.Fatalf("CreateForCommand failed: %v", err)
	}

	// Create a file, commit, and merge
	wt1, err := wm.GetWorkerPath("cmd_normal_merged", "worker1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt1, "normal.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges("cmd_normal_merged", "worker1", "add normal.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.MergeToIntegration("cmd_normal_merged", []string{"worker1"}); err != nil {
		t.Fatal(err)
	}

	stateBefore, err := wm.GetCommandState("cmd_normal_merged")
	if err != nil {
		t.Fatal(err)
	}
	if stateBefore.Integration.Status != model.IntegrationStatusMerged {
		t.Fatalf("pre-reconcile status = %q, want %q",
			stateBefore.Integration.Status, model.IntegrationStatusMerged)
	}

	wm.Reconcile()

	stateAfter, err := wm.GetCommandState("cmd_normal_merged")
	if err != nil {
		t.Fatal(err)
	}

	// Should remain "merged" — Reconcile should not touch it
	if stateAfter.Integration.Status != model.IntegrationStatusMerged {
		t.Errorf("post-reconcile integration status = %q, want %q",
			stateAfter.Integration.Status, model.IntegrationStatusMerged)
	}
	for _, ws := range stateAfter.Workers {
		if ws.Status != model.WorktreeStatusIntegrated {
			t.Errorf("worker %s status changed to %q after reconcile",
				ws.WorkerID, ws.Status)
		}
	}
}
