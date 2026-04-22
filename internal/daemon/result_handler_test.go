package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	"github.com/msageha/maestro_v2/internal/testutil/mocks"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func newTestResultHandler(maestroDir string) (*ResultHandler, *mocks.MockExecutor) {
	cfg := model.Config{
		Watcher: model.WatcherConfig{NotifyLeaseSec: 120},
	}
	lockMap := lock.NewMutexMap()
	ep := newTestExecutorProvider(maestroDir, cfg)
	rh := NewResultHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug, ep, RealClock{})

	mock := &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}
	ep.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return mock, nil
	})
	return rh, mock
}

func TestResultHandler_WorkerNotification_Basic(t *testing.T) {
	t.Parallel()
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
	if len(mock.Calls) != 1 {
		t.Fatalf("expected 1 executor call, got %d", len(mock.Calls))
	}
	call := mock.Calls[0]
	if call.AgentID != "planner" {
		t.Errorf("agent_id: got %s, want planner", call.AgentID)
	}
	if call.Mode != agent.ModeDeliver {
		t.Errorf("mode: got %s, want deliver", call.Mode)
	}

	// Verify result is now marked as notified
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var updated model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

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
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rh, mock := newTestResultHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:             "res_0000000001_aaaaaaaa",
				TaskID:         "task_0000000001_bbbbbbbb",
				CommandID:      "cmd_0000000001_cccccccc",
				Status:         model.StatusCompleted,
				NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now},
				CreatedAt:      now,
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	n := rh.processWorkerResultFile("worker1")
	if n != 0 {
		t.Fatalf("expected 0 notified, got %d", n)
	}
	if len(mock.Calls) != 0 {
		t.Fatalf("expected no executor calls, got %d", len(mock.Calls))
	}
}

func TestResultHandler_WorkerNotification_LeaseHeld(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rh, mock := newTestResultHandler(maestroDir)

	owner := "daemon:9999"
	expiresAt := time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:             "res_0000000001_aaaaaaaa",
				TaskID:         "task_0000000001_bbbbbbbb",
				CommandID:      "cmd_0000000001_cccccccc",
				Status:         model.StatusCompleted,
				NotifiableBase: model.NotifiableBase{NotifyLeaseOwner: &owner, NotifyLeaseExpiresAt: &expiresAt},
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	n := rh.processWorkerResultFile("worker1")
	if n != 0 {
		t.Fatalf("expected 0 (lease held), got %d", n)
	}
	if len(mock.Calls) != 0 {
		t.Fatalf("expected no executor calls, got %d", len(mock.Calls))
	}
}

func TestResultHandler_WorkerNotification_ExpiredLease(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rh, _ := newTestResultHandler(maestroDir)

	owner := "daemon:9999"
	expiresAt := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:             "res_0000000001_aaaaaaaa",
				TaskID:         "task_0000000001_bbbbbbbb",
				CommandID:      "cmd_0000000001_cccccccc",
				Status:         model.StatusCompleted,
				NotifiableBase: model.NotifiableBase{NotifyAttempts: 1, NotifyLeaseOwner: &owner, NotifyLeaseExpiresAt: &expiresAt},
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
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
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var updated model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if updated.Results[0].NotifyAttempts != 2 {
		t.Errorf("notify_attempts: got %d, want 2", updated.Results[0].NotifyAttempts)
	}
}

func TestResultHandler_WorkerNotification_Failure(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Watcher: model.WatcherConfig{NotifyLeaseSec: 120},
		Retry: model.RetryConfig{
			ResultNotifyInlineRetries:       ptr.Int(1),
			ResultNotifyInlineRetryDelaySec: ptr.Int(1),
			ResultNotifyDeliveryTimeoutSec:  ptr.Int(1),
		},
	}
	lockMap := lock.NewMutexMap()
	ep := newTestExecutorProvider(maestroDir, cfg)
	rh := NewResultHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug, ep, RealClock{})

	// Mock executor that fails
	failMock := &mocks.MockExecutor{Result: agent.ExecResult{
		Success: false,
		Error:   fmt.Errorf("planner busy"),
	}}
	ep.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
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
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var updated model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
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
		} else if expiresAt.Before(time.Now().UTC().Add(-2 * time.Second)) {
			t.Error("backoff lease should expire in the future (or within 2s tolerance for RFC3339 second-precision truncation)")
		}
	}
}

