package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
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
		yaml.Unmarshal(data, &qf)
		if qf["file_type"] != "queue_task" {
			t.Errorf("queue worker%d file_type: got %v", i, qf["file_type"])
		}

		// Verify results worker file
		data, err = os.ReadFile(resultFile)
		if err != nil {
			t.Errorf("results worker%d: %v", i, err)
			continue
		}
		yaml.Unmarshal(data, &qf)
		if qf["file_type"] != "result_task" {
			t.Errorf("results worker%d file_type: got %v", i, qf["file_type"])
		}
	}

	// Verify planner and orchestrator queue files
	plannerQ, _ := os.ReadFile(filepath.Join(base, "queue", "planner.yaml"))
	var pq map[string]any
	yaml.Unmarshal(plannerQ, &pq)
	if pq["file_type"] != "queue_command" {
		t.Errorf("queue planner file_type: got %v", pq["file_type"])
	}

	orchQ, _ := os.ReadFile(filepath.Join(base, "queue", "orchestrator.yaml"))
	var oq map[string]any
	yaml.Unmarshal(orchQ, &oq)
	if oq["file_type"] != "queue_notification" {
		t.Errorf("queue orchestrator file_type: got %v", oq["file_type"])
	}

	// Verify planner results file
	plannerR, _ := os.ReadFile(filepath.Join(base, "results", "planner.yaml"))
	var pr map[string]any
	yaml.Unmarshal(plannerR, &pr)
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
	yaml.Unmarshal(data, &metrics)
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
	yaml.Unmarshal(data, &continuous)
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

func TestRun_RejectsExistingDir(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "myproject")
	os.Mkdir(projectDir, 0755)
	os.Mkdir(filepath.Join(projectDir, ".maestro"), 0755)

	err := Run(projectDir, "")
	if err == nil {
		t.Fatal("expected error for existing .maestro/")
	}
}
