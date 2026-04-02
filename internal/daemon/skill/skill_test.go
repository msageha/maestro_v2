package skill

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkillFile(t *testing.T, skillsDir, skillName, content string) {
	t.Helper()
	dir := filepath.Join(skillsDir, skillName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestFormatSkillSection_Multiple(t *testing.T) {
	skills := []SkillContent{
		{SkillMetadata: SkillMetadata{ID: "s1", Name: "Skill One"}, Body: "Body one"},
		{SkillMetadata: SkillMetadata{ID: "s2", Name: "Skill Two"}, Body: "Body two"},
	}
	result := FormatSkillSection(skills, 0)
	if !strings.Contains(result, "スキル: Skill One") {
		t.Error("missing Skill One header")
	}
	if !strings.Contains(result, "Body one") {
		t.Error("missing Body one")
	}
	if !strings.Contains(result, "スキル: Skill Two") {
		t.Error("missing Skill Two header")
	}
	if !strings.Contains(result, "Body two") {
		t.Error("missing Body two")
	}
}

func TestFormatSkillSection_MaxBodyChars(t *testing.T) {
	p1 := intPtr(1)  // high priority (keep)
	p2 := intPtr(50) // lower priority (drop first)

	skills := []SkillContent{
		{SkillMetadata: SkillMetadata{ID: "important", Name: "Important", Priority: p1}, Body: "Keep me"},
		{SkillMetadata: SkillMetadata{ID: "optional", Name: "Optional", Priority: p2}, Body: strings.Repeat("x", 1000)},
	}

	// Set a small budget that can't fit both
	result := FormatSkillSection(skills, 50)
	if !strings.Contains(result, "Important") {
		t.Error("expected high-priority skill to be retained")
	}
	if strings.Contains(result, "Optional") {
		t.Error("expected low-priority skill to be dropped")
	}
}

func TestFormatSkillSection_Empty(t *testing.T) {
	result := FormatSkillSection(nil, 0)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}

	result = FormatSkillSection([]SkillContent{}, 0)
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

// ---------------------------------------------------------------------------
// isValidIdentifier edge cases
// ---------------------------------------------------------------------------

func TestIsValidIdentifier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty string", "", false},
		{"dot", ".", false},
		{"dot-dot", "..", false},
		{"path traversal", "../evil", false},
		{"forward slash", "a/b", false},
		{"backslash", "a\\b", false},
		{"null byte", "ab\x00cd", false},
		{"valid simple", "my-skill", true},
		{"valid with dots", "skill.v2", true},
		{"valid underscore", "skill_1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isValidIdentifier(tt.input)
			if got != tt.want {
				t.Errorf("isValidIdentifier(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseFrontmatter edge cases
// ---------------------------------------------------------------------------

func TestParseFrontmatter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		content   string
		wantErr   bool
		errSubstr string
		wantBody  string
		wantName  string // optional: verify metadata parsing
	}{
		{
			name:     "empty content",
			content:  "",
			wantErr:  false,
			wantBody: "",
		},
		{
			name:      "unclosed delimiter",
			content:   "---\nname: test\nno closing here",
			wantErr:   true,
			errSubstr: "unclosed frontmatter",
		},
		{
			name:      "invalid YAML",
			content:   "---\nname: [\n---\nbody here",
			wantErr:   true,
			errSubstr: "invalid YAML",
		},
		{
			name:     "no frontmatter",
			content:  "Just plain text\nSecond line",
			wantErr:  false,
			wantBody: "Just plain text\nSecond line",
		},
		{
			name:     "empty frontmatter block",
			content:  "---\n---\nbody after empty frontmatter",
			wantErr:  false,
			wantBody: "body after empty frontmatter",
		},
		{
			name:      "only opening delimiter no content after",
			content:   "---\n",
			wantErr:   true,
			errSubstr: "unclosed frontmatter",
		},
		{
			name:     "delimiter with surrounding whitespace",
			content:  "  ---  \nname: ws-test\n  ---  \nbody ws",
			wantErr:  false,
			wantBody: "body ws",
			wantName: "ws-test",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			meta, body, err := parseFrontmatter(tt.content)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("expected error containing %q, got %q", tt.errSubstr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
			if tt.wantName != "" && meta.Name != tt.wantName {
				t.Errorf("meta.Name = %q, want %q", meta.Name, tt.wantName)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ReadSkillWithRole
// ---------------------------------------------------------------------------

func TestReadSkillWithRole_RoleSpecificPriority(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Set up role-specific and shared with distinct descriptions
	writeSkillFile(t, filepath.Join(dir, "worker"), "my-skill", "---\ndescription: role-specific\n---\nRole body")
	writeSkillFile(t, filepath.Join(dir, "share"), "my-skill", "---\ndescription: shared\n---\nShare body")

	sc, err := ReadSkillWithRole(dir, "my-skill", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Description != "role-specific" {
		t.Errorf("expected role-specific, got %q", sc.Description)
	}
}

func TestReadSkillWithRole_ShareFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Only share exists
	writeSkillFile(t, filepath.Join(dir, "share"), "my-skill", "---\ndescription: shared\n---\nShare body")

	sc, err := ReadSkillWithRole(dir, "my-skill", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Description != "shared" {
		t.Errorf("expected shared, got %q", sc.Description)
	}
}

func TestReadSkillWithRole_NonexistentSkill(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	_, err := ReadSkillWithRole(dir, "nonexistent", "worker")
	if err == nil {
		t.Fatal("expected error for nonexistent skill")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestReadSkillWithRole_InvalidSkillName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	_, err := ReadSkillWithRole(dir, "../evil", "worker")
	if err == nil {
		t.Fatal("expected error for invalid skill name")
	}
	if !strings.Contains(err.Error(), "invalid skill name") {
		t.Errorf("expected 'invalid skill name' error, got %v", err)
	}
}

func TestReadSkillWithRole_MalformedRoleSpecific(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Role-specific has bad frontmatter — should return error, not fall through to share
	writeSkillFile(t, filepath.Join(dir, "worker"), "my-skill", "---\nname: [\n---\nbad yaml")
	writeSkillFile(t, filepath.Join(dir, "share"), "my-skill", "---\ndescription: shared\n---\nShare body")

	_, err := ReadSkillWithRole(dir, "my-skill", "worker")
	if err == nil {
		t.Fatal("expected error for malformed role-specific skill")
	}
	if !strings.Contains(err.Error(), "frontmatter") {
		t.Errorf("expected frontmatter parse error, got %v", err)
	}
}

func TestReadSkillWithRole_NameFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Skill without name in frontmatter should use skillName as fallback
	writeSkillFile(t, filepath.Join(dir, "worker"), "my-skill", "---\ndescription: A test skill\nversion: 1.0.0\ntags:\n  - go\n  - test\npriority: 10\n---\nThis is the body.\nSecond line.")

	sc, err := ReadSkillWithRole(dir, "my-skill", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.ID != "my-skill" {
		t.Errorf("expected ID 'my-skill', got %q", sc.ID)
	}
	if sc.Name != "my-skill" {
		t.Errorf("expected Name 'my-skill', got %q", sc.Name)
	}
	if sc.Description != "A test skill" {
		t.Errorf("expected Description 'A test skill', got %q", sc.Description)
	}
	if sc.EffectivePriority() != 10 {
		t.Errorf("expected priority 10, got %d", sc.EffectivePriority())
	}
	if sc.Body != "This is the body.\nSecond line." {
		t.Errorf("unexpected body: %q", sc.Body)
	}
}

func TestReadSkillWithRole_NoFrontmatter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	body := "Just a plain markdown file.\nNo frontmatter here."
	writeSkillFile(t, filepath.Join(dir, "share"), "plain", body)

	sc, err := ReadSkillWithRole(dir, "plain", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Name != "plain" {
		t.Errorf("expected Name fallback to 'plain', got %q", sc.Name)
	}
	if sc.Body != body {
		t.Errorf("expected body to be entire content, got %q", sc.Body)
	}
	if sc.EffectivePriority() != defaultPriority {
		t.Errorf("expected default priority %d, got %d", defaultPriority, sc.EffectivePriority())
	}
}

func TestReadSkillWithRole_FrontmatterNameFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Directory name is "my-dir-name" but frontmatter name is "My Display Name"
	writeSkillFile(t, filepath.Join(dir, "worker"), "my-dir-name",
		"---\nname: My Display Name\ndescription: A skill\n---\nSkill body")

	// Look up by frontmatter name (not directory name)
	sc, err := ReadSkillWithRole(dir, "My Display Name", "worker")
	if err != nil {
		t.Fatalf("expected frontmatter name fallback to succeed, got: %v", err)
	}
	if sc.Name != "My Display Name" {
		t.Errorf("expected Name 'My Display Name', got %q", sc.Name)
	}
	if sc.ID != "my-dir-name" {
		t.Errorf("expected ID 'my-dir-name', got %q", sc.ID)
	}
	if sc.Body != "Skill body" {
		t.Errorf("unexpected body: %q", sc.Body)
	}
}

func TestReadSkillWithRole_FrontmatterNameFallback_SharedDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Skill only in share, referenced by frontmatter name
	writeSkillFile(t, filepath.Join(dir, "share"), "shared-dir",
		"---\nname: Shared Display\n---\nShared body")

	sc, err := ReadSkillWithRole(dir, "Shared Display", "worker")
	if err != nil {
		t.Fatalf("expected shared frontmatter name fallback to succeed, got: %v", err)
	}
	if sc.Name != "Shared Display" {
		t.Errorf("expected Name 'Shared Display', got %q", sc.Name)
	}
	if sc.ID != "shared-dir" {
		t.Errorf("expected ID 'shared-dir', got %q", sc.ID)
	}
}

func TestReadSkillWithRole_DirectoryNameTakesPriorityOverFrontmatterName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Directory name matches lookup — should use fast path, not scan
	writeSkillFile(t, filepath.Join(dir, "worker"), "exact-match",
		"---\nname: Different Name\n---\nBody")

	sc, err := ReadSkillWithRole(dir, "exact-match", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Fast path sets ID = skillName, not directory name scan
	if sc.ID != "exact-match" {
		t.Errorf("expected ID 'exact-match', got %q", sc.ID)
	}
}

// ---------------------------------------------------------------------------
// ReadAllSkillsForRole
// ---------------------------------------------------------------------------

func TestReadAllSkillsForRole_RoleAndShared(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeSkillFile(t, filepath.Join(dir, "planner"), "plan-skill", "---\nname: Plan Skill\n---\nPlanner body")
	writeSkillFile(t, filepath.Join(dir, "share"), "shared-skill", "---\nname: Shared Skill\n---\nShared body")

	skills, err := ReadAllSkillsForRole(dir, "planner", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(skills))
	}

	names := map[string]bool{}
	for _, s := range skills {
		names[s.Name] = true
	}
	if !names["Plan Skill"] {
		t.Error("expected Plan Skill to be included")
	}
	if !names["Shared Skill"] {
		t.Error("expected Shared Skill to be included")
	}
}

func TestReadAllSkillsForRole_RoleOverridesShared(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Same skill name in both role-specific and share — role-specific wins
	writeSkillFile(t, filepath.Join(dir, "orchestrator"), "my-skill", "---\nname: Role Version\n---\nRole body")
	writeSkillFile(t, filepath.Join(dir, "share"), "my-skill", "---\nname: Share Version\n---\nShare body")

	skills, err := ReadAllSkillsForRole(dir, "orchestrator", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill (deduped), got %d", len(skills))
	}
	if skills[0].Name != "Role Version" {
		t.Errorf("expected role-specific version, got %q", skills[0].Name)
	}
	if skills[0].Body != "Role body" {
		t.Errorf("expected role body, got %q", skills[0].Body)
	}
}

func TestReadAllSkillsForRole_EmptyDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	skills, err := ReadAllSkillsForRole(dir, "planner", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("expected 0 skills, got %d", len(skills))
	}
}

func TestReadAllSkillsForRole_SkipsMalformed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeSkillFile(t, filepath.Join(dir, "planner"), "good-skill", "---\nname: Good\n---\nGood body")
	writeSkillFile(t, filepath.Join(dir, "planner"), "bad-skill", "---\nname: [\n---\nbad yaml")

	skills, err := ReadAllSkillsForRole(dir, "planner", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill (bad one skipped), got %d", len(skills))
	}
	if skills[0].Name != "Good" {
		t.Errorf("expected Good skill, got %q", skills[0].Name)
	}
}

func TestReadAllSkillsForRole_BrokenRoleSpecificBlocksSharedFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Role-specific is broken, shared has same name — shared should NOT be used
	writeSkillFile(t, filepath.Join(dir, "planner"), "my-skill", "---\nname: [\n---\nbad yaml")
	writeSkillFile(t, filepath.Join(dir, "share"), "my-skill", "---\nname: Shared Version\n---\nShared body")

	skills, err := ReadAllSkillsForRole(dir, "planner", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, s := range skills {
		if s.Name == "Shared Version" {
			t.Error("broken role-specific skill should block shared fallback for same name")
		}
	}
}

func intPtr(v int) *int {
	return &v
}
