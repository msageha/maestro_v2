package model

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/ptr"
)

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
		Enabled:      ptr.Bool(true),
		Default:      ptr.Bool(true),
		Models:       []string{"opus", "sonnet"},
		DefaultModel: ptr.String("opus"),
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
