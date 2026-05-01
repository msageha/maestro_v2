package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestRealRunner constructs a RealVerifyRunner with a discard logger and a
// configurable command runner. maestroDir is created on disk so verify config
// can be loaded (or absent for the Fallback path); projectDir is unused by
// the mock runner but must be non-empty to mirror production wiring.
func newTestRealRunner(t *testing.T) *RealVerifyRunner {
	t.Helper()
	maestroDir := t.TempDir()
	projectDir := t.TempDir()
	r := NewRealVerifyRunner(maestroDir, projectDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return r
}

// writeVerifyYAML writes the given verify config body into r.maestroDir.
func writeVerifyYAML(t *testing.T, r *RealVerifyRunner, body string) {
	t.Helper()
	path := filepath.Join(r.maestroDir, "verify.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write verify.yaml: %v", err)
	}
	writeVerifySnapshotYAML(t, r, "cmd-1", body)
}

func writeVerifySnapshotYAML(t *testing.T, r *RealVerifyRunner, commandID, body string) {
	t.Helper()
	path := verifySnapshotPath(r.maestroDir, commandID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create verify snapshot dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write verify snapshot: %v", err)
	}
}

// recordingRunner returns a runner func that appends each invocation to seen
// (under mu) and returns the configured outcome from results, keyed by cmd.
// Commands not present in results pass with empty output (exit 0).
type recordingRunner struct {
	mu      sync.Mutex
	seen    []string
	results map[string]struct {
		output   string
		exitCode int
		err      error
	}
}

func (rr *recordingRunner) run(_ context.Context, _ string, cmd string) (string, int, error) {
	rr.mu.Lock()
	rr.seen = append(rr.seen, cmd)
	out, ok := rr.results[cmd]
	rr.mu.Unlock()
	if !ok {
		return "", 0, nil
	}
	return out.output, out.exitCode, out.err
}

// TestRealVerifyRunner_FallbackUsesGitDiffCheck pins the language-agnostic
// fallback added in the 2026-04-30 redesign. When verify.yaml is missing,
// the runner executes the single generic check `git diff --check` for
// every project regardless of which marker files happen to be present at
// the project root. Language detection (DetectProjectLanguage) and the
// Default*ForLanguage helpers were deleted because the assumption "this
// is a software-engineering monorepo with one primary language" does not
// hold for polyglot, research, or documentation projects.
func TestRealVerifyRunner_FallbackUsesGitDiffCheck(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	// A go.mod sitting in the project root used to flip the fallback to
	// the Go-specific (go vet / gosec / go bench) bundle. After the
	// redesign the marker is irrelevant — the seeding here exists only to
	// pin that no language-conditional behaviour remains.
	if err := os.WriteFile(filepath.Join(r.projectDir, "go.mod"), []byte("module test\n"), 0o600); err != nil {
		t.Fatalf("seed go.mod: %v", err)
	}
	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass, got %+v", out)
	}
	if len(rr.seen) != 1 || rr.seen[0] != "git diff --check" {
		t.Errorf("expected fallback to run only `git diff --check`, got %v", rr.seen)
	}
}

// TestRealVerifyRunner_FallbackUsesGitDiffOnNonGoProject guards the
// project-aware fallback behaviour added to remove the Go-only assumption:
// when verify.yaml is missing and the project root has no go.mod, the runner
// must still execute a real repository-generic check. A zero-command fallback
// would silently pass unverified work.
func TestRealVerifyRunner_FallbackUsesGitDiffOnNonGoProject(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass with successful fallback command, got %+v", out)
	}
	if len(rr.seen) != 1 || rr.seen[0] != "git diff --check" {
		t.Errorf("expected single fallback command 'git diff --check', got %v", rr.seen)
	}
}

func TestRealVerifyRunner_DefinedConfigExecutesCategoriesInOrder(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - echo build1
    - echo build2
  lint:
    - echo lint1
  typecheck:
    - echo type1
  test:
    - echo test1
  security:
    - echo sec1
  performance:
    - echo perf1
`)

	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass, got %+v", out)
	}

	want := []string{
		"echo build1", "echo build2", "echo lint1",
		"echo type1", "echo test1", "echo sec1", "echo perf1",
	}
	if len(rr.seen) != len(want) {
		t.Fatalf("seen=%v want=%v", rr.seen, want)
	}
	for i := range want {
		if rr.seen[i] != want[i] {
			t.Errorf("seen[%d]=%q want %q (full order=%v)", i, rr.seen[i], want[i], rr.seen)
		}
	}
}

func TestRealVerifyRunner_CommandSnapshotOverridesGlobalConfig(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - echo global
`)
	writeVerifySnapshotYAML(t, r, "cmd_0000000001_snapshot", `verify:
  build:
    - echo snapshot
`)
	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd_0000000001_snapshot", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass, got %+v", out)
	}
	if len(rr.seen) != 1 || rr.seen[0] != "echo snapshot" {
		t.Fatalf("expected command snapshot to be used, got %v", rr.seen)
	}
}

