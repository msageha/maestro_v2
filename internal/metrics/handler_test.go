package metrics

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

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
	cfg := model.Config{}
	return NewHandler(maestroDir, cfg, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)
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

	if err := h.UpdateMetrics(cq, snapshots, nq, scanStart, time.Second, counters, Gauges{WorktreeCommandsStalled: 2, BakFilesCount: 3}); err != nil {
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
	if err := h.UpdateMetrics(cq, snapshots, nq, time.Now(), time.Second, counters1, Gauges{}); err != nil {
		t.Fatal(err)
	}

	// Second update
	counters2 := &ScanCounters{CommandsDispatched: 2, TasksDispatched: 1, DeadLetters: 4}
	if err := h.UpdateMetrics(cq, snapshots, nq, time.Now(), time.Second, counters2, Gauges{}); err != nil {
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
