// Package setup handles maestro project initialization.
package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	atomicyaml "github.com/msageha/maestro_v2/internal/yaml"
	"github.com/msageha/maestro_v2/templates"
)

// sanitizeProjectNameRe matches characters that are not alphanumeric, underscore, or hyphen.
var sanitizeProjectNameRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

const maestroDir = ".maestro"

// defaultGitignore is the content written to .gitignore when none exists.
// Covers Maestro-managed runtime directories (worktrees / state / results /
// queue) and a small set of common secret-bearing paths so a fresh repo
// does not accidentally commit them. Operators are free to extend this in
// the project after the file is created — Maestro never overwrites an
// existing .gitignore.
const defaultGitignore = `# Maestro
.maestro/worktrees/
.maestro/state/
.maestro/results/
.maestro/queue/

# Secrets
.env
.env.*
*.key
*.pem
*.secret
credentials.*
`

// Run initializes the .maestro/ directory in projectDir. When the directory
// already exists, Run operates in repair mode: every missing file is
// re-created, every existing file is left alone (so user customisations to
// config.yaml or personas survive). This eliminates the "partial .maestro
// blocks first start-up" failure mode where the directory was scaffolded but
// maestro.md or config.yaml went missing (interrupted setup, manual cleanup).
//
// projectName overrides the auto-detected name (defaults to directory basename
// when empty).
func Run(projectDir, projectName string) error {
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return fmt.Errorf("resolve project dir: %w", err)
	}

	base := filepath.Join(absDir, maestroDir)
	repair := false
	if _, err := os.Stat(base); err == nil {
		repair = true
	}

	// .gitignore is the only file we may write outside .maestro/. Even in
	// repair mode we still want to ensure it exists.
	created, err := ensureGitignore(absDir)
	if err != nil {
		return fmt.Errorf("ensure .gitignore: %w", err)
	}
	if created {
		gitTrackGitignore(absDir)
	}

	dirs := []string{
		"queue",
		"results",
		"state/commands",
		"locks",
		"logs",
		"dead_letters",
		"quarantine",
		"instructions",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(base, d), 0750); err != nil {
			return fmt.Errorf("create directory %s: %w", d, err)
		}
	}

	// maestro.md, dashboard.md, instructions/, skills/, persona/ are
	// versioned with the binary — they encode the contract between the
	// daemon and the agent roles, and a stale copy left over from a
	// previous Maestro version causes silent behavioural drift (e.g. the
	// commit_policy regression in Report 2026-05-05 where Worker
	// instructions referenced a retired schema). Always overwrite these
	// on setup, including repair runs, so the runtime artifacts track
	// the binary. User customisation belongs in config.yaml, queue/,
	// results/, state/ — those paths use writeIfMissing semantics.
	if err := copyTemplateFile("maestro.md", filepath.Join(base, "maestro.md")); err != nil {
		return err
	}
	if err := copyTemplateFile("dashboard.md", filepath.Join(base, "dashboard.md")); err != nil {
		return err
	}

	instructionFiles := []string{"orchestrator.md", "planner.md", "worker.md"}
	for _, name := range instructionFiles {
		src := filepath.Join("instructions", name)
		dst := filepath.Join(base, "instructions", name)
		if err := copyTemplateFile(src, dst); err != nil {
			return err
		}
	}

	if err := copyTemplateDirOverwrite("skills", filepath.Join(base, "skills")); err != nil {
		return err
	}
	if err := copyTemplateDirOverwrite("persona", filepath.Join(base, "persona")); err != nil {
		return err
	}

	cfg, err := generateConfig(absDir, projectName)
	if err != nil {
		return fmt.Errorf("generate config: %w", err)
	}

	workerCount := cfg.Agents.Workers.Count
	if workerCount < model.MinWorkers || workerCount > model.MaxWorkers {
		return fmt.Errorf("agents.workers.count must be %d-%d, got %d", model.MinWorkers, model.MaxWorkers, workerCount)
	}

	configPath := filepath.Join(base, "config.yaml")
	if !fileExists(configPath) {
		if err := writeYAMLAtomic(configPath, cfg); err != nil {
			return fmt.Errorf("write config.yaml: %w", err)
		}
	}

	if err := writeSchemaIfMissing(filepath.Join(base, "queue", "planner.yaml"), "queue_command", "commands"); err != nil {
		return err
	}
	if err := writeSchemaIfMissing(filepath.Join(base, "queue", "orchestrator.yaml"), "queue_notification", "notifications"); err != nil {
		return err
	}
	for i := 1; i <= workerCount; i++ {
		name := fmt.Sprintf("worker%d.yaml", i)
		if err := writeSchemaIfMissing(filepath.Join(base, "queue", name), "queue_task", "tasks"); err != nil {
			return err
		}
	}

	if err := writeSchemaIfMissing(filepath.Join(base, "results", "planner.yaml"), "result_command", "results"); err != nil {
		return err
	}
	for i := 1; i <= workerCount; i++ {
		name := fmt.Sprintf("worker%d.yaml", i)
		if err := writeSchemaIfMissing(filepath.Join(base, "results", name), "result_task", "results"); err != nil {
			return err
		}
	}

	metricsPath := filepath.Join(base, "state", "metrics.yaml")
	if !fileExists(metricsPath) {
		if err := writeMetrics(metricsPath, workerCount); err != nil {
			return fmt.Errorf("write metrics.yaml: %w", err)
		}
	}
	continuousPath := filepath.Join(base, "state", "continuous.yaml")
	if !fileExists(continuousPath) {
		if err := writeContinuous(continuousPath, cfg.Continuous.MaxIterations); err != nil {
			return fmt.Errorf("write continuous.yaml: %w", err)
		}
	}

	lockPath := filepath.Join(base, "locks", "daemon.lock")
	if !fileExists(lockPath) {
		if err := os.WriteFile(lockPath, nil, 0600); err != nil {
			return fmt.Errorf("create daemon.lock: %w", err)
		}
	}

	// Migrate legacy artifacts: prior Maestro versions distributed
	// `.claude/verify.sh` (a pnpm/turbo Stop-hook script). Verify hook
	// distribution was retired (operator handles language-specific lint /
	// test in their global ~/.claude/settings.json) but existing projects
	// still have the file, which keeps printing "Stop hook error" every
	// pane turn (Report 2026-05-06 round-3 P2). Remove it during setup so
	// upgrade is one-shot and self-healing.
	removeLegacyClaudeVerifyScript(absDir)

	if repair {
		slog.Info("setup_repair_completed", "path", base,
			"note", "filled missing files; existing user customisations were preserved")
	}
	return nil
}