func TestRealVerifyRunner_CommandWithoutSnapshotIgnoresMutableGlobal(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - echo global
`)
	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd_0000000002_nosnap", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass, got %+v", out)
	}
	if len(rr.seen) != 1 || rr.seen[0] != "git diff --check" {
		t.Fatalf("expected project fallback for missing command snapshot, got %v", rr.seen)
	}
}

// TestRealVerifyRunner_MissingWorkdirFailsClosed pins the 2026-04-29
// review fix: when the worker worktree has been cleaned up between
// dispatch and verify (e.g. fast-track stall cleanup races a verify
// already in flight), the runner must surface a hard failure rather
// than letting the per-category advisory path classify the chdir
// errors as passed_with_advisory_failures. Prior behaviour silently
// marked tasks completed even though no verify command actually ran.
func TestRealVerifyRunner_MissingWorkdirFailsClosed(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	rr := &recordingRunner{}
	r.runner = rr.run
	writeVerifyYAML(t, r, `verify:
  build:
    - echo build
  lint:
    - echo lint
`)

	missing := filepath.Join(t.TempDir(), "deleted_worktree")
	out, err := r.Run(context.Background(), "task-1", "cmd-1", missing, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected fail-closed outcome for missing workdir, got %+v", out)
	}
	if !strings.Contains(out.Reason, "verify_workdir_inaccessible") {
		t.Errorf("expected verify_workdir_inaccessible reason, got %q", out.Reason)
	}
	if len(rr.seen) != 0 {
		t.Errorf("missing workdir must not execute any verify commands, got %v", rr.seen)
	}
}

// TestRealVerifyRunner_NonDirectoryWorkdirFailsClosed pins the symmetric
// case: a path that exists but is a regular file (e.g. someone replaced
// the worktree directory with a placeholder file) must also fail rather
// than tunnel through to per-command chdir errors.
func TestRealVerifyRunner_NonDirectoryWorkdirFailsClosed(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	rr := &recordingRunner{}
	r.runner = rr.run
	writeVerifyYAML(t, r, `verify:
  build:
    - echo build
`)

	notADir := filepath.Join(t.TempDir(), "actually_a_file")
	if err := os.WriteFile(notADir, []byte("placeholder"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	out, err := r.Run(context.Background(), "task-1", "cmd-1", notADir, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected fail-closed outcome for non-directory workdir, got %+v", out)
	}
	if !strings.Contains(out.Reason, "verify_workdir_not_directory") {
		t.Errorf("expected verify_workdir_not_directory reason, got %q", out.Reason)
	}
	if len(rr.seen) != 0 {
		t.Errorf("non-directory workdir must not execute any verify commands, got %v", rr.seen)
	}
}

func TestRealVerifyRunner_EmptyVerifyConfigFailsClosed(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	rr := &recordingRunner{}
	r.runner = rr.run
	writeVerifyYAML(t, r, "verify: {}\n")

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected fail-closed outcome for empty verify config, got %+v", out)
	}
	if !strings.Contains(out.Reason, "verify_config_empty") {
		t.Fatalf("expected verify_config_empty reason, got %q", out.Reason)
	}
	if len(rr.seen) != 0 {
		t.Fatalf("empty verify config should not execute commands, got %v", rr.seen)
	}
}

func TestRealVerifyRunner_StopsOnFirstFailure(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - echo b1
  lint:
    - echo l1
  test:
    - echo t1
  security:
    - echo s1
`)

	rr := &recordingRunner{
		results: map[string]struct {
			output   string
			exitCode int
			err      error
		}{
			"echo l1": {output: "lint output stderr", exitCode: 2},
		},
	}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected fail, got pass")
	}
	if !strings.Contains(out.Reason, "category=lint") {
		t.Errorf("reason should include failing category, got %q", out.Reason)
	}
	if !strings.Contains(out.Reason, "exit=2") {
		t.Errorf("reason should include exit code, got %q", out.Reason)
	}
	if !strings.Contains(out.Reason, "lint output stderr") {
		t.Errorf("reason should include output tail, got %q", out.Reason)
	}

	// Build and Lint ran; Test/Security must NOT have run (fail-fast).
	for _, cmd := range rr.seen {
		if cmd == "echo t1" || cmd == "echo s1" {
			t.Errorf("post-failure command was executed: %q (seen=%v)", cmd, rr.seen)
		}
	}
	if len(rr.seen) != 2 {
		t.Errorf("expected 2 commands executed before fail-fast, got %d (%v)", len(rr.seen), rr.seen)
	}
}

