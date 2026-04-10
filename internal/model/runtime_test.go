package model

import "testing"

func TestValidateRuntime(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"claude-code", true},
		{"codex", true},
		{"gemini", true},
		{"openai", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := ValidateRuntime(tt.input); got != tt.want {
				t.Errorf("ValidateRuntime(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDefaultRuntime(t *testing.T) {
	if got := DefaultRuntime(); got != "claude-code" {
		t.Errorf("DefaultRuntime() = %q, want claude-code", got)
	}
}

func TestRuntimeDefinition_Fields(t *testing.T) {
	rd := RuntimeDefinition{
		Name:    "codex",
		Command: "codex",
		Args:    []string{"--model", "o4-mini"},
		EnvVars: map[string]string{"CODEX_TOKEN": "xxx"},
	}
	if rd.Name != "codex" {
		t.Errorf("Name = %q, want codex", rd.Name)
	}
	if rd.Command != "codex" {
		t.Errorf("Command = %q, want codex", rd.Command)
	}
	if len(rd.Args) != 2 {
		t.Errorf("Args count = %d, want 2", len(rd.Args))
	}
	if rd.EnvVars["CODEX_TOKEN"] != "xxx" {
		t.Error("EnvVars should contain CODEX_TOKEN")
	}
}

func TestRuntimeConfig_Defaults(t *testing.T) {
	rc := RuntimeConfig{}
	if rc.EffectiveEnabled() {
		t.Error("EffectiveEnabled() should default to false")
	}
	if rc.EffectiveDefault() {
		t.Error("EffectiveDefault() should default to false")
	}
	if rc.EffectiveDefaultModel() != "" {
		t.Errorf("EffectiveDefaultModel() = %q, want empty", rc.EffectiveDefaultModel())
	}
}

func TestRuntimeConfig_Configured(t *testing.T) {
	rc := RuntimeConfig{
		Enabled:      BoolPtr(true),
		Default:      BoolPtr(true),
		Models:       []string{"opus", "sonnet"},
		DefaultModel: StringPtr("opus"),
	}
	if !rc.EffectiveEnabled() {
		t.Error("EffectiveEnabled() should be true")
	}
	if !rc.EffectiveDefault() {
		t.Error("EffectiveDefault() should be true")
	}
	if rc.EffectiveDefaultModel() != "opus" {
		t.Errorf("EffectiveDefaultModel() = %q, want opus", rc.EffectiveDefaultModel())
	}
}
