package main

import (
	"errors"
	"testing"
)

func TestRunSetup_NoArgs(t *testing.T) {
	err := runSetup(nil)
	if err == nil {
		t.Fatal("expected error for no args")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunSetup_TooManyArgs(t *testing.T) {
	err := runSetup([]string{"/tmp/proj", "myproj", "extra"})
	if err == nil {
		t.Fatal("expected error for too many args")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunSetup_UnknownFlag(t *testing.T) {
	err := runSetup([]string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunSetup_ValidArgs(t *testing.T) {
	dir := t.TempDir()
	// runSetup with valid dir should not return a CLIError (it may fail in setup.Run but not flag parsing)
	err := runSetup([]string{dir})
	if err != nil {
		var ce *CLIError
		if errors.As(err, &ce) {
			t.Fatalf("unexpected CLIError: %v", ce)
		}
		// Non-CLIError is acceptable (e.g., setup.Run may fail), not a flag parsing issue
	}
}

func TestRunSetup_WithProjectName(t *testing.T) {
	dir := t.TempDir()
	err := runSetup([]string{dir, "myproject"})
	if err != nil {
		var ce *CLIError
		if errors.As(err, &ce) {
			t.Fatalf("unexpected CLIError: %v", ce)
		}
	}
}
