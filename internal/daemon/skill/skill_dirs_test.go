package skill

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newCaptureLogger returns a slog.Logger writing to the returned buffer so
// tests can assert on emitted WARN records.
func newCaptureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// ---------------------------------------------------------------------------
// ResolveSearchDirs
// ---------------------------------------------------------------------------

func TestResolveSearchDirs(t *testing.T) {
	t.Parallel()
	projectRoot := t.TempDir()
	bundledDir := filepath.Join(projectRoot, ".maestro", "skills")

	relDir := filepath.Join(projectRoot, "team-skills")
	if err := os.MkdirAll(relDir, 0755); err != nil {
		t.Fatal(err)
	}
	absDir := t.TempDir()

	notADir := filepath.Join(projectRoot, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		extra    []string
		want     []string
		wantWarn string
	}{
		{
			name:  "no extra dirs yields bundled only",
			extra: nil,
			want:  []string{bundledDir},
		},
		{
			name:  "relative entry resolved against project root, before bundled",
			extra: []string{"team-skills"},
			want:  []string{relDir, bundledDir},
		},
		{
			name:  "absolute entry kept as-is",
			extra: []string{absDir},
			want:  []string{absDir, bundledDir},
		},
		{
			name:     "missing directory skipped with warn",
			extra:    []string{"does-not-exist", "team-skills"},
			want:     []string{relDir, bundledDir},
			wantWarn: "skipping missing directory",
		},
		{
			name:     "non-directory entry skipped with warn",
			extra:    []string{"not-a-dir", "team-skills"},
			want:     []string{relDir, bundledDir},
			wantWarn: "skipping non-directory entry",
		},
		{
			name:     "empty entry skipped with warn",
			extra:    []string{"", "team-skills"},
			want:     []string{relDir, bundledDir},
			wantWarn: "skipping empty entry",
		},
		{
			name:     "duplicate entry skipped with warn",
			extra:    []string{"team-skills", "team-skills"},
			want:     []string{relDir, bundledDir},
			wantWarn: "skipping duplicate entry",
		},
		{
			name:  "list order preserved",
			extra: []string{absDir, "team-skills"},
			want:  []string{absDir, relDir, bundledDir},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, buf := newCaptureLogger()
			got := ResolveSearchDirs(tt.extra, projectRoot, bundledDir, logger)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("dirs[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
			if tt.wantWarn != "" && !strings.Contains(buf.String(), tt.wantWarn) {
				t.Errorf("expected warn containing %q, got log: %s", tt.wantWarn, buf.String())
			}
			if tt.wantWarn == "" && buf.Len() != 0 {
				t.Errorf("expected no log output, got: %s", buf.String())
			}
		})
	}
}

