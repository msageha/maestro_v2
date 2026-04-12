package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/uds"
)

// TestRunQueueWrite_CancelRequestDeprecationWarning verifies that the
// deprecated `queue write --type cancel-request` CLI surface emits a
// migration warning to stderr (H7: cancel route unification).
func TestRunQueueWrite_CancelRequestDeprecationWarning(t *testing.T) {
	var buf bytes.Buffer

	// Use a valid command-id so we hit the deprecation warning emission path
	// before the CLI tries to dial the daemon. The daemon dial will fail
	// (no socket in test env), but the warning is written before that.
	_ = newCLIApp().runQueueWrite([]string{"planner", "--type", "cancel-request",
		"--command-id", "cmd_0000000001_abcdef01",
		"--reason", "test",
	}, &buf)

	got := buf.String()
	if !strings.Contains(got, "deprecated") {
		t.Errorf("expected deprecation warning, got: %q", got)
	}
	if !strings.Contains(got, "plan request-cancel") {
		t.Errorf("expected migration hint to plan request-cancel, got: %q", got)
	}
}

func TestRunQueueWrite_MissingTarget(t *testing.T) {
	err := newCLIApp().runQueueWrite(nil, io.Discard)
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_MissingType(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"planner"}, io.Discard)
	if err == nil {
		t.Fatal("expected error for missing --type")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_UnknownFlag(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"planner", "--unknown"}, io.Discard)
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_UnknownType(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "bogus"}, io.Discard)
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_CommandMissingContent(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "command"}, io.Discard)
	if err == nil {
		t.Fatal("expected error for missing --content")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_TaskTypeRejectedFromCLI(t *testing.T) {
	// Task creation is the Planner's exclusive responsibility (audit C3).
	// `maestro queue write --type task` must be rejected at the CLI surface
	// to prevent Planner-bypass task injection.
	err := newCLIApp().runQueueWrite([]string{"worker1", "--type", "task",
		"--command-id", "cmd_0000000001_abcdef01",
		"--content", "test",
		"--purpose", "test",
		"--acceptance-criteria", "test",
		"--bloom-level", "3",
	}, io.Discard)
	if err == nil {
		t.Fatal("expected --type task to be rejected from CLI")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "not supported via CLI") {
		t.Errorf("expected 'not supported via CLI' in error, got: %s", ce.Msg)
	}
}

func TestRunQueueWrite_NotificationInvalidCommandID(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "notification",
		"--command-id", "../../bad",
		"--content", "test",
		"--source-result-id", "res1",
	}, io.Discard)
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

func TestRunQueueWrite_CancelRequestInvalidCommandID(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "cancel-request",
		"--command-id", "",
	}, io.Discard)
	if err == nil {
		t.Fatal("expected error for empty command-id")
	}
}

func TestRunQueueWrite_ErrorMessageFormat(t *testing.T) {
	// Verify error messages include "maestro queue write:" prefix
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "bogus"}, io.Discard)
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if errors.As(err, &ce) {
		if !strings.HasPrefix(ce.Msg, "maestro queue write:") {
			t.Errorf("expected 'maestro queue write:' prefix, got: %s", ce.Msg)
		}
	}
}

func TestRunQueue_Dispatcher(t *testing.T) {
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
			err := newCLIApp().runQueue(tt.args)
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

func TestSendQueueWrite_UDSSuccess(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(command string, params any) (*uds.Response, error) {
			if command != "queue_write" {
				t.Errorf("expected command queue_write, got %s", command)
			}
			return successResponse(map[string]string{"id": "cmd_0000000001_abcdef01"}), nil
		},
	})

	err := app.sendQueueWrite(map[string]any{"target": "planner", "type": "command", "content": "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendQueueWrite_UDSBackpressure(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return errorResponse("BACKPRESSURE", "server at capacity"), nil
		},
	})

	err := app.sendQueueWrite(map[string]any{"target": "planner", "type": "command", "content": "test"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if ce.Code != 2 {
		t.Errorf("expected exit code 2, got %d", ce.Code)
	}
}

func TestSendQueueWrite_UDSOtherError(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return errorResponse("VALIDATION_ERROR", "bad params"), nil
		},
	})

	err := app.sendQueueWrite(map[string]any{"target": "planner", "type": "command", "content": "test"})
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

func TestSendQueueWrite_UDSConnError(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return nil, errors.New("connection refused")
		},
	})

	err := app.sendQueueWrite(map[string]any{"target": "planner", "type": "command", "content": "test"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("expected connection error, got: %v", err)
	}
}

func TestSendQueueWrite_UDSResponseWithCommandID(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return successResponse(map[string]string{"command_id": "cmd_0000000001_abcdef01"}), nil
		},
	})

	err := app.sendQueueWrite(map[string]any{"target": "planner", "type": "command", "content": "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendQueueWrite_UDSResponseNoID(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return successResponse(map[string]string{"other": "value"}), nil
		},
	})

	// Should fail when both id and command_id are missing from response
	err := app.sendQueueWrite(map[string]any{"target": "planner", "type": "command", "content": "test"})
	if err == nil {
		t.Fatal("expected error when response missing both id and command_id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "missing both id and command_id") {
		t.Errorf("expected 'missing both id and command_id' in error, got: %s", ce.Msg)
	}
}

func TestSendQueueWrite_UDSNilErrorDetail(t *testing.T) {
	withMaestroDir(t)
	app := newTestApp(&mockUDSClient{
		sendCommandFunc: func(string, any) (*uds.Response, error) {
			return &uds.Response{Success: false, Error: nil}, nil
		},
	})

	err := app.sendQueueWrite(map[string]any{"target": "planner", "type": "command", "content": "test"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "unknown error") {
		t.Errorf("expected 'unknown error' in error, got: %s", ce.Msg)
	}
}

func TestRunQueueWrite_NotificationMissingFields(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "notification"}, io.Discard)
	if err == nil {
		t.Fatal("expected error for missing notification fields")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_CommandContentTooLong(t *testing.T) {
	longContent := strings.Repeat("x", 65537)
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "command", "--content", longContent}, io.Discard)
	if err == nil {
		t.Fatal("expected error for oversized content")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "exceeds maximum size") {
		t.Errorf("expected 'exceeds maximum size' in error, got: %s", ce.Msg)
	}
}

func TestRunQueueWrite_NotificationContentTooLong(t *testing.T) {
	longContent := strings.Repeat("x", 65537)
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "notification",
		"--command-id", "cmd_0000000001_abcdef01",
		"--content", longContent,
		"--source-result-id", "res1",
	}, io.Discard)
	if err == nil {
		t.Fatal("expected error for oversized content")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "exceeds maximum size") {
		t.Errorf("expected 'exceeds maximum size' in error, got: %s", ce.Msg)
	}
}

func TestRunQueueWrite_NotificationInvalidSourceResultID(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "notification",
		"--command-id", "cmd_0000000001_abcdef01",
		"--content", "test",
		"--source-result-id", "../../bad",
	}, io.Discard)
	if err == nil {
		t.Fatal("expected error for invalid source-result-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "invalid --source-result-id") {
		t.Errorf("expected 'invalid --source-result-id' in error, got: %s", ce.Msg)
	}
}

func TestRunQueueWrite_UnexpectedArg(t *testing.T) {
	err := newCLIApp().runQueueWrite([]string{"planner", "--type", "command", "--content", "test", "extra"}, io.Discard)
	if err == nil {
		t.Fatal("expected error for unexpected arg")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}