// removeLegacyClaudeVerifyScript deletes a `.claude/verify.sh` left over
// from earlier Maestro versions that distributed a Stop-hook template.
// Best-effort: missing files / read-only dirs are non-fatal, this is a
// migration cleanup, not a hard requirement.
func removeLegacyClaudeVerifyScript(projectDir string) {
	path := filepath.Join(projectDir, ".claude", "verify.sh")
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	if info.IsDir() {
		return
	}
	if err := os.Remove(path); err != nil {
		slog.Warn("setup: failed to remove legacy .claude/verify.sh",
			"path", path, "error", err,
			"hint", "delete this file manually to silence Stop hook errors")
		return
	}
	slog.Info("setup: removed legacy .claude/verify.sh", "path", path,
		"reason", "verify.sh template is retired; language-specific Stop hooks belong in ~/.claude/settings.json")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeSchemaIfMissing wraps writeSchemaFile with a pre-existence guard so
// repair mode does not overwrite a non-empty queue/results YAML.
func writeSchemaIfMissing(path, schemaKind, listKey string) error {
	if fileExists(path) {
		return nil
	}
	return writeSchemaFile(path, schemaKind, listKey)
}

// ensureGitignore creates a default .gitignore in projectDir if one does not
// already exist. If the file is already present it is left untouched.
// Returns (created, err): created is true only when this call wrote a fresh
// .gitignore. The caller uses this to decide whether it is safe to auto-commit
// the file (see gitTrackGitignore).
func ensureGitignore(projectDir string) (bool, error) {
	p := filepath.Join(projectDir, ".gitignore")
	if _, err := os.Stat(p); err == nil {
		return false, nil // already exists — do not overwrite
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat .gitignore: %w", err)
	}
	if err := os.WriteFile(p, []byte(defaultGitignore), 0644); err != nil { //nolint:gosec // .gitignore is user-readable
		return false, fmt.Errorf("write .gitignore: %w", err)
	}
	return true, nil
}

// gitTrackGitignore stages and commits .gitignore so that worktrees created
// later will inherit it. It is intentionally non-fatal: if the project is not
// a git repository or the file is already tracked, nothing bad happens.
func gitTrackGitignore(projectDir string) {
	// git add .gitignore
	add := exec.Command("git", "add", ".gitignore")
	add.Dir = projectDir
	if out, err := add.CombinedOutput(); err != nil {
		slog.Warn("git add .gitignore failed", "output", string(out), "error", err)
		return
	}

	// Check whether there is anything staged for .gitignore.
	// If there is no diff (already committed), skip the commit.
	diff := exec.Command("git", "diff", "--cached", "--quiet", "--", ".gitignore")
	diff.Dir = projectDir
	if err := diff.Run(); err == nil {
		// exit 0 means no staged changes — already tracked or nothing new.
		return
	}

	commit := exec.Command("git", "commit", "-m", "maestro: initialize .gitignore", "--", ".gitignore")
	commit.Dir = projectDir
	if out, err := commit.CombinedOutput(); err != nil {
		slog.Warn("git commit .gitignore failed", "output", string(out), "error", err)
	}
}

// copyTemplateDirOverwrite copies an embedded template directory tree to
// dstDir, overwriting any existing file at the destination. Used for
// versioned template artifacts (skills/, persona/) that must track the
// binary on every setup so previous-version copies cannot drift the
// agent contract.
func copyTemplateDirOverwrite(srcDir, dstDir string) error {
	return fs.WalkDir(templates.FS, srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, srcDir+"/")
		if rel == srcDir {
			rel = "."
		}
		dst := filepath.Join(dstDir, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o750)
		}
		data, err := fs.ReadFile(templates.FS, p)
		if err != nil {
			return fmt.Errorf("read template %s: %w", p, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil { //nolint:gosec // template files are user-readable config/docs
			return fmt.Errorf("write %s: %w", dst, err)
		}
		return nil
	})
}

