package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestStandby_EmptyQueueDir(t *testing.T) {
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755)

	statuses, err := Standby(StandbyOptions{
		MaestroDir: maestroDir,
		Config:     model.Config{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(statuses) != 0 {
		t.Fatalf("expected empty results, got %d", len(statuses))
	}
}

func TestStandby_NoQueueDir(t *testing.T) {
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")

	statuses, err := Standby(StandbyOptions{
		MaestroDir: maestroDir,
		Config:     model.Config{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(statuses) != 0 {
		t.Fatalf("expected empty results, got %d", len(statuses))
	}
}

func TestStandby_WorkerCounts(t *testing.T) {
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)

	// Worker1: 2 pending, 1 in_progress → busy
	writeYAML(t, filepath.Join(queueDir, "worker1.yaml"), model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{ID: "task_1", Status: model.StatusPending},
			{ID: "task_2", Status: model.StatusPending},
			{ID: "task_3", Status: model.StatusInProgress},
		},
	})

	// Worker2: 1 pending, 0 in_progress → busy (has pending tasks)
	writeYAML(t, filepath.Join(queueDir, "worker2.yaml"), model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{ID: "task_4", Status: model.StatusPending},
			{ID: "task_5", Status: model.StatusCompleted},
		},
	})

	// Worker3: 0 pending, 0 in_progress → idle
	writeYAML(t, filepath.Join(queueDir, "worker3.yaml"), model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks:         []model.Task{},
	})

	// Worker4: only completed/failed tasks → idle
	writeYAML(t, filepath.Join(queueDir, "worker4.yaml"), model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{ID: "task_6", Status: model.StatusCompleted},
			{ID: "task_7", Status: model.StatusFailed},
		},
	})

	cfg := model.Config{
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{
				Count:        4,
				DefaultModel: "sonnet",
				Models:       map[string]string{"worker1": "opus"},
			},
		},
	}

	statuses, err := Standby(StandbyOptions{
		MaestroDir: maestroDir,
		Config:     cfg,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(statuses) != 4 {
		t.Fatalf("expected 4 workers, got %d", len(statuses))
	}

	// Sorted by worker_id
	w1 := statuses[0]
	if w1.WorkerID != "worker1" || w1.Model != "opus" || w1.PendingCount != 2 || w1.InProgressCount != 1 || w1.Status != "busy" {
		t.Errorf("worker1: got %+v", w1)
	}

	w2 := statuses[1]
	if w2.WorkerID != "worker2" || w2.Model != "sonnet" || w2.PendingCount != 1 || w2.InProgressCount != 0 || w2.Status != "busy" {
		t.Errorf("worker2: got %+v (expected busy due to pending)", w2)
	}

	w3 := statuses[2]
	if w3.WorkerID != "worker3" || w3.Model != "sonnet" || w3.PendingCount != 0 || w3.InProgressCount != 0 || w3.Status != "idle" {
		t.Errorf("worker3: got %+v", w3)
	}

	w4 := statuses[3]
	if w4.WorkerID != "worker4" || w4.Model != "sonnet" || w4.PendingCount != 0 || w4.InProgressCount != 0 || w4.Status != "idle" {
		t.Errorf("worker4: got %+v (expected idle, only terminal tasks)", w4)
	}
}

func TestStandby_ModelFilter(t *testing.T) {
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)

	writeYAML(t, filepath.Join(queueDir, "worker1.yaml"), model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{ID: "task_1", Status: model.StatusPending},
		},
	})

	writeYAML(t, filepath.Join(queueDir, "worker2.yaml"), model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{ID: "task_2", Status: model.StatusPending},
		},
	})

	cfg := model.Config{
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{
				Count:        2,
				DefaultModel: "sonnet",
				Models:       map[string]string{"worker1": "opus"},
			},
		},
	}

	// Filter for opus only
	statuses, err := Standby(StandbyOptions{
		MaestroDir:  maestroDir,
		Config:      cfg,
		ModelFilter: "opus",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("expected 1 worker with opus, got %d", len(statuses))
	}
	if statuses[0].WorkerID != "worker1" {
		t.Errorf("expected worker1, got %s", statuses[0].WorkerID)
	}

	// Filter for sonnet only
	statuses, err = Standby(StandbyOptions{
		MaestroDir:  maestroDir,
		Config:      cfg,
		ModelFilter: "sonnet",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(statuses) != 1 {
		t.Fatalf("expected 1 worker with sonnet, got %d", len(statuses))
	}
	if statuses[0].WorkerID != "worker2" {
		t.Errorf("expected worker2, got %s", statuses[0].WorkerID)
	}
}

func TestStandbyJSON_Schema(t *testing.T) {
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)

	writeYAML(t, filepath.Join(queueDir, "worker1.yaml"), model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{ID: "task_1", Status: model.StatusPending},
			{ID: "task_2", Status: model.StatusInProgress},
		},
	})

	cfg := model.Config{
		Agents: model.AgentsConfig{
			Workers: model.WorkerConfig{
				Count:        1,
				DefaultModel: "sonnet",
			},
		},
	}

	output, err := StandbyJSON(StandbyOptions{
		MaestroDir: maestroDir,
		Config:     cfg,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Validate JSON structure
	var statuses []WorkerStatus
	if err := json.Unmarshal([]byte(output), &statuses); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, output)
	}

	if len(statuses) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(statuses))
	}

	w := statuses[0]
	if w.WorkerID == "" {
		t.Error("worker_id must not be empty")
	}
	if w.Model == "" {
		t.Error("model must not be empty")
	}
	if w.Status != "idle" && w.Status != "busy" {
		t.Errorf("status must be 'idle' or 'busy', got %q", w.Status)
	}

	// Verify all expected JSON fields match the spec (§5.10)
	var raw []map[string]any
	if err := json.Unmarshal([]byte(output), &raw); err != nil {
		t.Fatalf("invalid raw JSON: %v", err)
	}
	expectedFields := []string{"worker_id", "model", "pending_count", "in_progress_count", "status"}
	for _, field := range expectedFields {
		if _, ok := raw[0][field]; !ok {
			t.Errorf("missing field %q in JSON output", field)
		}
	}

	// Verify no extra unexpected fields
	if len(raw[0]) != len(expectedFields) {
		t.Errorf("expected %d fields, got %d: %v", len(expectedFields), len(raw[0]), raw[0])
	}
}

