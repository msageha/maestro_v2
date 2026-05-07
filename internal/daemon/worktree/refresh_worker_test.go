package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// forceWorkerStatus rewrites worker.Status directly through saveState, used
// by tests that need to simulate states (Conflict, Resolving) the public
// transition API would not let us reach in isolation.
func forceWorkerStatus(t *testing.T, wm *Manager, commandID, workerID string, status model.WorktreeStatus) {
	t.Helper()
	wm.mu.Lock()
	defer wm.mu.Unlock()
	state, err := wm.loadState(commandID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	for i := range state.Workers {
		if state.Workers[i].WorkerID == workerID {
			state.Workers[i].Status = status
			break
		}
	}
	if err := wm.saveState(commandID, state); err != nil {
		t.Fatalf("save state: %v", err)
	}
}

// commitWorkerFile writes a tracked file in the worker worktree, stages and
// commits it on the worker branch. Returns the new HEAD SHA. Lets refresh
// tests evolve a worker branch without going through the full auto-commit
// pipeline.
func commitWorkerFile(t *testing.T, wm *Manager, workerPath, message, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workerPath, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write worker file: %v", err)
	}
	if err := wm.gitRunInDir(workerPath, "add", name); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := wm.gitRunInDir(workerPath, "commit", "-m", message); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	out, err := wm.gitOutputInDir(workerPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(out)
}

// commitIntegrationFile writes a tracked file in the integration worktree,
// commits it on the integration branch, then returns the new HEAD SHA. Lets
// tests advance the integration tip independently of any worker.
func commitIntegrationFile(t *testing.T, wm *Manager, integPath, message, name, body string) string {
	return commitWorkerFile(t, wm, integPath, message, name, body)
}

func TestRefreshWorker_NoOpWhenAlreadyAtIntegrationHead(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_refresh_noop"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	// Worker created from base — HEAD == integration HEAD by construction.
	if err := wm.RefreshWorkerWorktreeToIntegrationHead(commandID, "worker1"); err != nil {
		t.Errorf("expected nil error for already-current worker, got: %v", err)
	}
}

func TestRefreshWorker_FastForwardsBehindWorker(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_refresh_ff"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	// Advance integration ahead of worker.
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	integPath := wm.integrationWorktreePath(commandID)
	newIntegHEAD := commitIntegrationFile(t, wm, integPath, "integration: add x.txt", "x.txt", "merged")

	// Sanity: worker HEAD still points at the original baseSHA, integration moved.
	workerPath, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	preWorkerHEAD, err := wm.gitOutputInDir(workerPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("worker rev-parse: %v", err)
	}
	if strings.TrimSpace(preWorkerHEAD) == newIntegHEAD {
		t.Fatalf("setup invariant violated: worker should still be on baseSHA")
	}

	if err := wm.RefreshWorkerWorktreeToIntegrationHead(commandID, "worker1"); err != nil {
		t.Fatalf("expected fast-forward to succeed, got: %v", err)
	}

	postWorkerHEAD, err := wm.gitOutputInDir(workerPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("post worker rev-parse: %v", err)
	}
	if strings.TrimSpace(postWorkerHEAD) != newIntegHEAD {
		t.Errorf("worker HEAD = %q, want %q (integration HEAD)", strings.TrimSpace(postWorkerHEAD), newIntegHEAD)
	}

	// File from integration must exist in the worker worktree.
	if _, err := os.Stat(filepath.Join(workerPath, "x.txt")); err != nil {
		t.Errorf("expected x.txt to be present in worker after fast-forward: %v", err)
	}

	_ = state // silence unused warning (state is loaded for sanity but not asserted)
}

// TestRefreshWorker_CommitsUntrackedOnlyDirty: when the dirty state is
// exclusively untracked entries, refresh now commits them inline (via
// `git add -A`) so no dirty residue carries into the next task. The
// previous "soft-skip" behaviour stalled phases when wave A produced
// new files and wave B then modified tracked sources — the worker stayed
// dirty across the wave boundary and the phase-boundary merge silently
// dropped wave A's untracked files. Capturing them here closes that gap.
//
// Integration is NOT advanced before refresh so the worker ends up
// strictly ahead of integration after the inline commit. That keeps the
// inline commit on the worker branch (the divergence-recovery path
// would otherwise reset the worker to integration HEAD when integration
// is also ahead).
func TestRefreshWorker_CommitsUntrackedOnlyDirty(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_refresh_untracked_only"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	workerPath, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workerPath, "investigation-summary.txt"), []byte("scratch"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	if err := wm.RefreshWorkerWorktreeToIntegrationHead(commandID, "worker1"); err != nil {
		t.Fatalf("expected refresh to commit untracked dirty inline, got: %v", err)
	}
	statusOut, err := wm.gitOutputInDir(workerPath, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(statusOut) != "" {
		t.Errorf("expected clean worktree after inline commit, got: %q", statusOut)
	}
	logOut, err := wm.gitOutputInDir(workerPath, "log", "--oneline", "-n", "5")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(logOut, "wave-crossing auto-commit") {
		t.Errorf("expected wave-crossing auto-commit in log, got:\n%s", logOut)
	}
}

// TestRefreshWorker_CommitsMixedTrackedAndUntrackedDirty: the wave-crossing
// inline commit captures tracked modifications AND untracked files in a
// single `git add -A` so neither side of the dirty residue ends up
// silently dropped at phase-boundary merge time.
func TestRefreshWorker_CommitsMixedTrackedAndUntrackedDirty(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_refresh_mixed_dirty"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	workerPath, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	readmePath := filepath.Join(workerPath, "README.md")
	if err := os.WriteFile(readmePath, []byte("README\n\nworker tracked edit\n"), 0o644); err != nil {
		t.Fatalf("write README modification: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workerPath, "scratch.txt"), []byte("untracked"), 0o644); err != nil {
		t.Fatalf("write untracked scratch: %v", err)
	}

	if err := wm.RefreshWorkerWorktreeToIntegrationHead(commandID, "worker1"); err != nil {
		t.Fatalf("expected refresh to commit mixed dirty inline, got: %v", err)
	}
	statusOut, err := wm.gitOutputInDir(workerPath, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(statusOut) != "" {
		t.Errorf("expected clean worktree after inline commit, got: %q", statusOut)
	}
	logOut, err := wm.gitOutputInDir(workerPath, "log", "--oneline", "-n", "5")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(logOut, "wave-crossing auto-commit") {
		t.Errorf("expected wave-crossing auto-commit in log, got:\n%s", logOut)
	}
	committed, err := wm.gitOutputInDir(workerPath, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if err != nil {
		t.Fatalf("diff-tree: %v", err)
	}
	for _, f := range []string{"README.md", "scratch.txt"} {
		if !strings.Contains(committed, f) {
			t.Errorf("expected %q to be committed inline, got:\n%s", f, committed)
		}
	}
}

// TestRefreshWorker_AutoCommitsTrackedDirtyChanges pins the wave-crossing
// inline auto-commit fast path: when the dirty state consists only of
// tracked-file modifications (the typical wave 1 → wave 2 dispatch case
// where the previous task on the same worker edited tracked sources and
// integration has NOT yet absorbed those edits — its phase-boundary
// commit hasn't fired), RefreshWorkerWorktreeToIntegrationHead must
// `git add -u` + commit inline and continue. After the inline commit
// the worker is strictly ahead of integration, which is the documented
// "no-op fast-forward" case (refresh returns nil), but the dirty bits
// must be gone and the inline commit must appear in the log.
func TestRefreshWorker_AutoCommitsTrackedDirtyChanges(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_refresh_inline_commit"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	// Worker has a tracked-file modification (no untracked files). This
	// mirrors the wave 1 task that edited a tracked source and reached
	// result_write before its phase-boundary auto-commit fired.
	// Integration is left untouched (no concurrent commit yet) — the
	// realistic wave-crossing shape, since phase-boundary merge into
	// integration runs strictly after the wave completes.
	workerPath, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	// Append to the seed README.md installed by InitTestGitRepo.
	readmePath := filepath.Join(workerPath, "README.md")
	if err := os.WriteFile(readmePath, []byte("README\n\nworker tracked edit\n"), 0o644); err != nil {
		t.Fatalf("write README modification: %v", err)
	}

	if err := wm.RefreshWorkerWorktreeToIntegrationHead(commandID, "worker1"); err != nil {
		t.Fatalf("expected refresh to succeed after inline auto-commit, got: %v", err)
	}

	// Worktree must be clean (the dirty file got committed inline) and
	// the inline-commit message must appear in the log so an operator
	// can audit what happened.
	statusOut, err := wm.gitOutputInDir(workerPath, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(statusOut) != "" {
		t.Errorf("worktree must be clean after inline auto-commit, got: %q", statusOut)
	}
	logOut, err := wm.gitOutputInDir(workerPath, "log", "--oneline", "-n", "5")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(logOut, "wave-crossing auto-commit") {
		t.Errorf("expected wave-crossing auto-commit in log, got:\n%s", logOut)
	}
}

// TestRefreshWorker_AutoResetsDivergedHistory: when worker history has
// truly diverged (own commits AND integration moved on), the refresh
// path resets the worker worktree to integration HEAD with a warning
// log, accepting the loss of the diverged worker commits in exchange
// for unblocking dispatch. The Planner/operator can re-dispatch any
// lost work as a fresh task.
func TestRefreshWorker_AutoResetsDivergedHistory(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_refresh_diverge"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	// Worker commits on its own branch (not yet merged into integration).
	workerPath, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	commitWorkerFile(t, wm, workerPath, "worker exclusive change", "w.txt", "worker-only")

	// Integration also gets its own commit (different file).
	integPath := wm.integrationWorktreePath(commandID)
	commitIntegrationFile(t, wm, integPath, "integration exclusive change", "i.txt", "integration-only")

	if err := wm.RefreshWorkerWorktreeToIntegrationHead(commandID, "worker1"); err != nil {
		t.Fatalf("expected auto-recovery from divergence, got error: %v", err)
	}

	// After auto-reset, worker HEAD must equal integration HEAD.
	workerHEAD, err := wm.gitOutputInDir(workerPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("post worker rev-parse: %v", err)
	}
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState: %v", err)
	}
	integHEAD, err := wm.gitOutputInDir(workerPath, "rev-parse", state.Integration.Branch)
	if err != nil {
		t.Fatalf("integration rev-parse: %v", err)
	}
	if strings.TrimSpace(workerHEAD) != strings.TrimSpace(integHEAD) {
		t.Errorf("after auto-reset, worker HEAD %q must equal integration HEAD %q",
			strings.TrimSpace(workerHEAD), strings.TrimSpace(integHEAD))
	}
}

func TestRefreshWorker_NoOpWhenWorkerAhead(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_refresh_ahead"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	// Worker has a commit not yet on integration.
	workerPath, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	preHEAD := commitWorkerFile(t, wm, workerPath, "worker ahead", "ahead.txt", "ahead")

	if err := wm.RefreshWorkerWorktreeToIntegrationHead(commandID, "worker1"); err != nil {
		t.Errorf("expected no-op for worker-ahead case, got: %v", err)
	}

	postHEAD, err := wm.gitOutputInDir(workerPath, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("post worker rev-parse: %v", err)
	}
	if strings.TrimSpace(postHEAD) != preHEAD {
		t.Errorf("worker HEAD changed unexpectedly: pre=%q post=%q", preHEAD, strings.TrimSpace(postHEAD))
	}
}

// TestRefreshWorker_SkipsConflictAndResolvingWorkers covers the conflict-
// resolution dispatch path: when a worker is intentionally diverged from
// integration so a Planner-issued resolution task can edit the conflicted
// state, RefreshWorkerWorktreeToIntegrationHead must NOT raise the diverged
// error (which would block the resolution task in a 5-attempt -> dead-letter
// loop). The skip is narrow — only the Conflict and Resolving statuses —
// so other statuses still get refreshed or fail-closed on real divergence.
func TestRefreshWorker_SkipsConflictAndResolvingWorkers(t *testing.T) {
	t.Parallel()
	for _, status := range []model.WorktreeStatus{
		model.WorktreeStatusConflict,
		model.WorktreeStatusResolving,
	} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			projectRoot := testutil.InitTestGitRepo(t)
			wm := newTestWorktreeManager(t, projectRoot)
			commandID := "cmd_refresh_skip_" + string(status)
			if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
				t.Fatalf("createForCommand: %v", err)
			}

			// Set up genuine divergence: both worker and integration have a
			// commit the other does not. This is the exact shape of a real
			// merge conflict awaiting resolution.
			workerPath, err := wm.GetWorkerPath(commandID, "worker1")
			if err != nil {
				t.Fatalf("GetWorkerPath: %v", err)
			}
			commitWorkerFile(t, wm, workerPath, "worker side", "w.txt", "worker")
			integPath := wm.integrationWorktreePath(commandID)
			commitIntegrationFile(t, wm, integPath, "integration side", "i.txt", "integration")

			// Force the worker into the conflict-flow status that the
			// dispatcher will see when handing the resolution task.
			forceWorkerStatus(t, wm, commandID, "worker1", status)

			err = wm.RefreshWorkerWorktreeToIntegrationHead(commandID, "worker1")
			if err != nil {
				t.Fatalf("expected refresh to no-op for status=%s, got: %v", status, err)
			}

			// Worker HEAD must not have changed — the resolution task is
			// supposed to land on the diverged state, not on integration HEAD.
			postHEAD, err := wm.gitOutputInDir(workerPath, "rev-parse", "HEAD")
			if err != nil {
				t.Fatalf("post worker rev-parse: %v", err)
			}
			integHEAD, err := wm.gitOutputInDir(workerPath, "rev-parse",
				"maestro/"+commandID+"/integration")
			if err != nil {
				t.Fatalf("integration rev-parse: %v", err)
			}
			if strings.TrimSpace(postHEAD) == strings.TrimSpace(integHEAD) {
				t.Errorf("worker HEAD must remain diverged from integration for status=%s; "+
					"got worker=%s integ=%s",
					status, strings.TrimSpace(postHEAD), strings.TrimSpace(integHEAD))
			}
		})
	}
}
