package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func newTestQueueStore(t *testing.T) (*QueueStoreImpl, string) {
	t.Helper()
	maestroDir := t.TempDir()
	queueDir := filepath.Join(maestroDir, "queue")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	lockMap := lock.NewMutexMap()
	dl := NewDaemonLoggerFromLegacy("test", log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	cfg := model.Config{}
	qs := NewQueueStore(maestroDir, cfg, RealClock{}, lockMap, dl)
	return qs, maestroDir
}

// --- C-A4: Backup recovery tests ---

func TestLoadCommandQueue_BackupRecovery(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	path := filepath.Join(maestroDir, "queue", "planner.yaml")

	// Write a valid queue, then corrupt the main file while keeping .bak
	validQueue := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{ID: "cmd_1", Status: "pending"},
		},
	}
	if err := yamlutil.AtomicWrite(path, validQueue); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	// AtomicWrite creates .bak only if file already exists, so write again to create .bak
	validQueue.Commands = append(validQueue.Commands, model.Command{ID: "cmd_2", Status: "pending"})
	if err := yamlutil.AtomicWrite(path, validQueue); err != nil {
		t.Fatalf("AtomicWrite (second): %v", err)
	}

	// Corrupt the main file
	if err := os.WriteFile(path, []byte("corrupted: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Load should recover from backup
	cq, qpath, err := qs.LoadCommandQueue()
	if err != nil {
		t.Fatalf("LoadCommandQueue: unexpected error: %v", err)
	}
	if qpath == "" {
		t.Fatal("expected non-empty path after backup recovery")
	}
	// Backup had 1 command (the version before the second write)
	if len(cq.Commands) == 0 {
		t.Fatal("expected commands to be recovered from backup")
	}
}

func TestLoadCommandQueue_NoBackupReturnsError(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	path := filepath.Join(maestroDir, "queue", "planner.yaml")

	// Write a corrupted file with no .bak
	if err := os.WriteFile(path, []byte("corrupted: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err := qs.LoadCommandQueue()
	if err == nil {
		t.Fatal("expected error when no backup exists for corrupted queue")
	}
}

func TestLoadCommandQueue_CorruptBackupReturnsError(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	path := filepath.Join(maestroDir, "queue", "planner.yaml")
	bakPath := path + ".bak"

	// Write corrupted file and corrupted backup
	if err := os.WriteFile(path, []byte("corrupted: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(bakPath, []byte("also_corrupted: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err := qs.LoadCommandQueue()
	if err == nil {
		t.Fatal("expected error when backup is also corrupted")
	}
}

func TestLoadAllTaskQueues_BackupRecovery(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	path := filepath.Join(maestroDir, "queue", "worker1.yaml")

	// Write valid task queue, then write again to create .bak
	validQueue := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks:         []model.Task{{ID: "task_1", Status: "pending"}},
	}
	if err := yamlutil.AtomicWrite(path, validQueue); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	validQueue.Tasks = append(validQueue.Tasks, model.Task{ID: "task_2", Status: "pending"})
	if err := yamlutil.AtomicWrite(path, validQueue); err != nil {
		t.Fatalf("AtomicWrite (second): %v", err)
	}

	// Corrupt the main file
	if err := os.WriteFile(path, []byte("corrupted: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	result, err := qs.LoadAllTaskQueues()
	if err != nil {
		t.Fatalf("LoadAllTaskQueues: unexpected error: %v", err)
	}
	if len(result) == 0 {
		t.Fatal("expected task queues to be recovered from backup")
	}
	for _, entry := range result {
		if len(entry.Queue.Tasks) == 0 {
			t.Fatal("expected tasks to be recovered from backup")
		}
	}
}

func TestLoadAllTaskQueues_NoBackupReturnsError(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	path := filepath.Join(maestroDir, "queue", "worker1.yaml")

	if err := os.WriteFile(path, []byte("corrupted: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := qs.LoadAllTaskQueues()
	if err == nil {
		t.Fatal("expected error when no backup exists for corrupted task queue")
	}
}

func TestLoadNotificationQueue_BackupRecovery(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	path := filepath.Join(maestroDir, "queue", "orchestrator.yaml")

	validQueue := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "queue_notification",
		Notifications: []model.Notification{{ID: "ntf_1", CommandID: "cmd_1", SourceResultID: "r_1", Type: model.NotificationTypeCommandCompleted}},
	}
	if err := yamlutil.AtomicWrite(path, validQueue); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	// Write again to create .bak
	if err := yamlutil.AtomicWrite(path, validQueue); err != nil {
		t.Fatalf("AtomicWrite (second): %v", err)
	}

	// Corrupt
	if err := os.WriteFile(path, []byte("corrupted: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	nq, qpath, err := qs.LoadNotificationQueue()
	if err != nil {
		t.Fatalf("LoadNotificationQueue: unexpected error: %v", err)
	}
	if qpath == "" {
		t.Fatal("expected non-empty path after backup recovery")
	}
	if len(nq.Notifications) == 0 {
		t.Fatal("expected notifications to be recovered from backup")
	}
}

func TestLoadNotificationQueue_NoBackupReturnsError(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	path := filepath.Join(maestroDir, "queue", "orchestrator.yaml")

	if err := os.WriteFile(path, []byte("corrupted: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err := qs.LoadNotificationQueue()
	if err == nil {
		t.Fatal("expected error when no backup exists")
	}
}

func TestLoadPlannerSignalQueue_BackupRecovery(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	path := filepath.Join(maestroDir, "queue", "planner_signals.yaml")

	validQueue := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "queue_planner_signal",
		Signals:       []model.PlannerSignal{{Kind: "test", CommandID: "cmd_1"}},
	}
	if err := yamlutil.AtomicWrite(path, validQueue); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	if err := yamlutil.AtomicWrite(path, validQueue); err != nil {
		t.Fatalf("AtomicWrite (second): %v", err)
	}

	if err := os.WriteFile(path, []byte("corrupted: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sq, qpath, err := qs.LoadPlannerSignalQueue()
	if err != nil {
		t.Fatalf("LoadPlannerSignalQueue: unexpected error: %v", err)
	}
	if qpath == "" {
		t.Fatal("expected non-empty path after backup recovery")
	}
	if len(sq.Signals) == 0 {
		t.Fatal("expected signals to be recovered from backup")
	}
}

func TestLoadPlannerSignalQueue_NoBackupReturnsError(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	path := filepath.Join(maestroDir, "queue", "planner_signals.yaml")

	if err := os.WriteFile(path, []byte("corrupted: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, _, err := qs.LoadPlannerSignalQueue()
	if err == nil {
		t.Fatal("expected error when no backup exists")
	}
}

// --- C-A5: FlushQueues concurrent safety tests ---

func TestFlushQueues_ConcurrentSafety(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)

	// Set up initial queue files
	cmdPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	taskPath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	notifPath := filepath.Join(maestroDir, "queue", "orchestrator.yaml")
	signalPath := filepath.Join(maestroDir, "queue", "planner_signals.yaml")

	cmdQueue := model.CommandQueue{SchemaVersion: 1, FileType: "command_queue"}
	taskQueue := model.TaskQueue{SchemaVersion: 1, FileType: "queue_task"}
	notifQueue := model.NotificationQueue{SchemaVersion: 1, FileType: "queue_notification"}
	signalQueue := model.PlannerSignalQueue{SchemaVersion: 1, FileType: "queue_planner_signal", Signals: []model.PlannerSignal{{Kind: "test"}}}

	// Write initial files
	for _, item := range []struct {
		path string
		data any
	}{
		{cmdPath, cmdQueue},
		{taskPath, taskQueue},
		{notifPath, notifQueue},
		{signalPath, signalQueue},
	} {
		if err := yamlutil.AtomicWrite(item.path, item.data); err != nil {
			t.Fatalf("AtomicWrite %s: %v", item.path, err)
		}
	}

	taskQueues := map[string]*taskQueueEntry{
		taskPath: {Queue: taskQueue, Path: taskPath},
	}
	taskDirty := map[string]bool{taskPath: true}

	// Run FlushQueues concurrently to verify no panics or races
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			qs.FlushQueues(
				cmdQueue, cmdPath, true,
				taskQueues, taskDirty,
				notifQueue, notifPath, true,
				signalQueue, signalPath, true,
			)
		}()
	}
	wg.Wait()

	// Verify files are still valid YAML after concurrent writes
	for _, p := range []string{cmdPath, taskPath, notifPath, signalPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file %s should exist after flush: %v", filepath.Base(p), err)
		}
	}
}

func TestFlushQueues_LockProtectsCommandQueue(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	cmdPath := filepath.Join(maestroDir, "queue", "planner.yaml")

	cmdQueue := model.CommandQueue{SchemaVersion: 1, FileType: "command_queue"}
	if err := yamlutil.AtomicWrite(cmdPath, cmdQueue); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}

	// Hold the queue:planner lock, then verify FlushQueues blocks
	qs.lockMap.Lock("queue:planner")

	done := make(chan struct{})
	go func() {
		qs.FlushQueues(
			cmdQueue, cmdPath, true,
			nil, nil,
			model.NotificationQueue{}, "", false,
			model.PlannerSignalQueue{}, "", false,
		)
		close(done)
	}()

	// Verify flush does not complete while lock is held
	select {
	case <-done:
		t.Fatal("FlushQueues should block when queue:planner lock is held")
	default:
		// Expected: FlushQueues is blocked
	}

	qs.lockMap.Unlock("queue:planner")
	<-done // Should complete after unlock
}

func TestFlushQueues_LockProtectsTaskQueue(t *testing.T) {
	qs, maestroDir := newTestQueueStore(t)
	taskPath := filepath.Join(maestroDir, "queue", "worker1.yaml")

	taskQueue := model.TaskQueue{SchemaVersion: 1, FileType: "queue_task"}
	if err := yamlutil.AtomicWrite(taskPath, taskQueue); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}

	taskQueues := map[string]*taskQueueEntry{
		taskPath: {Queue: taskQueue, Path: taskPath},
	}
	taskDirty := map[string]bool{taskPath: true}

	// Hold the queue:worker1 lock
	qs.lockMap.Lock("queue:worker1")

	done := make(chan struct{})
	go func() {
		qs.FlushQueues(
			model.CommandQueue{}, "", false,
			taskQueues, taskDirty,
			model.NotificationQueue{}, "", false,
			model.PlannerSignalQueue{}, "", false,
		)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("FlushQueues should block when queue:worker1 lock is held")
	default:
	}

	qs.lockMap.Unlock("queue:worker1")
	<-done
}
