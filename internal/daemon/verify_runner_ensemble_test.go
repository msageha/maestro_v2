package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/verification"
)

// newEnsembleVerifier builds a verification.Verifier with the given
// per-category weights and retry budget, mirroring the PhaseCManager C-3
// production wiring.
func newEnsembleVerifier(t *testing.T, weights map[string]float64, maxAutoRetries int) *verification.Verifier {
	t.Helper()
	v := verification.NewVerifier()
	v.SetMaxAutoRetries(maxAutoRetries)
	perspectives := make([]verification.Perspective, 0, len(weights))
	for name, w := range weights {
		perspectives = append(perspectives, verification.Perspective{Name: name, Weight: w})
	}
	if err := v.SetPerspectives(perspectives); err != nil {
		t.Fatalf("SetPerspectives: %v", err)
	}
	return v
}

// TestRealVerifyRunner_PerspectiveWeightDemotesCategoryToAdvisory pins the
// stage-4 wiring: a category whose perspective weight is < 1.0 no longer
// fails the run — its failure is recorded as advisory and later categories
// still execute.
func TestRealVerifyRunner_PerspectiveWeightDemotesCategoryToAdvisory(t *testing.T) {
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, "verify:\n  security:\n    - audit-cmd\n  test:\n    - test-cmd\n")

	rr := &recordingRunner{results: map[string]struct {
		output   string
		exitCode int
		err      error
	}{
		"audit-cmd": {output: "vuln found", exitCode: 1},
	}}
	r.runner = rr.run
	r.SetEnsembleVerifier(newEnsembleVerifier(t, map[string]float64{"security": 0.5}, 0))

	outcome, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !outcome.Passed {
		t.Fatalf("advisory-weight security failure must not fail the run, got %+v", outcome)
	}
	joined := strings.Join(rr.seen, ",")
	if !strings.Contains(joined, "test-cmd") {
		t.Fatalf("later categories must still run after an advisory failure, seen=%v", rr.seen)
	}
}

// TestRealVerifyRunner_CriticalCategoryStillFailsFast pins the default: a
// category with no perspective (or weight >= 1.0) keeps failing the run.
func TestRealVerifyRunner_CriticalCategoryStillFailsFast(t *testing.T) {
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, "verify:\n  build:\n    - build-cmd\n  test:\n    - test-cmd\n")

	rr := &recordingRunner{results: map[string]struct {
		output   string
		exitCode int
		err      error
	}{
		"build-cmd": {output: "compile error", exitCode: 2},
	}}
	r.runner = rr.run
	r.SetEnsembleVerifier(newEnsembleVerifier(t, map[string]float64{"security": 0.5}, 0))

	outcome, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Passed {
		t.Fatal("critical build failure must fail the run")
	}
	for _, cmd := range rr.seen {
		if cmd == "test-cmd" {
			t.Fatal("fail-fast must not execute later categories after a critical failure")
		}
	}
}

// flakyRunner fails the first invocation of each command and passes
// subsequent ones — the transient-flake shape the auto-retry absorbs.
type flakyRunner struct {
	mu    sync.Mutex
	seen  map[string]int
	calls []string
}

func (fr *flakyRunner) run(_ context.Context, _ string, cmd string) (string, int, error) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.seen == nil {
		fr.seen = make(map[string]int)
	}
	fr.seen[cmd]++
	fr.calls = append(fr.calls, cmd)
	if fr.seen[cmd] == 1 {
		return "transient failure", 1, nil
	}
	return "", 0, nil
}

// TestRealVerifyRunner_AutoRetryAbsorbsNovelFlake pins ShouldRetry wiring: a
// command that fails once with a novel fingerprint is re-executed within the
// MaxAutoRetries budget and the run passes when the retry succeeds.
func TestRealVerifyRunner_AutoRetryAbsorbsNovelFlake(t *testing.T) {
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, "verify:\n  test:\n    - flaky-test\n")

	fr := &flakyRunner{}
	r.runner = fr.run
	r.SetEnsembleVerifier(newEnsembleVerifier(t, nil, 1))

	outcome, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !outcome.Passed {
		t.Fatalf("flake absorbed by auto-retry must pass, got %+v", outcome)
	}
	if fr.seen["flaky-test"] != 2 {
		t.Fatalf("flaky command executions = %d, want 2 (original + one retry)", fr.seen["flaky-test"])
	}
}

// TestRealVerifyRunner_AutoRetryBudgetZero_NoRetry pins that the budget
// gates the retry: with max_auto_retries=0 the flake fails the run.
func TestRealVerifyRunner_AutoRetryBudgetZero_NoRetry(t *testing.T) {
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, "verify:\n  test:\n    - flaky-test\n")

	fr := &flakyRunner{}
	r.runner = fr.run
	r.SetEnsembleVerifier(newEnsembleVerifier(t, nil, 0))

	outcome, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Passed {
		t.Fatal("with a zero retry budget the flake must fail the run")
	}
	if fr.seen["flaky-test"] != 1 {
		t.Fatalf("flaky command executions = %d, want 1 (no retry)", fr.seen["flaky-test"])
	}
}

// TestRealVerifyRunner_NoEnsemble_LegacyBehaviour pins that a nil verifier
// preserves the pre-stage-4 semantics: every category critical, no retries.
func TestRealVerifyRunner_NoEnsemble_LegacyBehaviour(t *testing.T) {
	r := newTestRealRunner(t)
	writeVerifyYAML(t, r, "verify:\n  security:\n    - audit-cmd\n")

	rr := &recordingRunner{results: map[string]struct {
		output   string
		exitCode int
		err      error
	}{
		"audit-cmd": {output: "vuln found", exitCode: 1},
	}}
	r.runner = rr.run

	outcome, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.Passed {
		t.Fatal("without an ensemble verifier every category must stay critical")
	}
}
