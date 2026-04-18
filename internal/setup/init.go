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
// It covers maestro internal directories and common secret file patterns
// required by commit_policy.require_gitignore.
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

// Run initializes the .maestro/ directory structure in the given project directory.
// projectName overrides the auto-detected name (defaults to directory basename if empty).
func Run(projectDir, projectName string) error {
	absDir, err := filepath.Abs(projectDir)
	if err != nil {
		return fmt.Errorf("resolve project dir: %w", err)
	}

	base := filepath.Join(absDir, maestroDir)

	if _, err := os.Stat(base); err == nil {
		return fmt.Errorf("%s already exists", base)
	}

	// Ensure .gitignore exists (required by commit_policy.require_gitignore)
	if err := ensureGitignore(absDir); err != nil {
		return fmt.Errorf("ensure .gitignore: %w", err)
	}

	// Track .gitignore in git so worktrees inherit it.
	// Failures are non-fatal — log a warning and continue.
	gitTrackGitignore(absDir)

	// Create directory structure
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

	// Copy template files
	if err := copyTemplateFile("maestro.md", filepath.Join(base, "maestro.md")); err != nil {
		return err
	}
	if err := copyTemplateFile("dashboard.md", filepath.Join(base, "dashboard.md")); err != nil {
		return err
	}

	// Copy instructions
	instructionFiles := []string{"orchestrator.md", "planner.md", "worker.md"}
	for _, name := range instructionFiles {
		src := filepath.Join("instructions", name)
		dst := filepath.Join(base, "instructions", name)
		if err := copyTemplateFile(src, dst); err != nil {
			return err
		}
	}

	// Copy template skills
	if err := copyTemplateDir("skills", filepath.Join(base, "skills")); err != nil {
		return err
	}

	// Copy template persona
	if err := copyTemplateDir("persona", filepath.Join(base, "persona")); err != nil {
		return err
	}

	// Generate and write config.yaml with auto-filled fields
	cfg, err := generateConfig(absDir, projectName)
	if err != nil {
		return fmt.Errorf("generate config: %w", err)
	}

	workerCount := cfg.Agents.Workers.Count
	if workerCount < model.MinWorkers || workerCount > model.MaxWorkers {
		return fmt.Errorf("agents.workers.count must be %d-%d, got %d", model.MinWorkers, model.MaxWorkers, workerCount)
	}

	if err := writeYAMLAtomic(filepath.Join(base, "config.yaml"), cfg); err != nil {
		return fmt.Errorf("write config.yaml: %w", err)
	}

	// Create queue files
	if err := writeSchemaFile(filepath.Join(base, "queue", "planner.yaml"), "queue_command", "commands"); err != nil {
		return err
	}
	if err := writeSchemaFile(filepath.Join(base, "queue", "orchestrator.yaml"), "queue_notification", "notifications"); err != nil {
		return err
	}
	for i := 1; i <= workerCount; i++ {
		name := fmt.Sprintf("worker%d.yaml", i)
		if err := writeSchemaFile(filepath.Join(base, "queue", name), "queue_task", "tasks"); err != nil {
			return err
		}
	}

	// Create results files
	if err := writeSchemaFile(filepath.Join(base, "results", "planner.yaml"), "result_command", "results"); err != nil {
		return err
	}
	for i := 1; i <= workerCount; i++ {
		name := fmt.Sprintf("worker%d.yaml", i)
		if err := writeSchemaFile(filepath.Join(base, "results", name), "result_task", "results"); err != nil {
			return err
		}
	}

	// Create state files
	if err := writeMetrics(filepath.Join(base, "state", "metrics.yaml"), workerCount); err != nil {
		return fmt.Errorf("write metrics.yaml: %w", err)
	}
	if err := writeContinuous(filepath.Join(base, "state", "continuous.yaml"), cfg.Continuous.MaxIterations); err != nil {
		return fmt.Errorf("write continuous.yaml: %w", err)
	}

	// Create daemon.lock (empty)
	if err := os.WriteFile(filepath.Join(base, "locks", "daemon.lock"), nil, 0600); err != nil {
		return fmt.Errorf("create daemon.lock: %w", err)
	}

	return nil
}

// ensureGitignore creates a default .gitignore in projectDir if one does not
// already exist. If the file is already present it is left untouched.
func ensureGitignore(projectDir string) error {
	p := filepath.Join(projectDir, ".gitignore")
	if _, err := os.Stat(p); err == nil {
		return nil // already exists — do not overwrite
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat .gitignore: %w", err)
	}
	if err := os.WriteFile(p, []byte(defaultGitignore), 0644); err != nil { //nolint:gosec // .gitignore is user-readable
		return fmt.Errorf("write .gitignore: %w", err)
	}
	return nil
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

func copyTemplateDir(srcDir, dstDir string) error {
	return fs.WalkDir(templates.FS, srcDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Trim srcDir prefix to get relative path. Embedded FS uses forward slashes.
		rel := strings.TrimPrefix(p, srcDir+"/")
		if rel == srcDir {
			rel = "."
		}
		dst := filepath.Join(dstDir, filepath.FromSlash(rel))

		if d.IsDir() {
			return os.MkdirAll(dst, 0o750)
		}

		// Skip if file already exists (protect user customizations).
		if _, statErr := os.Stat(dst); statErr == nil {
			return nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", dst, statErr)
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
	for len(name) > 0 && !((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z') || (name[0] >= '0' && name[0] <= '9')) {
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
