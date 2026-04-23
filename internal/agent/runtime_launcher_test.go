package agent

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

func TestNewRuntimeLauncher_DefaultsClaudeCodeEnabled(t *testing.T) {
	rl := NewRuntimeLauncher(nil)
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
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeClaudeCode: {Enabled: ptr.Bool(true)},
	})
	cmd, _, err := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
}

func TestGetCommand_CodexEnabled(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeCodex: {Enabled: ptr.Bool(true)},
	})
	cmd, args, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "codex" {
		t.Errorf("expected codex, got %s", cmd)
	}
	// Verify base args: exec -a never -s workspace-write
	expected := []string{"exec", "-a", "never", "-s", "workspace-write"}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestGetCommand_GeminiEnabled(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeGemini: {Enabled: ptr.Bool(true)},
	})
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

func TestGetCommand_DisabledRuntime(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeCodex: {Enabled: ptr.Bool(false)},
	})
	_, _, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{})
	if err == nil {
		t.Fatal("expected error for disabled runtime")
	}
	if got := err.Error(); got != `runtime "codex" is disabled` {
		t.Errorf("unexpected error message: %s", got)
	}
}

func TestGetCommand_UnknownRuntime(t *testing.T) {
	rl := NewRuntimeLauncher(nil)
	_, _, err := rl.GetCommand("unknown-runtime", RuntimeLaunchOptions{})
	if err == nil {
		t.Fatal("expected error for unknown runtime")
	}
	if got := err.Error(); got != `unknown runtime "unknown-runtime"` {
		t.Errorf("unexpected error message: %s", got)
	}
}

func TestGetCommand_EmptyRuntimeDefaultsToClaudeCode(t *testing.T) {
	rl := NewRuntimeLauncher(nil)
	cmd, _, err := rl.GetCommand("", RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude (default), got %s", cmd)
	}
}

func TestGetCommand_WithModel(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeClaudeCode: {Enabled: ptr.Bool(true)},
	})
	_, args, err := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{Model: "opus"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(args) < 2 || args[0] != "--model" || args[1] != "opus" {
		t.Errorf("expected [--model opus], got %v", args)
	}
}

func TestFallbackToDefault_AlwaysClaudeCode(t *testing.T) {
	rl := NewRuntimeLauncher(nil)
	cmd, _ := rl.FallbackToDefault()
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
}

func TestFallbackToDefault_EvenWhenCodexEnabled(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeCodex: {Enabled: ptr.Bool(true)},
	})
	cmd, _ := rl.FallbackToDefault()
	if cmd != "claude" {
		t.Errorf("expected claude (fallback), got %s", cmd)
	}
}

func TestBackwardCompat_NoRuntimeFieldDefaultBehavior(t *testing.T) {
	rl := NewRuntimeLauncher(nil)
	cmd, args, err := rl.GetCommand("", RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
	if len(args) != 0 {
		t.Errorf("expected no args for default, got %v", args)
	}
}

func TestGetCommand_CodexAndGeminiEnabledByDefault(t *testing.T) {
	// codex and gemini are enabled by default so that model-name-based runtime
	// selection (e.g. model: "codex") works without an explicit runtimes: config.
	rl := NewRuntimeLauncher(nil)

	_, _, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{})
	if err != nil {
		t.Errorf("codex should be enabled by default, got: %v", err)
	}

	_, _, err = rl.GetCommand(model.RuntimeGemini, RuntimeLaunchOptions{})
	if err != nil {
		t.Errorf("gemini should be enabled by default, got: %v", err)
	}
}

func TestGetCommand_CanDisableCodexViaConfig(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeCodex: {Enabled: ptr.Bool(false)},
	})
	_, _, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{})
	if err == nil {
		t.Error("expected error: codex explicitly disabled via config")
	}
}

func TestNewRuntimeLauncher_IgnoresUnknownConfigRuntime(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		"unknown-agent": {Enabled: ptr.Bool(true)},
	})
	_, _, err := rl.GetCommand("unknown-agent", RuntimeLaunchOptions{})
	if err == nil {
		t.Error("expected error for unknown runtime not registered")
	}
}

func TestGetCommand_ArgsIsolation(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeClaudeCode: {Enabled: ptr.Bool(true)},
	})

	_, args1, _ := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{Model: "sonnet"})
	_, args2, _ := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{Model: "opus"})

	if len(args1) == len(args2) && len(args1) > 0 && args1[1] == args2[1] {
		t.Error("args should be independent copies")
	}
}

// --- New tests for multi-runtime arg building ---

func TestGetCommand_CodexWithModel(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeCodex: {Enabled: ptr.Bool(true)},
	})
	_, args, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{Model: "o3-mini"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Base args + --model o3-mini
	expected := []string{"exec", "-a", "never", "-s", "workspace-write", "--model", "o3-mini"}
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
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeGemini: {Enabled: ptr.Bool(true)},
	})
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
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeGemini: {Enabled: ptr.Bool(true)},
	})
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
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeGemini: {Enabled: ptr.Bool(true)},
	})
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
	// Prompt option is only used by gemini; claude-code handles prompts via buildLaunchArgs.
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeClaudeCode: {Enabled: ptr.Bool(true)},
	})
	_, args, err := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{
		Model:  "opus",
		Prompt: "ignored prompt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should only have --model, no -p
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

func TestGetCommand_CodexArgsIsolation(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeCodex: {Enabled: ptr.Bool(true)},
	})

	_, args1, _ := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{Model: "o3-mini"})
	_, args2, _ := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{})

	// args1 should have model appended, args2 should not
	if len(args1) == len(args2) {
		t.Error("args should differ: one has model, the other does not")
	}
	// Verify base args in args2 are not mutated
	if len(args2) != 5 {
		t.Errorf("expected 5 base args for codex without model, got %d: %v", len(args2), args2)
	}
}