func TestRealVerifyRunner_RunnerErrorReportsReason(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - echo crash
`)

	rr := &recordingRunner{
		results: map[string]struct {
			output   string
			exitCode int
			err      error
		}{
			"echo crash": {output: "", exitCode: -1, err: errors.New("exec spawn failed")},
		},
	}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected fail, got pass")
	}
	if !strings.Contains(out.Reason, "err=exec spawn failed") {
		t.Errorf("reason should include underlying error, got %q", out.Reason)
	}
}

func TestRealVerifyRunner_TruncatesLongOutputTail(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - echo huge
`)

	huge := strings.Repeat("x", 10*1024) + "TAIL_MARKER"
	rr := &recordingRunner{
		results: map[string]struct {
			output   string
			exitCode int
			err      error
		}{
			"echo huge": {output: huge, exitCode: 1},
		},
	}
	r.runner = rr.run
	r.maxOutputBytes = 256 // shrink for the test

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected fail")
	}
	if !strings.Contains(out.Reason, "TAIL_MARKER") {
		t.Errorf("reason should include trailing marker, got %q", out.Reason)
	}
	if strings.Count(out.Reason, "x") > 300 { // 256 + a little slack for non-x bytes
		t.Errorf("reason exceeded expected tail bound; len=%d", len(out.Reason))
	}
}

func TestRealVerifyRunner_TimeoutReportsTimeoutReason(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - sleep 5
`)

	r.commandTimeout = 50 * time.Millisecond

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected fail on timeout, got pass")
	}
	if !strings.Contains(out.Reason, "verify_timeout") {
		t.Errorf("reason should mention verify_timeout, got %q", out.Reason)
	}
	if !strings.Contains(out.Reason, "category=build") {
		t.Errorf("reason should include category, got %q", out.Reason)
	}
}

// TestRealVerifyRunner_WorkdirDisappearsDuringRunFailsAsEnvironmental
// pins the 2026-04-30 review correction: when worktree cleanup races a
// still-running verify command and the subprocess fails on a missing
// cwd (`getcwd: No such file or directory`), the runner must surface
// the failure as Passed=false with a `verify_runner_workdir_inaccessible`
// reason. The earlier behavior returned Passed=true under the assumption
// that the publish gate had already moved past the point where verify
// mattered, but the new state-side gate (collectWorktreePublishAndCleanup
// + HasNonTerminalTaskState) blocks publish exactly while a task is at
// verify_pending — so swallowing the failure here would mark the task
// completed in state, bypass the gate, and let an unverified change
// land on the base branch. Routing the disappearance through
// repair_pending preserves the gate and keeps the audit log truthful:
// verify did NOT pass, the runner just could not observe the outcome.
func TestRealVerifyRunner_WorkdirDisappearsDuringRunFailsAsEnvironmental(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	// Use a workdir we can delete mid-run.
	workDir := t.TempDir()
	writeVerifyYAML(t, r, `verify:
  test:
    - npm test
`)

	r.runner = func(_ context.Context, dir, _ string) (string, int, error) {
		// Simulate the race: cleanup unlinks the worktree while the
		// verify command is mid-execution. The subprocess sees its
		// cwd vanish and returns with a getcwd-style error.
		_ = os.RemoveAll(dir)
		return "shell-init: error retrieving current directory: getcwd: ENOENT", 7, nil
	}

	out, err := r.Run(context.Background(), "task-1", "cmd-1", workDir, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected verify run to surface mid-run workdir disappearance as Passed=false (environmental verify failure), got %+v", out)
	}
	if !strings.Contains(out.Reason, "verify_runner_workdir_inaccessible") {
		t.Errorf("Reason should include verify_runner_workdir_inaccessible marker, got %q", out.Reason)
	}
	if !strings.Contains(out.Reason, "category=test") {
		t.Errorf("Reason should include the failing category, got %q", out.Reason)
	}
}

// TestRealVerifyRunner_WorkdirIntactFailureStillFails guards the
// negative direction: a real failure (non-zero exit, workdir still
// alive) must still surface as failed. Otherwise the workdir-stat
// shortcut would silently swallow legitimate test failures.
func TestRealVerifyRunner_WorkdirIntactFailureStillFails(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  test:
    - npm test
`)
	r.runner = func(_ context.Context, _, _ string) (string, int, error) {
		// Workdir untouched; real test failure with a normal output tail.
		return "FAIL: 1 of 5 assertions", 1, nil
	}

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected real test failure to remain Passed=false")
	}
	if !strings.Contains(out.Reason, "category=test") {
		t.Errorf("reason should include category=test, got %q", out.Reason)
	}
}

