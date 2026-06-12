package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/featuregate"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/templates"
)

func TestRun_CreatesDirectoryStructure(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	base := filepath.Join(projectDir, ".maestro")

	// Verify directories exist
	expectedDirs := []string{
		"queue",
		"results",
		"state/commands",
		"locks",
		"logs",
		"dead_letters",
		"quarantine",
		"instructions",
	}
	for _, d := range expectedDirs {
		path := filepath.Join(base, d)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("directory %s does not exist: %v", d, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", d)
		}
	}
}

func TestRun_CopiesTemplateFiles(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	base := filepath.Join(projectDir, ".maestro")

	// Verify template files exist and are non-empty
	templateFiles := []string{
		"maestro.md",
		"dashboard.md",
		"config.yaml",
		"instructions/orchestrator.md",
		"instructions/planner.md",
		"instructions/worker.md",
	}
	for _, f := range templateFiles {
		path := filepath.Join(base, f)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("file %s does not exist: %v", f, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("file %s is empty", f)
		}
	}
}

func TestRun_GeneratesWorkerFiles(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	base := filepath.Join(projectDir, ".maestro")

	// Default worker count is 4
	for i := 1; i <= 4; i++ {
		queueFile := filepath.Join(base, "queue", workerFileName(i))
		resultFile := filepath.Join(base, "results", workerFileName(i))

		// Verify queue worker file
		data, err := os.ReadFile(queueFile)
		if err != nil {
			t.Errorf("queue worker%d: %v", i, err)
			continue
		}
		var qf map[string]any
		if err := yaml.Unmarshal(data, &qf); err != nil {
			t.Fatalf("unmarshal queue worker%d: %v", i, err)
		}
		if qf["file_type"] != "queue_task" {
			t.Errorf("queue worker%d file_type: got %v", i, qf["file_type"])
		}

		// Verify results worker file
		data, err = os.ReadFile(resultFile)
		if err != nil {
			t.Errorf("results worker%d: %v", i, err)
			continue
		}
		if err := yaml.Unmarshal(data, &qf); err != nil {
			t.Fatalf("unmarshal results worker%d: %v", i, err)
		}
		if qf["file_type"] != "result_task" {
			t.Errorf("results worker%d file_type: got %v", i, qf["file_type"])
		}
	}

	// Verify planner and orchestrator queue files
	plannerQ, _ := os.ReadFile(filepath.Join(base, "queue", "planner.yaml"))
	var pq map[string]any
	if err := yaml.Unmarshal(plannerQ, &pq); err != nil {
		t.Fatalf("unmarshal planner queue: %v", err)
	}
	if pq["file_type"] != "queue_command" {
		t.Errorf("queue planner file_type: got %v", pq["file_type"])
	}

	orchQ, _ := os.ReadFile(filepath.Join(base, "queue", "orchestrator.yaml"))
	var oq map[string]any
	if err := yaml.Unmarshal(orchQ, &oq); err != nil {
		t.Fatalf("unmarshal orchestrator queue: %v", err)
	}
	if oq["file_type"] != "queue_notification" {
		t.Errorf("queue orchestrator file_type: got %v", oq["file_type"])
	}

	// Verify planner results file
	plannerR, _ := os.ReadFile(filepath.Join(base, "results", "planner.yaml"))
	var pr map[string]any
	if err := yaml.Unmarshal(plannerR, &pr); err != nil {
		t.Fatalf("unmarshal planner results: %v", err)
	}
	if pr["file_type"] != "result_command" {
		t.Errorf("results planner file_type: got %v", pr["file_type"])
	}
}

func workerFileName(n int) string {
	return fmt.Sprintf("worker%d.yaml", n)
}

func TestRun_AutoFillsConfig(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	base := filepath.Join(projectDir, ".maestro")
	data, err := os.ReadFile(filepath.Join(base, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}

	var cfg model.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config.yaml: %v", err)
	}

	if cfg.Project.Name != "myproject" {
		t.Errorf("project.name: got %q, want %q", cfg.Project.Name, "myproject")
	}
	if cfg.Maestro.ProjectRoot == "" {
		t.Error("maestro.project_root is empty")
	}
	if cfg.Maestro.Created == "" {
		t.Error("maestro.created is empty")
	}
	if cfg.Maestro.Version != "2.0.0" {
		t.Errorf("maestro.version: got %q", cfg.Maestro.Version)
	}
	if cfg.Agents.Workers.Count != 4 {
		t.Errorf("agents.workers.count: got %d, want 4", cfg.Agents.Workers.Count)
	}
}

