package formation

import (
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func setupTestMaestroDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	dirs := []string{
		"queue", "results", "state/commands", "logs",
		"dead_letters", "quarantine", "locks",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(maestroDir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return maestroDir
}

func writeConfig(t *testing.T, maestroDir string, cfg model.Config) {
	t.Helper()
	configPath := filepath.Join(maestroDir, "config.yaml")
	if err := yamlutil.AtomicWrite(configPath, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestResolveModel_Default(t *testing.T) {
	cfg := model.Config{}
	if m := resolveModel(cfg, "orchestrator"); m != "sonnet" {
		t.Errorf("expected sonnet, got %s", m)
	}
	if m := resolveModel(cfg, "planner"); m != "sonnet" {
		t.Errorf("expected sonnet, got %s", m)
	}
	if m := resolveModel(cfg, "worker1"); m != "sonnet" {
		t.Errorf("expected sonnet, got %s", m)
	}
}

func TestResolveModel_OrchestratorOverride(t *testing.T) {
	cfg := model.Config{
		Agents: model.AgentsConfig{
			Orchestrator: model.AgentConfig{Model: "opus"},
		},
	}
	if m := resolveModel(cfg, "orchestrator"); m != "opus" {
		t.Errorf("expected opus, got %s", m)
	}
}

func TestResolveModel_PlannerOverride(t *testing.T) {
	cfg := model.Config{
		Agents: model.AgentsConfig{
			Planner: model.AgentConfig{Model: "haiku"},
		},
	}
	if m := resolveModel(cfg, "planner"); m != "haiku" {
		t.Errorf("expected haiku, got %s", m)
	}
}

func TestResolveModel_WorkerBoost(t *testing.T) {
	cfg := model.Config{
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{Boost: true, DefaultModel: "haiku"},
		},
	}
	if m := resolveModel(cfg, "worker1"); m != "opus" {
		t.Errorf("expected opus (boost), got %s", m)
	}
}

func TestResolveModel_WorkerPerWorkerOverride(t *testing.T) {
	cfg := model.Config{
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{
				Models:       map[string]string{"worker1": "haiku", "worker2": "opus"},
				DefaultModel: "sonnet",
			},
		},
	}
	if m := resolveModel(cfg, "worker1"); m != "haiku" {
		t.Errorf("expected haiku, got %s", m)
	}
	if m := resolveModel(cfg, "worker2"); m != "opus" {
		t.Errorf("expected opus, got %s", m)
	}
	if m := resolveModel(cfg, "worker3"); m != "sonnet" {
		t.Errorf("expected sonnet (default), got %s", m)
	}
}

func TestResolveModel_WorkerDefaultModel(t *testing.T) {
	cfg := model.Config{
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{DefaultModel: "haiku"},
		},
	}
	if m := resolveModel(cfg, "worker1"); m != "haiku" {
		t.Errorf("expected haiku, got %s", m)
	}
}

func TestClearYAMLFiles(t *testing.T) {
	dir := t.TempDir()

	// Create yaml, yml, and non-yaml files
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(dir, "b.yml"), []byte("b"), 0644)
	os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c"), 0644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0755)

	if err := clearYAMLFiles(dir); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	if names["a.yaml"] {
		t.Error("a.yaml should have been removed")
	}
	if names["b.yml"] {
		t.Error("b.yml should have been removed")
	}
	if !names["c.txt"] {
		t.Error("c.txt should be preserved")
	}
	if !names["subdir"] {
		t.Error("subdir should be preserved")
	}
}

func TestClearYAMLFiles_NonExistentDir(t *testing.T) {
	if err := clearYAMLFiles("/nonexistent/path"); err != nil {
		t.Errorf("expected nil for nonexistent dir, got %v", err)
	}
}

func TestClearAllFiles(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0644)
	os.Mkdir(filepath.Join(dir, "subdir"), 0755)

	if err := clearAllFiles(dir); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	if names["a.yaml"] || names["b.txt"] {
		t.Error("files should have been removed")
	}
	if !names["subdir"] {
		t.Error("subdir should be preserved (non-recursive)")
	}
}

