package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatPersonaSection_FileBasedPersona(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	personaDir := filepath.Join(dir, "persona")
	os.MkdirAll(personaDir, 0o755)
	os.WriteFile(filepath.Join(personaDir, "tester.md"), []byte("---\nname: tester\ndescription: \"Tester\"\n---\nTest all the things.\n"), 0o644)

	result := FormatPersonaSection("tester", dir)
	if !strings.Contains(result, "Test all the things.") {
		t.Errorf("expected file content in result, got %q", result)
	}
	if !strings.Contains(result, "ペルソナ: tester") {
		t.Error("missing persona name")
	}
}

func TestFormatPersonaSection_MissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "persona"), 0o755)

	result := FormatPersonaSection("dev", dir)
	if result != "" {
		t.Errorf("expected empty result when file is missing, got %q", result)
	}
}

func TestFormatPersonaSection_EmptyHint(t *testing.T) {
	t.Parallel()
	result := FormatPersonaSection("", "/some/dir")
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestFormatPersonaSection_EmptyMaestroDir(t *testing.T) {
	t.Parallel()
	result := FormatPersonaSection("dev", "")
	if result != "" {
		t.Errorf("expected empty result with no maestroDir, got %q", result)
	}
}

func TestFormatPersonaSection_EmptyFileBody(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	personaDir := filepath.Join(dir, "persona")
	os.MkdirAll(personaDir, 0o755)
	os.WriteFile(filepath.Join(personaDir, "dev.md"), []byte("---\nname: dev\n---\n  \n"), 0o644)

	result := FormatPersonaSection("dev", dir)
	if result != "" {
		t.Errorf("expected empty result for whitespace-only body, got %q", result)
	}
}

func TestFormatPersonaSection_PathTraversal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	personaDir := filepath.Join(dir, "persona")
	os.MkdirAll(personaDir, 0o755)
	// Create a file that would be reachable via traversal
	os.WriteFile(filepath.Join(dir, "secret.md"), []byte("secret data"), 0o644)

	tests := []struct {
		name string
		hint string
	}{
		{"dot-dot-slash", "../secret"},
		{"dot-dot", ".."},
		{"single-dot", "."},
		{"slash", "foo/bar"},
		{"backslash", "foo\\bar"},
		{"null-byte", "foo\x00bar"},
		{"embedded-dot-dot", "foo..bar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := FormatPersonaSection(tt.hint, dir)
			if result != "" {
				t.Errorf("expected empty result for hint %q, got %q", tt.hint, result)
			}
		})
	}
}

func TestStripFrontmatter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "with frontmatter",
			input: "---\nname: test\n---\nBody content.\n",
			want:  "Body content.\n",
		},
		{
			name:  "no frontmatter",
			input: "Just plain text.",
			want:  "Just plain text.",
		},
		{
			name:  "empty frontmatter",
			input: "---\n---\nBody.\n",
			want:  "Body.\n",
		},
		{
			name:  "unclosed frontmatter",
			input: "---\nname: test\nno closing",
			want:  "---\nname: test\nno closing",
		},
		{
			name:  "dashes in content not treated as fence",
			input: "---\nname: test\n---\n----\nBody.\n",
			want:  "----\nBody.\n",
		},
		{
			name:  "partial dashes not treated as fence",
			input: "---\nname: test\n---foo\n---\nBody.\n",
			want:  "Body.\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stripFrontmatter(tt.input)
			if got != tt.want {
				t.Errorf("stripFrontmatter(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
