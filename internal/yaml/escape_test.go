package yaml

import (
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// bs is a single backslash byte used to build test strings without Go source
// escape ambiguity.
const bs = "\x5c"

type contentHolder struct {
	Content string `yaml:"content"`
}

func TestSanitizeDoubleQuoteEscapes(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantParse bool   // should yamlv3.Unmarshal succeed after sanitisation?
		wantValue string // expected Content value (only checked when wantParse is true)
	}{
		{
			name:      "dq with backslash-bang",
			input:     "content: " + `"text with ` + bs + `! here"` + "\n",
			wantParse: true,
			wantValue: "text with " + bs + "! here",
		},
		{
			name:      "dq with backslash-colon",
			input:     "content: " + `"text with ` + bs + `: here"` + "\n",
			wantParse: true,
			wantValue: "text with " + bs + ": here",
		},
		{
			name:      "dq with backslash-hash",
			input:     "content: " + `"text with ` + bs + `# here"` + "\n",
			wantParse: true,
			wantValue: "text with " + bs + "# here",
		},
		{
			name:      "dq with backslash-paren",
			input:     "content: " + `"text with ` + bs + `( here"` + "\n",
			wantParse: true,
			wantValue: "text with " + bs + "( here",
		},
		{
			name:      "dq with valid escape newline preserved",
			input:     "content: " + `"line1` + bs + `nline2"` + "\n",
			wantParse: true,
			wantValue: "line1\nline2",
		},
		{
			name:      "dq with valid double-backslash preserved",
			input:     "content: " + `"text with ` + bs + bs + ` here"` + "\n",
			wantParse: true,
			wantValue: "text with " + bs + " here",
		},
		{
			name:      "dq with valid backslash-quote preserved",
			input:     "content: " + `"text with ` + bs + `" here"` + "\n",
			wantParse: false, // \\" closes the string then " here" is dangling — yaml error
		},
		{
			name:      "literal block scalar unchanged",
			input:     "content: |\n  text with " + bs + "! here\n",
			wantParse: true,
			wantValue: "text with " + bs + "! here\n", // block scalar preserves trailing newline
		},
		{
			name:      "plain scalar unchanged",
			input:     "content: text with " + bs + "! here\n",
			wantParse: true,
			wantValue: "text with " + bs + "! here",
		},
		{
			name:      "single-quoted string unchanged",
			input:     "content: 'text with " + bs + "! here'\n",
			wantParse: true,
			wantValue: "text with " + bs + "! here",
		},
		{
			name:      "multiple invalid escapes in one dq string",
			input:     "content: " + `"` + bs + `!` + bs + `:` + bs + `#` + `"` + "\n",
			wantParse: true,
			wantValue: bs + "!" + bs + ":" + bs + "#",
		},
		{
			name:      "mixed valid and invalid escapes",
			input:     "content: " + `"` + bs + `n` + bs + `!` + bs + bs + bs + `:` + `"` + "\n",
			wantParse: true,
			wantValue: "\n" + bs + "!" + bs + bs + ":",
		},
		{
			name:      "empty input",
			input:     "",
			wantParse: false,
		},
		{
			name: "full task YAML with backslash in content dq",
			input: "tasks:\n" +
				"  - name: task1\n" +
				"    purpose: test\n" +
				"    content: " + `"run ` + bs + `!cmd and ` + bs + `:check"` + "\n" +
				"    acceptance_criteria: done\n" +
				"    bloom_level: 2\n" +
				"    required: true\n",
			wantParse: true,
		},
		{
			name:      "dq after sequence indicator",
			input:     "items:\n  - " + `"value with ` + bs + `! here"` + "\n",
			wantParse: true,
		},
		{
			name:      "dq in flow sequence",
			input:     "items: [" + `"` + bs + `!a"` + ", " + `"` + bs + `:b"` + "]\n",
			wantParse: true,
		},
		{
			name:      "backslash at end of dq string",
			input:     "content: " + `"text` + bs + `"` + "\n",
			wantParse: false, // backslash-quote is valid escape, so string never closes
		},
		{
			name:      "comment with quote not modified",
			input:     "content: hello # some " + `"` + "comment\n",
			wantParse: true,
			wantValue: "hello",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sanitized := SanitizeDoubleQuoteEscapes([]byte(tt.input))

			var out contentHolder
			err := yamlv3.Unmarshal(sanitized, &out)

			if tt.wantParse {
				if err != nil {
					t.Fatalf("expected successful parse after sanitisation, got error: %v\ninput:     %q\nsanitized: %q", err, tt.input, string(sanitized))
				}
				if tt.wantValue != "" && out.Content != tt.wantValue {
					t.Errorf("Content = %q, want %q", out.Content, tt.wantValue)
				}
			} else {
				// We don't require parse success; just verify sanitiser doesn't panic.
				_ = err
			}
		})
	}
}

func TestSanitizeDoubleQuoteEscapes_NoOp(t *testing.T) {
	// Valid YAML should pass through unchanged.
	validInputs := []string{
		"key: value\n",
		"key: \"valid string\"\n",
		"key: 'single quoted'\n",
		"key: |\n  block scalar\n",
		"key: \"with \\n and \\t\"\n",
		"key: \"with \\\\ backslash\"\n",
	}
	for _, input := range validInputs {
		result := SanitizeDoubleQuoteEscapes([]byte(input))
		if string(result) != input {
			t.Errorf("SanitizeDoubleQuoteEscapes modified valid YAML:\n  input:  %q\n  output: %q", input, string(result))
		}
	}
}

// TestSanitizeDoubleQuoteEscapes_Roundtrip verifies that sanitised YAML
// produces the same parsed values as manually-escaped YAML.
func TestSanitizeDoubleQuoteEscapes_Roundtrip(t *testing.T) {
	// YAML with \! (invalid escape) after sanitisation should produce the
	// same value as YAML with \\! (explicit escaped backslash + literal !).
	invalid := "content: " + `"text ` + bs + `! here"` + "\n"
	manual := "content: " + `"text ` + bs + bs + `! here"` + "\n"

	sanitized := SanitizeDoubleQuoteEscapes([]byte(invalid))

	var got, want contentHolder
	if err := yamlv3.Unmarshal(sanitized, &got); err != nil {
		t.Fatalf("sanitized parse failed: %v", err)
	}
	if err := yamlv3.Unmarshal([]byte(manual), &want); err != nil {
		t.Fatalf("manual parse failed: %v", err)
	}
	if got.Content != want.Content {
		t.Errorf("roundtrip mismatch: got %q, want %q", got.Content, want.Content)
	}
}