func TestResolveSearchDirs_BundledNotDuplicatedWhenListedAsExtra(t *testing.T) {
	t.Parallel()
	projectRoot := t.TempDir()
	bundledDir := filepath.Join(projectRoot, ".maestro", "skills")
	if err := os.MkdirAll(bundledDir, 0755); err != nil {
		t.Fatal(err)
	}

	got := ResolveSearchDirs([]string{filepath.Join(".maestro", "skills")}, projectRoot, bundledDir, nil)
	if len(got) != 1 || got[0] != bundledDir {
		t.Errorf("expected bundled dir exactly once, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// ReadSkillWithRoleDirs
// ---------------------------------------------------------------------------

func TestReadSkillWithRoleDirs_ExtraDirWinsOverBundled(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	bundled := t.TempDir()

	writeSkillFile(t, filepath.Join(extra, "worker"), "my-skill", "---\ndescription: extra\n---\nExtra body")
	writeSkillFile(t, filepath.Join(bundled, "worker"), "my-skill", "---\ndescription: bundled\n---\nBundled body")

	logger, buf := newCaptureLogger()
	sc, err := ReadSkillWithRoleDirs([]string{extra, bundled}, "my-skill", "worker", logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Description != "extra" {
		t.Errorf("expected extra-dir copy to win, got %q", sc.Description)
	}
	if !strings.Contains(buf.String(), "skill name conflict") {
		t.Errorf("expected conflict warn, got log: %s", buf.String())
	}
	if !strings.Contains(buf.String(), filepath.Join(bundled, "worker", "my-skill", "SKILL.md")) {
		t.Errorf("expected shadowed bundled path in warn, got log: %s", buf.String())
	}
}

func TestReadSkillWithRoleDirs_BundledRoleBeatsExtraShare(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	bundled := t.TempDir()

	// Role scope always beats share scope, even across source directories.
	writeSkillFile(t, filepath.Join(extra, "share"), "my-skill", "---\ndescription: extra-share\n---\nBody")
	writeSkillFile(t, filepath.Join(bundled, "worker"), "my-skill", "---\ndescription: bundled-role\n---\nBody")

	logger, buf := newCaptureLogger()
	sc, err := ReadSkillWithRoleDirs([]string{extra, bundled}, "my-skill", "worker", logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Description != "bundled-role" {
		t.Errorf("expected bundled role-scope copy to win, got %q", sc.Description)
	}
	// Cross-scope shadowing is the documented fallback design, not a conflict.
	if strings.Contains(buf.String(), "skill name conflict") {
		t.Errorf("unexpected conflict warn for cross-scope duplicate: %s", buf.String())
	}
}

func TestReadSkillWithRoleDirs_FallsBackToBundled(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	bundled := t.TempDir()

	writeSkillFile(t, filepath.Join(bundled, "worker"), "bundled-only", "---\ndescription: bundled\n---\nBody")

	sc, err := ReadSkillWithRoleDirs([]string{extra, bundled}, "bundled-only", "worker", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sc.Description != "bundled" {
		t.Errorf("expected bundled copy, got %q", sc.Description)
	}
}

func TestReadSkillWithRoleDirs_FrontmatterNameFallbackAcrossDirs(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	bundled := t.TempDir()

	writeSkillFile(t, filepath.Join(extra, "worker"), "design-system",
		"---\nname: Design System\n---\nDesign body")

	sc, err := ReadSkillWithRoleDirs([]string{extra, bundled}, "Design System", "worker", nil)
	if err != nil {
		t.Fatalf("expected frontmatter name fallback across dirs, got: %v", err)
	}
	if sc.ID != "design-system" {
		t.Errorf("expected ID 'design-system', got %q", sc.ID)
	}
	if sc.Body != "Design body" {
		t.Errorf("unexpected body: %q", sc.Body)
	}
}

func TestReadSkillWithRoleDirs_NotFound(t *testing.T) {
	t.Parallel()
	_, err := ReadSkillWithRoleDirs([]string{t.TempDir(), t.TempDir()}, "missing", "worker", nil)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListSkillsWithRoleDirs
// ---------------------------------------------------------------------------

func TestListSkillsWithRoleDirs_MergesAndDetectsConflict(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	bundled := t.TempDir()

	writeSkillFile(t, filepath.Join(extra, "worker"), "team-skill", "---\nname: Team Skill\n---\nBody")
	writeSkillFile(t, filepath.Join(extra, "worker"), "dup-skill", "---\ndescription: extra\n---\nBody")
	writeSkillFile(t, filepath.Join(bundled, "worker"), "dup-skill", "---\ndescription: bundled\n---\nBody")
	writeSkillFile(t, filepath.Join(bundled, "worker"), "bundled-skill", "---\nname: Bundled Skill\n---\nBody")

	logger, buf := newCaptureLogger()
	skills, err := ListSkillsWithRoleDirs([]string{extra, bundled}, "worker", logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 3 {
		t.Fatalf("expected 3 skills (dup deduped), got %d", len(skills))
	}
	byID := map[string]Metadata{}
	for _, s := range skills {
		byID[s.ID] = s
	}
	if _, ok := byID["team-skill"]; !ok {
		t.Error("expected team-skill from extra dir to be listed")
	}
	if _, ok := byID["bundled-skill"]; !ok {
		t.Error("expected bundled-skill to be listed")
	}
	if got := byID["dup-skill"].Description; got != "extra" {
		t.Errorf("expected extra-dir copy of dup-skill to win, got %q", got)
	}
	if !strings.Contains(buf.String(), "skill name conflict") {
		t.Errorf("expected conflict warn, got log: %s", buf.String())
	}
}

func TestListSkillsWithRoleDirs_BrokenHighPrecedenceBlocksLower(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	bundled := t.TempDir()

	writeSkillFile(t, filepath.Join(extra, "worker"), "my-skill", "---\nname: [\n---\nbad yaml")
	writeSkillFile(t, filepath.Join(bundled, "worker"), "my-skill", "---\nname: Bundled\n---\nBody")

	skills, err := ListSkillsWithRoleDirs([]string{extra, bundled}, "worker", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The broken extra-dir copy must block the bundled copy so the listing
	// matches what resolution (which fails on the broken winner) would do.
	if len(skills) != 0 {
		t.Errorf("expected 0 skills, got %d: %+v", len(skills), skills)
	}
}

func TestListSkillsWithRoleDirs_EmptyRole(t *testing.T) {
	t.Parallel()
	skills, err := ListSkillsWithRoleDirs([]string{t.TempDir()}, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("expected no skills for empty role, got %d", len(skills))
	}
}

// ---------------------------------------------------------------------------
// ReadAllSkillsForRoleDirs
// ---------------------------------------------------------------------------

func TestReadAllSkillsForRoleDirs_PrecedenceAndMerge(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	bundled := t.TempDir()

	// Same-scope duplicate: extra wins, conflict warned.
	writeSkillFile(t, filepath.Join(extra, "worker"), "dup-skill", "---\nname: Extra Version\n---\nExtra body")
	writeSkillFile(t, filepath.Join(bundled, "worker"), "dup-skill", "---\nname: Bundled Version\n---\nBundled body")
	// Cross-scope duplicate: role wins over share, no conflict warn expected
	// for this pair (checked implicitly via the single warn line for dup-skill).
	writeSkillFile(t, filepath.Join(extra, "share"), "shared-skill", "---\nname: Extra Shared\n---\nShared body")
	writeSkillFile(t, filepath.Join(bundled, "worker"), "shared-skill", "---\nname: Bundled Role\n---\nRole body")

	logger, buf := newCaptureLogger()
	skills, err := ReadAllSkillsForRoleDirs([]string{extra, bundled}, "worker", logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byID := map[string]Content{}
	for _, s := range skills {
		byID[s.ID] = s
	}
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d: %+v", len(skills), skills)
	}
	if got := byID["dup-skill"].Name; got != "Extra Version" {
		t.Errorf("expected extra-dir copy of dup-skill to win, got %q", got)
	}
	if got := byID["shared-skill"].Name; got != "Bundled Role" {
		t.Errorf("expected role-scope copy of shared-skill to win over extra share, got %q", got)
	}
	if !strings.Contains(buf.String(), "skill name conflict") {
		t.Errorf("expected conflict warn for dup-skill, got log: %s", buf.String())
	}
	if strings.Count(buf.String(), "skill name conflict") != 1 {
		t.Errorf("expected exactly one conflict warn (cross-scope shadowing is not a conflict), got log: %s", buf.String())
	}
}

func TestReadAllSkillsForRoleDirs_ShareScopeMergedAcrossDirs(t *testing.T) {
	t.Parallel()
	extra := t.TempDir()
	bundled := t.TempDir()

	writeSkillFile(t, filepath.Join(extra, "share"), "extra-shared", "---\nname: Extra Shared\n---\nBody")
	writeSkillFile(t, filepath.Join(bundled, "share"), "bundled-shared", "---\nname: Bundled Shared\n---\nBody")

	// Empty role scans only the share scope (dispatch auto-inject path).
	skills, err := ReadAllSkillsForRoleDirs([]string{extra, bundled}, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(skills) != 2 {
		t.Fatalf("expected 2 shared skills, got %d", len(skills))
	}
}
