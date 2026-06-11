package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// TestIsWorkerAheadOrDirty_CleanInLockstep verifies that a worker whose
// HEAD matches integration HEAD and whose worktree is clean reports
// "no merge needed".
func TestIsWorkerAheadOrDirty_CleanInLockstep(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_lockstep"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	needs, err := wm.IsWorkerAheadOrDirty(commandID, "worker1")
	if err != nil {
		t.Fatalf("IsWorkerAheadOrDirty: %v", err)
	}
	if needs {
		t.Errorf("expected lockstep worker to NOT need merge, got needs=true")
	}
}

// TestIsWorkerAheadOrDirty_DirtyWorktree pins the regression that motivated
// this method: worker.Status may say `integrated` from a prior phase, but
// the worker has dirty (uncommitted) edits in its worktree from the
// current phase. The status-only check would falsely say "no merge
// needed"; IsWorkerAheadOrDirty must catch the dirt.
func TestIsWorkerAheadOrDirty_DirtyWorktree(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_dirty"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	// Simulate the prior-phase integrated state: status says integrated
	// even though the worker is now producing fresh edits.
	func() {
		wm.mu.Lock()
		defer wm.mu.Unlock()
		state, _ := wm.loadState(commandID)
		state.Workers[0].Status = model.WorktreeStatusIntegrated
		_ = wm.saveState(commandID, state)
	}()

	wt, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "fresh.txt"), []byte("dirt"), 0644); err != nil {
		t.Fatal(err)
	}

	needs, err := wm.IsWorkerAheadOrDirty(commandID, "worker1")
	if err != nil {
		t.Fatalf("IsWorkerAheadOrDirty: %v", err)
	}
	if !needs {
		t.Errorf("expected dirty worker to need merge, got needs=false (status-only check would have missed this)")
	}
}

// TestIsWorkerAheadOrDirty_AheadOfIntegration verifies the committed-but-
// unmerged case: worker has commits past integration HEAD but a clean
// worktree. The pre-merge gate must still trigger so those commits land
// in integration before a downstream RunOnIntegration task reads from it.
func TestIsWorkerAheadOrDirty_AheadOfIntegration(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_ahead"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}

	wt, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "ahead.txt"), []byte("ahead"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := wm.CommitWorkerChanges(commandID, "worker1", "worker1 ahead"); err != nil {
		t.Fatalf("CommitWorkerChanges: %v", err)
	}

	needs, err := wm.IsWorkerAheadOrDirty(commandID, "worker1")
	if err != nil {
		t.Fatalf("IsWorkerAheadOrDirty: %v", err)
	}
	if !needs {
		t.Errorf("expected committed-but-unmerged worker to need merge, got needs=false")
	}

	// After merge, the same worker must report clean.
	if _, err := wm.MergeToIntegration(context.Background(), commandID, []string{"worker1"}, nil); err != nil {
		t.Fatalf("MergeToIntegration: %v", err)
	}
	needs, err = wm.IsWorkerAheadOrDirty(commandID, "worker1")
	if err != nil {
		t.Fatalf("IsWorkerAheadOrDirty (post-merge): %v", err)
	}
	if needs {
		t.Errorf("expected post-merge worker to be in lockstep, got needs=true")
	}
}

// TestIsWorkerAheadOrDirty_ConflictResolvingSkipped verifies that workers
// in Conflict / Resolving (resume_merge owns) are reported as "no merge
// needed" so the pre-merge gate does not race the resolution pipeline.
func TestIsWorkerAheadOrDirty_ConflictResolvingSkipped(t *testing.T) {
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
			commandID := "cmd_conflict_" + string(status)
			if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
				t.Fatalf("createForCommand: %v", err)
			}
			func() {
				wm.mu.Lock()
				defer wm.mu.Unlock()
				state, _ := wm.loadState(commandID)
				if status == model.WorktreeStatusResolving {
					state.Workers[0].Status = model.WorktreeStatusConflict
					_ = wm.saveState(commandID, state)
					state.Workers[0].Status = model.WorktreeStatusResolving
				} else {
					state.Workers[0].Status = model.WorktreeStatusActive
					_ = wm.saveState(commandID, state)
					state.Workers[0].Status = model.WorktreeStatusConflict
				}
				_ = wm.saveState(commandID, state)
			}()
			needs, err := wm.IsWorkerAheadOrDirty(commandID, "worker1")
			if err != nil {
				t.Fatalf("IsWorkerAheadOrDirty: %v", err)
			}
			if needs {
				t.Errorf("status=%s: expected pre-merge gate to skip (resume_merge owns), got needs=true", status)
			}
		})
	}
}