func TestResultHandler_CommandNotification_Basic(t *testing.T) {
	t.Parallel()
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
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var updated model.CommandResultFile
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
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
	if err := yamlv3.Unmarshal(nqData, &nq); err != nil {
		t.Fatalf("unmarshal notification queue: %v", err)
	}
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
	t.Parallel()
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
	nqData, err := os.ReadFile(filepath.Join(maestroDir, "queue", "orchestrator.yaml"))
	if err != nil {
		t.Fatalf("read orchestrator queue: %v", err)
	}
	var updated model.NotificationQueue
	if err := yamlv3.Unmarshal(nqData, &updated); err != nil {
		t.Fatalf("unmarshal notification queue: %v", err)
	}
	if len(updated.Notifications) != 1 {
		t.Errorf("expected 1 notification (no duplicate), got %d", len(updated.Notifications))
	}
}

// TestResultHandler_CommandNotification_SupersedeOnTypeChange covers the H3
// derivative scenario: a previously-queued completed notification must be
// superseded when the result is reconciled to a different terminal status
// (e.g. cancelled). The notification entry is mutated in place — Type is
// updated and delivery state (Status, Attempts, Lease*, errors) is reset so
// the orchestrator picks up the corrected status. ID/CreatedAt/Priority are
// preserved.
func TestResultHandler_CommandNotification_SupersedeOnTypeChange(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rh, _ := newTestResultHandler(maestroDir)

	os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755)

	createdAt := "2026-01-01T00:00:00Z"
	leaseOwner := "orchestrator"
	leaseExpiresAt := "2026-01-01T00:05:00Z"
	lastErr := "tmux send-keys failed"

	// Pre-existing notification: previously dispatched as completed but failed
	// delivery (attempts > 0, has lease + last_error).
	nq := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "queue_notification",
		Notifications: []model.Notification{
			{
				ID:             "ntf_0000000001_eeeeeeee",
				CommandID:      "cmd_0000000001_cccccccc",
				Type:           model.NotificationTypeCommandCompleted,
				SourceResultID: "res_0000000001_dddddddd",
				Content:        "command cmd_0000000001_cccccccc completed",
				Priority:       100,
				Status:         model.StatusInProgress,
				Attempts:       3,
				LastError:      &lastErr,
				LeaseOwner:     &leaseOwner,
				LeaseExpiresAt: &leaseExpiresAt,
				LeaseEpoch:     2,
				CreatedAt:      createdAt,
				UpdatedAt:      createdAt,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "orchestrator.yaml"), nq)

	// H3 reconcile: same result ID but status forced to cancelled, Notified
	// reset so result_handler re-issues the notification.
	rf := model.CommandResultFile{
		SchemaVersion: 1,
		FileType:      "result_command",
		Results: []model.CommandResult{
			{
				ID:        "res_0000000001_dddddddd",
				CommandID: "cmd_0000000001_cccccccc",
				Status:    model.StatusCancelled,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "results", "planner.yaml"), rf)

	if n := rh.processCommandResultFile(); n != 1 {
		t.Fatalf("expected 1 notified, got %d", n)
	}

	nqData, err := os.ReadFile(filepath.Join(maestroDir, "queue", "orchestrator.yaml"))
	if err != nil {
		t.Fatalf("read orchestrator queue: %v", err)
	}
	var updated model.NotificationQueue
	if err := yamlv3.Unmarshal(nqData, &updated); err != nil {
		t.Fatalf("unmarshal notification queue: %v", err)
	}
	if len(updated.Notifications) != 1 {
		t.Fatalf("expected 1 notification (in-place supersede), got %d", len(updated.Notifications))
	}
	got := updated.Notifications[0]
	if got.ID != "ntf_0000000001_eeeeeeee" {
		t.Errorf("ID not preserved: got %s", got.ID)
	}
	if got.CreatedAt != createdAt {
		t.Errorf("CreatedAt not preserved: got %s", got.CreatedAt)
	}
	if got.Priority != 100 {
		t.Errorf("Priority not preserved: got %d", got.Priority)
	}
	if got.Type != model.NotificationTypeCommandCancelled {
		t.Errorf("Type not superseded: got %s, want command_cancelled", got.Type)
	}
	if got.Status != model.StatusPending {
		t.Errorf("Status not reset: got %s, want pending", got.Status)
	}
	if got.Attempts != 0 {
		t.Errorf("Attempts not reset: got %d", got.Attempts)
	}
	if got.LastError != nil {
		t.Errorf("LastError not cleared: got %v", *got.LastError)
	}
	if got.LeaseOwner != nil {
		t.Errorf("LeaseOwner not cleared: got %v", *got.LeaseOwner)
	}
	if got.LeaseExpiresAt != nil {
		t.Errorf("LeaseExpiresAt not cleared: got %v", *got.LeaseExpiresAt)
	}
	if got.UpdatedAt == createdAt {
		t.Errorf("UpdatedAt not refreshed")
	}
	if !strings.Contains(got.Content, "cancelled") {
		t.Errorf("Content not refreshed: got %s", got.Content)
	}
}

