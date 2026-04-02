package agent

import "testing"

func TestContainsControlChars(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// Normal strings
		{name: "plain ascii", input: "hello world", want: false},
		{name: "with digits and punctuation", input: "task_123: ok!", want: false},
		{name: "with tab (allowed)", input: "col1\tcol2", want: false},
		{name: "empty string", input: "", want: false},
		{name: "multibyte utf8", input: "こんにちは世界", want: false},
		{name: "emoji", input: "status: ok 🎉", want: false},

		// Control characters
		{name: "null byte", input: "hello\x00world", want: true},
		{name: "newline", input: "line1\nline2", want: true},
		{name: "carriage return", input: "line1\rline2", want: true},
		{name: "escape (0x1B)", input: "text\x1b[31mred", want: true},
		{name: "bell (0x07)", input: "alert\x07!", want: true},
		{name: "backspace (0x08)", input: "back\x08space", want: true},
		{name: "form feed (0x0C)", input: "page\x0cbreak", want: true},
		{name: "vertical tab (0x0B)", input: "vtab\x0b!", want: true},
		{name: "DEL (0x7F)", input: "del\x7fete", want: true},
		{name: "SOH (0x01)", input: "\x01start", want: true},
		{name: "US (0x1F)", input: "unit\x1fsep", want: true},

		// Edge cases
		{name: "only tab", input: "\t", want: false},
		{name: "only newline", input: "\n", want: true},
		{name: "mixed tab and newline", input: "\t\n", want: true},
		{name: "control at start", input: "\x00abc", want: true},
		{name: "control at end", input: "abc\x00", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsControlChars(tt.input)
			if got != tt.want {
				t.Errorf("containsControlChars(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
