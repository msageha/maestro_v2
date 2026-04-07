package main

import (
	"errors"
	"testing"
)

func TestRunPlan_NoSubcommand(t *testing.T) {
	err := runPlan(nil)
	if err == nil {
		t.Fatal("expected error for missing subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunPlan_UnknownSubcommand(t *testing.T) {
	err := runPlan([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunPlanSubmit_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"missing command-id", []string{}, true},
		{"unknown flag", []string{"--unknown"}, true},
		{"unexpected arg", []string{"--command-id", "c1", "extra"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runPlanSubmit(tt.args)
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

func TestRunPlanUnquarantine_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing command-id", []string{}},
		{"invalid command-id", []string{"--command-id", "../bad"}},
		{"unexpected arg", []string{"--command-id", "cmd_1", "extra"}},
		{"unknown flag", []string{"--unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runPlanUnquarantine(tt.args)
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

func TestRunPlanResumeMerge_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing command-id", []string{}},
		{"invalid command-id", []string{"--command-id", "../bad"}},
		{"unexpected arg", []string{"--command-id", "cmd_1", "extra"}},
		{"unknown flag", []string{"--unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runPlanResumeMerge(tt.args)
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

func TestRunResolveConflict_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"missing command-id", []string{"--phase-id", "p1", "--worker-id", "worker1"}},
		{"invalid command-id", []string{"--command-id", "../bad", "--phase-id", "p1", "--worker-id", "worker1"}},
		{"missing phase-id", []string{"--command-id", "cmd_1", "--worker-id", "worker1"}},
		{"invalid phase-id", []string{"--command-id", "cmd_1", "--phase-id", "../bad", "--worker-id", "worker1"}},
		{"missing worker-id", []string{"--command-id", "cmd_1", "--phase-id", "p1"}},
		{"invalid worker-id", []string{"--command-id", "cmd_1", "--phase-id", "p1", "--worker-id", "../bad"}},
		{"unexpected arg", []string{"--command-id", "cmd_1", "--phase-id", "p1", "--worker-id", "worker1", "extra"}},
		{"unknown flag", []string{"--unknown"}},
		{"conflicting-files without other flags", []string{"--conflicting-files", "a.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runResolveConflict(tt.args)
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

func TestRunPlan_RecoverySubcommandsRouted(t *testing.T) {
	// Sanity: runPlan should accept the new subcommand names without
	// returning "unknown subcommand". They will fail flag parsing instead.
	for _, sub := range []string{"unquarantine", "resume-merge"} {
		err := runPlan([]string{sub})
		if err == nil {
			t.Fatalf("%s: expected error", sub)
		}
		if msg := err.Error(); containsAny(msg, "unknown subcommand") {
			t.Fatalf("%s: was rejected as unknown subcommand: %v", sub, err)
		}
	}
}

func containsAny(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRunPlanComplete_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"missing command-id", []string{}, true},
		{"unknown flag", []string{"--unknown"}, true},
		{"unexpected arg", []string{"--command-id", "c1", "extra"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runPlanComplete(tt.args)
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

func TestRunPlanAddRetryTask_MissingFlags(t *testing.T) {
	err := runPlanAddRetryTask([]string{"--command-id", "c1"})
	if err == nil {
		t.Fatal("expected error for missing required flags")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunPlanAddRetryTask_ValidateID(t *testing.T) {
	allRequiredFlags := func(overrides map[string]string) []string {
		defaults := map[string]string{
			"--command-id":          "valid-cmd",
			"--retry-of":           "valid-task",
			"--purpose":            "test purpose",
			"--content":            "test content",
			"--acceptance-criteria": "test criteria",
			"--bloom-level":        "3",
		}
		for k, v := range overrides {
			defaults[k] = v
		}
		var args []string
		for k, v := range defaults {
			args = append(args, k, v)
		}
		return args
	}

	tests := []struct {
		name    string
		args    []string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "invalid command-id with path traversal",
			args:    allRequiredFlags(map[string]string{"--command-id": "../evil"}),
			wantErr: true,
			errMsg:  "invalid --command-id",
		},
		{
			name:    "invalid retry-of with path traversal",
			args:    allRequiredFlags(map[string]string{"--retry-of": "../evil"}),
			wantErr: true,
			errMsg:  "invalid --retry-of",
		},
		{
			name:    "bloom-level too low",
			args:    allRequiredFlags(map[string]string{"--bloom-level": "0"}),
			wantErr: true,
			errMsg:  "all required flags must be set",
		},
		{
			name:    "bloom-level too high",
			args:    allRequiredFlags(map[string]string{"--bloom-level": "7"}),
			wantErr: true,
			errMsg:  "--bloom-level must be between 1 and 6",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runPlanAddRetryTask(tt.args)
			if !tt.wantErr {
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
			if tt.errMsg != "" {
				if got := ce.Msg; !containsStr(got, tt.errMsg) {
					t.Errorf("error message %q does not contain %q", got, tt.errMsg)
				}
			}
		})
	}
}

func TestRunPlanAddRetryTask_InvalidBlockedBy(t *testing.T) {
	args := []string{
		"--command-id", "valid-cmd",
		"--retry-of", "valid-task",
		"--purpose", "p",
		"--content", "c",
		"--acceptance-criteria", "ac",
		"--bloom-level", "3",
		"--blocked-by", "../evil",
	}
	err := runPlanAddRetryTask(args)
	if err == nil {
		t.Fatal("expected error for invalid blocked-by")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && stringContains(s, substr))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestRunPlanRequestCancel_MissingCommandID(t *testing.T) {
	err := runPlanRequestCancel(nil)
	if err == nil {
		t.Fatal("expected error for missing command-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}

func TestRunPlanRebuild_MissingCommandID(t *testing.T) {
	err := runPlanRebuild(nil)
	if err == nil {
		t.Fatal("expected error for missing command-id")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CLIError, got %T: %v", err, err)
	}
}