func copyTemplateFile(name, dst string) error {
	data, err := fs.ReadFile(templates.FS, name)
	if err != nil {
		return fmt.Errorf("read template %s: %w", name, err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil { //nolint:gosec // template files are user-readable config/docs
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

func generateConfig(projectDir, projectName string) (*model.Config, error) {
	// Read template config as base
	data, err := fs.ReadFile(templates.FS, "config.yaml")
	if err != nil {
		return nil, fmt.Errorf("read config template: %w", err)
	}

	var cfg model.Config
	if err := yamlv3.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config template: %w", err)
	}

	// Auto-fill fields
	if projectName != "" {
		cfg.Project.Name = projectName
	} else {
		cfg.Project.Name = sanitizeProjectName(filepath.Base(projectDir))
	}
	cfg.Maestro.ProjectRoot = projectDir
	cfg.Maestro.Created = time.Now().Format(time.RFC3339)

	return &cfg, nil
}

// sanitizeProjectName converts a raw directory basename into a valid project
// name that satisfies validate.ProjectName (alphanumeric start, only
// alphanumeric/underscore/hyphen, max 64 chars). Characters like dots, spaces,
// and other invalid chars are replaced with hyphens. Leading non-alphanumeric
// characters are stripped. If the result is empty, falls back to "project".
func sanitizeProjectName(raw string) string {
	// Replace invalid characters with hyphens.
	name := sanitizeProjectNameRe.ReplaceAllString(raw, "-")
	// Trim leading/trailing hyphens and underscores.
	name = strings.Trim(name, "-_")
	// Strip leading non-alphanumeric characters (must start with [A-Za-z0-9]).
	for len(name) > 0 && (name[0] < 'A' || name[0] > 'Z') && (name[0] < 'a' || name[0] > 'z') && (name[0] < '0' || name[0] > '9') {
		name = name[1:]
	}
	// Truncate to 64 characters.
	if len(name) > 64 {
		name = name[:64]
	}
	// Fallback if empty after sanitization.
	if name == "" {
		name = "project"
	}
	return name
}

func writeYAMLAtomic(path string, v any) error {
	return atomicyaml.AtomicWrite(path, v)
}

func writeSchemaFile(path, fileType, listField string) error {
	content := fmt.Sprintf("schema_version: 1\nfile_type: %q\n%s: []\n", fileType, listField)
	return atomicyaml.AtomicWriteRaw(path, []byte(content))
}

type metricsFile struct {
	SchemaVersion   int                   `yaml:"schema_version"`
	FileType        string                `yaml:"file_type"`
	QueueDepth      metricsQueueDepth     `yaml:"queue_depth"`
	Counters        model.MetricsCounters `yaml:"counters"`
	DaemonHeartbeat *string               `yaml:"daemon_heartbeat"`
	UpdatedAt       *string               `yaml:"updated_at"`
}

type metricsQueueDepth struct {
	Planner      int            `yaml:"planner"`
	Orchestrator int            `yaml:"orchestrator"`
	Workers      map[string]int `yaml:"workers"`
}

func writeMetrics(path string, workerCount int) error {
	workers := make(map[string]int, workerCount)
	for i := 1; i <= workerCount; i++ {
		workers[fmt.Sprintf("worker%d", i)] = 0
	}

	m := metricsFile{
		SchemaVersion: 1,
		FileType:      "state_metrics",
		QueueDepth: metricsQueueDepth{
			Workers: workers,
		},
		Counters:        model.MetricsCounters{},
		DaemonHeartbeat: nil,
	}
	return writeYAMLAtomic(path, m)
}

type continuousFile struct {
	SchemaVersion    int     `yaml:"schema_version"`
	FileType         string  `yaml:"file_type"`
	CurrentIteration int     `yaml:"current_iteration"`
	MaxIterations    int     `yaml:"max_iterations"`
	Status           string  `yaml:"status"`
	PausedReason     *string `yaml:"paused_reason"`
	LastCommandID    *string `yaml:"last_command_id"`
	UpdatedAt        *string `yaml:"updated_at"`
}

func writeContinuous(path string, maxIterations int) error {
	c := continuousFile{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 0,
		MaxIterations:    maxIterations,
		Status:           "stopped",
		PausedReason:     nil,
		LastCommandID:    nil,
	}
	return writeYAMLAtomic(path, c)
}
