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

// TestSanitizeDoubleQuoteEscapes_BlockScalarPreserved is the regression test
// for the block scalar corruption bug: lines inside a block scalar that look
// like double-quoted values (leading indent then a quote) must be copied
// verbatim — YAML performs no escape processing there, so doubling the
// backslash silently changes the delivered content (\d became \\d).
func TestSanitizeDoubleQuoteEscapes_BlockScalarPreserved(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantValue string
	}{
		{
			name:      "regex in quoted line inside literal block",
			input:     "content: |\n  \"match " + bs + `d+ digits"` + "\n",
			wantValue: "\"match " + bs + "d+ digits\"\n",
		},
		{
			name:      "windows path in quoted line inside literal block",
			input:     "content: |\n  run \"C:" + bs + "Users" + bs + `temp" now` + "\n",
			wantValue: "run \"C:" + bs + "Users" + bs + "temp\" now\n",
		},
		{
			name:      "strip chomping variant",
			input:     "content: |-\n  \"" + bs + "d\"\n",
			wantValue: "\"" + bs + "d\"",
		},
		{
			name:      "keep chomping variant",
			input:     "content: |+\n  \"" + bs + "d\"\n",
			wantValue: "\"" + bs + "d\"\n",
		},
		{
			name:      "folded block scalar",
			input:     "content: >-\n  \"" + bs + "d\"\n",
			wantValue: "\"" + bs + "d\"",
		},
		{
			name:      "explicit indentation indicator",
			input:     "content: |2\n  \"" + bs + "d\"\n",
			wantValue: "\"" + bs + "d\"\n",
		},
		{
			name:      "comment after block header",
			input:     "content: | # note\n  \"" + bs + "d\"\n",
			wantValue: "\"" + bs + "d\"\n",
		},
		{
			name:      "anchored block scalar",
			input:     "content: &a |\n  \"" + bs + "d\"\n",
			wantValue: "\"" + bs + "d\"\n",
		},
		{
			name:      "blank lines inside block",
			input:     "content: |\n  \"" + bs + "d\"\n\n  tail\n",
			wantValue: "\"" + bs + "d\"\n\ntail\n",
		},
		{
			name:      "block scalar at end of input without trailing newline",
			input:     "content: |\n  \"" + bs + "d\"",
			wantValue: "\"" + bs + "d\"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sanitized := SanitizeDoubleQuoteEscapes([]byte(tt.input))
			if string(sanitized) != tt.input {
				t.Errorf("block scalar content was modified:\n  input:  %q\n  output: %q", tt.input, string(sanitized))
			}
			var out contentHolder
			if err := yamlv3.Unmarshal(sanitized, &out); err != nil {
				t.Fatalf("parse after sanitisation: %v", err)
			}
			if out.Content != tt.wantValue {
				t.Errorf("Content = %q, want %q", out.Content, tt.wantValue)
			}
		})
	}
}

// TestSanitizeDoubleQuoteEscapes_BlockScalarBoundary verifies that sanitisation
// resumes after the block scalar ends: broken escapes in double-quoted values
// outside the block are still repaired.
func TestSanitizeDoubleQuoteEscapes_BlockScalarBoundary(t *testing.T) {
	type doc struct {
		Content string `yaml:"content"`
		Other   string `yaml:"other"`
	}
	tests := []struct {
		name        string
		input       string
		wantContent string
		wantOther   string
	}{
		{
			name: "sibling key after block still sanitised",
			input: "content: |\n  \"regex " + bs + "d here\"\n" +
				"other: " + `"bad ` + bs + `! escape"` + "\n",
			wantContent: "\"regex " + bs + "d here\"\n",
			wantOther:   "bad " + bs + "! escape",
		},
		{
			name: "nested block under sequence entry",
			input: "tasks:\n" +
				"  - name: t1\n" +
				"    content: |\n" +
				"      grep \"" + bs + "d+\" file\n" +
				"    purpose: p\n" +
				"other: " + `"x` + bs + `!y"` + "\n",
			wantOther: "x" + bs + "!y",
		},
		{
			name:        "empty block scalar followed by sibling",
			input:       "content: |\nother: " + `"a` + bs + `!b"` + "\n",
			wantContent: "",
			wantOther:   "a" + bs + "!b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sanitized := SanitizeDoubleQuoteEscapes([]byte(tt.input))
			var out doc
			if err := yamlv3.Unmarshal(sanitized, &out); err != nil {
				t.Fatalf("parse after sanitisation: %v\ninput:     %q\nsanitized: %q", err, tt.input, string(sanitized))
			}
			if tt.wantContent != "" && out.Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", out.Content, tt.wantContent)
			}
			if out.Other != tt.wantOther {
				t.Errorf("Other = %q, want %q", out.Other, tt.wantOther)
			}
		})
	}
}

// TestSanitizeDoubleQuoteEscapes_PipeNotBlockScalar verifies that '|' and '>'
// characters that do not open a block scalar are still processed normally.
func TestSanitizeDoubleQuoteEscapes_PipeNotBlockScalar(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantValue string
	}{
		{
			name:      "pipe inside plain scalar",
			input:     "content: a | b\nother: \"c\"\n",
			wantValue: "a | b",
		},
		{
			name:      "pipe inside double-quoted value with broken escape after",
			input:     "content: \"a | " + bs + "! b\"\n",
			wantValue: "a | " + bs + "! b",
		},
		{
			name:      "gt inside plain scalar",
			input:     "content: a > b\n",
			wantValue: "a > b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sanitized := SanitizeDoubleQuoteEscapes([]byte(tt.input))
			var out contentHolder
			if err := yamlv3.Unmarshal(sanitized, &out); err != nil {
				t.Fatalf("parse after sanitisation: %v\nsanitized: %q", err, string(sanitized))
			}
			if out.Content != tt.wantValue {
				t.Errorf("Content = %q, want %q", out.Content, tt.wantValue)
			}
		})
	}

	// "a:|" is a plain scalar, not a block scalar header (no space after ':').
	// The indented double-quoted value on the next line must still be
	// sanitised — misdetecting ":|" would skip it verbatim.
	glued := "a:|\n  next: " + `"x` + bs + `!y"` + "\n"
	sanitized := string(SanitizeDoubleQuoteEscapes([]byte(glued)))
	want := "a:|\n  next: " + `"x` + bs + bs + `!y"` + "\n"
	if sanitized != want {
		t.Errorf("glued colon-pipe: got %q, want %q", sanitized, want)
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
