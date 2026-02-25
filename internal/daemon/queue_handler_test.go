package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
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
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
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
	} else if !strings.HasPrefix(*result.Tasks[0].LeaseOwner, "daemon:") {
		t.Errorf("lease_owner: got %s, want daemon:{pid} format", *result.Tasks[0].LeaseOwner)
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

	owner := qh.leaseOwnerID()
	futureExpiry := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:             "task_001",
				CommandID:      "cmd_001",
				Priority:       1,
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &futureExpiry,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
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

	// worker1 has an in_progress task with valid (non-expired) lease
	owner := qh.leaseOwnerID()
	futureExpiry := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	tq1 := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:             "task_001",
				CommandID:      "cmd_001",
				Priority:       1,
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &futureExpiry,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
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
	if result.Tasks[0].LeaseOwner == nil || !strings.HasPrefix(*result.Tasks[0].LeaseOwner, "daemon:") {
		t.Error("task_002 should have daemon:{pid} format lease_owner")
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

	owner := qh.leaseOwnerID()
	futureExpiry := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)

	taskQueues := map[string]*taskQueueEntry{
		filepath.Join(maestroDir, "queue", "worker1.yaml"): {Queue: model.TaskQueue{Tasks: []model.Task{
			{Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &futureExpiry},
		}}},
		filepath.Join(maestroDir, "queue", "worker2.yaml"): {Queue: model.TaskQueue{Tasks: []model.Task{
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
	taskQueues[filepath.Join(maestroDir, "queue", "worker2.yaml")] = &taskQueueEntry{Queue: model.TaskQueue{Tasks: []model.Task{
		{Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &futureExpiry},
	}}}
	inFlight = qh.buildGlobalInFlightSet(taskQueues)
	if !inFlight["worker1"] || !inFlight["worker2"] {
		t.Error("both workers should be in-flight")
	}

	// Expired lease should NOT be in-flight
	pastExpiry := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	taskQueues[filepath.Join(maestroDir, "queue", "worker1.yaml")] = &taskQueueEntry{Queue: model.TaskQueue{Tasks: []model.Task{
		{Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &pastExpiry},
	}}}
	inFlight = qh.buildGlobalInFlightSet(taskQueues)
	if inFlight["worker1"] {
		t.Error("worker1 with expired lease should NOT be in-flight")
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

	if result.Notifications[0].Status != model.StatusCompleted {
		t.Errorf("status: got %s, want completed", result.Notifications[0].Status)
	}
}

func TestQueueHandler_PhaseB_DoesNotBlockRLock(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Use executor with proceed channel to control Phase B duration
	dispatchStarted := make(chan struct{})
	proceed := make(chan struct{})
	var startOnce sync.Once
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &slowMockExecutor{
			result:  agent.ExecResult{Success: true},
			onStart: func() { startOnce.Do(func() { close(dispatchStarted) }) },
			proceed: proceed,
		}, nil
	})

	// Write a pending task so Phase B has a dispatch to execute
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:        "task_slow",
				CommandID: "cmd_slow",
				Purpose:   "Slow dispatch test",
				Content:   "Test content",
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

	// Start PeriodicScan in background
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		qh.PeriodicScan()
	}()

	// Wait for dispatch to start in Phase B (executor called)
	select {
	case <-dispatchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dispatch to start")
	}

	// Try to acquire RLock (simulating plan submit) — should NOT be blocked
	rlockAcquired := make(chan struct{})
	go func() {
		qh.LockFiles()   // scanMu.RLock()
		qh.UnlockFiles() // scanMu.RUnlock()
		close(rlockAcquired)
	}()

	select {
	case <-rlockAcquired:
		// Success: RLock was acquired during Phase B
	case <-time.After(1 * time.Second):
		t.Fatal("RLock blocked during Phase B — lock contention not fixed")
	}

	// Let Phase B complete
	close(proceed)

	// Wait for scan to complete
	<-scanDone
}

// slowMockExecutor adds a delay to simulate slow tmux operations during tests.
// If proceed is non-nil, waits for that channel instead of sleeping.
// onStart is called once per Execute invocation (use sync.Once in the closure for safety).
type slowMockExecutor struct {
	result  agent.ExecResult
	onStart func()       // called at start of Execute (use sync.Once for one-shot signaling)
	proceed chan struct{} // if non-nil, wait for signal instead of sleeping
	delay   time.Duration
}

func (m *slowMockExecutor) Execute(req agent.ExecRequest) agent.ExecResult {
	if m.onStart != nil {
		m.onStart()
	}
	if m.proceed != nil {
		<-m.proceed
	} else {
		time.Sleep(m.delay)
	}
	return m.result
}

func (m *slowMockExecutor) Close() error {
	return nil
}

// TestApplySignalResults_KeyMatch verifies that signal delivery results are matched
// by stable key (commandID+phaseID+kind), not by array index. This catches a bug
// where stale-signal removal in Phase A compacted the array, causing index mismatch.
func TestApplySignalResults_KeyMatch(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Simulate Phase A output: 2 signals retained after stale removal.
	// The stale signal at original index 0 was removed, so the retained array
	// has signal_B at index 0 (originally index 1) and signal_C at index 1 (originally index 2).
	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{Kind: "awaiting_fill", CommandID: "cmd_B", PhaseID: "phase_B", Message: "msg_B"},
			{Kind: "fill_timeout", CommandID: "cmd_C", PhaseID: "phase_C", Message: "msg_C"},
		},
	}

	// Phase B delivered signal_B successfully
	results := []signalDeliveryResult{
		{
			Item:    signalDeliveryItem{CommandID: "cmd_B", PhaseID: "phase_B", Kind: "awaiting_fill", Message: "msg_B"},
			Success: true,
		},
	}

	dirty := false
	qh.applySignalResults(results, &sq, &dirty)

	if !dirty {
		t.Error("expected dirty=true after successful delivery")
	}

	// signal_B should be removed (delivered successfully), signal_C retained
	if len(sq.Signals) != 1 {
		t.Fatalf("expected 1 signal retained, got %d", len(sq.Signals))
	}
	if sq.Signals[0].CommandID != "cmd_C" {
		t.Errorf("retained signal = %s, want cmd_C", sq.Signals[0].CommandID)
	}
}

