package model

import (
	"os"
	"path/filepath"
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

// TestRetiredConfigPaths_DetectionTable pins the detection logic for
// nested retired keys (the schema-walk side of the unknown-key WARN).
// The earlier check only saw top-level typos, so a legacy
// `worktree.commit_policy:` block sat in operator configs and looked
// "active" while the daemon ignored it (Report 2026-05-05).
func TestRetiredConfigPaths_DetectionTable(t *testing.T) {
	type tc struct {
		name string
		yaml string
		path string
		want bool
	}
	cases := []tc{
		{
			name: "worktree.commit_policy block present",
			yaml: "worktree:\n  enabled: true\n  commit_policy:\n    max_files: 50\n",
			path: "worktree.commit_policy",
			want: true,
		},
		{
			name: "no commit_policy nested",
			yaml: "worktree:\n  enabled: true\n",
			path: "worktree.commit_policy",
			want: false,
		},
		{
			name: "fallback top-level present",
			yaml: "fallback:\n  enabled: true\n",
			path: "fallback",
			want: true,
		},
		{
			name: "watcher.clear_second_enter_delay_ms scalar",
			yaml: "watcher:\n  clear_second_enter_delay_ms: 500\n",
			path: "watcher.clear_second_enter_delay_ms",
			want: true,
		},
		{
			name: "missing intermediate node",
			yaml: "agents:\n  workers:\n    count: 1\n",
			path: "worktree.commit_policy",
			want: false,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(c.yaml), &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			root := &doc
			if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
				root = root.Content[0]
			}
			got := nodeHasPath(root, splitDotPath(c.path))
			if got != c.want {
				t.Errorf("nodeHasPath(%q) = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestLoadConfig_IgnoresUnknownField verifies that the loader silently
// ignores unknown YAML fields. The previous strict-decode policy broke
// upgrade paths whenever a field was retired (e.g. the legacy
// `fallback:` block from the degraded-mode worker blacklist), forcing
// every operator to hand-edit config.yaml before the daemon would
// start. The autonomous LLM orchestration brief explicitly rejects
// configuration parsing as a daemon-startup failure mode, so unknown
// fields are now tolerated and surfaced only via the missing-default
// behaviour.
func TestLoadConfig_IgnoresUnknownField(t *testing.T) {
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
fallback:
  enabled: true
`)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("expected unknown fields to be ignored, got error: %v", err)
	}
	if cfg.Project.Name != "test" {
		t.Errorf("Project.Name = %q, want %q", cfg.Project.Name, "test")
	}
}