func TestClearAllFiles_NonExistentDir(t *testing.T) {
	if err := clearAllFiles("/nonexistent/path"); err != nil {
		t.Errorf("expected nil for nonexistent dir, got %v", err)
	}
}

func TestLoadConfigFromDir(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{Count: 3, DefaultModel: "haiku"},
		},
		Continuous: model.ContinuousConfig{Enabled: true},
	}
	writeConfig(t, maestroDir, cfg)

	loaded, err := loadConfigFromDir(maestroDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agents.Workers.Count != 3 {
		t.Errorf("expected worker count 3, got %d", loaded.Agents.Workers.Count)
	}
	if loaded.Agents.Workers.DefaultModel != "haiku" {
		t.Errorf("expected default model haiku, got %s", loaded.Agents.Workers.DefaultModel)
	}
	if !loaded.Continuous.Enabled {
		t.Error("expected continuous.enabled=true")
	}
}

func TestReflectFlags(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	writeConfig(t, maestroDir, model.Config{
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{Count: 2},
		},
	})

	opts := UpOptions{
		MaestroDir:    maestroDir,
		Boost:         true,
		Continuous:    true,
		BoostSet:      true,
		ContinuousSet: true,
	}

	if err := reflectFlags(opts); err != nil {
		t.Fatal(err)
	}

	// Reload and verify
	data, _ := os.ReadFile(filepath.Join(maestroDir, "config.yaml"))
	var cfg model.Config
	yamlv3.Unmarshal(data, &cfg)

	if !cfg.Agents.Workers.Boost {
		t.Error("expected boost=true")
	}
	if !cfg.Continuous.Enabled {
		t.Error("expected continuous.enabled=true")
	}
}

func TestStartupRecovery(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	if err := startupRecovery(maestroDir); err != nil {
		t.Fatal(err)
	}

	// Verify directories were created
	dirs := []string{
		"queue", "results", "state/commands", "logs",
		"dead_letters", "quarantine", "locks",
	}
	for _, d := range dirs {
		info, err := os.Stat(filepath.Join(maestroDir, d))
		if err != nil {
			t.Errorf("expected dir %s to exist: %v", d, err)
		} else if !info.IsDir() {
			t.Errorf("expected %s to be a directory", d)
		}
	}
}

