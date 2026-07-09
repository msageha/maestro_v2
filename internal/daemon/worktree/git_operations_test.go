package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/testutil"
)

func TestClassifyGitError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want gitErrorClass
	}{
		// Transient errors
		{
			name: "lock contention",
			err:  errors.New("Unable to create '/path/to/repo/.git/index.lock': File exists"),
			want: gitErrorTransient,
		},
		{
			name: "dotlock file",
			err:  errors.New("cannot create .lock file"),
			want: gitErrorTransient,
		},
		{
			// "Unable to create" without .lock evidence is not lock
			// contention (disk full, permission) — retrying cannot help.
			name: "unable to create generic is permanent",
			err:  errors.New("Unable to create temp file"),
			want: gitErrorPermanent,
		},
		{
			name: "cannot lock ref",
			err:  errors.New("error: cannot lock ref 'refs/heads/main': is at abc123 but expected def456"),
			want: gitErrorTransient,
		},
		{
			// Regression (W-G8): the old bare "lock" substring misclassified
			// permanent pathspec errors mentioning package-lock.json.
			name: "package-lock.json pathspec is permanent",
			err:  errors.New("error: pathspec 'package-lock.json' did not match any file(s) known to git"),
			want: gitErrorPermanent,
		},
		{
			// Regression (W-G8): "flock" must not be treated as git lock contention.
			name: "flock message is permanent",
			err:  errors.New("fatal: flock operation failed on shared resource"),
			want: gitErrorPermanent,
		},
		{
			// Regression (W-G8): lock contention stays transient even when the
			// message also contains a broad permanent keyword ("invalid" in a
			// ref name) — specific transient patterns are matched first.
			name: "lock contention containing invalid stays transient",
			err:  errors.New("Unable to create '/repo/.git/refs/heads/invalid.lock': File exists"),
			want: gitErrorTransient,
		},

		// Permanent errors
		{
			name: "bad object",
			err:  errors.New("fatal: bad object abc123"),
			want: gitErrorPermanent,
		},
		{
			name: "corrupt repo",
			err:  errors.New("error: object file is corrupt"),
			want: gitErrorPermanent,
		},
		{
			name: "not a git repository",
			err:  errors.New("fatal: not a git repository (or any parent up to mount point /)"),
			want: gitErrorPermanent,
		},
		{
			name: "invalid reference",
			err:  errors.New("fatal: invalid reference: refs/heads/nonexistent"),
			want: gitErrorPermanent,
		},

		// Timeout errors — must be Permanent (dirty worktree risk)
		{
			name: "context.DeadlineExceeded",
			err:  context.DeadlineExceeded,
			want: gitErrorPermanent,
		},
		{
			name: "wrapped DeadlineExceeded",
			err:  fmt.Errorf("git merge: timeout after 30s: %w", context.DeadlineExceeded),
			want: gitErrorPermanent,
		},
		{
			name: "timeout in error message",
			err:  errors.New("connection timeout while fetching"),
			want: gitErrorPermanent,
		},

		// New transient patterns: git process lock
		{
			name: "another git process running",
			err:  errors.New("Another git process seems to be running in this repository"),
			want: gitErrorTransient,
		},
		// New transient patterns: network errors
		{
			name: "connection refused",
			err:  errors.New("fatal: unable to access 'https://github.com/...': Connection refused"),
			want: gitErrorTransient,
		},
		{
			name: "could not resolve host",
			err:  errors.New("fatal: unable to access 'https://github.com/...': Could not resolve host: github.com"),
			want: gitErrorTransient,
		},
		{
			name: "connection timed out is transient",
			err:  errors.New("fatal: unable to access 'https://github.com/...': Connection timed out"),
			want: gitErrorTransient,
		},
		{
			name: "connection reset by peer",
			err:  errors.New("fatal: the remote end hung up unexpectedly: Connection reset by peer"),
			want: gitErrorTransient,
		},
		// Resource errors — permanent (won't resolve within backoff window)
		{
			name: "cannot allocate memory",
			err:  errors.New("fatal: cannot allocate memory"),
			want: gitErrorPermanent,
		},
		{
			name: "no space left on device",
			err:  errors.New("error: No space left on device"),
			want: gitErrorPermanent,
		},

		// Unknown errors default to Permanent
		{
			name: "unknown error",
			err:  errors.New("something completely unexpected happened"),
			want: gitErrorPermanent,
		},
		{
			name: "nil error",
			err:  nil,
			want: gitErrorPermanent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyGitError(tt.err)
			if got != tt.want {
				wantStr := "Permanent"
				gotStr := "Permanent"
				if tt.want == gitErrorTransient {
					wantStr = "Transient"
				}
				if got == gitErrorTransient {
					gotStr = "Transient"
				}
				t.Errorf("classifyGitError(%v) = %s, want %s", tt.err, gotStr, wantStr)
			}
		})
	}
}

