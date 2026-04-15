package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

// discardLogger is a no-op Logger for tests.
type discardLogger struct{}

func (discardLogger) Warnf(string, ...any) {}

// testClock is a Clock that delegates to time.Now.
type testClock struct{}

func (testClock) Now() time.Time { return time.Now() }

func setupTestMaestroDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"state", "queues", "results", "logs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newTestHandler(maestroDir string) *Handler {
	return NewHandler(maestroDir, discardLogger{}, testClock{})
}

func TestHandler_UpdateMetrics_CreateNew(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	h := newTestHandler(maestroDir)

	cq := model.CommandQueue{
		Commands: []model.Command{
			{ID: "cmd_001", Status: model.StatusPending},
			{ID: "cmd_002", Status: model.StatusInProgress},
		},
	}
	snapshots := []TaskQueueSnapshot{
		{
			WorkerID: "worker1",
			Tasks:    []model.Task{{ID: "t1", Status: model.StatusPending}, {ID: "t2", Status: model.StatusPending}},
		},
	}
	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{ID: "ntf_001", Status: model.StatusPending},
		},
	}

	scanStart := time.Now()
	counters := &ScanCounters{
		CommandsDispatched: 1,
		TasksDispatched:    2,
		DeadLetters:        1,
	}

	if err := h.UpdateMetrics(cq, snapshots, nq, scanStart, counters, Gauges{WorktreeCommandsStalled: 2, BakFilesCount: 3}); err != nil {
		t.Fatalf("UpdateMetrics: %v", err)
	}

	// Read back
	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "metrics.yaml"))
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	var metrics model.Metrics
	if err := yamlv3.Unmarshal(data, &metrics); err != nil {
		t.Fatalf("parse metrics: %v", err)
	}

	if metrics.SchemaVersion != 1 {
		t.Errorf("schema_version: got %d, want 1", metrics.SchemaVersion)
	}
	if metrics.QueueDepth.Planner != 1 {
		t.Errorf("planner depth: got %d, want 1", metrics.QueueDepth.Planner)
	}
	if metrics.QueueDepth.Orchestrator != 1 {
		t.Errorf("orchestrator depth: got %d, want 1", metrics.QueueDepth.Orchestrator)
	}
	if metrics.QueueDepth.Workers["worker1"] != 2 {
		t.Errorf("worker1 depth: got %d, want 2", metrics.QueueDepth.Workers["worker1"])
	}
	if metrics.Counters.CommandsDispatched != 1 {
		t.Errorf("commands_dispatched: got %d, want 1", metrics.Counters.CommandsDispatched)
	}
	if metrics.Counters.TasksDispatched != 2 {
		t.Errorf("tasks_dispatched: got %d, want 2", metrics.Counters.TasksDispatched)
	}
	if metrics.Counters.DeadLetters != 1 {
		t.Errorf("dead_letters: got %d, want 1", metrics.Counters.DeadLetters)
	}
	if metrics.DaemonHeartbeat == nil {
		t.Error("daemon_heartbeat should be set")
	}
	if metrics.WorktreeCommandsStalled != 2 {
		t.Errorf("worktree_commands_stalled: got %d, want 2", metrics.WorktreeCommandsStalled)
	}
	if metrics.BakFilesCount != 3 {
		t.Errorf("bak_files_count: got %d, want 3", metrics.BakFilesCount)
	}
}

func TestHandler_UpdateMetrics_Additive(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	h := newTestHandler(maestroDir)

	cq := model.CommandQueue{}
	snapshots := []TaskQueueSnapshot{}
	nq := model.NotificationQueue{}

	// First update
	counters1 := &ScanCounters{CommandsDispatched: 3, TasksDispatched: 5}
	if err := h.UpdateMetrics(cq, snapshots, nq, time.Now(), counters1, Gauges{}); err != nil {
		t.Fatal(err)
	}

	// Second update
	counters2 := &ScanCounters{CommandsDispatched: 2, TasksDispatched: 1, DeadLetters: 4}
	if err := h.UpdateMetrics(cq, snapshots, nq, time.Now(), counters2, Gauges{}); err != nil {
		t.Fatal(err)
	}

	// Read back — counters should be additive
	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "metrics.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var metrics model.Metrics
	if err := yamlv3.Unmarshal(data, &metrics); err != nil {
		t.Fatal(err)
	}

	if metrics.Counters.CommandsDispatched != 5 {
		t.Errorf("commands_dispatched: got %d, want 5 (3+2)", metrics.Counters.CommandsDispatched)
	}
	if metrics.Counters.TasksDispatched != 6 {
		t.Errorf("tasks_dispatched: got %d, want 6 (5+1)", metrics.Counters.TasksDispatched)
	}
	if metrics.Counters.DeadLetters != 4 {
		t.Errorf("dead_letters: got %d, want 4 (0+4)", metrics.Counters.DeadLetters)
	}
}

