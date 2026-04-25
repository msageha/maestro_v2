package agent

import (
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestNewRuntimeLauncher_ClaudeCodeAvailable(t *testing.T) {
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

func TestGetCommand_ClaudeCode(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, _, err := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
}

func TestGetCommand_Codex(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, args, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "codex" {
		t.Errorf("expected codex, got %s", cmd)
	}
	// --ask-for-approval never: auto-approve all tool calls (no interactive prompts).
	// --sandbox danger-full-access: required to allow UDS connections to daemon.sock.
	// (workspace-write and the --full-auto alias both block UDS connections.)
	expected := []string{"--ask-for-approval", "never", "--sandbox", "danger-full-access"}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestGetCommand_Gemini(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, args, err := rl.GetCommand(model.RuntimeGemini, RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "gemini" {
		t.Errorf("expected gemini, got %s", cmd)
	}
	// Verify base args: --approval-mode=yolo -s
	expected := []string{"--approval-mode=yolo", "-s"}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestGetCommand_UnknownRuntime(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, _, err := rl.GetCommand("unknown-runtime", RuntimeLaunchOptions{})
	if err == nil {
		t.Fatal("expected error for unknown runtime")
	}
	if got := err.Error(); got != `unknown runtime "unknown-runtime"` {
		t.Errorf("unexpected error message: %s", got)
	}
}

func TestGetCommand_EmptyRuntimeDefaultsToClaudeCode(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, _, err := rl.GetCommand("", RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude (default), got %s", cmd)
	}
}

func TestGetCommand_WithModel(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{Model: "opus"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(args) < 2 || args[0] != "--model" || args[1] != "opus" {
		t.Errorf("expected [--model opus], got %v", args)
	}
}

func TestFallbackToDefault_AlwaysClaudeCode(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, _ := rl.FallbackToDefault()
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
}

func TestGetCommand_ArgsIsolation(t *testing.T) {
	rl := NewRuntimeLauncher()

	_, args1, _ := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{Model: "sonnet"})
	_, args2, _ := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{Model: "opus"})

	if len(args1) == len(args2) && len(args1) > 0 && args1[1] == args2[1] {
		t.Error("args should be independent copies")
	}
}

func TestGetCommand_CodexWithModel(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{Model: "o3-mini"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Base args + --model o3-mini
	expected := []string{"--ask-for-approval", "never", "--sandbox", "danger-full-access", "--model", "o3-mini"}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestGetCommand_GeminiWithModel(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeGemini, RuntimeLaunchOptions{Model: "gemini-2.5-pro"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"--approval-mode=yolo", "-s", "--model", "gemini-2.5-pro"}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestGetCommand_GeminiWithPrompt(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeGemini, RuntimeLaunchOptions{
		Model:  "gemini-2.5-pro",
		Prompt: "You are a helpful assistant.",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"--approval-mode=yolo", "-s", "--model", "gemini-2.5-pro", "-p", "You are a helpful assistant."}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestGetCommand_GeminiPromptWithoutModel(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeGemini, RuntimeLaunchOptions{
		Prompt: "system prompt only",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"--approval-mode=yolo", "-s", "-p", "system prompt only"}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestGetCommand_ClaudeCodeIgnoresPromptOption(t *testing.T) {
	// Prompt option is only used by gemini (-p) and codex (positional arg).
	// claude-code handles prompts via buildLaunchArgs (--system-prompt / --append-system-prompt).
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{
		Model:  "opus",
		Prompt: "ignored prompt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should only have --model, no -p or positional prompt
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

func TestGetCommand_CodexWithPrompt(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{
		Prompt: "You are Maestro orchestrator.",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Base args + positional prompt
	expected := []string{"--ask-for-approval", "never", "--sandbox", "danger-full-access", "You are Maestro orchestrator."}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestGetCommand_CodexWithModelAndPrompt(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{
		Model:  "o3-mini",
		Prompt: "System prompt here.",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Base args + --model + positional prompt (prompt must be last)
	expected := []string{"--ask-for-approval", "never", "--sandbox", "danger-full-access", "--model", "o3-mini", "System prompt here."}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestGetCommand_CodexArgsIsolation(t *testing.T) {
	rl := NewRuntimeLauncher()

	_, args1, _ := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{Model: "o3-mini"})
	_, args2, _ := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{})

	// args1 should have model appended, args2 should not
	if len(args1) == len(args2) {
		t.Error("args should differ: one has model, the other does not")
	}
	// Verify base args in args2 are not mutated
	// (--approval-policy never --sandbox danger-full-access = 4 elements)
	if len(args2) != 4 {
		t.Errorf("expected 4 base args for codex without model, got %d: %v", len(args2), args2)
	}
}

// TestLaunchAlternativeRuntime_RejectsOrchestratorAndPlanner verifies the
// fail-closed defense-in-depth check that blocks codex/gemini from running
// the orchestrator or planner role even if config validation is bypassed.
// Tool-based role enforcement (--allowedTools, --disallowedTools) is
// claude-code-only; without it the delegation/planning contract cannot be
// enforced. Past incidents confirmed codex Orchestrator directly modified
// files on main, never reaching the daemon (see launcher.go doc comment).
func TestLaunchAlternativeRuntime_RejectsOrchestratorAndPlanner(t *testing.T) {
	cases := []struct {
		role    string
		runtime string
	}{
		{"orchestrator", model.RuntimeCodex},
		{"orchestrator", model.RuntimeGemini},
		{"planner", model.RuntimeCodex},
		{"planner", model.RuntimeGemini},
	}
	for _, tc := range cases {
		t.Run(tc.role+"/"+tc.runtime, func(t *testing.T) {
			err := launchAlternativeRuntime(tc.runtime, "", tc.role, "ignored prompt")
			if err == nil {
				t.Fatalf("expected error for role=%s runtime=%s", tc.role, tc.runtime)
			}
			// The error must mention the role so operators can act on it.
			if !strings.Contains(err.Error(), tc.role) {
				t.Errorf("error should mention role %q, got: %v", tc.role, err)
			}
		})
	}
}
