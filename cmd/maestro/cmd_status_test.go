package main

import (
	"errors"
	"testing"
)

func TestRunStatus_UnknownFlag(t *testing.T) {
	err := runStatus([]string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunStatus_UnexpectedArg(t *testing.T) {
	err := runStatus([]string{"extra"})
	if err == nil {
		t.Fatal("expected error for unexpected arg")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunDashboard_UnknownFlag(t *testing.T) {
	err := runDashboard([]string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunDashboard_UnexpectedArg(t *testing.T) {
	err := runDashboard([]string{"something"})
	if err == nil {
		t.Fatal("expected error for unexpected arg")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}
