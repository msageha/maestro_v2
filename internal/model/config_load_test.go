package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// writeTestConfig writes a minimal valid config.yaml into maestroDir.
func writeTestConfig(t *testing.T, maestroDir string, cfg Config) {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestroDir, "config.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// minimalConfig returns a Config that passes Validate.
func minimalConfig() Config {
	return Config{
		Project: ProjectConfig{Name: "test"},
		Maestro: MaestroConfig{Version: "2.0.0"},
		Agents: AgentsConfig{
			Workers: WorkerConfig{Count: 1, DefaultModel: "haiku"},
		},
		Worktree: WorktreeConfig{
			Enabled:          true,
			CleanupOnSuccess: true,
			CleanupOnFailure: false,
		},
	}
}

func TestLoadConfig_CacheHit(t *testing.T) {
	t.Cleanup(ResetConfigCache)

	dir := t.TempDir()
	cfg := minimalConfig()
	writeTestConfig(t, dir, cfg)

	// First call: reads from disk.
	c1, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Second call with same mtime: should return cached value.
	c2, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}

	if c1.Project.Name != c2.Project.Name {
		t.Errorf("expected cached config to match: got %q vs %q", c1.Project.Name, c2.Project.Name)
	}
}

func TestLoadConfig_CacheMissOnMtimeChange(t *testing.T) {
	t.Cleanup(ResetConfigCache)

	dir := t.TempDir()
	cfg := minimalConfig()
	writeTestConfig(t, dir, cfg)

	c1, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c1.Agents.Workers.Count != 1 {
		t.Fatalf("expected workers.count=1, got %d", c1.Agents.Workers.Count)
	}

	// Advance mtime to ensure the OS sees a different timestamp.
	cfgPath := filepath.Join(dir, "config.yaml")
	now := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(cfgPath, now, now); err != nil {
		t.Fatal(err)
	}

	// Rewrite config with a different value.
	cfg.Agents.Workers.Count = 2
	writeTestConfig(t, dir, cfg)

	c2, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Agents.Workers.Count != 2 {
		t.Errorf("expected workers.count=2 after mtime change, got %d", c2.Agents.Workers.Count)
	}
}

func TestLoadConfig_ResetClearsCache(t *testing.T) {
	t.Cleanup(ResetConfigCache)

	dir := t.TempDir()
	cfg := minimalConfig()
	writeTestConfig(t, dir, cfg)

	_, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}

	ResetConfigCache()

	// After reset, LoadConfig should still succeed (re-reads from disk).
	_, err = LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfig_DifferentPathsNotCached(t *testing.T) {
	t.Cleanup(ResetConfigCache)

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	cfg1 := minimalConfig()
	cfg1.Project.Name = "project-a"
	writeTestConfig(t, dir1, cfg1)

	cfg2 := minimalConfig()
	cfg2.Project.Name = "project-b"
	writeTestConfig(t, dir2, cfg2)

	c1, err := LoadConfig(dir1)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := LoadConfig(dir2)
	if err != nil {
		t.Fatal(err)
	}

	if c1.Project.Name != "project-a" {
		t.Errorf("expected project-a, got %q", c1.Project.Name)
	}
	if c2.Project.Name != "project-b" {
		t.Errorf("expected project-b, got %q", c2.Project.Name)
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	t.Cleanup(ResetConfigCache)

	_, err := LoadConfig(t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing config.yaml")
	}
}

func TestLoadConfig_RejectsUnknownField(t *testing.T) {
	t.Cleanup(ResetConfigCache)

	dir := t.TempDir()
	data := []byte(`
project:
  name: test
maestro:
  version: 2.0.0
agents:
  orchestrator:
    model: opus
  planner:
    model: opus
  workers:
    count: 1
    default_model: haiku
worktree:
  enabled: true
  cleanup_on_success: true
  cleanup_on_failure: false
verfiy:
  enabled: true
`)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected unknown field error")
	}
	if !strings.Contains(err.Error(), "field verfiy not found") {
		t.Fatalf("expected strict YAML unknown-field error, got: %v", err)
	}
}
