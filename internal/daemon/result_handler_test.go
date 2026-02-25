package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func newTestResultHandler(maestroDir string) (*ResultHandler, *mockExecutor) {
	cfg := model.Config{
		Watcher: model.WatcherConfig{NotifyLeaseSec: 120},
	}
	lockMap := lock.NewMutexMap()
	rh := NewResultHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	mock := &mockExecutor{result: agent.ExecResult{Success: true}}
	rh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return mock, nil
	})
	return rh, mock
}

func TestResultHandler_WorkerNotification_Basic(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rh, mock := newTestResultHandler(maestroDir)

	// Create a worker result file with one unnotified result
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_0000000001_aaaaaaaa",
				TaskID:    "task_0000000001_bbbbbbbb",
				CommandID: "cmd_0000000001_cccccccc",
				Status:    model.StatusCompleted,
				Summary:   "test done",
				Notified:  false,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		t.Fatalf("write result: %v", err)
	}

	n := rh.processWorkerResultFile("worker1")
	if n != 1 {
		t.Fatalf("expected 1 notified, got %d", n)
	}

	// Verify executor was called with correct message
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 executor call, got %d", len(mock.calls))
	}
	call := mock.calls[0]
	if call.AgentID != "planner" {
		t.Errorf("agent_id: got %s, want planner", call.AgentID)
	}
	if call.Mode != agent.ModeDeliver {
		t.Errorf("mode: got %s, want deliver", call.Mode)
	}

	// Verify result is now marked as notified
	data, _ := os.ReadFile(resultPath)
	var updated model.TaskResultFile
	yamlv3.Unmarshal(data, &updated)

	if !updated.Results[0].Notified {
		t.Error("expected notified=true")
	}
	if updated.Results[0].NotifiedAt == nil {
		t.Error("expected notified_at to be set")
	}
	if updated.Results[0].NotifyLeaseOwner != nil {
		t.Error("expected notify_lease_owner to be nil")
	}
}

func TestResultHandler_WorkerNotification_AlreadyNotified(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rh, mock := newTestResultHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:         "res_0000000001_aaaaaaaa",
				TaskID:     "task_0000000001_bbbbbbbb",
				CommandID:  "cmd_0000000001_cccccccc",
				Status:     model.StatusCompleted,
				Notified:   true,
				NotifiedAt: &now,
				CreatedAt:  now,
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	n := rh.processWorkerResultFile("worker1")
	if n != 0 {
		t.Fatalf("expected 0 notified, got %d", n)
	}
	if len(mock.calls) != 0 {
		t.Fatalf("expected no executor calls, got %d", len(mock.calls))
	}
}

func TestResultHandler_WorkerNotification_LeaseHeld(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rh, mock := newTestResultHandler(maestroDir)

	owner := "daemon:9999"
	expiresAt := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:                   "res_0000000001_aaaaaaaa",
				TaskID:               "task_0000000001_bbbbbbbb",
				CommandID:            "cmd_0000000001_cccccccc",
				Status:               model.StatusCompleted,
				Notified:             false,
				NotifyLeaseOwner:     &owner,
				NotifyLeaseExpiresAt: &expiresAt,
				CreatedAt:            time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	n := rh.processWorkerResultFile("worker1")
	if n != 0 {
		t.Fatalf("expected 0 (lease held), got %d", n)
	}
	if len(mock.calls) != 0 {
		t.Fatalf("expected no executor calls, got %d", len(mock.calls))
	}
}

func TestResultHandler_WorkerNotification_ExpiredLease(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rh, _ := newTestResultHandler(maestroDir)

	owner := "daemon:9999"
	expiresAt := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:                   "res_0000000001_aaaaaaaa",
				TaskID:               "task_0000000001_bbbbbbbb",
				CommandID:            "cmd_0000000001_cccccccc",
				Status:               model.StatusCompleted,
				Notified:             false,
				NotifyAttempts:       1,
				NotifyLeaseOwner:     &owner,
				NotifyLeaseExpiresAt: &expiresAt,
				CreatedAt:            time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	n := rh.processWorkerResultFile("worker1")
	if n != 1 {
		t.Fatalf("expected 1 (expired lease reclaimed), got %d", n)
	}

	// Verify attempts incremented
	data, _ := os.ReadFile(resultPath)
	var updated model.TaskResultFile
	yamlv3.Unmarshal(data, &updated)
	if updated.Results[0].NotifyAttempts != 2 {
		t.Errorf("notify_attempts: got %d, want 2", updated.Results[0].NotifyAttempts)
	}
}

