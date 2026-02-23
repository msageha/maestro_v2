package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	yamlv3 "gopkg.in/yaml.v3"
)

func newTestMetricsHandler(maestroDir string) *MetricsHandler {
	cfg := model.Config{}
	return NewMetricsHandler(maestroDir, cfg, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
}

func TestMetricsHandler_UpdateMetrics_CreateNew(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	mh := newTestMetricsHandler(maestroDir)

	cq := model.CommandQueue{
		Commands: []model.Command{
			{ID: "cmd_001", Status: model.StatusPending},
			{ID: "cmd_002", Status: model.StatusInProgress},
		},
	}
	tq := map[string]*taskQueueEntry{
		filepath.Join(maestroDir, "queue", "worker1.yaml"): {
			Path:  filepath.Join(maestroDir, "queue", "worker1.yaml"),
			Queue: model.TaskQueue{Tasks: []model.Task{{ID: "t1", Status: model.StatusPending}, {ID: "t2", Status: model.StatusPending}}},
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

	if err := mh.UpdateMetrics(cq, tq, nq, scanStart, time.Second, counters); err != nil {
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
}

func TestMetricsHandler_UpdateMetrics_Additive(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	mh := newTestMetricsHandler(maestroDir)

	cq := model.CommandQueue{}
	tq := map[string]*taskQueueEntry{}
	nq := model.NotificationQueue{}

	// First update
	counters1 := &ScanCounters{CommandsDispatched: 3, TasksDispatched: 5}
	if err := mh.UpdateMetrics(cq, tq, nq, time.Now(), time.Second, counters1); err != nil {
		t.Fatal(err)
	}

	// Second update
	counters2 := &ScanCounters{CommandsDispatched: 2, TasksDispatched: 1, DeadLetters: 4}
	if err := mh.UpdateMetrics(cq, tq, nq, time.Now(), time.Second, counters2); err != nil {
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

func TestMetricsHandler_UpdateDashboard(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	mh := newTestMetricsHandler(maestroDir)

	cq := model.CommandQueue{
		Commands: []model.Command{
			{ID: "cmd_001", Status: model.StatusInProgress, Priority: 10, Attempts: 2},
			{ID: "cmd_002", Status: model.StatusPending, Priority: 5},
		},
	}
	tq := map[string]*taskQueueEntry{
		filepath.Join(maestroDir, "queue", "worker1.yaml"): {
			Path: filepath.Join(maestroDir, "queue", "worker1.yaml"),
			Queue: model.TaskQueue{Tasks: []model.Task{
				{ID: "t1", Status: model.StatusPending},
				{ID: "t2", Status: model.StatusInProgress},
			}},
		},
	}
	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{ID: "ntf_001", Status: model.StatusPending},
		},
	}

	if err := mh.UpdateDashboard(cq, tq, nq); err != nil {
		t.Fatalf("UpdateDashboard: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(maestroDir, "dashboard.md"))
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	content := string(data)

	// Check sections exist
	if !strings.Contains(content, "# Maestro Dashboard") {
		t.Error("missing dashboard header")
	}
	if !strings.Contains(content, "## Queue Depth") {
		t.Error("missing queue depth section")
	}
	if !strings.Contains(content, "## Active Commands") {
		t.Error("missing active commands section")
	}
	if !strings.Contains(content, "cmd_001") {
		t.Error("missing active command cmd_001")
	}
	if !strings.Contains(content, "## Worker Tasks") {
		t.Error("missing worker tasks section")
	}
	if !strings.Contains(content, "worker1") {
		t.Error("missing worker1 in dashboard")
	}
}

func TestMetricsHandler_ComputeQueueDepth(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	mh := newTestMetricsHandler(maestroDir)

	cq := model.CommandQueue{
		Commands: []model.Command{
			{Status: model.StatusPending},
			{Status: model.StatusPending},
			{Status: model.StatusInProgress},
			{Status: model.StatusCompleted},
		},
	}
	tq := map[string]*taskQueueEntry{
		filepath.Join(maestroDir, "queue", "worker1.yaml"): {
			Path: filepath.Join(maestroDir, "queue", "worker1.yaml"),
			Queue: model.TaskQueue{Tasks: []model.Task{
				{Status: model.StatusPending},
				{Status: model.StatusInProgress},
				{Status: model.StatusPending},
			}},
		},
		filepath.Join(maestroDir, "queue", "worker2.yaml"): {
			Path: filepath.Join(maestroDir, "queue", "worker2.yaml"),
			Queue: model.TaskQueue{Tasks: []model.Task{
				{Status: model.StatusCompleted},
			}},
		},
	}
	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{Status: model.StatusPending},
			{Status: model.StatusPending},
			{Status: model.StatusCompleted},
		},
	}

	depth := mh.computeQueueDepth(cq, tq, nq)

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

func TestMetricsHandler_PeriodicScanIntegration(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Run a scan with empty queues — metrics should still be written
	qh.PeriodicScan()

	metricsPath := filepath.Join(maestroDir, "state", "metrics.yaml")
	if _, err := os.Stat(metricsPath); os.IsNotExist(err) {
		t.Error("metrics.yaml should be created after PeriodicScan")
	}

	dashboardPath := filepath.Join(maestroDir, "dashboard.md")
	if _, err := os.Stat(dashboardPath); os.IsNotExist(err) {
		t.Error("dashboard.md should be created after PeriodicScan")
	}
}
