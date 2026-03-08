package persona

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestFormatPersonaSection_EmptyHint(t *testing.T) {
	personas := map[string]model.PersonaConfig{
		"general": {Description: "General", Prompt: "Be helpful"},
	}
	result, found := FormatPersonaSection(personas, "")
	if !found {
		t.Error("expected found=true for empty hint")
	}
	if result != "" {
		t.Errorf("expected empty string for empty hint, got %q", result)
	}
}

func TestFormatPersonaSection_UnknownPersona(t *testing.T) {
	personas := map[string]model.PersonaConfig{
		"general": {Description: "General", Prompt: "Be helpful"},
	}
	result, found := FormatPersonaSection(personas, "nonexistent")
	if found {
		t.Error("expected found=false for unknown persona")
	}
	if result != "" {
		t.Errorf("expected empty string for unknown persona, got %q", result)
	}
}

func TestFormatPersonaSection_NilMap(t *testing.T) {
	result, found := FormatPersonaSection(nil, "general")
	if found {
		t.Error("expected found=false for nil map")
	}
	if result != "" {
		t.Errorf("expected empty string for nil map, got %q", result)
	}
}

func TestFormatPersonaSection_ValidPersona(t *testing.T) {
	personas := map[string]model.PersonaConfig{
		"quality-focused": {
			Description: "Quality focused",
			Prompt:      "Focus on code quality and testing.",
		},
	}
	result, found := FormatPersonaSection(personas, "quality-focused")
	if !found {
		t.Error("expected found=true for valid persona")
	}
	if !strings.Contains(result, "ペルソナ: quality-focused") {
		t.Error("missing persona name in section")
	}
	if !strings.Contains(result, "Focus on code quality and testing.") {
		t.Error("missing persona prompt in section")
	}
	if !strings.HasPrefix(result, "---\n") {
		t.Error("missing opening separator")
	}
	if !strings.Contains(result, "\n---\n\n") {
		t.Error("missing closing separator")
	}
}

func TestFormatPersonaSection_EmptyPrompt(t *testing.T) {
	personas := map[string]model.PersonaConfig{
		"empty": {Description: "Empty", Prompt: "  "},
	}
	result, found := FormatPersonaSection(personas, "empty")
	if !found {
		t.Error("expected found=true for existing persona with empty prompt")
	}
	if result != "" {
		t.Errorf("expected empty string for whitespace-only prompt, got %q", result)
	}
}

// --- FormatPersonaSectionWithFS tests ---

func TestFormatPersonaSectionWithFS_FileBasedPersona(t *testing.T) {
	fsys := fstest.MapFS{
		"persona/tester.md": &fstest.MapFile{
			Data: []byte("---\nname: tester\ndescription: \"Tester\"\n---\nTest all the things.\n"),
		},
	}
	personas := map[string]model.PersonaConfig{
		"tester": {Description: "Tester", File: "persona/tester.md"},
	}
	result, found := FormatPersonaSectionWithFS(fsys, personas, "tester")
	if !found {
		t.Error("expected found=true")
	}
	if !strings.Contains(result, "Test all the things.") {
		t.Errorf("expected file content in result, got %q", result)
	}
	if !strings.Contains(result, "ペルソナ: tester") {
		t.Error("missing persona name")
	}
}

func TestFormatPersonaSectionWithFS_FallbackToPrompt(t *testing.T) {
	fsys := fstest.MapFS{} // no files
	personas := map[string]model.PersonaConfig{
		"dev": {Description: "Dev", File: "persona/missing.md", Prompt: "Fallback prompt."},
	}
	result, found := FormatPersonaSectionWithFS(fsys, personas, "dev")
	if !found {
		t.Error("expected found=true")
	}
	if !strings.Contains(result, "Fallback prompt.") {
		t.Errorf("expected fallback prompt, got %q", result)
	}
}

func TestFormatPersonaSectionWithFS_EmptyHint(t *testing.T) {
	result, found := FormatPersonaSectionWithFS(nil, nil, "")
	if !found {
		t.Error("expected found=true for empty hint")
	}
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestFormatPersonaSectionWithFS_UnknownPersona(t *testing.T) {
	fsys := fstest.MapFS{}
	personas := map[string]model.PersonaConfig{
		"dev": {Description: "Dev", Prompt: "Dev prompt"},
	}
	result, found := FormatPersonaSectionWithFS(fsys, personas, "unknown")
	if found {
		t.Error("expected found=false for unknown persona")
	}
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestFormatPersonaSectionWithFS_NilFS(t *testing.T) {
	personas := map[string]model.PersonaConfig{
		"dev": {Description: "Dev", File: "persona/dev.md", Prompt: "Inline prompt."},
	}
	result, found := FormatPersonaSectionWithFS(nil, personas, "dev")
	if !found {
		t.Error("expected found=true")
	}
	if !strings.Contains(result, "Inline prompt.") {
		t.Errorf("expected inline prompt fallback with nil FS, got %q", result)
	}
}

func TestFormatPersonaSectionWithFS_PromptOnly(t *testing.T) {
	fsys := fstest.MapFS{}
	personas := map[string]model.PersonaConfig{
		"dev": {Description: "Dev", Prompt: "Pure inline."},
	}
	result, found := FormatPersonaSectionWithFS(fsys, personas, "dev")
	if !found {
		t.Error("expected found=true")
	}
	if !strings.Contains(result, "Pure inline.") {
		t.Errorf("expected inline prompt, got %q", result)
	}
}

func TestFormatPersonaSectionWithFS_InvalidPath(t *testing.T) {
	fsys := fstest.MapFS{
		"config.yaml": &fstest.MapFile{Data: []byte("secret data")},
	}
	personas := map[string]model.PersonaConfig{
		"evil": {Description: "Evil", File: "config.yaml", Prompt: "Fallback."},
	}
	result, found := FormatPersonaSectionWithFS(fsys, personas, "evil")
	if !found {
		t.Error("expected found=true")
	}
	// Should use fallback prompt, not read config.yaml (not under persona/ prefix)
	if !strings.Contains(result, "Fallback.") {
		t.Errorf("expected fallback for non-persona path, got %q", result)
	}
}

func TestStripFrontmatter(t *testing.T) {
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
			got := stripFrontmatter(tt.input)
			if got != tt.want {
				t.Errorf("stripFrontmatter(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
