package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestRealRunner constructs a RealVerifyRunner with a discard logger and a
// configurable command runner. maestroDir is created on disk so verify.yaml
// can be loaded (or absent for the Fallback path); projectDir is unused by
// the mock runner but must be non-empty to mirror production wiring.
func newTestRealRunner(t *testing.T) *RealVerifyRunner {
	t.Helper()
	maestroDir := t.TempDir()
	projectDir := t.TempDir()
	r := NewRealVerifyRunner(maestroDir, projectDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return r
}

// writeVerifyYAML writes the given verify.yaml body into r.maestroDir.
func writeVerifyYAML(t *testing.T, r *RealVerifyRunner, body string) {
	t.Helper()
	path := filepath.Join(r.maestroDir, "verify.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write verify.yaml: %v", err)
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
	rr := &recordingRunner{}
	r.runner = rr.run

	out, err := r.Run(context.Background(), "task-1", "cmd-1", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Passed {
		t.Fatalf("expected pass, got %+v", out)
	}
	if len(rr.seen) != 1 || rr.seen[0] != "go vet ./..." {
		t.Errorf("expected single fallback command 'go vet ./...', got %v", rr.seen)
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

	out, err := r.Run(context.Background(), "task-1", "cmd-1", nil)
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

	out, err := r.Run(context.Background(), "task-1", "cmd-1", nil)
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

	out, err := r.Run(context.Background(), "task-1", "cmd-1", nil)
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

	out, err := r.Run(context.Background(), "task-1", "cmd-1", nil)
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

	out, err := r.Run(context.Background(), "task-1", "cmd-1", nil)
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

	out, err := r.Run(ctx, "task-1", "cmd-1", nil)
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
	// `;` is in dangerousChars — Validate() will reject this command.
	writeVerifyYAML(t, r, `verify:
  build:
    - "echo a; rm -rf /"
`)

	out, err := r.Run(context.Background(), "task-1", "cmd-1", nil)
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

func TestRealVerifyRunner_RealExecHappyPath(t *testing.T) {
	t.Parallel()
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, `verify:
  build:
    - true
`)

	out, err := r.Run(context.Background(), "task-1", "cmd-1", nil)
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

	out, err := r.Run(context.Background(), "task-1", "cmd-1", nil)
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
	_ = fmt.Sprintf("%s", got)
}