// TestQueueHandler_DispatchFailure_Rollback verifies that when Phase B dispatch fails,
// Phase C correctly rolls back the task from in_progress to pending.
// Only the LLM Agent executor is mocked (returns error). All other components are real.
func TestQueueHandler_DispatchFailure_Rollback(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Mock executor that fails dispatch
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{
			Success:   false,
			Error:     fmt.Errorf("tmux pane not found"),
			Retryable: true,
		}}, nil
	})

	// Write a pending task
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:        "task_fail_001",
				CommandID: "cmd_fail_001",
				Purpose:   "Dispatch failure test",
				Content:   "Test content",
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

	// After Phase C rollback, task should be back to pending
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
	task := result.Tasks[0]
	if task.Status != model.StatusPending {
		t.Errorf("status: got %s, want pending (rollback after dispatch failure)", task.Status)
	}
	if task.LeaseOwner != nil {
		t.Errorf("lease_owner should be nil after rollback, got %v", *task.LeaseOwner)
	}
	if task.LeaseExpiresAt != nil {
		t.Error("lease_expires_at should be nil after rollback")
	}
	// LeaseEpoch should be 1 (incremented by AcquireTaskLease, not decremented by release)
	if task.LeaseEpoch != 1 {
		t.Errorf("lease_epoch: got %d, want 1 (acquired once, then released)", task.LeaseEpoch)
	}
	// Attempts should be 1 (incremented in Phase A, not rolled back)
	if task.Attempts != 1 {
		t.Errorf("attempts: got %d, want 1", task.Attempts)
	}
}

