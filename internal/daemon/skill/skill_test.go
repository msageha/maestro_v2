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

func TestReadSkill_WithFrontmatter(t *testing.T) {
	dir := t.TempDir()
	content := "---\nname: my-skill\ndescription: A test skill\nversion: 1.0.0\ntags:\n  - go\n  - test\npriority: 10\n---\nThis is the body.\nSecond line."
	writeSkillFile(t, dir, "my-skill", content)

	sc, err := ReadSkill(dir, "my-skill")
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
	if sc.Version != "1.0.0" {
		t.Errorf("expected Version '1.0.0', got %q", sc.Version)
	}
	if len(sc.Tags) != 2 || sc.Tags[0] != "go" || sc.Tags[1] != "test" {
		t.Errorf("unexpected Tags: %v", sc.Tags)
	}
	if sc.EffectivePriority() != 10 {
		t.Errorf("expected priority 10, got %d", sc.EffectivePriority())
	}
	if sc.Body != "This is the body.\nSecond line." {
		t.Errorf("unexpected body: %q", sc.Body)
	}
}

func TestReadSkill_NoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	body := "Just a plain markdown file.\nNo frontmatter here."
	writeSkillFile(t, dir, "plain", body)

	sc, err := ReadSkill(dir, "plain")
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

func TestReadSkill_NotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadSkill(dir, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent skill")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestListSkills_Multiple(t *testing.T) {
	dir := t.TempDir()
	writeSkillFile(t, dir, "skill-a", "---\nname: Alpha\n---\nBody A")
	writeSkillFile(t, dir, "skill-b", "---\nname: Beta\n---\nBody B")

	skills, err := ListSkills(dir)
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
	if !names["Alpha"] || !names["Beta"] {
		t.Errorf("expected Alpha and Beta, got %v", names)
	}
}

func TestListSkills_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	skills, err := ListSkills(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 0 {
		t.Fatalf("expected 0 skills, got %d", len(skills))
	}
}

func TestListSkills_NotExistDir(t *testing.T) {
	skills, err := ListSkills("/nonexistent/path/skills")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent dir, got %v", err)
	}
	if skills != nil {
		t.Fatalf("expected nil, got %v", skills)
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
	p1 := intPtr(1) // high priority (keep)
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

func TestReadSkill_RoleDirectoryPriority(t *testing.T) {
	// Verify that when both a role directory and share directory contain
	// the same skill name, reading from the role directory gets role-specific content.
	dir := t.TempDir()

	roleDir := filepath.Join(dir, "worker")
	shareDir := filepath.Join(dir, "share")

	writeSkillFile(t, roleDir, "my-skill", "---\nname: my-skill\ndescription: worker version\n---\nWorker body")
	writeSkillFile(t, shareDir, "my-skill", "---\nname: my-skill\ndescription: share version\n---\nShare body")

	// Reading from role directory should return the role-specific skill
	sc, err := ReadSkill(roleDir, "my-skill")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Description != "worker version" {
		t.Errorf("expected worker version, got %q", sc.Description)
	}
	if sc.Body != "Worker body" {
		t.Errorf("expected Worker body, got %q", sc.Body)
	}
}

func TestReadSkill_ShareFallback(t *testing.T) {
	// Verify that a skill only in the share directory can be read from there.
	dir := t.TempDir()

	roleDir := filepath.Join(dir, "worker")
	shareDir := filepath.Join(dir, "share")
	os.MkdirAll(roleDir, 0755)
	writeSkillFile(t, shareDir, "shared-skill", "---\nname: shared-skill\n---\nShared body")

	// Role directory has no skills, so ReadSkill on roleDir fails
	_, err := ReadSkill(roleDir, "shared-skill")
	if err == nil {
		t.Fatal("expected error reading from role dir without the skill")
	}

	// But reading from share directory succeeds
	sc, err := ReadSkill(shareDir, "shared-skill")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Body != "Shared body" {
		t.Errorf("expected Shared body, got %q", sc.Body)
	}
}

func TestListSkills_RoleDirectory(t *testing.T) {
	// Verify ListSkills works independently on role and share directories.
	dir := t.TempDir()

	workerDir := filepath.Join(dir, "worker")
	shareDir := filepath.Join(dir, "share")

	writeSkillFile(t, workerDir, "worker-only", "---\nname: Worker Only\n---\nW body")
	writeSkillFile(t, shareDir, "shared-a", "---\nname: Shared A\n---\nSA body")
	writeSkillFile(t, shareDir, "shared-b", "---\nname: Shared B\n---\nSB body")

	workerSkills, err := ListSkills(workerDir)
	if err != nil {
		t.Fatalf("ListSkills(worker): %v", err)
	}
	if len(workerSkills) != 1 {
		t.Errorf("expected 1 worker skill, got %d", len(workerSkills))
	}

	shareSkills, err := ListSkills(shareDir)
	if err != nil {
		t.Fatalf("ListSkills(share): %v", err)
	}
	if len(shareSkills) != 2 {
		t.Errorf("expected 2 share skills, got %d", len(shareSkills))
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
			name:    "empty content",
			content: "",
			wantErr: false,
			// empty content returns empty body, no error
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
// ReadSkillWithRole edge cases
// ---------------------------------------------------------------------------

func TestReadSkillWithRole_RoleSpecificPriority(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Set up all three locations with distinct descriptions
	writeSkillFile(t, filepath.Join(dir, "worker"), "my-skill", "---\ndescription: role-specific\n---\nRole body")
	writeSkillFile(t, filepath.Join(dir, "share"), "my-skill", "---\ndescription: shared\n---\nShare body")
	writeSkillFile(t, dir, "my-skill", "---\ndescription: flat\n---\nFlat body")

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

	// Only share and flat exist
	writeSkillFile(t, filepath.Join(dir, "share"), "my-skill", "---\ndescription: shared\n---\nShare body")
	writeSkillFile(t, dir, "my-skill", "---\ndescription: flat\n---\nFlat body")

	sc, err := ReadSkillWithRole(dir, "my-skill", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Description != "shared" {
		t.Errorf("expected shared, got %q", sc.Description)
	}
}

func TestReadSkillWithRole_FlatFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Only flat exists
	writeSkillFile(t, dir, "my-skill", "---\ndescription: flat\n---\nFlat body")

	sc, err := ReadSkillWithRole(dir, "my-skill", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Description != "flat" {
		t.Errorf("expected flat, got %q", sc.Description)
	}
}

func TestReadSkillWithRole_EmptyRole(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// With empty role, filepath.Join(dir, "", "my-skill") collapses to the flat
	// path, so the first candidate matches the flat entry before share is checked.
	writeSkillFile(t, filepath.Join(dir, "share"), "my-skill", "---\ndescription: shared\n---\nShare body")
	writeSkillFile(t, dir, "my-skill", "---\ndescription: flat\n---\nFlat body")

	sc, err := ReadSkillWithRole(dir, "my-skill", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Description != "flat" {
		t.Errorf("expected flat (collapsed empty-role path), got %q", sc.Description)
	}
}

func TestReadSkillWithRole_EmptyRoleShareOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// With empty role and no flat skill, share should be found
	writeSkillFile(t, filepath.Join(dir, "share"), "my-skill", "---\ndescription: shared\n---\nShare body")

	sc, err := ReadSkillWithRole(dir, "my-skill", "")
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

func intPtr(v int) *int {
	return &v
}
