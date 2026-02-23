package status

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetQueueDepths_EmptyQueues(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	os.Mkdir(queueDir, 0755)

	// Write empty queue files
	os.WriteFile(filepath.Join(queueDir, "planner.yaml"),
		[]byte("schema_version: 1\nfile_type: \"queue_command\"\ncommands: []\n"), 0644)
	os.WriteFile(filepath.Join(queueDir, "worker1.yaml"),
		[]byte("schema_version: 1\nfile_type: \"queue_task\"\ntasks: []\n"), 0644)

	queues := getQueueDepths(dir)
	if len(queues) != 2 {
		t.Fatalf("expected 2 queues, got %d", len(queues))
	}

	for _, q := range queues {
		if q.Pending != 0 || q.InProgress != 0 {
			t.Errorf("queue %s: expected 0/0, got %d/%d", q.Name, q.Pending, q.InProgress)
		}
	}
}

func TestGetQueueDepths_WithEntries(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	os.Mkdir(queueDir, 0755)

	content := `schema_version: 1
file_type: "queue_task"
tasks:
  - id: "task_0000000001_aaaaaaaa"
    status: "pending"
  - id: "task_0000000002_bbbbbbbb"
    status: "in_progress"
  - id: "task_0000000003_cccccccc"
    status: "pending"
  - id: "task_0000000004_dddddddd"
    status: "completed"
`
	os.WriteFile(filepath.Join(queueDir, "worker1.yaml"), []byte(content), 0644)

	queues := getQueueDepths(dir)
	if len(queues) != 1 {
		t.Fatalf("expected 1 queue, got %d", len(queues))
	}

	q := queues[0]
	if q.Name != "worker1" {
		t.Errorf("name: got %q", q.Name)
	}
	if q.Pending != 2 {
		t.Errorf("pending: got %d, want 2", q.Pending)
	}
	if q.InProgress != 1 {
		t.Errorf("in_progress: got %d, want 1", q.InProgress)
	}
}

func TestGetQueueDepths_NoQueueDir(t *testing.T) {
	dir := t.TempDir()
	queues := getQueueDepths(dir)
	if queues != nil {
		t.Errorf("expected nil for missing queue dir, got %v", queues)
	}
}

func TestGetQueueDepths_SkipsInvalidSchema(t *testing.T) {
	dir := t.TempDir()
	queueDir := filepath.Join(dir, "queue")
	os.Mkdir(queueDir, 0755)

	// Valid file
	os.WriteFile(filepath.Join(queueDir, "planner.yaml"),
		[]byte("schema_version: 1\nfile_type: \"queue_command\"\ncommands: []\n"), 0644)

	// Invalid schema (missing file_type)
	os.WriteFile(filepath.Join(queueDir, "bad.yaml"),
		[]byte("schema_version: 1\ntasks: []\n"), 0644)

	// Invalid YAML
	os.WriteFile(filepath.Join(queueDir, "corrupt.yaml"),
		[]byte(":::invalid yaml:::"), 0644)

	queues := getQueueDepths(dir)
	// Only the valid file should be processed
	if len(queues) != 1 {
		t.Fatalf("expected 1 queue (valid only), got %d", len(queues))
	}
	if queues[0].Name != "planner" {
		t.Errorf("expected planner, got %q", queues[0].Name)
	}
}

func TestCheckDaemon_NotRunning(t *testing.T) {
	// Non-existent socket should report not running
	status := checkDaemon("/tmp/nonexistent-maestro-test.sock")
	if status.Running {
		t.Error("expected daemon not running")
	}
}

func TestPrintStatus_DoesNotPanic(t *testing.T) {
	// Verify printing works without panicking for all cases
	s := FormationStatus{
		Daemon: DaemonStatus{Running: false},
	}
	printStatus(s)

	s.Daemon.Running = true
	s.Agents = []AgentStatus{
		{ID: "orchestrator", Role: "orchestrator", Model: "opus", Status: "idle"},
	}
	s.Queues = []QueueStatus{
		{Name: "planner", Pending: 3, InProgress: 1},
	}
	printStatus(s)
}
