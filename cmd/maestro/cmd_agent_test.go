package main

import (
	"errors"
	"testing"
)

func TestRunAgent_Dispatch(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no subcommand", nil, "missing subcommand"},
		{"unknown subcommand", []string{"bogus"}, "unknown subcommand"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runAgent(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
		})
	}
}

func TestRunAgentLaunch_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown flag", []string{"--unknown"}, "flag provided but not defined"},
		{"unexpected arg", []string{"extra"}, "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runAgentLaunch(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
		})
	}
}

func TestRunAgentExec_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"missing agent-id", []string{}, true},
		{"unknown flag", []string{"--unknown"}, true},
		{"unexpected arg", []string{"--agent-id", "a1", "extra"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runAgentExec(tt.args)
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
