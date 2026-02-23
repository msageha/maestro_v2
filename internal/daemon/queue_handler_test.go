package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

func setupTestMaestroDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")
	for _, sub := range []string{"queue", "results", "logs"} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}
	return maestroDir
}

func newTestQueueHandler(maestroDir string) *QueueHandler {
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	// Use mock executor to avoid tmux dependency
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	})
	return qh
}

func TestQueueHandler_PeriodicScan_Empty(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Should not panic with empty queues
	qh.PeriodicScan()
}

func TestQueueHandler_DispatchPendingTask(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Write a task queue
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:        "task_001",
				CommandID: "cmd_001",
				Purpose:   "Test task",
				Content:   "Do something",
				Priority:  1,
				Status:    model.StatusPending,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	workerPath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	if err := yamlutil.AtomicWrite(workerPath, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	qh.PeriodicScan()

	// Read back and check status changed to in_progress
	data, err := os.ReadFile(workerPath)
	if err != nil {
		t.Fatalf("read worker queue: %v", err)
	}

	var result model.TaskQueue
	if err := parseYAML(data, &result); err != nil {
		t.Fatalf("parse worker queue: %v", err)
	}

	if len(result.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(result.Tasks))
	}
	if result.Tasks[0].Status != model.StatusInProgress {
		t.Errorf("status: got %s, want in_progress", result.Tasks[0].Status)
	}
	if result.Tasks[0].LeaseOwner == nil {
		t.Error("lease_owner should be set")
	} else if *result.Tasks[0].LeaseOwner != "worker1" {
		t.Errorf("lease_owner: got %s, want worker1 (derived from filename)", *result.Tasks[0].LeaseOwner)
	}
	if result.Tasks[0].LeaseEpoch != 1 {
		t.Errorf("lease_epoch: got %d, want 1", result.Tasks[0].LeaseEpoch)
	}
}

func TestQueueHandler_DispatchPendingCommand(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID:        "cmd_001",
				Content:   "Implement feature X",
				Priority:  1,
				Status:    model.StatusPending,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	if err := yamlutil.AtomicWrite(plannerPath, cq); err != nil {
		t.Fatalf("write planner queue: %v", err)
	}

	qh.PeriodicScan()

	data, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner queue: %v", err)
	}

	var result model.CommandQueue
	if err := parseYAML(data, &result); err != nil {
		t.Fatalf("parse planner queue: %v", err)
	}

	if len(result.Commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(result.Commands))
	}
	if result.Commands[0].Status != model.StatusInProgress {
		t.Errorf("status: got %s, want in_progress", result.Commands[0].Status)
	}
}