func TestStandbyJSON_EmptyArray(t *testing.T) {
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755)

	output, err := StandbyJSON(StandbyOptions{
		MaestroDir: maestroDir,
		Config:     model.Config{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var statuses []WorkerStatus
	if err := json.Unmarshal([]byte(output), &statuses); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, output)
	}
	if statuses == nil {
		t.Fatal("expected non-nil empty array, got nil")
	}
	if len(statuses) != 0 {
		t.Fatalf("expected empty array, got %d elements", len(statuses))
	}
}

func TestStandby_IdleBusyLogic(t *testing.T) {
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)

	tests := []struct {
		name     string
		tasks    []model.Task
		expected string
	}{
		{
			name:     "no tasks",
			tasks:    []model.Task{},
			expected: "idle",
		},
		{
			name:     "only completed tasks",
			tasks:    []model.Task{{ID: "t1", Status: model.StatusCompleted}},
			expected: "idle",
		},
		{
			name:     "only failed tasks",
			tasks:    []model.Task{{ID: "t1", Status: model.StatusFailed}},
			expected: "idle",
		},
		{
			name:     "only cancelled tasks",
			tasks:    []model.Task{{ID: "t1", Status: model.StatusCancelled}},
			expected: "idle",
		},
		{
			name:     "one pending task",
			tasks:    []model.Task{{ID: "t1", Status: model.StatusPending}},
			expected: "busy",
		},
		{
			name:     "one in_progress task",
			tasks:    []model.Task{{ID: "t1", Status: model.StatusInProgress}},
			expected: "busy",
		},
		{
			name: "mixed pending and completed",
			tasks: []model.Task{
				{ID: "t1", Status: model.StatusCompleted},
				{ID: "t2", Status: model.StatusPending},
			},
			expected: "busy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up queue dir for each test
			os.RemoveAll(queueDir)
			os.MkdirAll(queueDir, 0755)

			writeYAML(t, filepath.Join(queueDir, "worker1.yaml"), model.TaskQueue{
				SchemaVersion: 1,
				FileType:      "task_queue",
				Tasks:         tt.tasks,
			})

			statuses, err := Standby(StandbyOptions{
				MaestroDir: maestroDir,
				Config:     model.Config{Agents: model.AgentsConfig{Workers: model.WorkerConfig{DefaultModel: "sonnet"}}},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(statuses) != 1 {
				t.Fatalf("expected 1 worker, got %d", len(statuses))
			}
			if statuses[0].Status != tt.expected {
				t.Errorf("expected status %q, got %q", tt.expected, statuses[0].Status)
			}
		})
	}
}

func writeYAML(t *testing.T, path string, v any) {
	t.Helper()
	data, err := yamlv3.Marshal(v)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}
