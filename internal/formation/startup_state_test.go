package formation

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestReadDaemonPID(t *testing.T) {
	tests := []struct {
		name    string
		content string // file content; empty string means file doesn't exist
		noFile  bool
		want    int
	}{
		{"valid pid", "12345", false, 12345},
		{"whitespace padded", "  42  \n", false, 42},
		{"non-numeric", "abc", false, 0},
		{"empty file", "", false, 0},
		{"missing file", "", true, 0},
		{"large pid", "9999999", false, 9999999},
		{"negative pid", "-1", false, 0},
		{"zero pid", "0", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			pidPath := filepath.Join(dir, "daemon.pid")
			if !tt.noFile {
				if err := os.WriteFile(pidPath, []byte(tt.content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.noFile {
				pidPath = filepath.Join(dir, "nonexistent.pid")
			}
			got := readDaemonPID(pidPath)
			if got != tt.want {
				t.Errorf("readDaemonPID() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestInferFileType(t *testing.T) {
	tests := []struct {
		name     string
		dir      string
		filename string
		want     string
	}{
		{"queue planner", "/tmp/queue", "planner.yaml", "queue_command"},
		{"queue orchestrator", "/tmp/queue", "orchestrator.yaml", "queue_notification"},
		{"queue planner_signals", "/tmp/queue", "planner_signals.yaml", "planner_signal_queue"},
		{"queue generic", "/tmp/queue", "worker1.yaml", "queue_task"},
		{"results planner", "/tmp/results", "planner.yaml", "result_command"},
		{"results generic", "/tmp/results", "task1.yaml", "result_task"},
		{"state commands", "/tmp/state/commands", "cmd1.yaml", "state_command"},
		{"unknown dir", "/tmp/unknown", "file.yaml", ""},
		{"nested queue path", "/a/b/c/queue", "worker2.yaml", "queue_task"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferFileType(tt.dir, tt.filename)
			if got != tt.want {
				t.Errorf("inferFileType(%q, %q) = %q, want %q", tt.dir, tt.filename, got, tt.want)
			}
		})
	}
}

func TestActivateContinuousMode_FreshFile(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{MaxIterations: 5},
	}

	if err := activateContinuousMode(maestroDir, cfg); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "continuous.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var state model.Continuous
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}

	if state.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", state.SchemaVersion)
	}
	if state.FileType != "state_continuous" {
		t.Errorf("file_type = %q, want %q", state.FileType, "state_continuous")
	}
	if state.Status != model.ContinuousStatusRunning {
		t.Errorf("status = %q, want %q", state.Status, model.ContinuousStatusRunning)
	}
	if state.MaxIterations != 5 {
		t.Errorf("max_iterations = %d, want 5", state.MaxIterations)
	}
	if state.UpdatedAt == "" {
		t.Error("updated_at should be set")
	}
	// Verify updated_at is valid RFC3339
	if _, err := time.Parse(time.RFC3339, state.UpdatedAt); err != nil {
		t.Errorf("updated_at %q is not valid RFC3339: %v", state.UpdatedAt, err)
	}
}

func TestActivateContinuousMode_OverwritePreservesIteration(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	continuousPath := filepath.Join(maestroDir, "state", "continuous.yaml")

	// Write initial state with current_iteration set
	initial := model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 3,
		Status:           model.ContinuousStatusStopped,
	}
	data, err := yamlv3.Marshal(&initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(continuousPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := model.Config{
		Continuous: model.ContinuousConfig{MaxIterations: 10},
	}
	if err := activateContinuousMode(maestroDir, cfg); err != nil {
		t.Fatal(err)
	}

	data, err = os.ReadFile(continuousPath)
	if err != nil {
		t.Fatal(err)
	}
	var state model.Continuous
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}

	// current_iteration should be preserved from existing file
	if state.CurrentIteration != 3 {
		t.Errorf("current_iteration = %d, want 3 (preserved)", state.CurrentIteration)
	}
	if state.Status != model.ContinuousStatusRunning {
		t.Errorf("status = %q, want %q", state.Status, model.ContinuousStatusRunning)
	}
	if state.MaxIterations != 10 {
		t.Errorf("max_iterations = %d, want 10", state.MaxIterations)
	}
}

func TestActivateContinuousMode_CorruptExistingYAML(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	continuousPath := filepath.Join(maestroDir, "state", "continuous.yaml")

	// Write corrupt YAML content
	if err := os.WriteFile(continuousPath, []byte("{{not valid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := model.Config{
		Continuous: model.ContinuousConfig{MaxIterations: 7},
	}
	// Should warn but not error - overwrites corrupt file
	if err := activateContinuousMode(maestroDir, cfg); err != nil {
		t.Fatalf("expected no error on corrupt YAML, got %v", err)
	}

	data, err := os.ReadFile(continuousPath)
	if err != nil {
		t.Fatal(err)
	}
	var state model.Continuous
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		t.Fatalf("result should be valid YAML: %v", err)
	}
	if state.Status != model.ContinuousStatusRunning {
		t.Errorf("status = %q, want %q", state.Status, model.ContinuousStatusRunning)
	}
}

// TestReflectFlags_NoExplicitFlags_PreservesConfigFile pins that reflectFlags
// does not touch config.yaml when no CLI flag was explicitly set: the write
// path re-serializes through the Config struct, which strips user comments
// and unknown fields, so an unconditional write destroyed hand-edited config
// on every `maestro up`.
func TestReflectFlags_NoExplicitFlags_PreservesConfigFile(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	configPath := filepath.Join(maestroDir, "config.yaml")
	original := []byte(`# operator notes: keep worker count at 2 until CI stabilises
schema_version: 1
file_type: config
project:
  name: test
maestro:
  version: "2.0.0"
agents:
  workers:
    count: 2 # inline comment
custom_unknown_field: preserved
`)
	if err := os.WriteFile(configPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	opts := UpOptions{
		MaestroDir: maestroDir,
		// Boost/Continuous carry values but are NOT explicitly set.
		Boost:      true,
		Continuous: true,
	}
	if err := reflectFlags(opts); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Errorf("config.yaml was rewritten without explicit flags:\n--- before ---\n%s\n--- after ---\n%s", original, after)
	}
}

func TestActivateContinuousMode_MissingStateDir(t *testing.T) {
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	// Don't create state/ dir - activateContinuousMode should create it

	cfg := model.Config{
		Continuous: model.ContinuousConfig{MaxIterations: 1},
	}
	if err := activateContinuousMode(maestroDir, cfg); err != nil {
		t.Fatalf("expected no error when state/ dir missing, got %v", err)
	}

	if _, err := os.Stat(filepath.Join(maestroDir, "state", "continuous.yaml")); err != nil {
		t.Errorf("continuous.yaml should exist after activation: %v", err)
	}
}