func TestWrapGitOutputError_IncludesStderr(t *testing.T) {
	t.Parallel()
	// Simulate exec.ExitError with Stderr containing a transient pattern.
	exitErr := &exec.ExitError{
		ProcessState: nil, // not used by our code
		Stderr:       []byte("Unable to create '/path/to/repo/.git/index.lock': File exists"),
	}
	wrapped := wrapGitOutputError(exitErr, []string{"merge", "main"})
	msg := wrapped.Error()

	if !strings.Contains(msg, "Unable to create") {
		t.Errorf("expected wrapped error to contain stderr, got: %s", msg)
	}
	if !strings.Contains(msg, "git merge main") {
		t.Errorf("expected wrapped error to contain command, got: %s", msg)
	}

	// The error should be classified as Transient thanks to stderr content.
	if classifyGitError(wrapped) != gitErrorTransient {
		t.Error("expected classifyGitError to return Transient for stderr with lock pattern")
	}
}

func TestWrapGitOutputError_EmptyStderr(t *testing.T) {
	t.Parallel()
	exitErr := &exec.ExitError{
		Stderr: []byte{},
	}
	wrapped := wrapGitOutputError(exitErr, []string{"status"})
	msg := wrapped.Error()

	// Should not contain "stderr:" when Stderr is empty.
	if strings.Contains(msg, "stderr:") {
		t.Errorf("expected no stderr in error message for empty Stderr, got: %s", msg)
	}
}

func TestClassifyGitError_ExitErrorWithTransientStderr(t *testing.T) {
	t.Parallel()
	// When exec.ExitError is wrapped with stderr via wrapGitOutputError,
	// classifyGitError should detect transient patterns.
	tests := []struct {
		name   string
		stderr string
		want   gitErrorClass
	}{
		{
			name:   "lock file in stderr",
			stderr: "Unable to create '/repo/.git/index.lock': File exists",
			want:   gitErrorTransient,
		},
		{
			name:   "dotlock in stderr",
			stderr: "error: cannot lock ref 'refs/heads/main': is at abc123 but expected def456",
			want:   gitErrorTransient,
		},
		{
			name:   "permanent error in stderr",
			stderr: "fatal: bad object abc123",
			want:   gitErrorPermanent,
		},
		{
			name:   "unknown stderr content",
			stderr: "exit status 128",
			want:   gitErrorPermanent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exitErr := &exec.ExitError{
				Stderr: []byte(tt.stderr),
			}
			wrapped := wrapGitOutputError(exitErr, []string{"merge", "main"})
			got := classifyGitError(wrapped)
			if got != tt.want {
				wantStr := "Permanent"
				gotStr := "Permanent"
				if tt.want == gitErrorTransient {
					wantStr = "Transient"
				}
				if got == gitErrorTransient {
					gotStr = "Transient"
				}
				t.Errorf("classifyGitError(wrapped stderr=%q) = %s, want %s", tt.stderr, gotStr, wantStr)
			}
		})
	}
}