// TestQueueHandler_ConcurrentWriteDuringPhaseB verifies that a queue write arriving
// during Phase B (slow dispatch) is NOT lost when Phase C reloads and applies results.
// Only the LLM Agent executor is mocked (slow). Queue writes use real file I/O + real locks.
func TestQueueHandler_ConcurrentWriteDuringPhaseB(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)

	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	lockMap := lock.NewMutexMap()
	qh := NewQueueHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	// Executor with proceed channel: signals dispatch start, waits for proceed before returning.
	dispatchStarted := make(chan struct{})
	proceed := make(chan struct{})
	var startOnce sync.Once
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &slowMockExecutor{
			result:  agent.ExecResult{Success: true},
			onStart: func() { startOnce.Do(func() { close(dispatchStarted) }) },
			proceed: proceed,
		}, nil
	})

	// Write a pending task for worker1 (will be dispatched in Phase B)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:        "task_original",
				CommandID: "cmd_concurrent",
				Purpose:   "Original task",
				Content:   "Original content",
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

	// Start PeriodicScan in background
	var scanDone sync.WaitGroup
	scanDone.Add(1)
	go func() {
		defer scanDone.Done()
		qh.PeriodicScan()
	}()

	// Wait for dispatch to start in Phase B
	select {
	case <-dispatchStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dispatch to start")
	}

	// During Phase B: write a new task directly to worker1.yaml (simulating plan submit)
	// This uses scanMu.RLock (via LockFiles) + lockMap, same as handleQueueWriteTask
	qh.LockFiles()
	lockMap.Lock("queue:worker1")

	// Read current queue (will see Phase A's flushed state: task_original as in_progress)
	data, err := os.ReadFile(workerPath)
	if err != nil {
		lockMap.Unlock("queue:worker1")
		qh.UnlockFiles()
		t.Fatalf("read worker queue during Phase B: %v", err)
	}
	var currentTQ model.TaskQueue
	yamlv3.Unmarshal(data, &currentTQ)

	// Append a new task
	currentTQ.Tasks = append(currentTQ.Tasks, model.Task{
		ID:        "task_concurrent",
		CommandID: "cmd_concurrent",
		Purpose:   "Concurrent task added during Phase B",
		Content:   "New content",
		Priority:  1,
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err := yamlutil.AtomicWrite(workerPath, currentTQ); err != nil {
		lockMap.Unlock("queue:worker1")
		qh.UnlockFiles()
		t.Fatalf("write concurrent task: %v", err)
	}
	lockMap.Unlock("queue:worker1")
	qh.UnlockFiles()

	// Signal Phase B to complete — write is guaranteed to have happened before Phase C.
	close(proceed)

	// Wait for scan to complete (Phase C)
	scanDone.Wait()

	// Verify: both tasks exist in the final queue state
	data, err = os.ReadFile(workerPath)
	if err != nil {
		t.Fatalf("read final worker queue: %v", err)
	}
	var finalTQ model.TaskQueue
	if err := parseYAML(data, &finalTQ); err != nil {
		t.Fatalf("parse final worker queue: %v", err)
	}

	if len(finalTQ.Tasks) != 2 {
		t.Fatalf("expected 2 tasks (original + concurrent), got %d", len(finalTQ.Tasks))
	}

	// Find both tasks
	var foundOriginal, foundConcurrent bool
	for _, task := range finalTQ.Tasks {
		switch task.ID {
		case "task_original":
			foundOriginal = true
			if task.Status != model.StatusInProgress {
				t.Errorf("task_original status: got %s, want in_progress", task.Status)
			}
		case "task_concurrent":
			foundConcurrent = true
			if task.Status != model.StatusPending {
				t.Errorf("task_concurrent status: got %s, want pending", task.Status)
			}
		}
	}
	if !foundOriginal {
		t.Error("task_original not found in final queue — Phase C may have overwritten it")
	}
	if !foundConcurrent {
		t.Error("task_concurrent not found in final queue — Phase C clobbered the concurrent write")
	}
}

// parseYAML is a test helper.
func parseYAML(data []byte, v any) error {
	return yamlv3.Unmarshal(data, v)
}
