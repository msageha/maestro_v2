package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/uds"
)

func TestRunResultWrite_MissingReporter(t *testing.T) {
	err := newCLIApp().runResultWrite(nil)
	if err == nil {
		t.Fatal("expected error for missing reporter")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunResultWrite_MissingRequiredFlags(t *testing.T) {
	err := newCLIApp().runResultWrite([]string{"worker1"})
	if err == nil {
		t.Fatal("expected error for missing required flags")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunResultWrite_UnknownFlag(t *testing.T) {
	err := newCLIApp().runResultWrite([]string{"worker1", "--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunResultWrite_MissingLeaseEpoch(t *testing.T) {
	err := newCLIApp().runResultWrite([]string{"worker1",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "cmd_0000000001_abcdef01",
		"--status", "completed",
	})
	if err == nil {
		t.Fatal("expected error for missing --lease-epoch")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "--lease-epoch") {
		t.Errorf("expected '--lease-epoch' in error, got: %s", ce.Msg)
	}
}

func TestRunResultWrite_LeaseEpochZeroIsValid(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(command string, params any) (*uds.Response, error) {
			return successResponse(map[string]string{"result_id": "res1"}), nil
		},
	})

	err := app.runResultWrite([]string{"worker1",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "cmd_0000000001_abcdef01",
		"--lease-epoch", "0",
		"--status", "completed",
	})
	if err != nil {
		t.Fatalf("unexpected error: lease-epoch 0 should be valid: %v", err)
	}
}

func TestRunResultWrite_UnexpectedArg(t *testing.T) {
	err := newCLIApp().runResultWrite([]string{"worker1", "--task-id", "t1", "--command-id", "c1", "--status", "completed", "extra"})
	if err == nil {
		t.Fatal("expected error for unexpected arg")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunResultWrite_InvalidReporter(t *testing.T) {
	err := newCLIApp().runResultWrite([]string{"../evil",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "cmd_0000000001_abcdef01",
		"--lease-epoch", "1",
		"--status", "completed",
	})
	if err == nil {
		t.Fatal("expected error for invalid reporter")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "invalid reporter") {
		t.Errorf("expected 'invalid reporter' in error, got: %s", ce.Msg)
	}
}

func TestRunResultWrite_InvalidTaskID(t *testing.T) {
	err := newCLIApp().runResultWrite([]string{"worker1",
		"--task-id", "../bad",
		"--command-id", "cmd_0000000001_abcdef01",
		"--lease-epoch", "1",
		"--status", "completed",
	})
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

func TestRunResultWrite_InvalidCommandID(t *testing.T) {
	err := newCLIApp().runResultWrite([]string{"worker1",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "../../bad",
		"--lease-epoch", "1",
		"--status", "completed",
	})
	if err == nil {
		t.Fatal("expected error for invalid command-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "invalid --command-id") {
		t.Errorf("expected 'invalid --command-id' in error, got: %s", ce.Msg)
	}
}

func TestRunResultWrite_SummaryTooLong(t *testing.T) {
	longSummary := strings.Repeat("x", 65537)
	err := newCLIApp().runResultWrite([]string{"worker1",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "cmd_0000000001_abcdef01",
		"--lease-epoch", "1",
		"--status", "completed",
		"--summary", longSummary,
	})
	if err == nil {
		t.Fatal("expected error for oversized summary")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "exceeds maximum size") {
		t.Errorf("expected 'exceeds maximum size' in error, got: %s", ce.Msg)
	}
}

func TestRunResultWrite_ErrorMessageFormat(t *testing.T) {
	// Verify error messages include "maestro result write:" prefix
	err := newCLIApp().runResultWrite([]string{"worker1"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if errors.As(err, &ce) {
		if !strings.HasPrefix(ce.Msg, "maestro result write:") {
			t.Errorf("expected 'maestro result write:' prefix, got: %s", ce.Msg)
		}
	}
}

func TestRunResult_Dispatcher(t *testing.T) {
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
			err := newCLIApp().runResult(tt.args)
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

func TestRunResultWrite_UDSSuccess(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(command string, params any) (*uds.Response, error) {
			if command != "result_write" {
				t.Errorf("expected command result_write, got %s", command)
			}
			return successResponse(map[string]string{"result_id": "res1"}), nil
		},
	})

	err := app.runResultWrite([]string{"worker1",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "cmd_0000000001_abcdef01",
		"--lease-epoch", "1",
		"--status", "completed",
		"--summary", "done",
		"--files-changed", "a.go",
		"--learnings", "something useful",
		"--skill-candidates", "new-skill",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunResultWrite_UDSFencingReject(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return errorResponse("FENCING_REJECT", "epoch mismatch"), nil
		},
	})

	err := app.runResultWrite([]string{"worker1",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "cmd_0000000001_abcdef01",
		"--lease-epoch", "1",
		"--status", "completed",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if ce.Code != 2 {
		t.Errorf("expected exit code 2 for FENCING_REJECT, got %d", ce.Code)
	}
}

func TestRunResultWrite_UDSOtherError(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return errorResponse("NOT_FOUND", "task not found"), nil
		},
	})

	err := app.runResultWrite([]string{"worker1",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "cmd_0000000001_abcdef01",
		"--lease-epoch", "1",
		"--status", "completed",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if ce.Code != 1 {
		t.Errorf("expected exit code 1, got %d", ce.Code)
	}
}

func TestRunResultWrite_UDSConnError(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return nil, errors.New("connection refused")
		},
	})

	err := app.runResultWrite([]string{"worker1",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "cmd_0000000001_abcdef01",
		"--lease-epoch", "1",
		"--status", "completed",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected connection error, got: %v", err)
	}
}

func TestRunResultWrite_PartialChangesAndNoRetrySafe(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(command string, params any) (*uds.Response, error) {
			return successResponse(map[string]string{"result_id": "res1"}), nil
		},
	})

	err := app.runResultWrite([]string{"worker1",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "cmd_0000000001_abcdef01",
		"--lease-epoch", "1",
		"--status", "failed",
		"--partial-changes",
		"--no-retry-safe",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
