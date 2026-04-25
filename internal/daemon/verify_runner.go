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
// The interface is intentionally minimal; future revisions can add structured
// VerifyResult details (per-command output, durations, etc.) without
// rewriting all callers.
type VerifyRunner interface {
	Run(ctx context.Context, taskID, commandID string, expectedPaths []string) (VerifyOutcome, error)
}

// stubVerifyRunner is the §S1-1 minimum-viable Verification Runner. It always
// reports Passed=true and is the runner that ships in this iteration.
//
// REQUIREMENTS.md §S1-1 mandates that the Daemon "auto-supplements and
// executes a Fallback Verify" when verify.yaml is undefined. Until the
// command-execution path lands in a follow-up task, the stub fulfils the
// state-machine half of the requirement (verify_pending → completed) so the
// §2.1 lifecycle progression is observable in TaskStates without tying every
// E2E test to a concrete verify implementation.
type stubVerifyRunner struct{}

// NewStubVerifyRunner returns a VerifyRunner that always passes. Production
// callers should replace it with a real runner once the verify-execution
// machinery is wired in.
func NewStubVerifyRunner() VerifyRunner {
	return stubVerifyRunner{}
}

// Run reports a successful verification. The arguments are ignored.
func (stubVerifyRunner) Run(_ context.Context, _, _ string, _ []string) (VerifyOutcome, error) {
	return VerifyOutcome{Passed: true}, nil
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
func (r fixedVerifyRunner) Run(_ context.Context, _, _ string, _ []string) (VerifyOutcome, error) {
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
