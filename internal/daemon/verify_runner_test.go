package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestStubVerifyRunner_AlwaysPasses(t *testing.T) {
	r := NewStubVerifyRunner()
	out, err := r.Run(context.Background(), "task-1", "cmd-1", []string{"src/foo.go"})
	if err != nil {
		t.Fatalf("stub runner returned error: %v", err)
	}
	if !out.Passed {
		t.Errorf("stub runner outcome.Passed = false; want true")
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
			got, err := r.Run(context.Background(), "task-1", "cmd-1", nil)
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