func TestRun_CreatesStateFiles(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	base := filepath.Join(projectDir, ".maestro")

	// Verify metrics.yaml
	data, err := os.ReadFile(filepath.Join(base, "state", "metrics.yaml"))
	if err != nil {
		t.Fatalf("read metrics.yaml: %v", err)
	}
	var metrics map[string]any
	if err := yaml.Unmarshal(data, &metrics); err != nil {
		t.Fatalf("unmarshal metrics: %v", err)
	}
	if metrics["file_type"] != "state_metrics" {
		t.Errorf("metrics file_type: got %v", metrics["file_type"])
	}
	if metrics["schema_version"] != 1 {
		t.Errorf("metrics schema_version: got %v", metrics["schema_version"])
	}
	// updated_at should be present (nil initial value)
	if _, ok := metrics["updated_at"]; !ok {
		t.Error("metrics: updated_at field missing")
	}

	// Verify continuous.yaml
	data, err = os.ReadFile(filepath.Join(base, "state", "continuous.yaml"))
	if err != nil {
		t.Fatalf("read continuous.yaml: %v", err)
	}
	var continuous map[string]any
	if err := yaml.Unmarshal(data, &continuous); err != nil {
		t.Fatalf("unmarshal continuous: %v", err)
	}
	if continuous["file_type"] != "state_continuous" {
		t.Errorf("continuous file_type: got %v", continuous["file_type"])
	}
	if continuous["status"] != "stopped" {
		t.Errorf("continuous status: got %v", continuous["status"])
	}
	if v, ok := continuous["current_iteration"].(int); !ok || v != 0 {
		t.Errorf("continuous current_iteration: got %v", continuous["current_iteration"])
	}
	// updated_at should be present (nil initial value)
	if _, ok := continuous["updated_at"]; !ok {
		t.Error("continuous: updated_at field missing")
	}
}

func TestRun_CreatesDaemonLock(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	lockPath := filepath.Join(projectDir, ".maestro", "locks", "daemon.lock")
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("daemon.lock does not exist: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("daemon.lock permissions: got %04o, want 0600", info.Mode().Perm())
	}
}

func TestRun_CopiesSkillTemplates(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	skillsBase := filepath.Join(projectDir, ".maestro", "skills")

	// Verify role-based directory structure exists
	roleDirs := []string{"share", "worker", "orchestrator"}
	for _, role := range roleDirs {
		roleDir := filepath.Join(skillsBase, role)
		info, err := os.Stat(roleDir)
		if err != nil {
			t.Errorf("skills/%s directory does not exist: %v", role, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("skills/%s is not a directory", role)
			continue
		}

		// Verify each role directory contains at least one skill subdirectory with SKILL.md
		entries, err := os.ReadDir(roleDir)
		if err != nil {
			t.Errorf("read skills/%s: %v", role, err)
			continue
		}
		skillCount := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			skillFile := filepath.Join(roleDir, e.Name(), "SKILL.md")
			info, err := os.Stat(skillFile)
			if err != nil {
				t.Errorf("skills/%s/%s/SKILL.md does not exist: %v", role, e.Name(), err)
				continue
			}
			if info.Size() == 0 {
				t.Errorf("skills/%s/%s/SKILL.md is empty", role, e.Name())
			}
			skillCount++
		}
		if skillCount == 0 {
			t.Errorf("skills/%s has no skill subdirectories", role)
		}
	}
}

func TestRun_CreatesGitignoreWhenMissing(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	gitignorePath := filepath.Join(projectDir, ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf(".gitignore does not exist: %v", err)
	}

	content := string(data)

	// Must contain maestro worktrees directory
	if !contains(content, ".maestro/worktrees/") {
		t.Error(".gitignore does not contain .maestro/worktrees/")
	}

	// Must contain .env
	if !contains(content, ".env") {
		t.Error(".gitignore does not contain .env")
	}
}

func TestRun_DoesNotOverwriteExistingGitignore(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	// Pre-create a custom .gitignore
	existing := "# my custom gitignore\nnode_modules/\n"
	gitignorePath := filepath.Join(projectDir, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte(existing), 0644); err != nil {
		t.Fatalf("write existing .gitignore: %v", err)
	}

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}

	if string(data) != existing {
		t.Errorf("existing .gitignore was modified: got %q, want %q", string(data), existing)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// initGitRepo initialises a bare-minimum git repository so that
// git add / git commit succeed during Run.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.local"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s (%v)", args, out, err)
		}
	}
}