func TestIsTransientGitError(t *testing.T) {
	t.Parallel()
	if !isTransientGitError(errors.New("Unable to create '/repo/.git/index.lock': File exists")) {
		t.Error("expected lock error to be transient")
	}
	if isTransientGitError(errors.New("fatal: bad object")) {
		t.Error("expected bad object error to not be transient")
	}
	if isTransientGitError(context.DeadlineExceeded) {
		t.Error("expected DeadlineExceeded to not be transient")
	}
}

func TestGitOutputWithRetry_Success(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	output, err := wm.gitOutputWithRetry(context.Background(), projectRoot, 3, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("gitOutputWithRetry: %v", err)
	}
	if len(output) < 7 {
		t.Errorf("expected SHA output, got %q", output)
	}
}

func TestSanitizeGitStderr_StripsAbsolutePaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		check func(t *testing.T, out string)
	}{
		{
			name:  "absolute path stripped",
			input: "Unable to create '/Users/mzk/project/.git/index.lock': File exists",
			check: func(t *testing.T, out string) {
				if strings.Contains(out, "/Users/mzk") {
					t.Errorf("output still contains absolute path: %s", out)
				}
				if !strings.Contains(out, "Unable to create") {
					t.Errorf("lost classification keyword 'Unable to create': %s", out)
				}
				if !strings.Contains(out, "index.lock") {
					t.Errorf("lost basename 'index.lock': %s", out)
				}
			},
		},
		{
			name:  "preserves non-path content",
			input: "fatal: bad object abc123",
			check: func(t *testing.T, out string) {
				if out != "fatal: bad object abc123" {
					t.Errorf("unexpected modification: %s", out)
				}
			},
		},
		{
			name:  "truncates long output",
			input: strings.Repeat("a", 300),
			check: func(t *testing.T, out string) {
				if len(out) > 260 { // 256 + "..."
					t.Errorf("output not truncated, len=%d", len(out))
				}
				if !strings.HasSuffix(out, "...") {
					t.Errorf("truncated output should end with '...': %s", out)
				}
			},
		},
		{
			name:  "multiple paths stripped",
			input: "error in /home/user/repo/.git and /tmp/git-merge-xxx/file.txt",
			check: func(t *testing.T, out string) {
				if strings.Contains(out, "/home/user") {
					t.Errorf("first path not stripped: %s", out)
				}
				if strings.Contains(out, "/tmp/git-merge") {
					t.Errorf("second path not stripped: %s", out)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := sanitizeGitStderr(tt.input)
			tt.check(t, out)
		})
	}
}

func TestWrapGitOutputError_SanitizesStderr(t *testing.T) {
	t.Parallel()
	exitErr := &exec.ExitError{
		Stderr: []byte("fatal: unable to access '/Users/secret/project/.git/': Permission denied"),
	}
	wrapped := wrapGitOutputError(exitErr, []string{"fetch", "origin"})
	msg := wrapped.Error()
	if strings.Contains(msg, "/Users/secret") {
		t.Errorf("error message leaks internal path: %s", msg)
	}
	if !strings.Contains(msg, "git fetch origin") {
		t.Errorf("error message missing command: %s", msg)
	}
}

// TestGitOutputWithRetry_EmptyOutputNilError tests that gitOutputWithRetry
// correctly handles the case where git returns empty output with no error
// (e.g., git status --porcelain in a clean repo).
func TestGitOutputWithRetry_EmptyOutputNilError(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// git status --porcelain returns empty output when the repo is clean
	output, err := wm.gitOutputWithRetry(context.Background(), projectRoot, 3, "status", "--porcelain")
	if err != nil {
		t.Fatalf("gitOutputWithRetry: unexpected error: %v", err)
	}
	if output != "" {
		t.Errorf("expected empty output for clean repo, got %q", output)
	}
}

