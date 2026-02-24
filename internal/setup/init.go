// Package setup handles maestro project initialization.
package setup

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	atomicyaml "github.com/msageha/maestro_v2/internal/yaml"
	"github.com/msageha/maestro_v2/templates"
)

const maestroDir = ".maestro"

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
		if err := os.MkdirAll(filepath.Join(base, d), 0755); err != nil {
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

	// Generate and write config.yaml with auto-filled fields
	cfg, err := generateConfig(absDir, projectName)
	if err != nil {
		return fmt.Errorf("generate config: %w", err)
	}

	workerCount := cfg.Agents.Workers.Count
	if workerCount < 1 || workerCount > 8 {
		return fmt.Errorf("agents.workers.count must be 1-8, got %d", workerCount)
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

func copyTemplateFile(name, dst string) error {
	data, err := fs.ReadFile(templates.FS, name)
	if err != nil {
		return fmt.Errorf("read template %s: %w", name, err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
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
		cfg.Project.Name = filepath.Base(projectDir)
	}
	cfg.Maestro.ProjectRoot = projectDir
	cfg.Maestro.Created = time.Now().Format(time.RFC3339)

	return &cfg, nil
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
