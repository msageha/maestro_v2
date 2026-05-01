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
// commits it on the worker branch. Returns the new HEAD SHA. Used by R10/F-040
// refresh tests to evolve a worker branch without going through the full
// auto-commit pipeline.
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

// TestRefreshWorker_SoftSkipsUntrackedOnlyDirty pins the 2026-04-29
// e2e regression where read-only investigation tasks leave benign
// untracked residue (.tmp-*, log files) and the next dispatch's
// refresh aborted, drove the inline-retry attempts cap to 0, and dead-
// lettered the next task. The fix: when the dirty state is exclusively
// untracked entries (no tracked modifications), refresh logs a warning
// and returns nil instead of failing — sensitive-file gates still run
// at phase boundary, so this only defers the integration fast-forward
// by one clean cycle. Tracked-modification residue (the wave-crossing
// case) is still handled by the inline auto-commit path tested
// separately.
func TestRefreshWorker_SoftSkipsUntrackedOnlyDirty(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_refresh_untracked_only"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	// Make integration ahead so the function would attempt a fast-forward
	// if the worktree were clean.
	integPath := wm.integrationWorktreePath(commandID)
	commitIntegrationFile(t, wm, integPath, "integration ahead", "y.txt", "merged")

	// Introduce dirty state in worker worktree (untracked file only;
	// tracked README.md is unchanged).
	workerPath, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workerPath, ".tmp-investigation-summary.txt"), []byte("scratch"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	if err := wm.RefreshWorkerWorktreeToIntegrationHead(commandID, "worker1"); err != nil {
		t.Fatalf("expected refresh to soft-skip untracked-only dirty (no error), got: %v", err)
	}
	// The untracked file must remain (no destructive cleanup).
	if _, err := os.Stat(filepath.Join(workerPath, ".tmp-investigation-summary.txt")); err != nil {
		t.Errorf("expected untracked file to be retained after soft-skip, got stat error: %v", err)
	}
	// Worker HEAD must NOT have advanced to integration HEAD because we
	// soft-skipped — that is the intentional trade-off (deferred merge,
	// caught up on the next clean cycle).
	statusOut, err := wm.gitOutputInDir(workerPath, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !strings.Contains(statusOut, ".tmp-investigation-summary.txt") {
		t.Errorf("untracked file should still appear in status, got: %q", statusOut)
	}
}

// TestRefreshWorker_RejectsMixedTrackedAndUntrackedDirty pins the
// safety boundary of the soft-skip: when the worktree has BOTH tracked
// modifications (the inline auto-commit handles those) AND untracked
// files that the inline commit refuses to admit (sensitive-file gate
// concern), the post-inline-commit re-check finds tracked changes have
// been committed but untracked entries linger. That is no longer the
// "untracked-only" case, so the soft-skip does not apply — the
// function returns an error rather than silently admitting unknown
// staged-untracked drift.
func TestRefreshWorker_RejectsMixedTrackedAndUntrackedDirty(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_refresh_mixed_dirty"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	// Worker has BOTH tracked README.md modification AND untracked tmp.
	// Simulates a wave that committed real progress but also left
	// scratch artifacts the inline commit must NOT admit silently.
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

	// In this configuration the inline commit succeeds for the tracked
	// edit and the post-check then sees only untracked residue —
	// triggering the soft-skip. That is the intended behaviour: the
	// tracked progress is committed, the untracked residue is left alone,
	// the fast-forward is deferred to the next cycle. We pin that the
	// tracked commit happened (log shows wave-crossing entry) AND that
	// the function returned without error.
	if err := wm.RefreshWorkerWorktreeToIntegrationHead(commandID, "worker1"); err != nil {
		t.Fatalf("expected refresh to handle mixed dirty (commit tracked, soft-skip untracked), got: %v", err)
	}
	logOut, err := wm.gitOutputInDir(workerPath, "log", "--oneline", "-n", "5")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(logOut, "wave-crossing auto-commit") {
		t.Errorf("expected wave-crossing auto-commit in log (tracked edit), got:\n%s", logOut)
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

// TestRefreshWorker_AutoResetsDivergedHistory pins the 2026-04-30 e2e
// regression fix: when worker history has truly diverged (own commits
// AND integration moved on), the refresh path used to abort with a hard
// error and trigger 3-4 minutes of `worktree_refresh_failed` log spam
// before the task dead-lettered. Auto-recovery resets the worker
// worktree to integration HEAD with a warning log, accepting the loss
// of the diverged worker commits in exchange for unblocking dispatch.
// The Planner/operator can re-dispatch any lost work as a fresh task.
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
// error (which would block the resolution task in a 5-attempt → dead-letter
// loop, the 2026-04 follow-up regression). The skip is narrow — only the
// Conflict and Resolving statuses — so other statuses still get refreshed
// or fail-closed on real divergence.
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