func TestGitOutputWithRetry_PermanentNoRetry(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	start := time.Now()
	_, err := wm.gitOutputWithRetry(context.Background(), projectRoot, 3, "rev-parse", "nonexistent_ref_xyz999")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for nonexistent ref, got nil")
	}

	// The retry policy backs off via gitOutputBackoff, which on attempt #2
	// alone sleeps gitOutputBackoffBase (default 200ms). A clean
	// no-retry path runs git exactly once, so even a worst-case loaded
	// CI runner finishes in well under one full retry cycle (200ms +
	// process spawn). 1.5s leaves ample headroom for parallel git tests
	// while still detecting an extra retry: a single retry would burn
	// ≥ 200ms backoff plus another git exec, pushing total runtime past
	// the bound only when a regression actually re-enters the loop.
	// Earlier 300ms cap intermittently failed under parallel git load on
	// macOS — see retest8 flaky failure report.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("gitOutputWithRetry took %v, expected fast return for permanent error (no retry)", elapsed)
	}
}

// Regression (W-G2): the raw combined output must preserve nested paths for
// the unstattable fallbacks, while the returned error stays sanitized.
func TestGitRunRawInDir_RawOutputPreservesNestedPaths(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	raw, err := wm.gitRunRawInDir(projectRoot, "add", "sub/dir/nonexistent.txt")
	if err == nil {
		t.Fatal("expected git add of a nonexistent path to fail")
	}
	if !strings.Contains(raw, "'sub/dir/nonexistent.txt'") {
		t.Errorf("raw output must preserve the full nested path, got: %q", raw)
	}
	// The error embeds only the sanitized git output: the quoted stderr path
	// is mangled to the …/<basename> form (the command-args echo at the
	// error head legitimately keeps the argv verbatim).
	if strings.Contains(err.Error(), "'sub/dir/nonexistent.txt'") {
		t.Errorf("returned error must embed sanitized output only, got: %v", err)
	}
	if !strings.Contains(err.Error(), "'sub…/nonexistent.txt'") {
		t.Errorf("returned error should carry the sanitized …/<basename> form, got: %v", err)
	}
}

// Regression (W-G2): extracting unstattable paths from the sanitized error
// text mangles nested paths into non-existent targets (`a/b/c.env` →
// `a…/c.env`) — the fallbacks must extract from the RAW output.
func TestExtractUnstattablePaths_RawVsSanitized(t *testing.T) {
	t.Parallel()
	const path = "internal/config/.env.example"
	raw := "fatal: unable to stat '" + path + "': Operation not permitted"

	got := extractUnstattablePaths(raw)
	if len(got) != 1 || got[0] != path {
		t.Fatalf("raw extraction = %v, want [%s]", got, path)
	}
	for _, p := range extractUnstattablePaths(sanitizeGitStderr(raw)) {
		if p == path {
			t.Fatalf("sanitized extraction preserved the nested path — the raw plumbing regression guard is stale: %v", p)
		}
	}
}

// Regression (W-G2): sanitizeGitStderr's 256-byte truncation can cut the
// classification keywords out of a long message; detection must therefore
// run on the raw output.
func TestIsUnstattableMessage_TruncationSafeOnRawOnly(t *testing.T) {
	t.Parallel()
	long := "error: unable to stat '" + strings.Repeat("deep/", 60) + ".env': Operation not permitted"
	if !isUnstattableMessage(long) {
		t.Error("raw long message must classify as unstattable")
	}
	if isUnstattableMessage(sanitizeGitStderr(long)) {
		t.Error("sanitized long message classified as unstattable — truncation guard is stale, revisit the raw plumbing rationale")
	}
}