func TestRun_GitTracksGitignore(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	initGitRepo(t, projectDir)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// .gitignore should be tracked (git ls-files returns it).
	cmd := exec.Command("git", "ls-files", ".gitignore")
	cmd.Dir = projectDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	if got := string(out); got == "" {
		t.Error(".gitignore is not tracked by git after Run")
	}
}

// TestRun_GitTracksGitignore_PreservesPreExistingStagedChanges verifies that
// gitTrackGitignore's path-scoped commit (`git commit -- .gitignore`) does not
// silently sweep unrelated staged changes into a maestro commit. Setup is
// supposed to be a no-op for the rest of the working tree.
func TestRun_GitTracksGitignore_PreservesPreExistingStagedChanges(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}
	initGitRepo(t, projectDir)

	// Create and stage an unrelated file before Run.
	userFile := filepath.Join(projectDir, "user_work.txt")
	if err := os.WriteFile(userFile, []byte("user staged work\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", projectDir, "add", "user_work.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add user_work.txt: %v (%s)", err, out)
	}

	// Verify the file is staged before Run.
	stagedBefore, err := exec.Command("git", "-C", projectDir, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatalf("git diff --cached (before): %v", err)
	}
	if !strings.Contains(string(stagedBefore), "user_work.txt") {
		t.Fatalf("pre-condition: user_work.txt should be staged, got: %s", stagedBefore)
	}

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// user_work.txt must STILL be staged (not committed by setup).
	stagedAfter, err := exec.Command("git", "-C", projectDir, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatalf("git diff --cached (after): %v", err)
	}
	if !strings.Contains(string(stagedAfter), "user_work.txt") {
		t.Errorf("user_work.txt should remain staged after Run, got: %s", stagedAfter)
	}

	// HEAD's most recent commit must touch only .gitignore.
	headFiles, err := exec.Command("git", "-C", projectDir, "show", "--name-only", "--pretty=format:", "HEAD").Output()
	if err != nil {
		t.Fatalf("git show HEAD: %v", err)
	}
	files := strings.Fields(strings.TrimSpace(string(headFiles)))
	if len(files) != 1 || files[0] != ".gitignore" {
		t.Errorf("HEAD commit should touch only .gitignore, got: %v", files)
	}
}

func TestRun_GitTracksGitignoreIdempotent(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	initGitRepo(t, projectDir)

	// First run — creates and commits .gitignore.
	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run (first): %v", err)
	}

	// Remove .maestro/ so Run can execute again.
	os.RemoveAll(filepath.Join(projectDir, ".maestro"))

	// Second run — .gitignore already tracked; should not error.
	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run (second): %v", err)
	}
}

// TestRun_RepairsExistingDir verifies the partial-repair contract: an
// existing `.maestro/` does not error; missing template files are filled
// in while user-customisable files (config.yaml etc.) are left alone.
func TestRun_RepairsExistingDir(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(projectDir, ".maestro"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run repair mode failed: %v", err)
	}

	// Required scaffolding must now exist.
	for _, p := range []string{
		filepath.Join(".maestro", "maestro.md"),
		filepath.Join(".maestro", "config.yaml"),
		filepath.Join(".maestro", "queue", "planner.yaml"),
		filepath.Join(".maestro", "instructions", "orchestrator.md"),
	} {
		if _, err := os.Stat(filepath.Join(projectDir, p)); err != nil {
			t.Errorf("repair did not create %s: %v", p, err)
		}
	}
}

// TestRun_RepairPreservesUserManagedFiles pins the split contract:
//
//   - User-managed artefacts (config.yaml, queue/, results/, state/) survive
//     repair so an operator's customisations are not blown away.
//   - Versioned template artefacts (maestro.md, dashboard.md, instructions/,
//     skills/, persona/) are always rewritten so a previous-version copy
//     cannot drift the agent contract — this is the regression class behind
//     the 2026-05-05 commit_policy stale-instruction report.
func TestRun_RepairPreservesUserManagedFiles(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run (initial): %v", err)
	}

	// Customise a user-managed artefact (config.yaml) and a versioned
	// template artefact (maestro.md). The repair run should preserve the
	// former and refresh the latter.
	cfgPath := filepath.Join(projectDir, ".maestro", "config.yaml")
	originalCfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	customCfg := append(originalCfg, []byte("\n# user note\n")...)
	if err := os.WriteFile(cfgPath, customCfg, 0644); err != nil {
		t.Fatal(err)
	}

	customisedTemplate := []byte("# tampered template\n")
	mdPath := filepath.Join(projectDir, ".maestro", "maestro.md")
	if err := os.WriteFile(mdPath, customisedTemplate, 0644); err != nil {
		t.Fatal(err)
	}

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run (repair): %v", err)
	}

	gotCfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotCfg) != string(customCfg) {
		t.Errorf("repair overwrote user-managed config.yaml")
	}

	gotMD, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotMD) == string(customisedTemplate) {
		t.Errorf("repair did not refresh versioned template maestro.md (still tampered content)")
	}
}

