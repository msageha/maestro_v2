package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestCountBakFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mustWrite := func(p string) {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(dir, "a.bak"))
	mustWrite(filepath.Join(dir, "sub", "b.bak"))
	mustWrite(filepath.Join(dir, "sub", "c.yaml"))
	mustWrite(filepath.Join(dir, "deep", "nested", "d.bak"))

	if got := countBakFiles(dir); got != 3 {
		t.Errorf("countBakFiles: got %d, want 3", got)
	}
	if got := countBakFiles(""); got != 0 {
		t.Errorf("countBakFiles empty: got %d, want 0", got)
	}
}

func TestCountWorktreeCommandsStalled_NoManager(t *testing.T) {
	t.Parallel()
	qh := &QueueHandler{}
	cq := model.CommandQueue{Commands: []model.Command{{ID: "cmd_001"}}}
	if got := qh.countWorktreeCommandsStalled(cq); got != 0 {
		t.Errorf("got %d, want 0 when worktreeManager is nil", got)
	}
}

func TestQueueHandler_UpdateDashboard(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

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

	if err := qh.updateDashboard(cq, tq, nq); err != nil {
		t.Fatalf("updateDashboard: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(maestroDir, "dashboard.md"))
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	content := string(data)

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

func TestTaskQueuesToSnapshots(t *testing.T) {
	t.Parallel()
	maestroDir := "/fake/path"

	tq := map[string]*taskQueueEntry{
		filepath.Join(maestroDir, "queue", "worker1.yaml"): {
			Path: filepath.Join(maestroDir, "queue", "worker1.yaml"),
			Queue: model.TaskQueue{Tasks: []model.Task{
				{ID: "t1", Status: model.StatusPending},
				{ID: "t2", Status: model.StatusInProgress},
			}},
		},
		filepath.Join(maestroDir, "queue", "worker2.yaml"): {
			Path: filepath.Join(maestroDir, "queue", "worker2.yaml"),
			Queue: model.TaskQueue{Tasks: []model.Task{
				{ID: "t3", Status: model.StatusCompleted},
			}},
		},
	}

	snapshots := taskQueuesToSnapshots(tq)
	if len(snapshots) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snapshots))
	}

	byWorker := make(map[string]int)
	for _, snap := range snapshots {
		byWorker[snap.WorkerID] = len(snap.Tasks)
	}
	if byWorker["worker1"] != 2 {
		t.Errorf("worker1 tasks: got %d, want 2", byWorker["worker1"])
	}
	if byWorker["worker2"] != 1 {
		t.Errorf("worker2 tasks: got %d, want 1", byWorker["worker2"])
	}
}

func TestMetricsHandler_PeriodicScanIntegration(t *testing.T) {
	t.Parallel()
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