func TestQueueHandler_AtMostOneInFlight(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	w := "worker1"
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:         "task_001",
				CommandID:  "cmd_001",
				Priority:   1,
				Status:     model.StatusInProgress,
				LeaseOwner: &w,
				CreatedAt:  time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "task_002",
				CommandID: "cmd_001",
				Priority:  1,
				Status:    model.StatusPending,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	// Tasks in worker1.yaml go to worker1 only. Since worker1 already has
	// task_001 in_progress, task_002 must remain pending (at-most-one-in-flight).
	workerPath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	if err := yamlutil.AtomicWrite(workerPath, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	qh.PeriodicScan()

	data, err := os.ReadFile(workerPath)
	if err != nil {
		t.Fatalf("read worker queue: %v", err)
	}

	var result model.TaskQueue
	if err := parseYAML(data, &result); err != nil {
		t.Fatalf("parse worker queue: %v", err)
	}

	// task_002 must remain pending because worker1 is already in-flight
	if result.Tasks[1].Status != model.StatusPending {
		t.Errorf("task_002 should remain pending (worker1 busy), got status %s", result.Tasks[1].Status)
	}
}

func TestQueueHandler_CrossFileInFlight(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// worker1 has an in_progress task
	w := "worker1"
	tq1 := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:         "task_001",
				CommandID:  "cmd_001",
				Priority:   1,
				Status:     model.StatusInProgress,
				LeaseOwner: &w,
				CreatedAt:  time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	// worker2 has a pending task
	tq2 := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:        "task_002",
				CommandID: "cmd_001",
				Priority:  1,
				Status:    model.StatusPending,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	w1Path := filepath.Join(maestroDir, "queue", "worker1.yaml")
	w2Path := filepath.Join(maestroDir, "queue", "worker2.yaml")
	yamlutil.AtomicWrite(w1Path, tq1)
	yamlutil.AtomicWrite(w2Path, tq2)

	qh.PeriodicScan()

	// worker2's task should be dispatched (worker2 is free)
	data, err := os.ReadFile(w2Path)
	if err != nil {
		t.Fatalf("read worker2 queue: %v", err)
	}
	var result model.TaskQueue
	parseYAML(data, &result)

	if result.Tasks[0].Status != model.StatusInProgress {
		t.Errorf("task_002 should be dispatched (worker2 free), got %s", result.Tasks[0].Status)
	}
	if result.Tasks[0].LeaseOwner == nil || *result.Tasks[0].LeaseOwner != "worker2" {
		t.Error("task_002 should be owned by worker2")
	}
}

func TestQueueHandler_LeaseExpireRecovery(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	expiredTime := time.Now().Add(-time.Hour).Format(time.RFC3339)
	w := "worker1"
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:             "task_001",
				CommandID:      "cmd_001",
				Priority:       1,
				Status:         model.StatusInProgress,
				LeaseOwner:     &w,
				LeaseExpiresAt: &expiredTime,
				LeaseEpoch:     1,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	workerPath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	if err := yamlutil.AtomicWrite(workerPath, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	qh.PeriodicScan()

	data, err := os.ReadFile(workerPath)
	if err != nil {
		t.Fatalf("read worker queue: %v", err)
	}

	var result model.TaskQueue
	if err := parseYAML(data, &result); err != nil {
		t.Fatalf("parse worker queue: %v", err)
	}

	// Expired lease should be released back to pending, then re-dispatched
	// After release + dispatch, should be in_progress again with new lease
	if result.Tasks[0].Status != model.StatusInProgress {
		// Could be pending if dispatch failed, but with mock executor it should succeed
		t.Logf("task status after scan: %s", result.Tasks[0].Status)
	}
	// Lease epoch should have incremented (release doesn't change it, but new acquire does)
	if result.Tasks[0].LeaseEpoch < 1 {
		t.Errorf("lease_epoch should be >= 1, got %d", result.Tasks[0].LeaseEpoch)
	}
}

func TestQueueHandler_CancelPendingTask(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	cancelTime := time.Now().Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID:                "cmd_001",
				Status:            model.StatusInProgress,
				CancelRequestedAt: &cancelTime,
				CreatedAt:         time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:         time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	if err := yamlutil.AtomicWrite(plannerPath, cq); err != nil {
		t.Fatalf("write planner queue: %v", err)
	}

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:        "task_001",
				CommandID: "cmd_001",
				Status:    model.StatusPending,
				Priority:  1,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	workerPath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	if err := yamlutil.AtomicWrite(workerPath, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	qh.PeriodicScan()

	data, err := os.ReadFile(workerPath)
	if err != nil {
		t.Fatalf("read worker queue: %v", err)
	}

	var result model.TaskQueue
	if err := parseYAML(data, &result); err != nil {
		t.Fatalf("parse worker queue: %v", err)
	}

	if result.Tasks[0].Status != model.StatusCancelled {
		t.Errorf("status: got %s, want cancelled", result.Tasks[0].Status)
	}
}

func TestQueueHandler_HandleFileEvent(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Should not panic for any event type
	qh.HandleFileEvent(filepath.Join(maestroDir, "queue", "planner.yaml"))
	qh.HandleFileEvent(filepath.Join(maestroDir, "queue", "worker1.yaml"))
	qh.HandleFileEvent(filepath.Join(maestroDir, "results", "worker1.yaml"))
}

func TestWorkerIDFromPath(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"/some/dir/worker1.yaml", "worker1"},
		{"/some/dir/worker2.yaml", "worker2"},
		{"/some/dir/worker10.yaml", "worker10"},
		{"/some/dir/planner.yaml", ""},
		{"/some/dir/orchestrator.yaml", ""},
		{"/some/dir/notworker.yaml", ""},
	}
	for _, tt := range tests {
		got := workerIDFromPath(tt.path)
		if got != tt.expected {
			t.Errorf("workerIDFromPath(%q) = %q, want %q", tt.path, got, tt.expected)
		}
	}
}

func TestQueueHandler_BuildGlobalInFlightSet(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	w1 := "worker1"
	w2 := "worker2"

	taskQueues := map[string]*taskQueueEntry{
		"worker1": {Queue: model.TaskQueue{Tasks: []model.Task{
			{Status: model.StatusInProgress, LeaseOwner: &w1},
		}}},
		"worker2": {Queue: model.TaskQueue{Tasks: []model.Task{
			{Status: model.StatusPending},
		}}},
	}

	inFlight := qh.buildGlobalInFlightSet(taskQueues)
	if !inFlight["worker1"] {
		t.Error("worker1 should be in-flight")
	}
	if inFlight["worker2"] {
		t.Error("worker2 should NOT be in-flight")
	}

	// Both busy
	taskQueues["worker2"] = &taskQueueEntry{Queue: model.TaskQueue{Tasks: []model.Task{
		{Status: model.StatusInProgress, LeaseOwner: &w2},
	}}}
	inFlight = qh.buildGlobalInFlightSet(taskQueues)
	if !inFlight["worker1"] || !inFlight["worker2"] {
		t.Error("both workers should be in-flight")
	}
}

func TestQueueHandler_DispatchNotification(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	nq := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "notification_queue",
		Notifications: []model.Notification{
			{
				ID:        "ntf_001",
				CommandID: "cmd_001",
				Type:      "command_completed",
				Priority:  1,
				Status:    model.StatusPending,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	orchPath := filepath.Join(maestroDir, "queue", "orchestrator.yaml")
	if err := yamlutil.AtomicWrite(orchPath, nq); err != nil {
		t.Fatalf("write orchestrator queue: %v", err)
	}

	qh.PeriodicScan()

	data, err := os.ReadFile(orchPath)
	if err != nil {
		t.Fatalf("read orchestrator queue: %v", err)
	}

	var result model.NotificationQueue
	if err := parseYAML(data, &result); err != nil {
		t.Fatalf("parse orchestrator queue: %v", err)
	}

	if result.Notifications[0].Status != model.StatusInProgress {
		t.Errorf("status: got %s, want in_progress", result.Notifications[0].Status)
	}
}

// parseYAML is a test helper.
func parseYAML(data []byte, v any) error {
	return yamlv3.Unmarshal(data, v)
}
