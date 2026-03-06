package main

import (
	"errors"
	"testing"
)

func TestRunWorker_NoSubcommand(t *testing.T) {
	err := runWorker(nil)
	if err == nil {
		t.Fatal("expected error for missing subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunWorker_UnknownSubcommand(t *testing.T) {
	err := runWorker([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunWorkerStandby_UnknownFlag(t *testing.T) {
	err := runWorkerStandby([]string{"--unknown"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunWorkerStandby_UnexpectedArg(t *testing.T) {
	err := runWorkerStandby([]string{"extra"})
	if err == nil {
		t.Fatal("expected error for unexpected arg")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}
