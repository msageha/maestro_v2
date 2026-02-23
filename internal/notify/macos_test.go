package notify

import "testing"

func TestEscapeAppleScript(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{`say "hello"`, `say \"hello\"`},
		{`path\to\file`, `path\\to\\file`},
		{`"quote" and \backslash`, `\"quote\" and \\backslash`},
		{"", ""},
	}
	for _, tt := range tests {
		got := escapeAppleScript(tt.input)
		if got != tt.want {
			t.Errorf("escapeAppleScript(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSend_InvalidCommand(t *testing.T) {
	// Verify Send returns an error gracefully when osascript fails.
	// We pass an empty title/message which osascript should handle,
	// but this test mainly ensures we don't panic.
	// On CI without macOS GUI, osascript may fail — that's fine.
	err := Send("", "")
	// We don't assert success or failure since behavior depends on the environment.
	// Just verify it doesn't panic.
	_ = err
}

func TestSend_SpecialCharacters(t *testing.T) {
	// Test that special characters in title/message don't cause panics.
	err := Send(`Test "Title"`, `Message with \backslash and "quotes"`)
	_ = err
}
