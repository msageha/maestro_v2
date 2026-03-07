package personas

import (
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/model"
)

func TestResolvePersonaSection_EmptyHint(t *testing.T) {
	personas := map[string]model.PersonaConfig{
		"reviewer": {Prompt: "You are a code reviewer."},
	}
	section, found := ResolvePersonaSection("", personas)
	if found {
		t.Fatal("expected found=false for empty persona_hint")
	}
	if section != "" {
		t.Fatalf("expected empty section, got %q", section)
	}
}

func TestResolvePersonaSection_NilMap(t *testing.T) {
	section, found := ResolvePersonaSection("reviewer", nil)
	if found {
		t.Fatal("expected found=false for nil personas map")
	}
	if section != "" {
		t.Fatalf("expected empty section, got %q", section)
	}
}

func TestResolvePersonaSection_EmptyMap(t *testing.T) {
	section, found := ResolvePersonaSection("reviewer", map[string]model.PersonaConfig{})
	if found {
		t.Fatal("expected found=false for empty personas map")
	}
	if section != "" {
		t.Fatalf("expected empty section, got %q", section)
	}
}

func TestResolvePersonaSection_UnknownPersona(t *testing.T) {
	personas := map[string]model.PersonaConfig{
		"reviewer": {Prompt: "You are a code reviewer."},
	}
	section, found := ResolvePersonaSection("unknown-persona", personas)
	if found {
		t.Fatal("expected found=false for unknown persona")
	}
	if section != "" {
		t.Fatalf("expected empty section, got %q", section)
	}
}

func TestResolvePersonaSection_ValidPersona(t *testing.T) {
	personas := map[string]model.PersonaConfig{
		"reviewer": {Description: "Code reviewer", Prompt: "You are a meticulous code reviewer."},
	}
	section, found := ResolvePersonaSection("reviewer", personas)
	if !found {
		t.Fatal("expected found=true for valid persona")
	}
	if !strings.Contains(section, "You are a meticulous code reviewer.") {
		t.Fatalf("expected prompt in section, got %q", section)
	}
	if !strings.Contains(section, "ペルソナ指示:") {
		t.Fatalf("expected section header, got %q", section)
	}
	if !strings.HasPrefix(section, "\n\n---\n") {
		t.Fatalf("expected separator prefix, got %q", section)
	}
}

func TestResolvePersonaSection_WhitespaceOnlyPrompt(t *testing.T) {
	personas := map[string]model.PersonaConfig{
		"empty": {Prompt: "   \t\n  "},
	}
	section, found := ResolvePersonaSection("empty", personas)
	if found {
		t.Fatal("expected found=false for whitespace-only prompt")
	}
	if section != "" {
		t.Fatalf("expected empty section, got %q", section)
	}
}

func TestResolvePersonaSection_MultilinePrompt(t *testing.T) {
	prompt := "You are a senior engineer.\nFocus on:\n- Performance\n- Security"
	personas := map[string]model.PersonaConfig{
		"senior": {Prompt: prompt},
	}
	section, found := ResolvePersonaSection("senior", personas)
	if !found {
		t.Fatal("expected found=true for multiline prompt")
	}
	if !strings.Contains(section, "- Performance") {
		t.Error("multiline prompt content not preserved")
	}
	if !strings.Contains(section, "- Security") {
		t.Error("multiline prompt content not preserved")
	}
}

func TestFormatPersonaSection(t *testing.T) {
	got := FormatPersonaSection("Be thorough in your review.")
	want := "\n\n---\nペルソナ指示:\nBe thorough in your review.\n"
	if got != want {
		t.Errorf("FormatPersonaSection mismatch\nwant: %q\n got: %q", want, got)
	}
}

func TestResolvePersonaSection_CoexistsWithLearnings(t *testing.T) {
	personas := map[string]model.PersonaConfig{
		"reviewer": {Prompt: "You are a code reviewer."},
	}
	personaSection, found := ResolvePersonaSection("reviewer", personas)
	if !found {
		t.Fatal("expected persona to be found")
	}

	learningsSection := learnings.FormatLearningsSection([]model.Learning{
		{Content: "Always run tests"},
	})

	content := "Original task content"
	combined := content + personaSection + learningsSection

	if !strings.Contains(combined, "Original task content") {
		t.Error("original content missing")
	}
	if !strings.Contains(combined, "ペルソナ指示:") {
		t.Error("persona section missing")
	}
	if !strings.Contains(combined, "参考: 過去の学習知見") {
		t.Error("learnings section missing")
	}
	if !strings.Contains(combined, "Always run tests") {
		t.Error("learnings content missing")
	}
}