func TestRun_ExplicitProjectName(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, "custom-name"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, ".maestro", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}

	var cfg model.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config.yaml: %v", err)
	}

	if cfg.Project.Name != "custom-name" {
		t.Errorf("project.name: got %q, want %q", cfg.Project.Name, "custom-name")
	}
}

func TestRun_NonExistentProjectDir(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "does-not-exist")

	err := Run(projectDir, "")
	if err == nil {
		t.Fatal("expected error for non-existent project directory")
	}
}

func TestSanitizeProjectName(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "simple", raw: "myproject", want: "myproject"},
		{name: "with dots", raw: "my.project.name", want: "my-project-name"},
		{name: "with spaces", raw: "my project", want: "my-project"},
		{name: "leading special", raw: "---myproject", want: "myproject"},
		{name: "trailing special", raw: "myproject---", want: "myproject"},
		{name: "leading underscore", raw: "_myproject", want: "myproject"},
		{name: "all special", raw: "!@#$%", want: "project"},
		{name: "empty", raw: "", want: "project"},
		{name: "mixed case", raw: "MyProject", want: "MyProject"},
		{name: "with numbers", raw: "project123", want: "project123"},
		{name: "number start", raw: "123project", want: "123project"},
		{name: "hyphens and underscores", raw: "my-project_v2", want: "my-project_v2"},
		{name: "unicode", raw: "プロジェクト", want: "project"},
		{name: "mixed unicode and ascii", raw: "テストproject", want: "project"},
		{name: "long name truncated", raw: strings.Repeat("a", 100), want: strings.Repeat("a", 64)},
		{name: "exactly 64", raw: strings.Repeat("b", 64), want: strings.Repeat("b", 64)},
		{name: "multiple consecutive special", raw: "a!!!b", want: "a-b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeProjectName(tt.raw)
			if got != tt.want {
				t.Errorf("sanitizeProjectName(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRun_SpecialCharProjectDirName(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "my.special project!")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, ".maestro", "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}

	var cfg model.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config.yaml: %v", err)
	}

	if cfg.Project.Name == "" {
		t.Error("project.name should not be empty for special char dir name")
	}
	// Should be sanitized - no dots or spaces
	if contains(cfg.Project.Name, ".") || contains(cfg.Project.Name, " ") || contains(cfg.Project.Name, "!") {
		t.Errorf("project.name %q contains unsanitized characters", cfg.Project.Name)
	}
}

func TestRun_MetricsWorkerEntries(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, ".maestro", "state", "metrics.yaml"))
	if err != nil {
		t.Fatalf("read metrics.yaml: %v", err)
	}

	var metrics struct {
		QueueDepth struct {
			Workers map[string]int `yaml:"workers"`
		} `yaml:"queue_depth"`
	}
	if err := yaml.Unmarshal(data, &metrics); err != nil {
		t.Fatalf("unmarshal metrics: %v", err)
	}

	// Default worker count is 4
	if len(metrics.QueueDepth.Workers) != 4 {
		t.Errorf("metrics workers count: got %d, want 4", len(metrics.QueueDepth.Workers))
	}
	for i := 1; i <= 4; i++ {
		key := fmt.Sprintf("worker%d", i)
		val, ok := metrics.QueueDepth.Workers[key]
		if !ok {
			t.Errorf("metrics workers missing key %q", key)
		} else if val != 0 {
			t.Errorf("metrics workers[%q] = %d, want 0", key, val)
		}
	}
}

func TestRun_ContinuousMaxIterations(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, ".maestro", "state", "continuous.yaml"))
	if err != nil {
		t.Fatalf("read continuous.yaml: %v", err)
	}

	var continuous struct {
		MaxIterations int    `yaml:"max_iterations"`
		Status        string `yaml:"status"`
		SchemaVersion int    `yaml:"schema_version"`
	}
	if err := yaml.Unmarshal(data, &continuous); err != nil {
		t.Fatalf("unmarshal continuous: %v", err)
	}

	if continuous.SchemaVersion != 1 {
		t.Errorf("continuous schema_version: got %d, want 1", continuous.SchemaVersion)
	}
	if continuous.Status != "stopped" {
		t.Errorf("continuous status: got %q, want %q", continuous.Status, "stopped")
	}
	if continuous.MaxIterations <= 0 {
		t.Errorf("continuous max_iterations should be positive: got %d", continuous.MaxIterations)
	}
}