func TestResultHandler_ScanAllResults(t *testing.T) {
	t.Parallel()
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
				NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now}, CreatedAt: now,
			},
			{
				ID: "res_0000000002_aaaaaaaa", TaskID: "task_0000000002_aaa22222",
				CommandID: "cmd_0000000001_cccccccc", Status: model.StatusFailed,
				CreatedAt: now,
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
				CreatedAt: now,
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
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rh, mock := newTestResultHandler(maestroDir)

	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID: "res_0000000001_aaaaaaaa", TaskID: "task_0000000001_bbbbbbbb",
				CommandID: "cmd_0000000001_cccccccc", Status: model.StatusCompleted,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	rh.HandleResultFileEvent(resultPath)

	if len(mock.Calls) != 1 {
		t.Fatalf("expected 1 executor call, got %d", len(mock.Calls))
	}
}

func TestResultHandler_WorkerNotification_MaxRetryExhausted(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Watcher: model.WatcherConfig{NotifyLeaseSec: 120},
	}
	lockMap := lock.NewMutexMap()
	ep := newTestExecutorProvider(maestroDir, cfg)
	rh := NewResultHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug, ep, RealClock{})

	failMock := &mocks.MockExecutor{Result: agent.ExecResult{
		Success: false,
		Error:   fmt.Errorf("no server running"),
	}}
	ep.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
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
				NotifiableBase: model.NotifiableBase{NotifyAttempts: maxNotifyAttempts}, // already at max
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
	if len(failMock.Calls) != 0 {
		t.Fatalf("expected no executor calls for exhausted entry, got %d", len(failMock.Calls))
	}
}

func TestResultHandler_WorkerNotification_BackoffPreventsImmediateRetry(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Watcher: model.WatcherConfig{NotifyLeaseSec: 120},
		Retry: model.RetryConfig{
			ResultNotifyInlineRetries:       ptr.Int(1), // minimize inline retries for test speed
			ResultNotifyInlineRetryDelaySec: ptr.Int(1),
			ResultNotifyDeliveryTimeoutSec:  ptr.Int(1),
		},
	}
	lockMap := lock.NewMutexMap()
	ep := newTestExecutorProvider(maestroDir, cfg)
	rh := NewResultHandler(maestroDir, cfg, lockMap, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug, ep, RealClock{})

	failMock := &mocks.MockExecutor{Result: agent.ExecResult{
		Success: false,
		Error:   fmt.Errorf("no server running"),
	}}
	ep.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
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
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	// First attempt: inline retry exhausts all attempts (1 initial + 1 retry = 2 calls), then sets backoff
	expectedCalls := 1 + cfg.Retry.EffectiveResultNotifyInlineRetries() // initial + inline retries
	n := rh.processWorkerResultFile("worker1")
	if n != 0 {
		t.Fatalf("expected 0 (failed), got %d", n)
	}
	if len(failMock.Calls) != expectedCalls {
		t.Fatalf("expected %d executor calls (initial + inline retries), got %d", expectedCalls, len(failMock.Calls))
	}

	// Second attempt immediately: should be skipped due to backoff
	failMock.Calls = nil
	n = rh.processWorkerResultFile("worker1")
	if n != 0 {
		t.Fatalf("expected 0 (backoff), got %d", n)
	}
	if len(failMock.Calls) != 0 {
		t.Fatalf("expected no executor calls during backoff, got %d", len(failMock.Calls))
	}
}

