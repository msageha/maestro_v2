package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestSkipVerifyRunner_PassesWithSkipReason(t *testing.T) {
	r := NewSkipVerifyRunner()
	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", []string{"src/foo.go"})
	if err != nil {
		t.Fatalf("skip runner returned error: %v", err)
	}
	if !out.Passed {
		t.Errorf("skip runner outcome.Passed = false; want true")
	}
	// Reason carries an audit string so the verify.enabled=false rollback
	// is visible in the result-write log instead of looking like a real pass.
	if out.Reason == "" {
		t.Errorf("skip runner reason is empty; want skip audit string")
	}
}

func TestUnconfiguredVerifyRunner_FailsClosed(t *testing.T) {
	r := newUnconfiguredVerifyRunner()
	out, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
	if err != nil {
		t.Fatalf("unconfigured runner returned error: %v", err)
	}
	if out.Passed {
		t.Error("unconfigured runner reported pass; want fail-closed")
	}
	if out.Reason != "verify_runner_not_configured" {
		t.Errorf("reason = %q, want verify_runner_not_configured (audit signal)", out.Reason)
	}
}

func TestFixedVerifyRunner_ReturnsConfiguredOutcome(t *testing.T) {
	tests := []struct {
		name    string
		outcome VerifyOutcome
		err     error
	}{
		{"pass_no_reason", VerifyOutcome{Passed: true}, nil},
		{"fail_with_reason", VerifyOutcome{Passed: false, Reason: "lint_failed"}, nil},
		{"runner_error", VerifyOutcome{}, errors.New("boom")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewFixedVerifyRunner(tt.outcome, tt.err)
			got, err := r.Run(context.Background(), "task-1", "cmd-1", "", nil)
			if err != tt.err { //nolint:errorlint // exact identity is intended for this fake
				t.Errorf("err = %v, want %v", err, tt.err)
			}
			if got != tt.outcome {
				t.Errorf("outcome = %+v, want %+v", got, tt.outcome)
			}
		})
	}
}

func TestClassifyVerifyOutcome(t *testing.T) {
	tests := []struct {
		name       string
		outcome    VerifyOutcome
		err        error
		wantStatus model.Status
		wantReason string
	}{
		{
			name:       "pass",
			outcome:    VerifyOutcome{Passed: true},
			wantStatus: model.StatusCompleted,
			wantReason: "",
		},
		{
			// The skip runner reports why it passed without running anything
			// ("verify_skipped: verify.enabled=false"); the reason must
			// survive classification so verify_outcome_applied logs are
			// self-explanatory instead of showing reason="".
			name:       "pass_with_reason_propagated",
			outcome:    VerifyOutcome{Passed: true, Reason: "verify_skipped: verify.enabled=false"},
			wantStatus: model.StatusCompleted,
			wantReason: "verify_skipped: verify.enabled=false",
		},
		{
			name:       "fail_with_reason",
			outcome:    VerifyOutcome{Passed: false, Reason: "test_failed"},
			wantStatus: model.StatusRepairPending,
			wantReason: "test_failed",
		},
		{
			name:       "fail_default_reason",
			outcome:    VerifyOutcome{Passed: false},
			wantStatus: model.StatusRepairPending,
			wantReason: "verify_failed",
		},
		{
			name:       "runner_error_routes_to_repair",
			outcome:    VerifyOutcome{},
			err:        errors.New("exec failed"),
			wantStatus: model.StatusRepairPending,
			wantReason: "verify_runner_error: exec failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStatus, gotReason := classifyVerifyOutcome(tt.outcome, tt.err)
			if gotStatus != tt.wantStatus {
				t.Errorf("status = %s, want %s", gotStatus, tt.wantStatus)
			}
			if gotReason != tt.wantReason {
				t.Errorf("reason = %q, want %q", gotReason, tt.wantReason)
			}
		})
	}
}
