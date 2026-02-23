package formation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// UpOptions holds configuration for the 'maestro up' command.
type UpOptions struct {
	MaestroDir string
	Config     model.Config
	Reset      bool
	Boost      bool
	Continuous bool
	NoNotify   bool
	ResetOnly  bool // --reset with no other formation flags

	// Explicit flags: only overwrite config when explicitly set by CLI
	BoostSet      bool
	ContinuousSet bool
	NoNotifySet   bool
}

// RunUp executes the 'maestro up' command.
func RunUp(opts UpOptions) error {
	if opts.Reset {
		if err := resetFormation(opts.MaestroDir); err != nil {
			return fmt.Errorf("reset: %w", err)
		}
		fmt.Println("Formation reset complete.")
		if opts.ResetOnly {
			return nil
		}
	}

	// Reflect flags into config
	if err := reflectFlags(opts); err != nil {
		return fmt.Errorf("reflect flags: %w", err)
	}

	// Reload config after flag reflection
	cfg, err := loadConfigFromDir(opts.MaestroDir)
	if err != nil {
		return fmt.Errorf("reload config: %w", err)
	}

	// Transition continuous mode to running if enabled
	if cfg.Continuous.Enabled {
		if err := activateContinuousMode(opts.MaestroDir, cfg); err != nil {
			return fmt.Errorf("activate continuous: %w", err)
		}
	}

	// Startup recovery
	if err := startupRecovery(opts.MaestroDir); err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	}

	// Create tmux formation
	if err := createFormation(cfg); err != nil {
		return fmt.Errorf("create formation: %w", err)
	}

	// Start daemon in background
	if err := startDaemon(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Wait briefly for daemon to initialize
	time.Sleep(500 * time.Millisecond)

	fmt.Println("Maestro formation is up.")
	return nil
}

// resetFormation clears all transient state, preserving quarantine for forensics.
func resetFormation(maestroDir string) error {
	// Stop existing daemon (best-effort)
	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	client := uds.NewClient(socketPath)
	client.SetTimeout(5 * time.Second)
	_, _ = client.SendCommand("shutdown", nil) // ignore errors

	// Kill existing tmux session (best-effort)
	_ = tmux.KillSession()

	// Wait briefly for daemon to stop
	time.Sleep(1 * time.Second)

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
	cfg, err := loadConfigFromDir(opts.MaestroDir)
	if err != nil {
		return err
	}

	if opts.BoostSet {
		cfg.Agents.Workers.Boost = opts.Boost
	}
	if opts.ContinuousSet {
		cfg.Continuous.Enabled = opts.Continuous
	}
	if opts.NoNotifySet {
		cfg.Notify.Enabled = !opts.NoNotify
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
			if err := yamlutil.ValidateSchemaHeader(filePath, expectedType); err != nil {
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

// createFormation creates the tmux session with orchestrator, planner, and worker windows.
func createFormation(cfg model.Config) error {
	// Kill existing session if any
	if tmux.SessionExists() {
		if err := tmux.KillSession(); err != nil {
			return fmt.Errorf("kill existing session: %w", err)
		}
	}

	// Window 0: orchestrator
	if err := tmux.CreateSession("orchestrator"); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	orchPane := fmt.Sprintf("%s:0.0", tmux.SessionName)
	if err := setAgentVars(orchPane, "orchestrator", "orchestrator", resolveModel(cfg, "orchestrator")); err != nil {
		return err
	}

	// Window 1: planner
	if err := tmux.CreateWindow("planner"); err != nil {
		return fmt.Errorf("create planner window: %w", err)
	}

	plannerPane := fmt.Sprintf("%s:1.0", tmux.SessionName)
	if err := setAgentVars(plannerPane, "planner", "planner", resolveModel(cfg, "planner")); err != nil {
		return err
	}

	// Window 2: workers
	workerCount := cfg.Agents.Workers.Count
	if workerCount < 1 {
		workerCount = 1
	}

	if err := tmux.CreateWindow("workers"); err != nil {
		return fmt.Errorf("create workers window: %w", err)
	}

	workerWindow := fmt.Sprintf("%s:2", tmux.SessionName)
	panes, err := tmux.SetupWorkerGrid(workerWindow, workerCount)
	if err != nil {
		return fmt.Errorf("setup worker grid: %w", err)
	}

	for i, pane := range panes {
		agentID := fmt.Sprintf("worker%d", i+1)
		workerModel := resolveModel(cfg, agentID)
		if err := setAgentVars(pane, agentID, "worker", workerModel); err != nil {
			return err
		}
	}

	// Launch agents in each pane
	allPanes := make([]string, 0, 2+cfg.Agents.Workers.Count)
	allPanes = append(allPanes, orchPane, plannerPane)
	allPanes = append(allPanes, panes...)

	for _, pane := range allPanes {
		if err := tmux.SendCommand(pane, "maestro agent launch"); err != nil {
			return fmt.Errorf("launch agent in %s: %w", pane, err)
		}
	}

	return nil
}

func setAgentVars(pane, agentID, role, agentModel string) error {
	vars := map[string]string{
		"agent_id": agentID,
		"role":     role,
		"model":    agentModel,
		"status":   "idle",
	}
	for k, v := range vars {
		if err := tmux.SetUserVar(pane, k, v); err != nil {
			return fmt.Errorf("set @%s on %s: %w", k, pane, err)
		}
	}
	return nil
}

// resolveModel determines the model for a given agent.
func resolveModel(cfg model.Config, agentID string) string {
	switch agentID {
	case "orchestrator":
		if m := cfg.Agents.Orchestrator.Model; m != "" {
			return m
		}
	case "planner":
		if m := cfg.Agents.Planner.Model; m != "" {
			return m
		}
	default:
		// Worker: check per-worker override, then boost, then default
		if cfg.Agents.Workers.Boost {
			return "opus"
		}
		if m, ok := cfg.Agents.Workers.Models[agentID]; ok {
			return m
		}
		if m := cfg.Agents.Workers.DefaultModel; m != "" {
			return m
		}
	}
	return "sonnet"
}

// startDaemon starts the maestro daemon as a background process.
func startDaemon() error {
	execPath, err := os.Executable()
	if err != nil {
		execPath = "maestro" // fallback to PATH lookup
	}
	cmd := exec.Command(execPath, "daemon")
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Detach from the parent process group
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Don't wait — daemon runs in background
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}

func loadConfigFromDir(maestroDir string) (model.Config, error) {
	data, err := os.ReadFile(filepath.Join(maestroDir, "config.yaml"))
	if err != nil {
		return model.Config{}, fmt.Errorf("read config.yaml: %w", err)
	}
	var cfg model.Config
	if err := yamlv3.Unmarshal(data, &cfg); err != nil {
		return model.Config{}, fmt.Errorf("parse config.yaml: %w", err)
	}
	return cfg, nil
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
		_ = yamlv3.Unmarshal(data, &state)
	}

	state.SchemaVersion = 1
	state.FileType = "state_continuous"
	state.Status = model.ContinuousStatusRunning
	state.MaxIterations = cfg.Continuous.MaxIterations
	now := time.Now().UTC().Format(time.RFC3339)
	state.UpdatedAt = &now

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
