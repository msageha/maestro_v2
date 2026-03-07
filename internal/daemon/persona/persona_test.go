package persona

import (
	"strings"
	"testing"

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
