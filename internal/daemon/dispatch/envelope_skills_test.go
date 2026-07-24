package dispatch

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// writeDispatchSkill creates <root>/<scope>/<name>/SKILL.md with the given content.
func writeDispatchSkill(t *testing.T, root, scope, name, content string) {
	t.Helper()
	dir := filepath.Join(root, scope, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func newSkillsTestDispatcher(t *testing.T, extraDirs []string) (*Dispatcher, string) {
	t.Helper()
	projectRoot := t.TempDir()
	maestroDir := filepath.Join(projectRoot, ".maestro")
	if err := os.MkdirAll(filepath.Join(maestroDir, "skills"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := model.Config{
		Skills: model.SkillsConfig{
			Enabled:   true,
			ExtraDirs: extraDirs,
		},
	}
	disp := New(maestroDir, cfg, log.New(io.Discard, "", 0), core.LogLevelDebug, nil, core.RealClock{})
	return disp, projectRoot
}

func TestBuildSkillsSection_ExtraDirSkillResolved(t *testing.T) {
	t.Parallel()
	disp, projectRoot := newSkillsTestDispatcher(t, []string{"team-skills"})

	writeDispatchSkill(t, filepath.Join(projectRoot, "team-skills"), "worker", "design-system",
		"---\nname: Design System\n---\nTeam design-system body")

	section, err := disp.buildSkillsSection([]string{"design-system"}, "task_x", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(section, "Team design-system body") {
		t.Errorf("expected extra-dir skill body in section, got %q", section)
	}
}

func TestBuildSkillsSection_ExtraDirShadowsBundled(t *testing.T) {
	t.Parallel()
	disp, projectRoot := newSkillsTestDispatcher(t, []string{"team-skills"})

	writeDispatchSkill(t, filepath.Join(projectRoot, "team-skills"), "worker", "tdd",
		"---\nname: TDD\n---\nTeam TDD body")
	writeDispatchSkill(t, filepath.Join(projectRoot, ".maestro", "skills"), "worker", "tdd",
		"---\nname: TDD\n---\nBundled TDD body")

	section, err := disp.buildSkillsSection([]string{"tdd"}, "task_x", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(section, "Team TDD body") {
		t.Errorf("expected extra-dir copy to win, got %q", section)
	}
	if strings.Contains(section, "Bundled TDD body") {
		t.Errorf("bundled copy must be shadowed, got %q", section)
	}
}

func TestBuildSkillsSection_MissingExtraDirFallsBackToBundled(t *testing.T) {
	t.Parallel()
	disp, projectRoot := newSkillsTestDispatcher(t, []string{"does-not-exist"})

	writeDispatchSkill(t, filepath.Join(projectRoot, ".maestro", "skills"), "worker", "tdd",
		"---\nname: TDD\n---\nBundled TDD body")

	section, err := disp.buildSkillsSection([]string{"tdd"}, "task_x", "worker")
	if err != nil {
		t.Fatalf("missing extra dir must not fail dispatch: %v", err)
	}
	if !strings.Contains(section, "Bundled TDD body") {
		t.Errorf("expected bundled skill body, got %q", section)
	}
}

func TestBuildSkillsSection_SharedSkillsAutoInjectedFromExtraDir(t *testing.T) {
	t.Parallel()
	disp, projectRoot := newSkillsTestDispatcher(t, []string{"team-skills"})

	writeDispatchSkill(t, filepath.Join(projectRoot, "team-skills"), "share", "team-conventions",
		"---\nname: Team Conventions\n---\nTeam conventions body")
	writeDispatchSkill(t, filepath.Join(projectRoot, ".maestro", "skills"), "share", "bundled-shared",
		"---\nname: Bundled Shared\n---\nBundled shared body")

	// No skill_refs: only the auto-injected shared skills should appear.
	section, err := disp.buildSkillsSection(nil, "task_x", "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(section, "Team conventions body") {
		t.Errorf("expected extra-dir shared skill auto-injected, got %q", section)
	}
	if !strings.Contains(section, "Bundled shared body") {
		t.Errorf("expected bundled shared skill auto-injected, got %q", section)
	}
}
