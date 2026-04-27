package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/uds"
)

func TestRunTask_Dispatcher(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing subcommand", nil, "missing subcommand"},
		{"unknown subcommand", []string{"bogus"}, "unknown subcommand: bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCLIApp().runTask(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
			if !strings.Contains(ce.Msg, tt.wantErr) {
				t.Errorf("error %q does not contain %q", ce.Msg, tt.wantErr)
			}
		})
	}
}

func TestRunTaskHeartbeat_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"unknown flag", []string{"--unknown"}, true},
		{"missing task-id", []string{"--worker-id", "w1"}, true},
		{"missing worker-id", []string{"--task-id", "t1"}, true},
		{"unexpected arg", []string{"--task-id", "t1", "--worker-id", "w1", "extra"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newCLIApp().runTaskHeartbeat(tt.args)
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

func TestRunTaskHeartbeat_MissingEpoch(t *testing.T) {
	err := newCLIApp().runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "w1"})
	if err == nil {
		t.Fatal("expected error for missing --epoch")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "--epoch is required") {
		t.Errorf("expected '--epoch is required' in error, got: %s", ce.Msg)
	}
}

func TestRunTaskHeartbeat_EpochZeroIsValid(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(command string, params any) (*uds.Response, error) {
			return successResponse(nil), nil
		},
	})

	err := app.runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "w1", "--epoch", "0"})
	if err != nil {
		t.Fatalf("unexpected error: epoch 0 should be valid: %v", err)
	}
}

func TestRunTaskHeartbeat_InvalidTaskID(t *testing.T) {
	err := newCLIApp().runTaskHeartbeat([]string{"--task-id", "../evil", "--worker-id", "w1", "--epoch", "1"})
	if err == nil {
		t.Fatal("expected error for invalid task-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "invalid --task-id") {
		t.Errorf("expected 'invalid --task-id' in error, got: %s", ce.Msg)
	}
}

func TestRunTaskHeartbeat_InvalidWorkerID(t *testing.T) {
	err := newCLIApp().runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "../evil", "--epoch", "1"})
	if err == nil {
		t.Fatal("expected error for invalid worker-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "invalid --worker-id") {
		t.Errorf("expected 'invalid --worker-id' in error, got: %s", ce.Msg)
	}
}

func TestRunTaskHeartbeat_UDSSuccess(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(command string, params any) (*uds.Response, error) {
			if command != "task_heartbeat" {
				t.Errorf("expected command task_heartbeat, got %s", command)
			}
			return successResponse(nil), nil
		},
	})

	err := app.runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "w1", "--epoch", "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunTaskHeartbeat_UDSMaxRuntimeExceeded(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return errorResponse(uds.ErrCodeMaxRuntimeExceeded, "max runtime exceeded"), nil
		},
	})

	err := app.runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "w1", "--epoch", "1"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	// F-019 step 2: max_runtime_exceeded now uses the dedicated exit code 11.
	if ce.Code != ExitCodeMaxRuntimeExceeded {
		t.Errorf("expected exit code %d (ExitCodeMaxRuntimeExceeded), got %d", ExitCodeMaxRuntimeExceeded, ce.Code)
	}
	if !ce.Silent {
		t.Error("expected silent error for MAX_RUNTIME_EXCEEDED")
	}
}

// TestRunTaskHeartbeat_UDSFencingReject pins F-019 step 2: a generic
// FENCING_REJECT (no structured Details — older daemons) maps to the
// status-mismatch exit code via the legacy-code fallback in
// classifyFencingExitCode. Renamed from `_UDSOtherError` because the prior
// behaviour (exit 1) was the F-019 step 2 target to fix.
func TestRunTaskHeartbeat_UDSFencingReject(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return errorResponse("FENCING_REJECT", "epoch mismatch"), nil
		},
	})

	err := app.runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "w1", "--epoch", "1"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if ce.Code != ExitCodeFencingStatus {
		t.Errorf("expected exit code %d (ExitCodeFencingStatus, F-019 fallback), got %d", ExitCodeFencingStatus, ce.Code)
	}
	if ce.Silent {
		t.Error("FENCING_REJECT must NOT be silent (operator visibility)")
	}
}

func TestRunTaskHeartbeat_UDSGenericInternalError(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return errorResponse(uds.ErrCodeInternal, "boom"), nil
		},
	})

	err := app.runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "w1", "--epoch", "1"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	// Non-fencing UDS errors still fall through to the generic exit code 1
	// path so operators can distinguish them from fencing rejects.
	if ce.Code != 1 {
		t.Errorf("expected exit code 1 for INTERNAL_ERROR, got %d", ce.Code)
	}
}

func TestRunTaskHeartbeat_UDSGenericFailure(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return &uds.Response{Success: false, Error: nil}, nil
		},
	})

	err := app.runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "w1", "--epoch", "1"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "heartbeat failed") {
		t.Errorf("expected 'heartbeat failed' in error, got: %s", ce.Msg)
	}
}

func TestRunTaskHeartbeat_UDSConnError(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return nil, errors.New("connection refused")
		},
	})

	err := app.runTaskHeartbeat([]string{"--task-id", "t1", "--worker-id", "w1", "--epoch", "1"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected connection error, got: %v", err)
	}
}