func TestRealVerifyRunner_AbortedContextReturnsError(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - echo ok
`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before running
	r.runner = func(_ context.Context, _, _ string) (string, int, error) {
		t.Fatal("runner should not be invoked when ctx is already cancelled")
		return "", 0, nil
	}

	out, err := r.Run(ctx, "task-1", "cmd-1", "", nil)
	if err == nil {
		t.Fatalf("expected ctx error to propagate")
	}
	if out.Passed {
		t.Fatalf("expected fail when ctx cancelled")
	}
	if !strings.Contains(out.Reason, "verify_aborted") {
		t.Errorf("reason should report verify_aborted, got %q", out.Reason)
	}
}

func TestRealVerifyRunner_InvalidVerifyYAMLReportsConfigError(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	// `;` is unsupported shell syntax, so Validate rejects this command.
	writeVerifyYAML(t, r, `verify:
  build:
    - "echo a; rm -rf /"
`)

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected fail when verify.yaml is invalid")
	}
	if !strings.Contains(out.Reason, "verify_config_invalid") {
		t.Errorf("reason should report config error, got %q", out.Reason)
	}
}

// TestRealVerifyRunner_AdvisoryOnDirtyFilesOutsideExpectedPaths pins the
// 2026-04-30 e2e regression fix: changes outside expected_paths must be
// advisory (logged, verify continues) rather than a hard failure. The
// previous strict gate routinely false-failed legitimate fixes that
// touched ancillary files (proxy/cmd/osv-ingest/main.go in the
// reproduced case) and forced commands into permanent stuck states.
// Commit-policy still enforces the boundary at integration time.
func TestRealVerifyRunner_AdvisoryOnDirtyFilesOutsideExpectedPaths(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	r := newTestRealRunner(t)
	if out, err := exec.Command("git", "-C", r.projectDir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v output=%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(r.projectDir, "src", "allowed"), 0o755); err != nil {
		t.Fatalf("mkdir allowed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(r.projectDir, "src", "forbidden"), 0o755); err != nil {
		t.Fatalf("mkdir forbidden: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.projectDir, "src", "allowed", "main.go"), []byte("package allowed\n"), 0o600); err != nil {
		t.Fatalf("write allowed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.projectDir, "src", "forbidden", "main.go"), []byte("package forbidden\n"), 0o600); err != nil {
		t.Fatalf("write forbidden: %v", err)
	}
	writeVerifyYAML(t, r, `verify:
  build:
    - true
`)
	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", []string{"src/allowed/"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass — out-of-bound paths are advisory now, got reason=%q", out.Reason)
	}
	if len(rr.seen) != 1 || rr.seen[0] != "true" {
		t.Fatalf("verify command must still execute despite advisory, got %v", rr.seen)
	}
}

// TestRealVerifyRunner_AllowsUntrackedFileInsideNewDirectory pins the
// 2026-04-29 fix: when a task creates a new directory containing a file,
// `git status --porcelain` (default `-unormal`) collapses the directory to
// a single entry like `notes/`, which then false-fails the expected_paths
// matcher because `notes` does not match `notes/note_b.txt`. Without
// `-uall`, every task that introduces a fresh directory hits a 100%
// reproducible verify_expected_paths_violation. This test wires the
// scenario end-to-end: a fresh `notes/note_b.txt` in a virgin git repo
// must be recognised as inside expected_paths=["notes/note_b.txt"].
func TestRealVerifyRunner_AllowsUntrackedFileInsideNewDirectory(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	r := newTestRealRunner(t)
	if out, err := exec.Command("git", "-C", r.projectDir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v output=%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(r.projectDir, "notes"), 0o755); err != nil {
		t.Fatalf("mkdir notes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.projectDir, "notes", "note_b.txt"), []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("write note_b: %v", err)
	}
	writeVerifyYAML(t, r, `verify:
  build:
    - true
`)
	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", []string{"notes/note_b.txt"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass for file inside expected_paths, got %+v", out)
	}
}

// TestRealVerifyRunner_AllowsDependencyManifestChanges pins the 2026-04-29
// dependency-manifest auto-allow rule. Routine package-manager operations
// (npm install -D vitest, pip install ..., cargo add ...) update lockfiles
// and source manifests as a side effect. Before this fix, every Node task
// that pulled in a new dev dependency tripped expected_paths_violation
// because the planner naturally declares only the source files it intends
// the task to touch — not the lockfile that the package manager mutates
// automatically. The auto-allow lets workers add legitimate dependencies
// without forcing a planner repair cycle to widen expected_paths.
func TestRealVerifyRunner_AllowsDependencyManifestChanges(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	r := newTestRealRunner(t)
	if out, err := exec.Command("git", "-C", r.projectDir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v output=%s", err, out)
	}
	// Worker touched the source file the planner declared AND the
	// lockfile that npm install updated as a side effect.
	if err := os.MkdirAll(filepath.Join(r.projectDir, "src"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.projectDir, "src", "main.ts"), []byte("export {};\n"), 0o600); err != nil {
		t.Fatalf("write main.ts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.projectDir, "package.json"), []byte("{\"name\":\"x\"}\n"), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.projectDir, "package-lock.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write package-lock.json: %v", err)
	}

	writeVerifyYAML(t, r, `verify:
  build:
    - true
`)
	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", []string{"src/main.ts"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass — package.json/package-lock.json must auto-allow alongside src/main.ts; got %+v", out)
	}
}

// TestVerifyPathAllowed_DependencyManifestBasenames is a focused unit
// test that exercises the basename-based admission across every package
// manager listed in dependencyManifestBasenames. Nested locations (e.g.
// monorepo workspaces under apps/web/) must be admitted just like
// project-root manifests, because the rule matches on path.Base().
func TestVerifyPathAllowed_DependencyManifestBasenames(t *testing.T) {
	t.Parallel()
	cases := []string{
		"package.json",
		"package-lock.json",
		"yarn.lock",
		"pnpm-lock.yaml",
		"go.mod",
		"go.sum",
		"Cargo.lock",
		"pyproject.toml",
		"uv.lock",
		"poetry.lock",
		"requirements.txt",
		"Gemfile.lock",
		"composer.lock",
		"mix.lock",
		// Nested monorepo workspaces should also be admitted.
		"apps/web/package.json",
		"crates/core/Cargo.lock",
	}
	for _, p := range cases {
		if !verifyPathAllowed(p, nil) {
			t.Errorf("verifyPathAllowed(%q, nil) = false, want true (dependency manifest auto-allow)", p)
		}
	}
}

// TestVerifyPathAllowed_NonManifestNotAllowed pins that the auto-allow
// rule is narrow: a similar-looking file (e.g. a `lock.json` in src/)
// must NOT be admitted just because the basename mentions a lockfile
// keyword. The list is exact-match, not heuristic.
func TestVerifyPathAllowed_NonManifestNotAllowed(t *testing.T) {
	t.Parallel()
	cases := []string{
		"src/lock.json",            // not in the allow list
		"src/my-package-lock.json", // basename mismatch
		"src/Cargo.lock.bak",
	}
	for _, p := range cases {
		if verifyPathAllowed(p, nil) {
			t.Errorf("verifyPathAllowed(%q, nil) = true; want false (not a real manifest)", p)
		}
	}
}

// TestGitChangedFiles_UntrackedFilesInsideNewDirectory directly exercises
// the helper to lock in `-uall` behaviour. Without the flag, git reports
// the directory only; with it, every file is reported individually.
func TestGitChangedFiles_UntrackedFilesInsideNewDirectory(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v output=%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "sub", "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "sub", "b.txt"), []byte("b"), 0o600); err != nil {
		t.Fatalf("write b: %v", err)
	}

	files, err := gitChangedFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("gitChangedFiles: %v", err)
	}
	want := map[string]bool{"pkg/sub/a.txt": false, "pkg/sub/b.txt": false}
	for _, f := range files {
		if _, ok := want[f]; ok {
			want[f] = true
		}
	}
	for f, seen := range want {
		if !seen {
			t.Errorf("expected file %q in changed list, got %v", f, files)
		}
	}
	for _, f := range files {
		if f == "pkg/" || f == "pkg/sub/" {
			t.Errorf("directory entry %q must not appear; -uall should expand it", f)
		}
	}
}

func TestRealVerifyRunner_RealExecHappyPath(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - true
`)

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass for `true`, got %+v", out)
	}
}