func TestResultHandler_MultipleResults_ProcessedInOrder(t *testing.T) {
	t.Parallel()
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
				CreatedAt: now,
			},
			{
				ID: "res_0000000002_aaaaaaaa", TaskID: "task_0000000002_22222222",
				CommandID: "cmd_0000000001_cccccccc", Status: model.StatusFailed,
				CreatedAt: now,
			},
		},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	yamlutil.AtomicWrite(resultPath, rf)

	n := rh.processWorkerResultFile("worker1")
	if n != 2 {
		t.Fatalf("expected 2 notified, got %d", n)
	}
	if len(mock.Calls) != 2 {
		t.Fatalf("expected 2 executor calls, got %d", len(mock.Calls))
	}

	// Verify both are marked notified
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var updated model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	for i, r := range updated.Results {
		if !r.Notified {
			t.Errorf("result[%d] expected notified=true", i)
		}
	}
}

// TestResultHandler_SweepExhaustedNotifications_Worker verifies that worker
// results whose notification retries were exhausted are transitioned into the
// notify-dead-letter terminal state, archived, and surfaced via an orchestrator
// notification instead of being silently skipped forever.
func TestResultHandler_SweepExhaustedNotifications_Worker(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rh, _ := newTestResultHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{{
			ID:        "res_0000001000_exhaust1",
			TaskID:    "task_0000001000_aaaaaaaa",
			CommandID: "cmd_0000001000_bbbbbbbb",
			Status:    model.StatusCompleted,
			Summary:   "done",
			NotifiableBase: model.NotifiableBase{
				NotifyAttempts: maxNotifyAttempts, // already at the ceiling
			},
			CreatedAt: now,
		}},
	}
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		t.Fatalf("write result: %v", err)
	}

	// Trigger one scan — sweep should fire before the notify loop.
	_ = rh.processWorkerResultFile("worker1")

	// File should now show dead-letter state.
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var updated model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &updated); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !updated.Results[0].NotifyDeadLettered {
		t.Fatalf("expected NotifyDeadLettered=true, got %+v", updated.Results[0])
	}
	if updated.Results[0].NotifyDeadLetterReason == nil {
		t.Error("expected NotifyDeadLetterReason to be set")
	}

	// Archive should exist under dead_letters/.
	entries, err := os.ReadDir(filepath.Join(maestroDir, "dead_letters"))
	if err != nil {
		t.Fatalf("read dead_letters: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), "res_0000001000_exhaust1") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected archive for exhausted result, entries=%v", entries)
	}

	// Orchestrator notification queue should contain a command_failed entry.
	nqData, err := os.ReadFile(filepath.Join(maestroDir, "queue", "orchestrator.yaml"))
	if err != nil {
		t.Fatalf("read orchestrator queue: %v", err)
	}
	var nq model.NotificationQueue
	if err := yamlv3.Unmarshal(nqData, &nq); err != nil {
		t.Fatalf("unmarshal nq: %v", err)
	}
	if len(nq.Notifications) != 1 || nq.Notifications[0].Type != model.NotificationTypeCommandFailed {
		t.Errorf("expected one command_failed notification, got: %+v", nq.Notifications)
	}
	if nq.Notifications[0].SourceResultID != "result_dl_res_0000001000_exhaust1" {
		t.Errorf("unexpected SourceResultID: %s", nq.Notifications[0].SourceResultID)
	}

	// Running the scan a second time must NOT emit a duplicate notification.
	_ = rh.processWorkerResultFile("worker1")
	nqData, _ = os.ReadFile(filepath.Join(maestroDir, "queue", "orchestrator.yaml"))
	_ = yamlv3.Unmarshal(nqData, &nq)
	if len(nq.Notifications) != 1 {
		t.Errorf("sweep must be idempotent, got %d notifications", len(nq.Notifications))
	}
}

