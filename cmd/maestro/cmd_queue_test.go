package main

import (
	"errors"
	"strings"
	"testing"
)

func TestRunQueue_NoSubcommand(t *testing.T) {
	err := runQueue(nil)
	if err == nil {
		t.Fatal("expected error for missing subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueue_UnknownSubcommand(t *testing.T) {
	err := runQueue([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_MissingTarget(t *testing.T) {
	err := runQueueWrite(nil)
	if err == nil {
		t.Fatal("expected error for missing target")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_MissingType(t *testing.T) {
	err := runQueueWrite([]string{"planner"})
	if err == nil {
		t.Fatal("expected error for missing --type")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_UnknownFlag(t *testing.T) {
	err := runQueueWrite([]string{"planner", "--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_UnknownType(t *testing.T) {
	err := runQueueWrite([]string{"planner", "--type", "bogus"})
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_CommandMissingContent(t *testing.T) {
	err := runQueueWrite([]string{"planner", "--type", "command"})
	if err == nil {
		t.Fatal("expected error for missing --content")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunQueueWrite_TaskInvalidCommandID(t *testing.T) {
	// Command ID with path traversal characters should be rejected
	err := runQueueWrite([]string{"worker1", "--type", "task",
		"--command-id", "../evil",
		"--content", "test",
		"--purpose", "test",
		"--acceptance-criteria", "test",
		"--bloom-level", "3",
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

func TestRunQueueWrite_TaskBloomLevelOutOfRange(t *testing.T) {
	tests := []struct {
		name  string
		level string
	}{
		{"too high", "7"},
		{"negative", "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runQueueWrite([]string{"worker1", "--type", "task",
				"--command-id", "cmd_0000000001_abcdef01",
				"--content", "test",
				"--purpose", "test",
				"--acceptance-criteria", "test",
				"--bloom-level", tt.level,
			})
			if err == nil {
				t.Fatal("expected error for bloom-level out of range")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
			if !strings.Contains(ce.Msg, "--bloom-level must be between 1 and 6") {
				t.Errorf("expected bloom-level range error, got: %s", ce.Msg)
			}
		})
	}
}

func TestRunQueueWrite_NotificationInvalidCommandID(t *testing.T) {
	err := runQueueWrite([]string{"planner", "--type", "notification",
		"--command-id", "../../bad",
		"--content", "test",
		"--source-result-id", "res1",
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

func TestRunQueueWrite_CancelRequestInvalidCommandID(t *testing.T) {
	err := runQueueWrite([]string{"planner", "--type", "cancel-request",
		"--command-id", "",
	})
	if err == nil {
		t.Fatal("expected error for empty command-id")
	}
}

func TestRunQueueWrite_ErrorMessageFormat(t *testing.T) {
	// Verify error messages include "maestro queue write:" prefix
	err := runQueueWrite([]string{"planner", "--type", "bogus"})
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