// Regression (W-G1): a --no-checkout worktree has an EMPTY per-worktree
// index; materializeNoCheckoutWorktree must populate it via `read-tree HEAD`
// before checkout-index, otherwise nothing is materialised and the worker
// receives a worktree containing only .git (whose `git add -A` would then
// commit the deletion of every tracked file).
func TestMaterializeNoCheckoutWorktree_PopulatesIndexAndFiles(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Nested tracked content so materialisation covers subdirectories.
	if err := os.MkdirAll(filepath.Join(projectRoot, "sub", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "sub", "dir", "nested.txt"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := wm.gitRun("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if err := wm.gitRun("commit", "-m", "add nested"); err != nil {
		t.Fatal(err)
	}

	wtPath := filepath.Join(projectRoot, ".maestro", "worktrees", "cmd_mat", "worker1")
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := wm.gitRun("worktree", "add", "--no-checkout", "-b", "maestro/cmd_mat/worker1", wtPath, "HEAD"); err != nil {
		t.Fatalf("worktree add --no-checkout: %v", err)
	}

	if err := wm.materializeNoCheckoutWorktree("cmd_mat", wtPath, nil); err != nil {
		t.Fatalf("materializeNoCheckoutWorktree: %v", err)
	}

	if _, err := os.Stat(filepath.Join(wtPath, "sub", "dir", "nested.txt")); err != nil {
		t.Errorf("tracked file not materialised: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Errorf("root tracked file not materialised: %v", err)
	}
	statusOut, err := wm.gitOutputInDir(wtPath, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(statusOut) != "" {
		t.Errorf("worktree must be clean after materialisation (an empty index reports mass deletions), got:\n%s", statusOut)
	}
}

// Regression (W-G1): a materialisation failure must fail the fallback
// (fail-fast) instead of reporting an unusable worktree as recovered.
func TestMaterializeNoCheckoutWorktree_FailsFastOnReadTreeFailure(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// A plain directory is not a worktree: read-tree HEAD must fail and the
	// helper must surface it.
	if err := wm.materializeNoCheckoutWorktree("cmd_mat_bad", t.TempDir(), nil); err == nil {
		t.Fatal("expected read-tree failure to fail the fallback, got nil")
	}
}

// Regression (W-G5): git subprocesses must run with the message locale
// pinned to C so error classification never depends on the daemon locale.
func TestGitExec_PinsMessageLocale(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("LC_ALL", "ja_JP.UTF-8")
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// `git -c alias.x=!env x` dumps the environment the git subprocess sees.
	out, err := wm.gitOutput("-c", "alias.envdump=!env", "envdump")
	if err != nil {
		t.Fatalf("git alias envdump: %v", err)
	}
	env := make(map[string]string)
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	if env["LC_ALL"] != "C" {
		t.Errorf("git subprocess LC_ALL = %q, want C (daemon locale must not leak)", env["LC_ALL"])
	}
	if env["LANG"] != "C" {
		t.Errorf("git subprocess LANG = %q, want C", env["LANG"])
	}
}

// Regression (W-G6): a failed `worktree add -b` against a PRE-EXISTING
// branch must not delete that branch on the error path — it may hold
// unpublished commits from an earlier run.
func TestEnsureWorkerWorktree_PreservesPreexistingBranchOnFailure(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_g6_worker"
	branch := "maestro/" + commandID + "/worker1"
	if err := wm.gitRun("branch", branch, "HEAD"); err != nil {
		t.Fatal(err)
	}
	wantSHA, err := wm.gitOutput("rev-parse", branch)
	if err != nil {
		t.Fatal(err)
	}

	if err := wm.EnsureWorkerWorktree(commandID, "worker1"); err == nil {
		t.Fatal("expected worktree add -b against a pre-existing branch to fail")
	}

	gotSHA, err := wm.gitOutput("rev-parse", branch)
	if err != nil {
		t.Fatalf("pre-existing branch must survive the failed attempt: %v", err)
	}
	if strings.TrimSpace(gotSHA) != strings.TrimSpace(wantSHA) {
		t.Errorf("pre-existing branch moved: %s -> %s", strings.TrimSpace(wantSHA), strings.TrimSpace(gotSHA))
	}
}