func TestHandler_UpdateMetrics_RecomputedCounters(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	h := newTestHandler(maestroDir)

	cq := model.CommandQueue{}
	snapshots := []TaskQueueSnapshot{}
	nq := model.NotificationQueue{}

	// Create result files with completed and failed tasks.
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{ID: "r1", TaskID: "t1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: "2025-01-01T00:00:00Z"},
			{ID: "r2", TaskID: "t2", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: "2025-01-01T00:00:00Z"},
			{ID: "r3", TaskID: "t3", CommandID: "cmd1", Status: model.StatusFailed, CreatedAt: "2025-01-01T00:00:00Z"},
		},
	}
	rfData, err := yamlv3.Marshal(rf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestroDir, "results", "worker1.yaml"), rfData, 0644); err != nil {
		t.Fatal(err)
	}

	// First update
	counters1 := &ScanCounters{CommandsDispatched: 1}
	if err := h.UpdateMetrics(cq, snapshots, nq, time.Now(), counters1, Gauges{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "metrics.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var metrics model.Metrics
	if err := yamlv3.Unmarshal(data, &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.Counters.TasksCompleted != 2 {
		t.Errorf("TasksCompleted: got %d, want 2", metrics.Counters.TasksCompleted)
	}
	if metrics.Counters.TasksFailed != 1 {
		t.Errorf("TasksFailed: got %d, want 1", metrics.Counters.TasksFailed)
	}

	// Second update with same result files — re-computed counters should NOT accumulate.
	counters2 := &ScanCounters{CommandsDispatched: 1}
	if err := h.UpdateMetrics(cq, snapshots, nq, time.Now(), counters2, Gauges{}); err != nil {
		t.Fatal(err)
	}

	data, err = os.ReadFile(filepath.Join(maestroDir, "state", "metrics.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yamlv3.Unmarshal(data, &metrics); err != nil {
		t.Fatal(err)
	}
	// Pattern 1: re-computed — stays at 2, not 4
	if metrics.Counters.TasksCompleted != 2 {
		t.Errorf("TasksCompleted after second update: got %d, want 2 (re-computed, not accumulated)", metrics.Counters.TasksCompleted)
	}
	if metrics.Counters.TasksFailed != 1 {
		t.Errorf("TasksFailed after second update: got %d, want 1 (re-computed, not accumulated)", metrics.Counters.TasksFailed)
	}
	// Pattern 2: incremental — should accumulate
	if metrics.Counters.CommandsDispatched != 2 {
		t.Errorf("CommandsDispatched: got %d, want 2 (1+1)", metrics.Counters.CommandsDispatched)
	}
}

func TestHandler_LoadAllResultFiles_NonResultTaskIgnored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	h := newTestHandler(maestroDir)

	cq := model.CommandQueue{}
	snapshots := []TaskQueueSnapshot{}
	nq := model.NotificationQueue{}

	// Create a result file with file_type != "result_task" (e.g. "result_command").
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_command",
		Results: []model.TaskResult{
			{ID: "r1", TaskID: "t1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: "2025-01-01T00:00:00Z"},
		},
	}
	rfData, err := yamlv3.Marshal(rf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestroDir, "results", "planner.yaml"), rfData, 0644); err != nil {
		t.Fatal(err)
	}

	counters := &ScanCounters{}
	if err := h.UpdateMetrics(cq, snapshots, nq, time.Now(), counters, Gauges{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "metrics.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var metrics model.Metrics
	if err := yamlv3.Unmarshal(data, &metrics); err != nil {
		t.Fatal(err)
	}
	// Non "result_task" entries must not be counted.
	if metrics.Counters.TasksCompleted != 0 {
		t.Errorf("TasksCompleted: got %d, want 0 (non-result_task files ignored)", metrics.Counters.TasksCompleted)
	}
}

func TestHandler_LoadAllResultFiles_MalformedYAMLSkipped(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	h := newTestHandler(maestroDir)

	cq := model.CommandQueue{}
	snapshots := []TaskQueueSnapshot{}
	nq := model.NotificationQueue{}

	// Write a valid result file.
	validRF := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{ID: "r1", TaskID: "t1", CommandID: "cmd1", Status: model.StatusCompleted, CreatedAt: "2025-01-01T00:00:00Z"},
		},
	}
	validData, err := yamlv3.Marshal(validRF)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestroDir, "results", "worker1.yaml"), validData, 0644); err != nil {
		t.Fatal(err)
	}

	// Write a malformed YAML result file alongside it.
	if err := os.WriteFile(filepath.Join(maestroDir, "results", "worker2.yaml"), []byte("{{{{not valid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	counters := &ScanCounters{}
	if err := h.UpdateMetrics(cq, snapshots, nq, time.Now(), counters, Gauges{}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "metrics.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var metrics model.Metrics
	if err := yamlv3.Unmarshal(data, &metrics); err != nil {
		t.Fatal(err)
	}
	// Valid file's completed task should still be counted; malformed file is skipped.
	if metrics.Counters.TasksCompleted != 1 {
		t.Errorf("TasksCompleted: got %d, want 1 (malformed file skipped, valid file counted)", metrics.Counters.TasksCompleted)
	}
}

func TestHandler_LoadAllResultFiles_NoResultsDir(t *testing.T) {
	t.Parallel()
	// Create maestroDir without results/ subdirectory.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0755); err != nil {
		t.Fatal(err)
	}
	h := newTestHandler(dir)

	cq := model.CommandQueue{}
	snapshots := []TaskQueueSnapshot{}
	nq := model.NotificationQueue{}
	counters := &ScanCounters{}

	// Should not error — missing results dir returns nil result map.
	if err := h.UpdateMetrics(cq, snapshots, nq, time.Now(), counters, Gauges{}); err != nil {
		t.Fatalf("UpdateMetrics with missing results dir: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "state", "metrics.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var metrics model.Metrics
	if err := yamlv3.Unmarshal(data, &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.Counters.TasksCompleted != 0 {
		t.Errorf("TasksCompleted: got %d, want 0", metrics.Counters.TasksCompleted)
	}
	if metrics.Counters.TasksFailed != 0 {
		t.Errorf("TasksFailed: got %d, want 0", metrics.Counters.TasksFailed)
	}
}

func TestHandler_ComputeQueueDepth(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	h := newTestHandler(maestroDir)

	cq := model.CommandQueue{
		Commands: []model.Command{
			{Status: model.StatusPending},
			{Status: model.StatusPending},
			{Status: model.StatusInProgress},
			{Status: model.StatusCompleted},
		},
	}
	snapshots := []TaskQueueSnapshot{
		{
			WorkerID: "worker1",
			Tasks: []model.Task{
				{Status: model.StatusPending},
				{Status: model.StatusInProgress},
				{Status: model.StatusPending},
			},
		},
		{
			WorkerID: "worker2",
			Tasks: []model.Task{
				{Status: model.StatusCompleted},
			},
		},
	}
	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{Status: model.StatusPending},
			{Status: model.StatusPending},
			{Status: model.StatusCompleted},
		},
	}

	depth := h.computeQueueDepth(cq, snapshots, nq)

	if depth.Planner != 2 {
		t.Errorf("planner: got %d, want 2", depth.Planner)
	}
	if depth.Orchestrator != 2 {
		t.Errorf("orchestrator: got %d, want 2", depth.Orchestrator)
	}
	if depth.Workers["worker1"] != 2 {
		t.Errorf("worker1: got %d, want 2", depth.Workers["worker1"])
	}
	if depth.Workers["worker2"] != 0 {
		t.Errorf("worker2: got %d, want 0", depth.Workers["worker2"])
	}
}
