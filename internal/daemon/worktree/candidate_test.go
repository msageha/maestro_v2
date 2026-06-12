package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/testutil"
)

func TestEnsureCandidateWorktree_CreatesFromScratch(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_ab_scratch"
	taskID := "task_ab_001"

	// No prior worktree state: integration must be created lazily.
	path, branch, err := wm.EnsureCandidateWorktree(commandID, taskID)
	if err != nil {
		t.Fatalf("EnsureCandidateWorktree: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("candidate worktree dir missing: %v", err)
	}
	if want := "maestro/" + commandID + "/candidate/" + taskID; branch != want {
		t.Errorf("branch = %q, want %q", branch, want)
	}
	if _, err := wm.gitOutput("rev-parse", "--verify", branch); err != nil {
		t.Errorf("candidate branch missing: %v", err)
	}
	if _, err := wm.gitOutput("rev-parse", "--verify", "maestro/"+commandID+"/integration"); err != nil {
		t.Errorf("integration branch should be created lazily: %v", err)
	}

	// Idempotent: second call returns the same path/branch.
	path2, branch2, err := wm.EnsureCandidateWorktree(commandID, taskID)
	if err != nil {
		t.Fatalf("EnsureCandidateWorktree (2nd): %v", err)
	}
	if path2 != path || branch2 != branch {
		t.Errorf("idempotency violated: (%q,%q) != (%q,%q)", path2, branch2, path, branch)
	}

	// Durable record present.
	state, err := wm.loadState(commandID)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if c := findCandidate(state, taskID); c == nil || c.Path != path {
		t.Errorf("candidate not recorded in state: %+v", state.Candidates)
	}
}

func TestEnsureCandidateWorktree_WithExistingState(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_ab_existing"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	path, _, err := wm.EnsureCandidateWorktree(commandID, "task_ab_002")
	if err != nil {
		t.Fatalf("EnsureCandidateWorktree: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("candidate worktree dir missing: %v", err)
	}
	// Worker entries must be untouched.
	state, _ := wm.loadState(commandID)
	if len(state.Workers) != 1 || state.Workers[0].WorkerID != "worker1" {
		t.Errorf("worker entries disturbed: %+v", state.Workers)
	}
}

func TestCommitCandidateChanges_CommitsAndIsCleanNoop(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_ab_commit"
	taskID := "task_ab_003"
	path, branch, err := wm.EnsureCandidateWorktree(commandID, taskID)
	if err != nil {
		t.Fatalf("EnsureCandidateWorktree: %v", err)
	}

	// Clean worktree → successful no-op.
	if err := wm.CommitCandidateChanges(commandID, taskID); err != nil {
		t.Fatalf("CommitCandidateChanges (clean): %v", err)
	}

	before, _ := wm.gitOutput("rev-parse", branch)
	if err := os.WriteFile(filepath.Join(path, "ab_change.txt"), []byte("candidate work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitCandidateChanges(commandID, taskID); err != nil {
		t.Fatalf("CommitCandidateChanges: %v", err)
	}
	after, _ := wm.gitOutput("rev-parse", branch)
	if strings.TrimSpace(before) == strings.TrimSpace(after) {
		t.Error("candidate branch HEAD did not advance after commit")
	}
}

func TestComputeCandidateDiff(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_ab_diff"
	taskID := "task_ab_004"
	path, _, err := wm.EnsureCandidateWorktree(commandID, taskID)
	if err != nil {
		t.Fatalf("EnsureCandidateWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "diffed.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitCandidateChanges(commandID, taskID); err != nil {
		t.Fatalf("CommitCandidateChanges: %v", err)
	}

	files, diff, err := wm.ComputeCandidateDiff(commandID, taskID)
	if err != nil {
		t.Fatalf("ComputeCandidateDiff: %v", err)
	}
	if len(files) != 1 || files[0] != "diffed.txt" {
		t.Errorf("changedFiles = %v, want [diffed.txt]", files)
	}
	if !strings.Contains(diff, "diffed.txt") {
		t.Errorf("diff does not mention changed file:\n%s", diff)
	}
}

func TestRemoveCandidateWorktree(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_ab_remove"
	taskID := "task_ab_005"
	path, branch, err := wm.EnsureCandidateWorktree(commandID, taskID)
	if err != nil {
		t.Fatalf("EnsureCandidateWorktree: %v", err)
	}

	if err := wm.RemoveCandidateWorktree(commandID, taskID); err != nil {
		t.Fatalf("RemoveCandidateWorktree: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("candidate worktree dir should be removed, stat err=%v", err)
	}
	if _, err := wm.gitOutput("rev-parse", "--verify", branch); err == nil {
		t.Error("candidate branch should be deleted")
	}
	state, _ := wm.loadState(commandID)
	if findCandidate(state, taskID) != nil {
		t.Error("candidate entry should be dropped from state")
	}

	// Idempotent.
	if err := wm.RemoveCandidateWorktree(commandID, taskID); err != nil {
		t.Fatalf("RemoveCandidateWorktree (2nd): %v", err)
	}
}

func TestCleanupCommand_RemovesCandidates(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_ab_cleanup"
	taskID := "task_ab_006"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}
	_, branch, err := wm.EnsureCandidateWorktree(commandID, taskID)
	if err != nil {
		t.Fatalf("EnsureCandidateWorktree: %v", err)
	}

	if err := wm.CleanupCommand(commandID); err != nil {
		t.Fatalf("CleanupCommand: %v", err)
	}
	if _, err := wm.gitOutput("rev-parse", "--verify", branch); err == nil {
		t.Error("candidate branch should be removed by command cleanup")
	}
}