// TestExtractUnstattablePaths covers the regex used by the git-add
// fallback to pull paths out of "unable to stat 'X': Operation not
// permitted" error strings.
func TestExtractUnstattablePaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
		want []string
	}{
		{
			name: "single",
			msg:  "git add -A: exit status 128\nstderr: fatal: unable to stat '.env.example': Operation not permitted\n",
			want: []string{".env.example"},
		},
		{
			name: "multiple_distinct",
			msg:  "fatal: unable to stat 'a.txt': Operation not permitted\nfatal: unable to stat 'b/c.txt': Operation not permitted\n",
			want: []string{"a.txt", "b/c.txt"},
		},
		{
			name: "duplicate_collapsed",
			msg:  "unable to stat 'x': Operation not permitted\nunable to stat 'x': Operation not permitted\n",
			want: []string{"x"},
		},
		{
			name: "no_match",
			msg:  "fatal: pathspec 'foo' did not match any file(s) known to git",
			want: nil,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractUnstattablePaths(tc.msg)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestIsUnstattableError verifies the detector accepts canonical OS-deny
// shapes (both `git add` and `git worktree add` phrasings) and rejects
// unrelated errors.
func TestIsUnstattableError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want bool
	}{
		// `git add -A` shape (quoted path).
		{"fatal: unable to stat '.env': Operation not permitted", true},
		{"unable to stat 'x': Permission denied", true},
		// `git worktree add` checkout shape (unquoted, "just-written file").
		{"error: unable to stat just-written file .env.example: Operation not permitted", true},
		{"error: unable to stat just-written file path/to/file: Permission denied", true},
		{"fatal: pathspec 'a' did not match", false},
		{"fatal: bad object", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = errFromString(c.msg)
		}
		got := isUnstattableError(err)
		if got != c.want {
			t.Errorf("isUnstattableError(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

// TestExtractUnstattablePathsForWorktreeAdd pins the merged regex
// behaviour: the helper must pick up paths from BOTH `git worktree add`
// (unquoted, "just-written file <path>:") AND `git add -A`
// (quoted, "unable to stat '<path>'") error shapes, deduplicating
// across both shapes when an error message contains both.
func TestExtractUnstattablePathsForWorktreeAdd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  string
		want []string
	}{
		{
			name: "worktree_add_just_written",
			msg:  "error: unable to stat just-written file .env.example: Operation not permitted\n",
			want: []string{".env.example"},
		},
		{
			name: "git_add_quoted",
			msg:  "fatal: unable to stat 'a.txt': Operation not permitted\n",
			want: []string{"a.txt"},
		},
		{
			name: "both_shapes_dedup",
			msg: "error: unable to stat just-written file .env: Operation not permitted\n" +
				"fatal: unable to stat '.env': Operation not permitted\n",
			want: []string{".env"},
		},
		{
			name: "no_match",
			msg:  "fatal: pathspec 'foo' did not match any file(s) known to git",
			want: []string{},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractUnstattablePathsForWorktreeAdd(tc.msg)
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestWorktreePathFromArgs covers both argv shapes the daemon emits
// for `git worktree add`: the integration form (4-arg, path immediately
// after "add") and the worker form (6-arg, "-b <branch> <path> <baseSHA>").
// Also pins behaviour when --no-checkout is already present (fallback
// re-execution must still find the path).
func TestWorktreePathFromArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "integration_4arg",
			args: []string{"worktree", "add", "/tmp/integration", "branch1"},
			want: "/tmp/integration",
		},
		{
			name: "worker_6arg",
			args: []string{"worktree", "add", "-b", "branch1", "/tmp/worker1", "abc123"},
			want: "/tmp/worker1",
		},
		{
			name: "worker_with_no_checkout_flag_inline",
			args: []string{"worktree", "add", "--no-checkout", "-b", "branch1", "/tmp/worker1", "abc123"},
			want: "/tmp/worker1",
		},
		{
			name: "integration_with_no_checkout",
			args: []string{"worktree", "add", "--no-checkout", "/tmp/integration", "branch1"},
			want: "/tmp/integration",
		},
		{
			name: "missing_path",
			args: []string{"worktree", "add"},
			want: "",
		},
		{
			name: "wrong_command",
			args: []string{"branch", "-D", "x"},
			want: "",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := worktreePathFromArgs(tc.args)
			if got != tc.want {
				t.Errorf("got %q, want %q (args=%v)", got, tc.want, tc.args)
			}
		})
	}
}

// TestAppendToGitInfoExclude_LinkedWorktree verifies the exclude entries
// land in the SHARED .git/info/exclude — the only exclude file git reads
// for a linked worktree (the per-worktree info/exclude is silently
// ignored) — and that the pattern actually hides the path from
// `git status` inside the worker worktree. removeMaestroExcludeBlocks
// must then GC exactly the command's marker block.
func TestAppendToGitInfoExclude_LinkedWorktree(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	commandID := "cmd_exclude"
	if err := createForCommand(wm, commandID, []string{"worker1"}); err != nil {
		t.Fatalf("createForCommand: %v", err)
	}
	wt, err := wm.GetWorkerPath(commandID, "worker1")
	if err != nil {
		t.Fatal(err)
	}

	gitStatus := func() string {
		out, sErr := wm.gitOutputInDir(wt, "status", "--porcelain")
		if sErr != nil {
			t.Fatalf("git status: %v", sErr)
		}
		return out
	}

	// An untracked file inside the worker worktree is visible before the
	// exclude is written.
	if err := os.WriteFile(filepath.Join(wt, "blocked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gitStatus(), "blocked.txt") {
		t.Fatal("untracked file should be visible before exclude")
	}

	if err := appendToGitInfoExclude(wt, commandID, []string{"blocked.txt", "build/output.bin"}); err != nil {
		t.Fatalf("appendToGitInfoExclude: %v", err)
	}

	// Effective: git no longer reports the excluded path in the worktree.
	if strings.Contains(gitStatus(), "blocked.txt") {
		t.Error("excluded file still visible to git status — exclude not effective for linked worktree")
	}

	// The entries live in the shared exclude with the command marker block.
	excludePath, err := resolveGitSharedExcludePath(wt)
	if err != nil {
		t.Fatalf("resolveGitSharedExcludePath: %v", err)
	}
	wantShared := filepath.Join(projectRoot, ".git", "info", "exclude")
	gotResolved, _ := filepath.EvalSymlinks(excludePath)
	wantResolved, _ := filepath.EvalSymlinks(wantShared)
	if gotResolved != wantResolved {
		t.Errorf("exclude path = %q, want shared %q", excludePath, wantShared)
	}
	data, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		maestroExcludeBeginMarker(commandID),
		"/blocked.txt",
		"/build/output.bin",
		maestroExcludeEndMarker(commandID),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exclude file missing %q; got:\n%s", want, body)
		}
	}

	// GC removes the block (and only the block); the file becomes visible
	// to git again.
	wm.removeMaestroExcludeBlocks(commandID)
	data, err = os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude after GC: %v", err)
	}
	if strings.Contains(string(data), "blocked.txt") || strings.Contains(string(data), "maestro-exclude") {
		t.Errorf("exclude block should be removed by GC; got:\n%s", string(data))
	}
	if !strings.Contains(gitStatus(), "blocked.txt") {
		t.Error("file should be visible again after exclude block GC")
	}
}

type stringErr string

func (e stringErr) Error() string { return string(e) }

func errFromString(s string) error { return stringErr(s) }
