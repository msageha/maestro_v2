package agent

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestNewRuntimeLauncher_DefaultsClaudeCodeEnabled(t *testing.T) {
	rl := NewRuntimeLauncher(nil)
	cmd, args, err := rl.GetCommand(model.RuntimeClaudeCode, "")
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
		model.RuntimeClaudeCode: {Enabled: model.BoolPtr(true)},
	})
	cmd, _, err := rl.GetCommand(model.RuntimeClaudeCode, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
}

func TestGetCommand_CodexEnabled(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeCodex: {Enabled: model.BoolPtr(true)},
	})
	cmd, _, err := rl.GetCommand(model.RuntimeCodex, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "codex" {
		t.Errorf("expected codex, got %s", cmd)
	}
}

func TestGetCommand_GeminiEnabled(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeGemini: {Enabled: model.BoolPtr(true)},
	})
	cmd, _, err := rl.GetCommand(model.RuntimeGemini, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "gemini" {
		t.Errorf("expected gemini, got %s", cmd)
	}
}

func TestGetCommand_DisabledRuntime(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeCodex: {Enabled: model.BoolPtr(false)},
	})
	_, _, err := rl.GetCommand(model.RuntimeCodex, "")
	if err == nil {
		t.Fatal("expected error for disabled runtime")
	}
	if got := err.Error(); got != `runtime "codex" is disabled` {
		t.Errorf("unexpected error message: %s", got)
	}
}

func TestGetCommand_UnknownRuntime(t *testing.T) {
	rl := NewRuntimeLauncher(nil)
	_, _, err := rl.GetCommand("unknown-runtime", "")
	if err == nil {
		t.Fatal("expected error for unknown runtime")
	}
	if got := err.Error(); got != `unknown runtime "unknown-runtime"` {
		t.Errorf("unexpected error message: %s", got)
	}
}

func TestGetCommand_EmptyRuntimeDefaultsToClaudeCode(t *testing.T) {
	rl := NewRuntimeLauncher(nil)
	cmd, _, err := rl.GetCommand("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude (default), got %s", cmd)
	}
}

func TestGetCommand_WithModel(t *testing.T) {
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeClaudeCode: {Enabled: model.BoolPtr(true)},
	})
	_, args, err := rl.GetCommand(model.RuntimeClaudeCode, "opus")
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
		model.RuntimeCodex: {Enabled: model.BoolPtr(true)},
	})
	cmd, _ := rl.FallbackToDefault()
	if cmd != "claude" {
		t.Errorf("expected claude (fallback), got %s", cmd)
	}
}

func TestBackwardCompat_NoRuntimeFieldDefaultBehavior(t *testing.T) {
	// Simulate: Task.Runtime is empty, no runtimes configured
	rl := NewRuntimeLauncher(nil)
	cmd, args, err := rl.GetCommand("", "")
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

func TestGetCommand_CodexDisabledByDefault(t *testing.T) {
	// Without config, codex and gemini are disabled
	rl := NewRuntimeLauncher(nil)

	_, _, err := rl.GetCommand(model.RuntimeCodex, "")
	if err == nil {
		t.Error("expected error: codex should be disabled by default")
	}

	_, _, err = rl.GetCommand(model.RuntimeGemini, "")
	if err == nil {
		t.Error("expected error: gemini should be disabled by default")
	}
}

func TestNewRuntimeLauncher_IgnoresUnknownConfigRuntime(t *testing.T) {
	// Unknown runtime names in config should be ignored gracefully
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		"unknown-agent": {Enabled: model.BoolPtr(true)},
	})
	_, _, err := rl.GetCommand("unknown-agent", "")
	if err == nil {
		t.Error("expected error for unknown runtime not registered")
	}
}

func TestGetCommand_ArgsIsolation(t *testing.T) {
	// Verify that returned args are independent copies
	rl := NewRuntimeLauncher(map[string]model.RuntimeConfig{
		model.RuntimeClaudeCode: {Enabled: model.BoolPtr(true)},
	})

	_, args1, _ := rl.GetCommand(model.RuntimeClaudeCode, "sonnet")
	_, args2, _ := rl.GetCommand(model.RuntimeClaudeCode, "opus")

	if len(args1) == len(args2) && len(args1) > 0 && args1[1] == args2[1] {
		t.Error("args should be independent copies")
	}
}
