package agent

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestRuntimeLauncher_ClaudeCodeAvailable(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, args, err := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

func TestRuntimeLauncher_RejectsUnsupportedRuntimes(t *testing.T) {
	rl := NewRuntimeLauncher()
	for _, runtime := range []string{model.RuntimeCodex, model.RuntimeGemini, "unknown-runtime"} {
		t.Run(runtime, func(t *testing.T) {
			_, _, err := rl.GetCommand(runtime, RuntimeLaunchOptions{})
			if err == nil {
				t.Fatal("expected unsupported runtime error")
			}
		})
	}
}

func TestRuntimeLauncher_EmptyRuntimeDefaultsToClaudeCode(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, _, err := rl.GetCommand("", RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
}

func TestRuntimeLauncher_ClaudeCodeWithModel(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{
		Model:  "opus",
		Prompt: "ignored prompt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"--model", "opus"}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestFallbackToDefault_AlwaysClaudeCode(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, args := rl.FallbackToDefault()
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
	if len(args) != 0 {
		t.Errorf("expected no default args, got %v", args)
	}
}

func TestLaunchAlternativeRuntimeRejectsManagedRoles(t *testing.T) {
	tests := []struct {
		role    string
		runtime string
	}{
		{"orchestrator", model.RuntimeCodex},
		{"orchestrator", model.RuntimeGemini},
		{"planner", model.RuntimeCodex},
		{"planner", model.RuntimeGemini},
		{"worker", model.RuntimeCodex},
		{"worker", model.RuntimeGemini},
	}
	for _, tc := range tests {
		t.Run(tc.role+"_"+tc.runtime, func(t *testing.T) {
			err := launchAlternativeRuntime(tc.runtime, "", tc.role, "ignored prompt")
			if err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
