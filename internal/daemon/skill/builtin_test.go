package skill

import (
	"testing"

	"github.com/msageha/maestro_v2/templates"
)

func TestReadBuiltinSkills_FromTemplatesFS(t *testing.T) {
	skills, err := ReadBuiltinSkills(templates.FS)
	if err != nil {
		t.Fatalf("ReadBuiltinSkills failed: %v", err)
	}

	expectedSkills := map[string]bool{
		"structured-ai-communication": false,
		"self-evaluation-patterns":    false,
		"context-window-efficiency":   false,
		"error-diagnosis-patterns":    false,
	}

	for _, s := range skills {
		if _, ok := expectedSkills[s.ID]; ok {
			expectedSkills[s.ID] = true
		}
	}

	for name, found := range expectedSkills {
		if !found {
			t.Errorf("expected builtin skill %q not found", name)
		}
	}

	if len(skills) != 4 {
		t.Errorf("expected 4 builtin skills, got %d", len(skills))
	}
}

func TestReadBuiltinSkills_MetadataValid(t *testing.T) {
	skills, err := ReadBuiltinSkills(templates.FS)
	if err != nil {
		t.Fatalf("ReadBuiltinSkills failed: %v", err)
	}

	for _, s := range skills {
		t.Run(s.ID, func(t *testing.T) {
			if s.Name == "" {
				t.Error("name is empty")
			}
			if s.Description == "" {
				t.Error("description is empty")
			}
			if s.Version == "" {
				t.Error("version is empty")
			}
			if len(s.AppliesTo) == 0 {
				t.Error("applies_to is empty")
			}

			// Check applies_to contains all three roles
			roles := map[string]bool{}
			for _, r := range s.AppliesTo {
				roles[r] = true
			}
			for _, role := range []string{"orchestrator", "planner", "worker"} {
				if !roles[role] {
					t.Errorf("applies_to missing %q", role)
				}
			}

			if len(s.Tags) == 0 {
				t.Error("tags is empty")
			}
			if s.Priority == nil {
				t.Error("priority is nil")
			}
			if s.Body == "" {
				t.Error("body is empty")
			}
		})
	}
}

func TestReadBuiltinSkillByName(t *testing.T) {
	s, err := ReadBuiltinSkillByName(templates.FS, "structured-ai-communication")
	if err != nil {
		t.Fatalf("ReadBuiltinSkillByName failed: %v", err)
	}
	if s.ID != "structured-ai-communication" {
		t.Errorf("expected ID 'structured-ai-communication', got %q", s.ID)
	}
	if s.Name != "structured-ai-communication" {
		t.Errorf("expected Name 'structured-ai-communication', got %q", s.Name)
	}
}

func TestReadBuiltinSkillByName_NotExist(t *testing.T) {
	_, err := ReadBuiltinSkillByName(templates.FS, "nonexistent-skill")
	if err == nil {
		t.Fatal("expected error for nonexistent skill")
	}
}
