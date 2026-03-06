package main

import (
	"errors"
	"testing"
)

func TestRunDaemon_UnknownFlag(t *testing.T) {
	err := runDaemon([]string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if ce.Code != 1 {
		t.Errorf("Code = %d, want 1", ce.Code)
	}
}

func TestRunDaemon_UnexpectedArg(t *testing.T) {
	err := runDaemon([]string{"extra"})
	if err == nil {
		t.Fatal("expected error for unexpected arg")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
	if ce.Code != 1 {
		t.Errorf("Code = %d, want 1", ce.Code)
	}
}