func TestResetFormation_ClearsTransientState(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	// Create transient files
	os.WriteFile(filepath.Join(maestroDir, "queue", "cmd1.yaml"), []byte("test"), 0644)
	os.WriteFile(filepath.Join(maestroDir, "results", "res1.yaml"), []byte("test"), 0644)
	os.WriteFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"), []byte("test"), 0644)
	os.WriteFile(filepath.Join(maestroDir, "dead_letters", "dl1.yaml"), []byte("test"), 0644)

	// Create quarantine file (should be preserved)
	os.WriteFile(filepath.Join(maestroDir, "quarantine", "corrupt.yaml"), []byte("test"), 0644)

	// Write a minimal config so resetFormation's UDS client can at least attempt
	writeConfig(t, maestroDir, model.Config{})

	if err := resetFormation(maestroDir); err != nil {
		t.Fatal(err)
	}

	// Verify transient files are cleared
	for _, path := range []string{
		filepath.Join(maestroDir, "queue", "cmd1.yaml"),
		filepath.Join(maestroDir, "results", "res1.yaml"),
		filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"),
		filepath.Join(maestroDir, "dead_letters", "dl1.yaml"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed", path)
		}
	}

	// Verify quarantine is preserved
	if _, err := os.Stat(filepath.Join(maestroDir, "quarantine", "corrupt.yaml")); err != nil {
		t.Errorf("quarantine should be preserved: %v", err)
	}

	// Verify continuous.yaml was reset
	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "continuous.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cont model.Continuous
	if err := yamlv3.Unmarshal(data, &cont); err != nil {
		t.Fatal(err)
	}
	if cont.CurrentIteration != 0 {
		t.Errorf("expected current_iteration=0, got %d", cont.CurrentIteration)
	}
	if cont.Status != model.ContinuousStatusStopped {
		t.Errorf("expected status=stopped, got %s", cont.Status)
	}

	// Verify metrics.yaml was reset
	mdata, err := os.ReadFile(filepath.Join(maestroDir, "state", "metrics.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var metrics model.Metrics
	if err := yamlv3.Unmarshal(mdata, &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.SchemaVersion != 1 {
		t.Errorf("expected schema_version=1, got %d", metrics.SchemaVersion)
	}
}

func TestValidateDaemonPID_NoLockFile(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	os.WriteFile(pidPath, []byte("12345"), 0644)

	// No lock file → PID cannot be validated
	pid := validateDaemonPID(maestroDir)
	if pid != 0 {
		t.Errorf("expected 0 when lock file missing, got %d", pid)
	}
	// PID file should be removed as untrusted
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected daemon.pid to be removed when lock file missing")
	}
}

func TestValidateDaemonPID_MatchingLock(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	os.WriteFile(pidPath, []byte("12345"), 0644)
	os.WriteFile(lockPath, []byte("12345\n"), 0644)

	pid := validateDaemonPID(maestroDir)
	if pid != 12345 {
		t.Errorf("expected 12345, got %d", pid)
	}
}

func TestValidateDaemonPID_MismatchingLock(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	os.WriteFile(pidPath, []byte("12345"), 0644)
	os.WriteFile(lockPath, []byte("99999\n"), 0644)

	pid := validateDaemonPID(maestroDir)
	if pid != 0 {
		t.Errorf("expected 0 for mismatching lock, got %d", pid)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected daemon.pid to be removed on mismatch")
	}
}

func TestValidateDaemonPID_NoPidFile(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	pid := validateDaemonPID(maestroDir)
	if pid != 0 {
		t.Errorf("expected 0 when daemon.pid missing, got %d", pid)
	}
}

func TestStartupRecovery_CleansStaleFiles(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	// Create stale PID file and socket
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	socketPath := filepath.Join(maestroDir, "daemon.sock")
	os.WriteFile(pidPath, []byte("12345"), 0644)
	os.WriteFile(socketPath, []byte{}, 0644)

	if err := startupRecovery(maestroDir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected daemon.pid to be removed by startupRecovery")
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Error("expected socket to be removed by startupRecovery")
	}
}

func TestProcessAlive_DeadPID(t *testing.T) {
	// Very large PID should not be alive
	if processAlive(99999999) {
		t.Error("expected processAlive(99999999) to return false")
	}
}

func TestProcessAlive_ZeroAndNegative(t *testing.T) {
	if processAlive(0) {
		t.Error("expected processAlive(0) to return false")
	}
	if processAlive(-1) {
		t.Error("expected processAlive(-1) to return false")
	}
}

func TestRunUp_SessionExistsGuard(t *testing.T) {
	// Test that ErrSessionExists is returned when session already exists and Force is false.
	// We use a mock approach: directly test the guard logic by simulating SessionExists.

	// Since we can't easily mock tmux.SessionExists in unit tests without tmux,
	// we test the error type is properly exported and usable.
	if ErrSessionExists == nil {
		t.Fatal("ErrSessionExists should not be nil")
	}
	if ErrSessionExists.Error() != "maestro session already exists (use --force to recreate)" {
		t.Errorf("unexpected error message: %s", ErrSessionExists.Error())
	}
}

func TestUpOptions_ForceField(t *testing.T) {
	opts := UpOptions{
		Force: true,
	}
	if !opts.Force {
		t.Error("expected Force=true")
	}

	opts2 := UpOptions{}
	if opts2.Force {
		t.Error("expected Force=false by default")
	}
}