func TestResultHandler_WorkerNotification_Failure(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Watcher: model.WatcherConfig{NotifyLeaseSec: 120},
	}
	lockMap := lock.NewMutexMap()
	rh := NewResultHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	// Mock executor that fails
	failMock := &mockExecutor{result: agent.ExecResult{
		Success: false,
		Error:   fmt.Errorf("planner busy"),
	}}
	rh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return failMock, nil
	})

	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_0000000001_aaaaaaaa",
				TaskID:    "task_0000000001_bbbbbbbb",
				CommandID: "cmd_0000000001_cccccccc",
				Status:    model.StatusCompleted,
				Notified:  false,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	n := rh.processWorkerResultFile("worker1")
	if n != 0 {
		t.Fatalf("expected 0 (notification failed), got %d", n)
	}

	// Verify notify_last_error is set and lease is cleared
	data, _ := os.ReadFile(resultPath)
	var updated model.TaskResultFile
	yamlv3.Unmarshal(data, &updated)
	if updated.Results[0].Notified {
		t.Error("should not be marked as notified")
	}
	if updated.Results[0].NotifyLastError == nil {
		t.Error("expected notify_last_error to be set")
	}
	// After failure, a backoff lease should be set to prevent immediate retry
	if updated.Results[0].NotifyLeaseOwner == nil || *updated.Results[0].NotifyLeaseOwner != "backoff" {
		t.Errorf("expected notify_lease_owner='backoff', got %v", updated.Results[0].NotifyLeaseOwner)
	}
	if updated.Results[0].NotifyLeaseExpiresAt == nil {
		t.Error("expected notify_lease_expires_at to be set for backoff")
	} else {
		expiresAt, err := time.Parse(time.RFC3339, *updated.Results[0].NotifyLeaseExpiresAt)
		if err != nil {
			t.Errorf("invalid notify_lease_expires_at: %v", err)
		} else if !expiresAt.After(time.Now().UTC()) {
			t.Error("backoff lease should expire in the future")
		}
	}
}

func TestResultHandler_CommandNotification_Basic(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rh, _ := newTestResultHandler(maestroDir)

	// Ensure orchestrator queue dir exists
	os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755)

	rf := model.CommandResultFile{
		SchemaVersion: 1,
		FileType:      "result_command",
		Results: []model.CommandResult{
			{
				ID:        "res_0000000001_dddddddd",
				CommandID: "cmd_0000000001_cccccccc",
				Status:    model.StatusCompleted,
				Summary:   "all tasks done",
				Notified:  false,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "planner.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	n := rh.processCommandResultFile()
	if n != 1 {
		t.Fatalf("expected 1 notified, got %d", n)
	}

	// Verify result is marked as notified
	data, _ := os.ReadFile(resultPath)
	var updated model.CommandResultFile
	yamlv3.Unmarshal(data, &updated)
	if !updated.Results[0].Notified {
		t.Error("expected notified=true")
	}

	// Verify orchestrator queue has notification
	nqPath := filepath.Join(maestroDir, "queue", "orchestrator.yaml")
	nqData, err := os.ReadFile(nqPath)
	if err != nil {
		t.Fatalf("read orchestrator queue: %v", err)
	}
	var nq model.NotificationQueue
	yamlv3.Unmarshal(nqData, &nq)
	if len(nq.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(nq.Notifications))
	}
	if nq.Notifications[0].CommandID != "cmd_0000000001_cccccccc" {
		t.Errorf("notification command_id: got %s", nq.Notifications[0].CommandID)
	}
	if nq.Notifications[0].Type != "command_completed" {
		t.Errorf("notification type: got %s, want command_completed", nq.Notifications[0].Type)
	}
	if nq.Notifications[0].SourceResultID != "res_0000000001_dddddddd" {
		t.Errorf("source_result_id: got %s", nq.Notifications[0].SourceResultID)
	}
}

func TestResultHandler_CommandNotification_Idempotent(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rh, _ := newTestResultHandler(maestroDir)

	os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755)

	// Pre-existing orchestrator notification with same source_result_id
	nq := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "queue_notification",
		Notifications: []model.Notification{
			{
				ID:             "ntf_0000000001_eeeeeeee",
				CommandID:      "cmd_0000000001_cccccccc",
				Type:           "command_completed",
				SourceResultID: "res_0000000001_dddddddd",
				Status:         model.StatusPending,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), nq)

	rf := model.CommandResultFile{
		SchemaVersion: 1,
		FileType:      "result_command",
		Results: []model.CommandResult{
			{
				ID:        "res_0000000001_dddddddd",
				CommandID: "cmd_0000000001_cccccccc",
				Status:    model.StatusCompleted,
				Notified:  false,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	n := rh.processCommandResultFile()
	if n != 1 {
		t.Fatalf("expected 1 (idempotent write success), got %d", n)
	}

	// Verify no duplicate notification
	nqData, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "orchestrator.yaml"))
	var updated model.NotificationQueue
	yamlv3.Unmarshal(nqData, &updated)
	if len(updated.Notifications) != 1 {
		t.Errorf("expected 1 notification (no duplicate), got %d", len(updated.Notifications))
	}
}

func TestResultHandler_ScanAllResults(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rh, _ := newTestResultHandler(maestroDir)

	os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755)

	// Create worker1 results with 1 unnotified + 1 already notified
	now := time.Now().UTC().Format(time.RFC3339)
	rf1 := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID: "res_0000000001_aaaaaaaa", TaskID: "task_0000000001_aaa11111",
				CommandID: "cmd_0000000001_cccccccc", Status: model.StatusCompleted,
				Notified: true, NotifiedAt: &now, CreatedAt: now,
			},
			{
				ID: "res_0000000002_aaaaaaaa", TaskID: "task_0000000002_aaa22222",
				CommandID: "cmd_0000000001_cccccccc", Status: model.StatusFailed,
				Notified: false, CreatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker1.yaml"), rf1)

	// Create worker2 results with 1 unnotified
	rf2 := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID: "res_0000000003_aaaaaaaa", TaskID: "task_0000000003_aaa33333",
				CommandID: "cmd_0000000001_cccccccc", Status: model.StatusCompleted,
				Notified: false, CreatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "worker2.yaml"), rf2)

	total := rh.ScanAllResults()
	if total != 2 {
		t.Fatalf("expected 2 notified, got %d", total)
	}
}

