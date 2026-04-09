package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestRunQueueWrite_CancelRequestDeprecationWarning verifies that the
// deprecated `queue write --type cancel-request` CLI surface emits a
// migration warning to stderr (H7: cancel route unification).
func TestRunQueueWrite_CancelRequestDeprecationWarning(t *testing.T) {
	var buf bytes.Buffer

	// Use a valid command-id so we hit the deprecation warning emission path
	// before the CLI tries to dial the daemon. The daemon dial will fail
	// (no socket in test env), but the warning is written before that.
	_ = runQueueWrite([]string{"planner", "--type", "cancel-request",
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
	err := runQueueWrite(nil, io.Discard)
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_MissingType(t *testing.T) {
	err := runQueueWrite([]string{"planner"}, io.Discard)
	if err == nil {
		t.Fatal("expected error for missing --type")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_UnknownFlag(t *testing.T) {
	err := runQueueWrite([]string{"planner", "--unknown"}, io.Discard)
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_UnknownType(t *testing.T) {
	err := runQueueWrite([]string{"planner", "--type", "bogus"}, io.Discard)
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_CommandMissingContent(t *testing.T) {
	err := runQueueWrite([]string{"planner", "--type", "command"}, io.Discard)
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
	err := runQueueWrite([]string{"worker1", "--type", "task",
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
	err := runQueueWrite([]string{"planner", "--type", "notification",
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
	err := runQueueWrite([]string{"planner", "--type", "cancel-request",
		"--command-id", "",
	}, io.Discard)
	if err == nil {
		t.Fatal("expected error for empty command-id")
	}
}

func TestRunQueueWrite_ErrorMessageFormat(t *testing.T) {
	// Verify error messages include "maestro queue write:" prefix
	err := runQueueWrite([]string{"planner", "--type", "bogus"}, io.Discard)
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
