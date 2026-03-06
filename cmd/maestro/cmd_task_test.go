package main

import (
	"errors"
	"testing"
)

func TestRunTask_NoSubcommand(t *testing.T) {
	err := runTask(nil)
	if err == nil {
		t.Fatal("expected error for missing subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunTask_UnknownSubcommand(t *testing.T) {
	err := runTask([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
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
			err := runTaskHeartbeat(tt.args)
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
