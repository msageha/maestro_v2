package main

import (
	"errors"
	"strings"
	"testing"
)

func TestRunResult_NoSubcommand(t *testing.T) {
	err := runResult(nil)
	if err == nil {
		t.Fatal("expected error for missing subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunResult_UnknownSubcommand(t *testing.T) {
	err := runResult([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunResultWrite_MissingReporter(t *testing.T) {
	err := runResultWrite(nil)
	if err == nil {
		t.Fatal("expected error for missing reporter")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunResultWrite_MissingRequiredFlags(t *testing.T) {
	err := runResultWrite([]string{"worker1"})
	if err == nil {
		t.Fatal("expected error for missing required flags")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunResultWrite_UnknownFlag(t *testing.T) {
	err := runResultWrite([]string{"worker1", "--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunResultWrite_UnexpectedArg(t *testing.T) {
	err := runResultWrite([]string{"worker1", "--task-id", "t1", "--command-id", "c1", "--status", "completed", "extra"})
	if err == nil {
		t.Fatal("expected error for unexpected arg")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunResultWrite_InvalidReporter(t *testing.T) {
	err := runResultWrite([]string{"../evil",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "cmd_0000000001_abcdef01",
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
	err := runResultWrite([]string{"worker1",
		"--task-id", "../bad",
		"--command-id", "cmd_0000000001_abcdef01",
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
	err := runResultWrite([]string{"worker1",
		"--task-id", "task_0000000001_abcdef01",
		"--command-id", "../../bad",
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

func TestRunResultWrite_ErrorMessageFormat(t *testing.T) {
	// Verify error messages include "maestro result write:" prefix
	err := runResultWrite([]string{"worker1"})
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