func TestResultHandler_HandleResultFileEvent(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rh, mock := newTestResultHandler(maestroDir)

	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID: "res_0000000001_aaaaaaaa", TaskID: "task_0000000001_bbbbbbbb",
				CommandID: "cmd_0000000001_cccccccc", Status: model.StatusCompleted,
				Notified: false, CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	rh.HandleResultFileEvent(resultPath)

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 executor call, got %d", len(mock.calls))
	}
}

func TestResultHandler_WorkerNotification_MaxRetryExhausted(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Watcher: model.WatcherConfig{NotifyLeaseSec: 120},
	}
	lockMap := lock.NewMutexMap()
	rh := NewResultHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	failMock := &mockExecutor{result: agent.ExecResult{
		Success: false,
		Error:   fmt.Errorf("no server running"),
	}}
	rh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return failMock, nil
	})

	// Create a result that has already exhausted retries
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:             "res_0000000001_aaaaaaaa",
				TaskID:         "task_0000000001_bbbbbbbb",
				CommandID:      "cmd_0000000001_cccccccc",
				Status:         model.StatusCompleted,
				Notified:       false,
				NotifyAttempts: maxNotifyAttempts, // already at max
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	n := rh.processWorkerResultFile("worker1")
	if n != 0 {
		t.Fatalf("expected 0 (exhausted), got %d", n)
	}
	// No executor calls should be made for exhausted entries
	if len(failMock.calls) != 0 {
		t.Fatalf("expected no executor calls for exhausted entry, got %d", len(failMock.calls))
	}
}

func TestResultHandler_WorkerNotification_BackoffPreventsImmediateRetry(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Watcher: model.WatcherConfig{NotifyLeaseSec: 120},
	}
	lockMap := lock.NewMutexMap()
	rh := NewResultHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	failMock := &mockExecutor{result: agent.ExecResult{
		Success: false,
		Error:   fmt.Errorf("no server running"),
	}}
	rh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return failMock, nil
	})

	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_0000000001_aaaaaaaa",
				TaskID:    "task_0000000001_bbbbbbbb",
				CommandID: "cmd_0000000001_cccccccc",
				Status:    model.StatusCompleted,
				Notified:  false,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	// First attempt: fails and sets backoff
	n := rh.processWorkerResultFile("worker1")
	if n != 0 {
		t.Fatalf("expected 0 (failed), got %d", n)
	}
	if len(failMock.calls) != 1 {
		t.Fatalf("expected 1 executor call, got %d", len(failMock.calls))
	}

	// Second attempt immediately: should be skipped due to backoff
	failMock.calls = nil
	n = rh.processWorkerResultFile("worker1")
	if n != 0 {
		t.Fatalf("expected 0 (backoff), got %d", n)
	}
	if len(failMock.calls) != 0 {
		t.Fatalf("expected no executor calls during backoff, got %d", len(failMock.calls))
	}
}

func TestResultHandler_MultipleResults_ProcessedInOrder(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rh, mock := newTestResultHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID: "res_0000000001_aaaaaaaa", TaskID: "task_0000000001_11111111",
				CommandID: "cmd_0000000001_cccccccc", Status: model.StatusCompleted,
				Notified: false, CreatedAt: now,
			},
			{
				ID: "res_0000000002_aaaaaaaa", TaskID: "task_0000000002_22222222",
				CommandID: "cmd_0000000001_cccccccc", Status: model.StatusFailed,
				Notified: false, CreatedAt: now,
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	n := rh.processWorkerResultFile("worker1")
	if n != 2 {
		t.Fatalf("expected 2 notified, got %d", n)
	}
	if len(mock.calls) != 2 {
		t.Fatalf("expected 2 executor calls, got %d", len(mock.calls))
	}

	// Verify both are marked notified
	data, _ := os.ReadFile(resultPath)
	var updated model.TaskResultFile
	yamlv3.Unmarshal(data, &updated)
	for i, r := range updated.Results {
		if !r.Notified {
			t.Errorf("result[%d] expected notified=true", i)
		}
	}
}
