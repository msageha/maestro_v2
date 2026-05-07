package daemon

import (
	"context"
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
)

// VerifyOutcome captures the result of a single Verification Runner invocation
// for a task that has reached verify_pending. The reason field is non-empty
// when Passed is false so callers can record the rejection in audit logs.
type VerifyOutcome struct {
	Passed bool
	Reason string
}

// VerifyRunner is the §S1-1 verification entry point. The Daemon invokes it
// each time a task transitions into verify_pending and uses the outcome to
// route the task to either completed (passed) or repair_pending (failed).
//
// workingDir specifies the directory in which verification commands must run.
// For tasks executed in a worker worktree, this is the worker worktree path
// (so verify sees the worker's uncommitted changes). For RunOnIntegration
// tasks, it is the integration worktree. For RunOnMain or worktree-disabled
// modes, it is the project root. Callers MUST resolve workingDir before
// calling Run; an empty value lets RealVerifyRunner fall back to its own
// projectDir, preserving the legacy behaviour for tests that do not depend
// on worktree-scoped verification.
type VerifyRunner interface {
	Run(ctx context.Context, taskID, commandID, workingDir string, expectedPaths []string) (VerifyOutcome, error)
}

// skipVerifyRunner short-circuits verification with Passed=true. Wired
// when `verify.enabled: false` in config.yaml — the supported "no
// machine-checkable verify step" mode for projects that are not running
// software-development workflows (research, documentation, note-taking,
// …). The autonomous LLM Orchestration design treats opt-in to verify as
// the operator's responsibility (`.maestro/verify.yaml` plus
// `verify.enabled: true`); the runner therefore must accept the
// disabled state as a normal configuration, not an emergency gate.
//
// Tests that need a deterministic pass result use NewFixedVerifyRunner
// instead, which carries an explicit "this is a test fixture" intent.
type skipVerifyRunner struct{}

// NewSkipVerifyRunner returns a VerifyRunner that reports pass without
// executing any commands. Used when verify.enabled=false; production
// wiring with verify enabled uses NewRealVerifyRunner.
func NewSkipVerifyRunner() VerifyRunner {
	return skipVerifyRunner{}
}

// Run reports a successful verification. The arguments are ignored.
func (skipVerifyRunner) Run(_ context.Context, _, _, _ string, _ []string) (VerifyOutcome, error) {
	return VerifyOutcome{Passed: true, Reason: "verify_skipped: verify.enabled=false"}, nil
}

// unconfiguredVerifyRunner reports a §S1-1 violation reason whenever it is
// invoked. The result-write handler falls back to this runner when no
// VerifyRunner has been wired, so that a config bug or test wiring miss
// surfaces as a verify failure (→ repair_pending) rather than silently
// passing every task. Operators see "verify_runner_not_configured" in the
// audit log and can fix the daemon wiring.
type unconfiguredVerifyRunner struct{}

func newUnconfiguredVerifyRunner() VerifyRunner { return unconfiguredVerifyRunner{} }

func (unconfiguredVerifyRunner) Run(_ context.Context, _, _, _ string, _ []string) (VerifyOutcome, error) {
	return VerifyOutcome{Passed: false, Reason: "verify_runner_not_configured"}, nil
}

// fixedVerifyRunner returns a deterministic outcome for every Run call.
// Tests that need to exercise the repair_pending path use this runner; it is
// not exposed through NewStubVerifyRunner because production callers should
// not be able to short-circuit verification with a fixed-fail config.
type fixedVerifyRunner struct {
	outcome VerifyOutcome
	err     error
}

// NewFixedVerifyRunner returns a VerifyRunner whose Run always reports the
// supplied outcome and error. Used by tests to drive the verify_pending →
// repair_pending or verify_pending → completed branches deterministically.
func NewFixedVerifyRunner(outcome VerifyOutcome, err error) VerifyRunner {
	return fixedVerifyRunner{outcome: outcome, err: err}
}

// Run returns the configured outcome.
func (r fixedVerifyRunner) Run(_ context.Context, _, _, _ string, _ []string) (VerifyOutcome, error) {
	return r.outcome, r.err
}

// classifyVerifyOutcome maps a VerifyOutcome to the §2.1 follow-up state for
// a task currently in verify_pending. err is treated as a verification
// failure for routing purposes; the caller is responsible for emitting any
// audit log line that distinguishes "fail" from "verify-runner crash".
func classifyVerifyOutcome(outcome VerifyOutcome, err error) (next model.Status, reason string) {
	if err != nil {
		// Treat runner failure as a verify failure so the §2.1 pipeline does
		// not stall in verify_pending. The reason string captures both so the
		// post-mortem is unambiguous.
		return model.StatusRepairPending, fmt.Sprintf("verify_runner_error: %v", err)
	}
	if outcome.Passed {
		return model.StatusCompleted, ""
	}
	reason = outcome.Reason
	if reason == "" {
		reason = "verify_failed"
	}
	return model.StatusRepairPending, reason
}
