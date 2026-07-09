package model

import "testing"

func TestFamily(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"sonnet", "sonnet"},
		{"opus", "opus"},
		{"haiku", "haiku"},
		{"claude-opus-4-7", "opus"},
		{"claude-sonnet-4-6", "sonnet"},
		{"claude-haiku-4-5-20251001", "haiku"},
		// Non-claude and unknown names pass through unchanged.
		{"codex", "codex"},
		{"codex-5", "codex-5"},
		{"gemini-2.5-pro", "gemini-2.5-pro"},
		{"claude-fable-5", "claude-fable-5"},
	}
	for _, tt := range tests {
		if got := Family(tt.in); got != tt.want {
			t.Errorf("Family(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