// TestRun_RemovesLegacyClaudeVerifyScript pins post-2026-05-06 round-3 P2:
// `maestro setup` (including repair) deletes any pre-existing
// `.claude/verify.sh` left over from older Maestro versions that
// distributed a Stop-hook template. The script is the source of the
// "Stop hook error" output that the user sees on every pane turn.
func TestRun_RemovesLegacyClaudeVerifyScript(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	claudeDir := filepath.Join(projectDir, ".claude")
	if err := os.Mkdir(claudeDir, 0755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	verifyPath := filepath.Join(claudeDir, "verify.sh")
	if err := os.WriteFile(verifyPath, []byte("#!/bin/bash\nturbo run lint test\n"), 0755); err != nil {
		t.Fatalf("write verify.sh: %v", err)
	}

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(verifyPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".claude/verify.sh should be removed by setup migration, got stat err=%v", err)
	}
}

// TestRun_VerifyScriptRemovalIsBestEffort pins that absence of
// `.claude/verify.sh` (clean install) is not an error.
func TestRun_VerifyScriptRemovalIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	if err := os.Mkdir(projectDir, 0755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run on project without legacy verify.sh: %v", err)
	}
}

func TestRun_PersonaTemplatesCopied(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)

	if err := Run(projectDir, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	personaDir := filepath.Join(projectDir, ".maestro", "persona")
	info, err := os.Stat(personaDir)
	if err != nil {
		t.Fatalf("persona directory does not exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("persona is not a directory")
	}

	entries, err := os.ReadDir(personaDir)
	if err != nil {
		t.Fatalf("read persona dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("persona directory is empty, expected template files")
	}
}

// TestTemplateConfigFeatureProfilesMatchCodeDefaults locks the
// feature_profiles block in templates/config.yaml to the SSOT
// (daemon/featuregate.Evaluator.DefaultProfiles). The template header
// declares that its values mirror the code defaults, and README documents
// the same values; drift between them (Report 2026-06-10 P1-3:
// standard.adaptive_model_selection diverged) makes code / template /
// README disagree about what a profile enables.
//
// Earlier versions compared against a model-package copy of the defaults;
// that copy was itself a divergence hazard and has been removed, so the
// comparison now reads the runtime SSOT directly.
func TestTemplateConfigFeatureProfilesMatchCodeDefaults(t *testing.T) {
	data, err := fs.ReadFile(templates.FS, "config.yaml")
	if err != nil {
		t.Fatalf("read embedded config.yaml: %v", err)
	}

	var cfg struct {
		FeatureProfiles map[string]model.FeatureProfile `yaml:"feature_profiles"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal config.yaml: %v", err)
	}

	want := featuregate.NewEvaluator().DefaultProfiles()
	for level, wantProfile := range want {
		got, ok := cfg.FeatureProfiles[string(level)]
		if !ok {
			t.Errorf("templates/config.yaml feature_profiles missing level %q", level)
			continue
		}
		gotFeatures := map[featuregate.Feature]bool{
			featuregate.FeatureCrossAgentReview:       got.EffectiveCrossAgentReview(),
			featuregate.FeatureExploratoryOpt:         got.EffectiveExploratoryOptimization(),
			featuregate.FeatureEvolutionaryQuality:    got.EffectiveEvolutionaryQuality(),
			featuregate.FeatureAdaptiveModelSelection: got.EffectiveAdaptiveModelSelection(),
			featuregate.FeatureSelfImprovement:        got.EffectiveSelfImprovement(),
			featuregate.FeatureAdaptiveDepth:          got.EffectiveAdaptiveDepth(),
		}
		if !reflect.DeepEqual(gotFeatures, wantProfile.EnabledFeatures) {
			t.Errorf("templates/config.yaml feature_profiles[%q] = %v, want %v (sync with featuregate.DefaultProfiles)",
				level, gotFeatures, wantProfile.EnabledFeatures)
		}
	}
	for level := range cfg.FeatureProfiles {
		if _, ok := want[featuregate.ProfileLevel(level)]; !ok {
			t.Errorf("templates/config.yaml feature_profiles has unknown level %q", level)
		}
	}
}
