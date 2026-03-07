package formation

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// resetFormation clears all transient state, preserving quarantine for forensics.
func resetFormation(maestroDir string) error {
	// Stop existing daemon — must succeed before we tear down tmux
	if err := stopDaemon(maestroDir); err != nil {
		return fmt.Errorf("stop daemon: %w", err)
	}

	// Kill existing tmux session (best-effort)
	fmt.Println("[debug] resetFormation: killing existing tmux session (best-effort)")
	_ = tmux.KillSession()

	// Clear queue/ YAML files
	if err := clearYAMLFiles(filepath.Join(maestroDir, "queue")); err != nil {
		return fmt.Errorf("clear queue: %w", err)
	}

	// Clear results/ YAML files
	if err := clearYAMLFiles(filepath.Join(maestroDir, "results")); err != nil {
		return fmt.Errorf("clear results: %w", err)
	}

	// Clear state/commands/ YAML files
	if err := clearYAMLFiles(filepath.Join(maestroDir, "state", "commands")); err != nil {
		return fmt.Errorf("clear state/commands: %w", err)
	}

	// Reset state/continuous.yaml
	continuousPath := filepath.Join(maestroDir, "state", "continuous.yaml")
	if err := os.MkdirAll(filepath.Dir(continuousPath), 0755); err != nil {
		return err
	}
	if err := yamlutil.AtomicWrite(continuousPath, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 0,
		Status:           model.ContinuousStatusStopped,
	}); err != nil {
		return fmt.Errorf("reset continuous: %w", err)
	}

	// Reset state/metrics.yaml
	metricsPath := filepath.Join(maestroDir, "state", "metrics.yaml")
	if err := yamlutil.AtomicWrite(metricsPath, &model.Metrics{
		SchemaVersion: 1,
		FileType:      "state_metrics",
	}); err != nil {
		return fmt.Errorf("reset metrics: %w", err)
	}

	// Clear dead_letters/
	if err := clearAllFiles(filepath.Join(maestroDir, "dead_letters")); err != nil {
		return fmt.Errorf("clear dead_letters: %w", err)
	}

	// quarantine/ is preserved (forensics)

	return nil
}

// reflectFlags updates config.yaml with CLI flags (only when explicitly set).
func reflectFlags(opts UpOptions) error {
	cfg, err := model.LoadConfig(opts.MaestroDir)
	if err != nil {
		return err
	}

	if opts.BoostSet {
		cfg.Agents.Workers.Boost = opts.Boost
	}
	if opts.ContinuousSet {
		cfg.Continuous.Enabled = opts.Continuous
	}

	configPath := filepath.Join(opts.MaestroDir, "config.yaml")
	return yamlutil.AtomicWrite(configPath, cfg)
}

// startupRecovery ensures directory structure and file integrity.
func startupRecovery(maestroDir string) error {
	// Ensure required directories exist
	dirs := []string{
		"queue", "results", "state/commands", "logs",
		"dead_letters", "quarantine", "locks",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(maestroDir, d), 0755); err != nil {
			return fmt.Errorf("ensure dir %s: %w", d, err)
		}
	}

	// Check daemon lock is available (no other daemon running)
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	fl := lock.NewFileLock(lockPath)
	if err := fl.TryLock(); err != nil {
		return fmt.Errorf("daemon lock check: another instance may be running: %w", err)
	}
	_ = fl.Unlock()

	// Clean stale PID file and socket left behind by a crashed daemon
	_ = os.Remove(filepath.Join(maestroDir, "daemon.pid"))
	_ = os.Remove(filepath.Join(maestroDir, uds.DefaultSocketName))

	// YAML validation: scan all YAML files and quarantine corrupt ones
	validateAndRecoverYAML(maestroDir)

	// Oneshot reconciliation will happen automatically when daemon starts and runs PeriodicScan

	return nil
}

// validateAndRecoverYAML scans all YAML files for syntax errors and quarantines corrupt ones.
func validateAndRecoverYAML(maestroDir string) {
	yamlDirs := map[string]string{
		filepath.Join(maestroDir, "queue"):             "",
		filepath.Join(maestroDir, "results"):           "",
		filepath.Join(maestroDir, "state", "commands"): "state_command",
	}

	for dir, expectedType := range yamlDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			ext := filepath.Ext(entry.Name())
			if ext != ".yaml" && ext != ".yml" {
				continue
			}

			filePath := filepath.Join(dir, entry.Name())
			fileExpectedType := expectedType
			if fileExpectedType == "" {
				fileExpectedType = inferFileType(dir, entry.Name())
			}
			if err := yamlutil.ValidateSchemaHeader(filePath, fileExpectedType); err != nil {
				fmt.Printf("Warning: corrupt YAML detected: %s (%v)\n", filePath, err)
				if recErr := yamlutil.RecoverCorruptedFile(maestroDir, filePath, inferFileType(dir, entry.Name())); recErr != nil {
					fmt.Printf("Warning: recovery failed for %s: %v\n", filePath, recErr)
				}
			}
		}
	}

	// Validate state-level files
	for _, sf := range []struct {
		path     string
		fileType string
	}{
		{filepath.Join(maestroDir, "state", "continuous.yaml"), "state_continuous"},
		{filepath.Join(maestroDir, "state", "metrics.yaml"), "state_metrics"},
	} {
		if _, err := os.Stat(sf.path); err != nil {
			continue
		}
		if err := yamlutil.ValidateSchemaHeader(sf.path, sf.fileType); err != nil {
			fmt.Printf("Warning: corrupt YAML detected: %s (%v)\n", sf.path, err)
			if recErr := yamlutil.RecoverCorruptedFile(maestroDir, sf.path, sf.fileType); recErr != nil {
				fmt.Printf("Warning: recovery failed for %s: %v\n", sf.path, recErr)
			}
		}
	}
}

// inferFileType derives the expected file_type from directory and filename.
func inferFileType(dir, filename string) string {
	base := filepath.Base(dir)
	switch base {
	case "queue":
		if filename == "planner.yaml" {
			return "queue_command"
		}
		if filename == "orchestrator.yaml" {
			return "queue_notification"
		}
		if filename == "planner_signals.yaml" {
			return "planner_signal_queue"
		}
		return "queue_task"
	case "results":
		if filename == "planner.yaml" {
			return "result_command"
		}
		return "result_task"
	case "commands":
		return "state_command"
	}
	return ""
}

// activateContinuousMode sets state/continuous.yaml status to running.
func activateContinuousMode(maestroDir string, cfg model.Config) error {
	continuousPath := filepath.Join(maestroDir, "state", "continuous.yaml")
	if err := os.MkdirAll(filepath.Dir(continuousPath), 0755); err != nil {
		return err
	}

	// Load existing state if present, otherwise create new
	var state model.Continuous
	data, err := os.ReadFile(continuousPath)
	if err == nil {
		if err := yamlv3.Unmarshal(data, &state); err != nil {
			fmt.Printf("Warning: failed to parse %s: %v\n", continuousPath, err)
		}
	}

	state.SchemaVersion = 1
	state.FileType = "state_continuous"
	state.Status = model.ContinuousStatusRunning
	state.MaxIterations = cfg.Continuous.MaxIterations
	now := time.Now().UTC().Format(time.RFC3339)
	state.UpdatedAt = now

	return yamlutil.AtomicWrite(continuousPath, &state)
}

// clearYAMLFiles removes all .yaml files in a directory.
func clearYAMLFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext == ".yaml" || ext == ".yml" {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// clearAllFiles removes all files in a directory (non-recursive).
func clearAllFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