func TestRealVerifyRunner_RealExecFailingCommand(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  lint:
    - false
`)

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected fail for `false`")
	}
	if !strings.Contains(out.Reason, "category=lint") {
		t.Errorf("reason should include failing category, got %q", out.Reason)
	}
	if !strings.Contains(out.Reason, "exit=1") {
		t.Errorf("reason should include exit=1, got %q", out.Reason)
	}
}

func TestExecVerifyCommand_SupportsQuotesAndEnvAssignments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out, exitCode, err := execVerifyCommand(context.Background(), dir, `printf "hello world"`)
	if err != nil {
		t.Fatalf("execVerifyCommand quotes: output=%q exit=%d err=%v", out, exitCode, err)
	}
	if exitCode != 0 || out != "hello world" {
		t.Fatalf("quotes exit=%d output=%q, want 0 and hello world", exitCode, out)
	}
	out, exitCode, err = execVerifyCommand(context.Background(), dir, `VERIFY_SAMPLE=value printenv VERIFY_SAMPLE`)
	if err != nil {
		t.Fatalf("execVerifyCommand env: output=%q exit=%d err=%v", out, exitCode, err)
	}
	if exitCode != 0 || strings.TrimSpace(out) != "value" {
		t.Fatalf("env exit=%d output=%q, want 0 and value", exitCode, out)
	}
}

func TestTailBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"shorter_than_n_returns_full", "abc", 10, "abc"},
		{"equal_to_n_returns_full", "abcdef", 6, "abcdef"},
		{"longer_than_n_returns_tail", "abcdef", 3, "def"},
		{"zero_n_returns_empty", "abc", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tailBytes(tt.in, tt.n); got != tt.want {
				t.Errorf("tailBytes(%q,%d)=%q want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

func TestTailBytes_TrimsPartialUTF8Prefix(t *testing.T) {
	t.Parallel()
	// "あいう" is 9 bytes (3×3-byte runes). Cut at 7 bytes — that leaves a
	// continuation byte at the head of the tail. tailBytes must trim it.
	s := "あいう"
	got := tailBytes(s, 7)
	// The result should start at a valid rune boundary — either "いう" (6 bytes)
	// or "う" (3 bytes) depending on how many continuation bytes were trimmed.
	for i, b := range []byte(got) {
		if i == 0 && b&0xC0 == 0x80 {
			t.Errorf("tail begins with continuation byte: %x (got=%q)", b, got)
		}
	}
	if got != "いう" && got != "う" {
		t.Errorf("unexpected tail %q (want \"いう\" or \"う\")", got)
	}
	// Sanity: no surprise — printable, no error.
	_ = got
}

// TestRealVerifyRunner_AdvisoryWhenExpectedPathsCheckFails pins the
// 2026-04-30 advisory contract: when expected_paths is supplied but
// `git status` fails (e.g. running outside a git repository), the
// runner must NOT fail verify — it logs an advisory and proceeds to
// run the configured commands. expected_paths is only meaningful for
// software engineering inside a git repo; orchestrating research /
// documentation tasks outside git must still reach the verify
// commands the operator listed in verify.yaml.
func TestRealVerifyRunner_AdvisoryWhenExpectedPathsCheckFails(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// projectDir is t.TempDir() and intentionally NOT initialised as a git repo.
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - true
`)
	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", []string{"src/"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass — git_status failure must be advisory only, got reason=%q", out.Reason)
	}
	if len(rr.seen) != 1 || rr.seen[0] != "true" {
		t.Fatalf("verify command must still execute despite advisory git status failure, got %v", rr.seen)
	}
}

// 2026-04-30 redesign: the previous EnsembleVerifier perspective tests
// (TestRealVerifyRunner_EnsembleVerifierAddsMissingPerspectiveCommands,
// TestRealVerifyRunner_EnsembleAdvisoryFailureDoesNotFailRun,
// TestRealVerifyRunner_EnsembleCriticalFailureFailsRun) were removed
// alongside the perspective_weights / advisory-vs-critical wiring.
// verify.yaml is now the single source of truth: every listed category
// runs at the critical weight, and non-listed categories do not run.
// See buildVerifyCategories for the simplified merge.
