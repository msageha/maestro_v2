package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

func TestRunAgent_Dispatch(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no subcommand", nil, "missing subcommand"},
		{"unknown subcommand", []string{"bogus"}, "unknown subcommand"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runAgent(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
		})
	}
}

func TestRunAgentLaunch_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown flag", []string{"--unknown"}, "flag provided but not defined"},
		{"unexpected arg", []string{"extra"}, "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runAgentLaunch(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
		})
	}
}

// TestMapAgentExecError_SubmitConfirmUncertain pins the contract reported
// by the 3-cycle E2E run: when the deliverer surfaces
// ErrSubmitConfirmUncertain, the CLI must NOT collapse it into either a
// generic error (exit 1) or a retryable transport error (exit 2). It must
// route to the dedicated ExitCodeSubmitUncertain so that scripts can
// branch on "delivery likely succeeded but unverified" without mistaking
// it for a hard failure that should be re-run.
func TestMapAgentExecError_SubmitConfirmUncertain(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("confirm submitted: %w", agent.ErrSubmitConfirmUncertain)
	err := mapAgentExecError(model.ExecResult{Error: wrapped, Retryable: false}, "orchestrator")
	if err == nil {
		t.Fatal("expected CLIError, got nil")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CLIError, got %T: %v", err, err)
	}
	if ce.Code != ExitCodeSubmitUncertain {
		t.Errorf("Code = %d, want %d (ExitCodeSubmitUncertain)", ce.Code, ExitCodeSubmitUncertain)
	}
	if !strings.Contains(ce.Msg, "orchestrator") {
		t.Errorf("Msg should mention the agent ID; got %q", ce.Msg)
	}
	if !strings.Contains(ce.Msg, "duplicate submit") {
		t.Errorf("Msg should warn about duplicate submit risk; got %q", ce.Msg)
	}
}

// TestMapAgentExecError_Retryable ensures retryable transport errors
// continue to map to ExitCodeRetryable (exit 2), preserving the behavior
// the daemon-side dispatcher and shell wrappers depend on.
func TestMapAgentExecError_Retryable(t *testing.T) {
	t.Parallel()
	err := mapAgentExecError(model.ExecResult{
		Error:     fmt.Errorf("ensure claude running: pane is shell"),
		Retryable: true,
	}, "worker1")
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CLIError, got %T: %v", err, err)
	}
	if ce.Code != ExitCodeRetryable {
		t.Errorf("Code = %d, want %d (ExitCodeRetryable)", ce.Code, ExitCodeRetryable)
	}
}

// TestMapAgentExecError_HardFailure pins the default branch: a non-retryable
// error that is NOT ErrSubmitConfirmUncertain must surface as a generic
// fmt.Errorf — the existing exit code 1 fallback in main.go.
func TestMapAgentExecError_HardFailure(t *testing.T) {
	t.Parallel()
	err := mapAgentExecError(model.ExecResult{
		Error:     fmt.Errorf("unknown exec mode: bogus"),
		Retryable: false,
	}, "worker1")
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if errors.As(err, &ce) {
		t.Fatalf("expected non-CLIError (generic exit code 1 path), got *CLIError code=%d", ce.Code)
	}
	if !strings.Contains(err.Error(), "unknown exec mode") {
		t.Errorf("error should preserve the original message; got %q", err.Error())
	}
}

// TestMapAgentExecError_Nil ensures success cases short-circuit cleanly.
func TestMapAgentExecError_Nil(t *testing.T) {
	t.Parallel()
	if err := mapAgentExecError(model.ExecResult{Success: true}, "worker1"); err != nil {
		t.Fatalf("expected nil for successful result, got %v", err)
	}
}

func TestRunAgentExec_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"missing agent-id", []string{}, true},
		{"unknown flag", []string{"--unknown"}, true},
		{"unexpected arg", []string{"--agent-id", "a1", "extra"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runAgentExec(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				var ce *CLIError
				if !errors.As(err, &ce) {
					t.Fatalf("expected CLIError, got %T: %v", err, err)
				}
			}
		})
	}
}
