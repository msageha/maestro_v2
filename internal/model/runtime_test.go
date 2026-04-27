package model

import (
	"testing"
)

func TestParseRuntimeFromModel(t *testing.T) {
	tests := []struct {
		model         string
		wantRuntime   string
		wantEffective string
	}{
		// Runtime name itself → use that runtime, no model override
		{"codex", RuntimeCodex, ""},
		{"gemini", RuntimeGemini, ""},
		// Gemini-prefixed model → gemini runtime with model override
		{"gemini-2.5-pro", RuntimeGemini, "gemini-2.5-pro"},
		{"gemini-2.5-flash", RuntimeGemini, "gemini-2.5-flash"},
		{"gemini-1.5-pro", RuntimeGemini, "gemini-1.5-pro"},
		// claude-code models → unchanged, claude-code runtime
		{"sonnet", RuntimeClaudeCode, "sonnet"},
		{"opus", RuntimeClaudeCode, "opus"},
		{"haiku", RuntimeClaudeCode, "haiku"},
		{"claude-sonnet-4-5", RuntimeClaudeCode, "claude-sonnet-4-5"},
		// edge cases
		{"", RuntimeClaudeCode, ""},
		{"claude-code", RuntimeClaudeCode, "claude-code"},
		{"geminiZZZ", RuntimeClaudeCode, "geminiZZZ"}, // not "gemini-" prefix; "ZZZ" suffix avoids being mistaken for an XXX-style TODO marker (F-066)
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			gotRuntime, gotEffective := ParseRuntimeFromModel(tt.model)
			if gotRuntime != tt.wantRuntime {
				t.Errorf("runtime: got %q, want %q", gotRuntime, tt.wantRuntime)
			}
			if gotEffective != tt.wantEffective {
				t.Errorf("effectiveModel: got %q, want %q", gotEffective, tt.wantEffective)
			}
		})
	}
}

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
