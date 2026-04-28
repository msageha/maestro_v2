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

	"github.com/msageha/maestro_v2/internal/daemon/verification"
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

func TestRealVerifyRunner_FallbackUsesDefaultGoVet(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	// DefaultVerifyConfigForProject only returns the Go fallback when go.mod
	// exists at the project root. Without this stub the fallback would now be
	// empty (intentional: avoid running `go vet` against non-Go repos).
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
	// DefaultVerifyConfigForProject now populates Build + Security + Performance
	// for Go projects so extended_verification.security_check / performance_bench
	// have language-appropriate tools to run by default. The Go fallback must
	// still include `go vet ./...` as Build; Security/Performance are advisory
	// extras that may or may not be wired by the operator's verify.yaml.
	wantCmds := map[string]bool{
		"go vet ./...":           false,
		"gosec ./...":            false,
		"go test -bench=. ./...": false,
	}
	for _, c := range rr.seen {
		if _, ok := wantCmds[c]; ok {
			wantCmds[c] = true
		}
	}
	if !wantCmds["go vet ./..."] {
		t.Errorf("Go fallback must execute `go vet ./...`; saw=%v", rr.seen)
	}
	for cmd, executed := range wantCmds {
		if !executed {
			t.Errorf("expected Go fallback to include %q, got %v", cmd, rr.seen)
		}
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

func TestRealVerifyRunner_RejectsDirtyFilesOutsideExpectedPaths(t *testing.T) {
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
	if out.Passed {
		t.Fatalf("expected expected_paths violation, got pass")
	}
	if !strings.Contains(out.Reason, "verify_expected_paths_violation") {
		t.Fatalf("reason = %q, want verify_expected_paths_violation", out.Reason)
	}
	if len(rr.seen) != 0 {
		t.Fatalf("verify commands should not run after expected_paths violation, got %v", rr.seen)
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

// TestRealVerifyRunner_RejectsExpectedPathsCheckOutsideGitRepo asserts that
// when a non-empty expected_paths is supplied but `git status` fails (e.g.
// the working directory is not a git repository), the verify outcome reports
// the failure as expected_paths-derived rather than a generic verify failure.
// F-008: ensure operators can disambiguate worktree-resolution bugs from
// command failures.
func TestRealVerifyRunner_RejectsExpectedPathsCheckOutsideGitRepo(t *testing.T) {
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
	if out.Passed {
		t.Fatalf("expected fail when expected_paths check runs outside a git repo, got pass")
	}
	if !strings.Contains(out.Reason, "verify_expected_paths_check_failed") {
		t.Fatalf("reason = %q, want it to mention verify_expected_paths_check_failed (F-008)", out.Reason)
	}
	if len(rr.seen) != 0 {
		t.Fatalf("verify commands should not run after expected_paths check failure, got %v", rr.seen)
	}
}

// TestRealVerifyRunner_EnsembleVerifierAddsMissingPerspectiveCommands pins
// the Phase C-3 wiring: when extended_verification configures perspectives
// (security / performance) and the verify snapshot does not list those
// categories, the runner must execute the perspectives' commands so the
// security gate actually runs. Before the wiring fix, EnsembleVerifier
// was constructed in PhaseCManager but never consulted; operators saw
// "ensemble verifier security perspective enabled" in the startup log
// while no security command was ever invoked.
func TestRealVerifyRunner_EnsembleVerifierAddsMissingPerspectiveCommands(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	// Snapshot only configures Build — Security/Performance are absent so
	// the EnsembleVerifier must supply them.
	writeVerifyYAML(t, r, `verify:
  build:
    - echo build1
`)

	v := verification.NewVerifier()
	if err := v.SetPerspectives([]verification.Perspective{
		{Name: "build", Commands: []string{"go build ./..."}, Weight: 1.0},
		{Name: "security", Commands: []string{"echo sec1"}, Weight: 1.0},
		{Name: "performance", Commands: []string{"echo perf1"}, Weight: 1.0},
	}); err != nil {
		t.Fatalf("SetPerspectives: %v", err)
	}
	r.SetEnsembleVerifier(v)

	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass, got %+v", out)
	}

	wantContain := []string{"echo build1", "echo sec1", "echo perf1"}
	for _, want := range wantContain {
		found := false
		for _, c := range rr.seen {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected runner to invoke %q (perspective should augment missing snapshot category); seen=%v", want, rr.seen)
		}
	}
}

// TestRealVerifyRunner_EnsembleAdvisoryFailureDoesNotFailRun pins the
// per-perspective weight semantics: a non-critical perspective (weight
// < 1.0) that fails must NOT abort the run. The legacy fail-fast path
// short-circuited on the first non-zero exit regardless of perspective
// weight, so an "advisory" security finding immediately failed verify
// and routed the task to repair_pending — defeating the point of weight
// configuration.
func TestRealVerifyRunner_EnsembleAdvisoryFailureDoesNotFailRun(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - echo b1
`)

	v := verification.NewVerifier()
	if err := v.SetPerspectives([]verification.Perspective{
		{Name: "build", Commands: []string{"go build ./..."}, Weight: 1.0},
		{Name: "security", Commands: []string{"echo sec_advisory"}, Weight: 0.5},
	}); err != nil {
		t.Fatalf("SetPerspectives: %v", err)
	}
	r.SetEnsembleVerifier(v)

	rr := &recordingRunner{
		results: map[string]struct {
			output   string
			exitCode int
			err      error
		}{
			"echo sec_advisory": {output: "advisory finding", exitCode: 1},
		},
	}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass — advisory perspective failure must not fail the run, got %+v", out)
	}

	// Both commands must have run; advisory failure does not short-circuit.
	if len(rr.seen) != 2 {
		t.Errorf("expected both build and advisory security commands to run, got %v", rr.seen)
	}
}

// TestRealVerifyRunner_EnsembleCriticalFailureFailsRun mirrors the
// advisory test above but with a critical perspective (weight >= 1.0):
// a failure of a critical perspective must fail-fast and route the task
// to repair_pending.
func TestRealVerifyRunner_EnsembleCriticalFailureFailsRun(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - echo b1
`)

	v := verification.NewVerifier()
	if err := v.SetPerspectives([]verification.Perspective{
		{Name: "build", Commands: []string{"go build ./..."}, Weight: 1.0},
		{Name: "security", Commands: []string{"echo sec_critical"}, Weight: 1.0},
	}); err != nil {
		t.Fatalf("SetPerspectives: %v", err)
	}
	r.SetEnsembleVerifier(v)

	rr := &recordingRunner{
		results: map[string]struct {
			output   string
			exitCode int
			err      error
		}{
			"echo sec_critical": {output: "critical finding", exitCode: 2},
		},
	}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Passed {
		t.Fatalf("expected fail — critical perspective failure must fail the run, got %+v", out)
	}
	if !strings.Contains(out.Reason, "category=security") {
		t.Errorf("expected reason to include category=security, got %q", out.Reason)
	}
}
