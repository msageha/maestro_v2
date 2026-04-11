package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/reconcile"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil/mocks"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// =============================================================================
// Notification Dispatch — Boundary Tests
// =============================================================================

// TestNotificationDispatch_InProgressBlocks verifies that a valid in-progress
// notification blocks dispatch of pending notifications.
func TestNotificationDispatch_InProgressBlocks(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	owner := "daemon:1234"
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)

	nq := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "queue_notification",
		Notifications: []model.Notification{
			{
				ID: "ntf_ip", CommandID: "cmd1", Type: "command_completed",
				Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &future, LeaseEpoch: 1,
				CreatedAt: now, UpdatedAt: now,
			},
			{
				ID: "ntf_pending", CommandID: "cmd2", Type: "command_completed",
				Status: model.StatusPending, Priority: 1,
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	nqPath := filepath.Join(maestroDir, "queue", "orchestrator.yaml")
	yamlutil.AtomicWrite(nqPath, nq)

	qh.PeriodicScan()

	data, _ := os.ReadFile(nqPath)
	var result model.NotificationQueue
	parseYAML(data, &result)

	for _, ntf := range result.Notifications {
		if ntf.ID == "ntf_pending" && ntf.Status != model.StatusPending {
			t.Errorf("ntf_pending: got %s, want pending (blocked by in-progress notification)", ntf.Status)
		}
	}
}

// TestNotificationDispatch_ExpiredLeaseUnblocks verifies that an expired
// in-progress notification does NOT block new dispatches.
func TestNotificationDispatch_ExpiredLeaseUnblocks(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	owner := "daemon:1234"
	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)

	nq := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "queue_notification",
		Notifications: []model.Notification{
			{
				ID: "ntf_exp", CommandID: "cmd1", Type: "command_completed",
				Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &expired, LeaseEpoch: 1,
				CreatedAt: now, UpdatedAt: now,
			},
			{
				ID: "ntf_wait", CommandID: "cmd2", Type: "command_completed",
				Status: model.StatusPending, Priority: 1,
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	nqPath := filepath.Join(maestroDir, "queue", "orchestrator.yaml")
	yamlutil.AtomicWrite(nqPath, nq)

	qh.PeriodicScan()

	data, _ := os.ReadFile(nqPath)
	var result model.NotificationQueue
	parseYAML(data, &result)

	// Expired notification should be released (pending)
	for _, ntf := range result.Notifications {
		if ntf.ID == "ntf_exp" && ntf.Status == model.StatusInProgress {
			t.Errorf("ntf_exp should be released from expired lease, got in_progress")
		}
	}
}

// =============================================================================
// Mixed Queue Conditions — Comprehensive Tests
// =============================================================================

// TestMixedQueue_ExpiredLeasesPrioritizeRecovery verifies that when expired
// leases exist, dispatch is skipped and recovery is prioritized.
func TestMixedQueue_ExpiredLeasesPrioritizeRecovery(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)
	qh.scanExecutor.busyChecker = BusyCheckerFunc(func(string) bool { return false })

	now := time.Now().UTC().Format(time.RFC3339)
	owner := "worker1"
	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)

	// Worker1 has expired task, Worker2 has pending task
	tq1 := model.TaskQueue{
		SchemaVersion: 1, FileType: "task_queue",
		Tasks: []model.Task{
			{
				ID: "task_exp", CommandID: "cmd_001",
				Status: model.StatusInProgress, LeaseOwner: &owner,
				LeaseExpiresAt: &expired, LeaseEpoch: 1,
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	tq2 := model.TaskQueue{
		SchemaVersion: 1, FileType: "task_queue",
		Tasks: []model.Task{
			{
				ID: "task_wait", CommandID: "cmd_001", Priority: 1,
				Status:    model.StatusPending,
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker1.yaml"), tq1)
	yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "worker2.yaml"), tq2)

	qh.PeriodicScan()

	// Expired task should be recovered (pending)
	data1, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "worker1.yaml"))
	var result1 model.TaskQueue
	parseYAML(data1, &result1)
	if result1.Tasks[0].Status != model.StatusPending {
		t.Errorf("task_exp: got %s, want pending (recovered)", result1.Tasks[0].Status)
	}

	// Worker2's pending task should NOT have been dispatched (recovery takes priority)
	data2, _ := os.ReadFile(filepath.Join(maestroDir, "queue", "worker2.yaml"))
	var result2 model.TaskQueue
	parseYAML(data2, &result2)
	if result2.Tasks[0].Status != model.StatusPending {
		t.Errorf("task_wait: got %s, want pending (dispatch skipped during recovery)", result2.Tasks[0].Status)
	}
}

// TestMultipleWorkersDispatch verifies that tasks on different workers can be
// dispatched concurrently in the same scan.
func TestMultipleWorkersDispatch(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	for i := 1; i <= 2; i++ {
		tq := model.TaskQueue{
			SchemaVersion: 1, FileType: "task_queue",
			Tasks: []model.Task{
				{
					ID: fmt.Sprintf("task_w%d", i), CommandID: "cmd_001",
					Purpose: "test", Content: "work", Priority: 1,
					Status:    model.StatusPending,
					CreatedAt: now, UpdatedAt: now,
				},
			},
		}
		yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", fmt.Sprintf("worker%d.yaml", i)), tq)
	}

	qh.PeriodicScan()

	dispatched := 0
	for i := 1; i <= 2; i++ {
		data, _ := os.ReadFile(filepath.Join(maestroDir, "queue", fmt.Sprintf("worker%d.yaml", i)))
		var result model.TaskQueue
		parseYAML(data, &result)
		if result.Tasks[0].Status == model.StatusInProgress {
			dispatched++
		}
	}
	if dispatched != 2 {
		t.Errorf("expected 2 workers dispatched, got %d", dispatched)
	}
}

// =============================================================================
// workerIDFromPath — Edge Cases
// =============================================================================

func TestWorkerIDFromPath_EdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want string
	}{
		{"/path/to/queue/worker1.yaml", "worker1"},
		{"/path/to/queue/worker10.yaml", "worker10"},
		{"/path/to/queue/planner.yaml", ""},
		{"/path/to/queue/orchestrator.yaml", ""},
		{"/path/to/queue/worker.yaml", "worker"},
		{"worker1.yaml", "worker1"},
		{"/path/to/queue/workerXYZ.yaml", "workerXYZ"},
		{"/path/to/queue/worker1.yml", ""},     // wrong extension
		{"/path/to/queue/notworker1.yaml", ""}, // wrong prefix
	}

	for _, tt := range tests {
		got := workerIDFromPath(tt.path)
		if got != tt.want {
			t.Errorf("workerIDFromPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

// =============================================================================
// Reconciler R5 — Edge Cases
// =============================================================================

// TestReconciler_R5_NotNotified_NoRepair verifies that R5 skips results where
// notified=false (not yet processed by result handler).
func TestReconciler_R5_NotNotified_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{
				ID: "res_r5_nonotify", CommandID: "cmd_r5_nn", Status: model.StatusCompleted,
				CreatedAt: now, // not yet notified
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	nq := model.NotificationQueue{SchemaVersion: 1, FileType: "queue_notification"}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "orchestrator.yaml"), nq)

	repairs, _ := rec.Reconcile()
	r5 := filterRepairs(repairs, reconcile.PatternR5)
	if len(r5) != 0 {
		t.Fatalf("expected 0 R5 repairs for non-notified result, got %d", len(r5))
	}
}

// TestReconciler_R5_NonTerminalResult_NoRepair verifies R5 skips non-terminal results.
func TestReconciler_R5_NonTerminalResult_NoRepair(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{
				ID: "res_r5_ip", CommandID: "cmd_r5_ip", Status: model.StatusInProgress, // non-terminal
				NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now},
				CreatedAt: now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	nq := model.NotificationQueue{SchemaVersion: 1, FileType: "queue_notification"}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "orchestrator.yaml"), nq)

	repairs, _ := rec.Reconcile()
	r5 := filterRepairs(repairs, reconcile.PatternR5)
	if len(r5) != 0 {
		t.Fatalf("expected 0 R5 repairs for non-terminal result, got %d", len(r5))
	}
}

// =============================================================================
// Planner Signal Upsert — Deduplication
// =============================================================================

func TestUpsertPlannerSignal_Deduplication(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	sq := model.PlannerSignalQueue{}
	dirty := false

	sig := model.PlannerSignal{
		Kind: "awaiting_fill", CommandID: "cmd_dup", PhaseID: "p1", PhaseName: "phase1",
		Message: "first", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}

	qh.upsertPlannerSignal(&sq, &dirty, sig, buildSignalIndex(sq.Signals))
	if !dirty || len(sq.Signals) != 1 {
		t.Fatal("first signal should be added")
	}

	dirty = false
	sig2 := sig
	sig2.Message = "duplicate"
	qh.upsertPlannerSignal(&sq, &dirty, sig2, buildSignalIndex(sq.Signals))
	if dirty || len(sq.Signals) != 1 {
		t.Errorf("duplicate signal should be rejected: dirty=%v, len=%d", dirty, len(sq.Signals))
	}
	if sq.Signals[0].Message != "first" {
		t.Errorf("original signal should be preserved, got message=%s", sq.Signals[0].Message)
	}

	// Different kind should be added
	dirty = false
	sig3 := sig
	sig3.Kind = "fill_timeout"
	qh.upsertPlannerSignal(&sq, &dirty, sig3, buildSignalIndex(sq.Signals))
	if !dirty || len(sq.Signals) != 2 {
		t.Errorf("different kind should be added: dirty=%v, len=%d", dirty, len(sq.Signals))
	}
}

// =============================================================================
// hasExpiredLeases — Comprehensive Tests
// =============================================================================

func TestHasExpiredLeases_NoLeases(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	cq := model.CommandQueue{}
	nq := model.NotificationQueue{}
	taskQueues := map[string]*taskQueueEntry{}

	if qh.hasExpiredLeases(taskQueues, &cq, &nq) {
		t.Error("expected false with no entries")
	}
}

func TestHasExpiredLeases_ValidLeases(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	owner := "daemon:1234"

	cq := model.CommandQueue{Commands: []model.Command{
		{ID: "cmd_1", Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &future},
	}}
	nq := model.NotificationQueue{}
	taskQueues := map[string]*taskQueueEntry{}

	if qh.hasExpiredLeases(taskQueues, &cq, &nq) {
		t.Error("expected false with valid (non-expired) leases")
	}
}

func TestHasExpiredLeases_ExpiredCommand(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	owner := "daemon:1234"

	cq := model.CommandQueue{Commands: []model.Command{
		{ID: "cmd_1", Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &expired},
	}}
	nq := model.NotificationQueue{}
	taskQueues := map[string]*taskQueueEntry{}

	if !qh.hasExpiredLeases(taskQueues, &cq, &nq) {
		t.Error("expected true with expired command lease")
	}
}

func TestHasExpiredLeases_ExpiredNotification(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	owner := "daemon:1234"

	cq := model.CommandQueue{}
	nq := model.NotificationQueue{Notifications: []model.Notification{
		{ID: "ntf_1", Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &expired},
	}}
	taskQueues := map[string]*taskQueueEntry{}

	if !qh.hasExpiredLeases(taskQueues, &cq, &nq) {
		t.Error("expected true with expired notification lease")
	}
}

func TestHasExpiredLeases_PendingIgnored(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	owner := "daemon:1234"

	// Pending command with expired lease should NOT be considered
	cq := model.CommandQueue{Commands: []model.Command{
		{ID: "cmd_1", Status: model.StatusPending, LeaseOwner: &owner, LeaseExpiresAt: &expired},
	}}
	nq := model.NotificationQueue{}
	taskQueues := map[string]*taskQueueEntry{}

	if qh.hasExpiredLeases(taskQueues, &cq, &nq) {
		t.Error("expected false: pending entries with expired lease_expires_at are not expired leases")
	}
}

// TestHasExpiredLeases_ExpiredTask verifies the task branch of hasExpiredLeases.
func TestHasExpiredLeases_ExpiredTask(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	owner := "worker1"

	cq := model.CommandQueue{}
	nq := model.NotificationQueue{}
	taskQueues := map[string]*taskQueueEntry{
		"worker1.yaml": {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{ID: "task_1", Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &expired},
				},
			},
		},
	}

	if !qh.hasExpiredLeases(taskQueues, &cq, &nq) {
		t.Error("expected true with expired task lease")
	}
}

// TestHasExpiredLeases_ValidTaskNotExpired verifies valid task leases don't trigger.
func TestHasExpiredLeases_ValidTaskNotExpired(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	owner := "worker1"

	cq := model.CommandQueue{}
	nq := model.NotificationQueue{}
	taskQueues := map[string]*taskQueueEntry{
		"worker1.yaml": {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{ID: "task_1", Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &future},
				},
			},
		},
	}

	if qh.hasExpiredLeases(taskQueues, &cq, &nq) {
		t.Error("expected false with valid task lease")
	}
}

// =============================================================================
// Notification in_progress + lease_expires_at=nil (malformed)
// =============================================================================

// TestNotificationDispatch_InProgressNilLease verifies that an in_progress
// notification with nil lease_expires_at does NOT block pending dispatch
// (the blocking guard checks LeaseExpiresAt != nil).
func TestNotificationDispatch_InProgressNilLease(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	owner := "daemon:1234"

	nq := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "queue_notification",
		Notifications: []model.Notification{
			{
				ID: "ntf_malformed", CommandID: "cmd1", Type: "command_completed",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: nil, // malformed: in_progress but nil lease
				LeaseEpoch:     1,
				CreatedAt:      now, UpdatedAt: now,
			},
			{
				ID: "ntf_pending", CommandID: "cmd2", Type: "command_completed",
				Status: model.StatusPending, Priority: 1,
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	nqPath := filepath.Join(maestroDir, "queue", "orchestrator.yaml")
	if err := yamlutil.AtomicWrite(nqPath, nq); err != nil {
		t.Fatalf("write notification queue: %v", err)
	}

	qh.PeriodicScan()

	data, err := os.ReadFile(nqPath)
	if err != nil {
		t.Fatalf("read notification queue: %v", err)
	}
	var result model.NotificationQueue
	if err := parseYAML(data, &result); err != nil {
		t.Fatalf("parse notification queue: %v", err)
	}

	// Behavior analysis:
	// 1. Guard at line 686: checks ntf.LeaseExpiresAt != nil → nil fails, so does NOT block.
	// 2. hasExpiredLeases: IsLeaseExpired(nil) returns true → recovery mode entered.
	// 3. In recovery mode, dispatches are skipped → ntf_pending stays pending.
	// 4. recoverExpiredNotificationLeases: directly releases malformed notification
	//    (notifications don't go through busy checks, unlike tasks).
	//    So ntf_malformed is released back to pending.

	var malformedNtf, pendingNtf *model.Notification
	for i := range result.Notifications {
		switch result.Notifications[i].ID {
		case "ntf_malformed":
			malformedNtf = &result.Notifications[i]
		case "ntf_pending":
			pendingNtf = &result.Notifications[i]
		}
	}
	if malformedNtf == nil || pendingNtf == nil {
		t.Fatal("both notifications should be present in the queue")
	}

	// ntf_pending must remain pending: recovery mode blocks dispatch
	if pendingNtf.Status != model.StatusPending {
		t.Errorf("ntf_pending: got %s, want pending (recovery mode blocks dispatch)", pendingNtf.Status)
	}

	// ntf_malformed released to pending: recoverExpiredNotificationLeases always
	// releases expired notification leases (no busy check needed for notifications).
	if malformedNtf.Status != model.StatusPending {
		t.Errorf("ntf_malformed: got %s, want pending (expired lease released)", malformedNtf.Status)
	}
	if malformedNtf.LeaseOwner != nil {
		t.Errorf("ntf_malformed: lease_owner should be nil after release, got %v", *malformedNtf.LeaseOwner)
	}
}

// =============================================================================
// Stale Busy Check Fencing — Task
// =============================================================================

// TestBusyCheckFencing_StaleTask verifies that applyTaskBusyCheckResult
// rejects results when the task has changed since Phase A.
func TestBusyCheckFencing_StaleTask(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	owner := "worker1"
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)

	taskQueues := map[string]*taskQueueEntry{
		"worker1.yaml": {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{
						ID: "task_bc", CommandID: "cmd_bc",
						Status: model.StatusInProgress, LeaseOwner: &owner,
						LeaseExpiresAt: &future, LeaseEpoch: 5,
						CreatedAt: now, UpdatedAt: now,
					},
				},
			},
		},
	}
	taskDirty := map[string]bool{}

	// Stale busy check: epoch mismatch
	staleBc := busyCheckResult{
		Item: busyCheckItem{
			EntryID:   "task_bc",
			QueueFile: "worker1.yaml",
			Epoch:     2, // stale
			ExpiresAt: future,
			UpdatedAt: now,
		},
		Busy: false,
	}
	qh.applyTaskBusyCheckResult(staleBc, taskQueues, taskDirty)

	// Task should not be modified
	task := taskQueues["worker1.yaml"].Queue.Tasks[0]
	if task.Status != model.StatusInProgress {
		t.Errorf("task should remain in_progress after stale busy check, got %s", task.Status)
	}
	if taskDirty["worker1.yaml"] {
		t.Error("taskDirty should be false — stale busy check rejected")
	}
}

// =============================================================================
// R4 canComplete == nil — Skip Branch
// =============================================================================

// TestReconciler_R4_CanCompleteNil_Skip verifies that R4 is skipped when
// canComplete callback is not wired.
func TestReconciler_R4_CanCompleteNil_Skip(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetCanComplete(nil) // explicitly unset

	now := time.Now().UTC().Format(time.RFC3339)

	// Set up a result file with terminal result
	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{
				ID: "res_r4_nil", CommandID: "cmd_r4_nil", Status: model.StatusCompleted,
				NotifiableBase: model.NotifiableBase{Notified: true, NotifiedAt: &now},
				CreatedAt: now,
			},
		},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf); err != nil {
		t.Fatalf("write result file: %v", err)
	}

	// Set up state that is non-terminal (to trigger R4)
	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  "cmd_r4_nil",
		PlanStatus: model.PlanStatusSealed,
		TaskStates: map[string]model.Status{"task_1": model.StatusPending},
		CreatedAt:  now, UpdatedAt: now,
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r4_nil.yaml"), state); err != nil {
		t.Fatalf("write command state: %v", err)
	}

	// Queue with non-terminal command
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	nq := model.NotificationQueue{SchemaVersion: 1, FileType: "queue_notification"}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "orchestrator.yaml"), nq)
	cq := model.CommandQueue{
		SchemaVersion: 1, FileType: "command_queue",
		Commands: []model.Command{
			{ID: "cmd_r4_nil", Status: model.StatusInProgress, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "planner.yaml"), cq)

	repairs, _ := rec.Reconcile()
	r4 := filterRepairs(repairs, reconcile.PatternR4)
	// R4 should be skipped because canComplete is nil
	if len(r4) != 0 {
		t.Fatalf("expected 0 R4 repairs when canComplete is nil, got %d", len(r4))
	}
}

// =============================================================================
// R6 Boundary: deadline == now, and deadline parse error
// =============================================================================

// TestReconciler_R6_DeadlineParseError verifies that R6 skips a phase with
// an unparseable FillDeadlineAt.
func TestReconciler_R6_DeadlineParseError(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	badDeadline := "not-a-valid-timestamp"
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  "cmd_r6_parse",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID: "p1", Name: "phase1",
				Status:         model.PhaseStatusAwaitingFill,
				FillDeadlineAt: &badDeadline,
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_parse.yaml"), state); err != nil {
		t.Fatalf("write command state: %v", err)
	}

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, reconcile.PatternR6)
	if len(r6) != 0 {
		t.Fatalf("expected 0 R6 repairs for unparseable deadline, got %d", len(r6))
	}
}

// TestReconciler_R6_DeadlineExactlyNow verifies R6 at the exact boundary.
// The R6 check is: clock.Now().UTC().Before(deadline) → continue (skip).
// When deadline == now, Before returns false → R6 triggers.
// Uses a fake clock to avoid time.Sleep and flakiness.
func TestReconciler_R6_DeadlineExactlyNow(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	// Set deadline to a fixed point in time
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deadlineStr := baseTime.Format(time.RFC3339)
	nowStr := baseTime.Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  "cmd_r6_now",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID: "p1", Name: "phase1",
				Status:         model.PhaseStatusAwaitingFill,
				FillDeadlineAt: &deadlineStr,
			},
		},
		CreatedAt: nowStr, UpdatedAt: nowStr,
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_now.yaml"), state); err != nil {
		t.Fatalf("write command state: %v", err)
	}

	// Inject a fake clock that returns a time 2 seconds after the deadline
	rec.SetClock(&testClock{now: baseTime.Add(2 * time.Second)})

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, reconcile.PatternR6)
	if len(r6) != 1 {
		t.Fatalf("expected 1 R6 repair for deadline at exact boundary, got %d", len(r6))
	}
}

// testClock implements core.Clock for deterministic testing.
type testClock struct {
	now time.Time
}

func (tc *testClock) Now() time.Time { return tc.now }

// =============================================================================
// DeferredNotification Content Verification
// =============================================================================

// TestDeferredNotification_R0b_ReFill verifies that R0b produces a "re_fill"
// deferred notification with the correct CommandID.
func TestDeferredNotification_R0b_ReFill(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	oldTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  "cmd_r0b_notif",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID: "p1", Name: "stuck-phase",
				Status:  model.PhaseStatusFilling,
				TaskIDs: []string{"task_stuck"},
			},
		},
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0b_notif.yaml"), state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// Create worker queue with the partial task
	queueDir := filepath.Join(maestroDir, "queue")
	os.MkdirAll(queueDir, 0755)
	tq := model.TaskQueue{
		SchemaVersion: 1, FileType: "queue_task",
		Tasks: []model.Task{
			{ID: "task_stuck", CommandID: "cmd_r0b_notif", Status: model.StatusPending,
				CreatedAt: oldTime, UpdatedAt: oldTime},
		},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(queueDir, "worker1.yaml"), tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	repairs, notifications := rec.Reconcile()

	r0b := filterRepairs(repairs, reconcile.PatternR0b)
	if len(r0b) != 1 {
		t.Fatalf("expected 1 R0b repair, got %d", len(r0b))
	}

	// Verify DeferredNotification content
	if len(notifications) != 1 {
		t.Fatalf("expected 1 deferred notification from R0b, got %d", len(notifications))
	}
	n := notifications[0]
	if n.Kind != reconcile.NotifyReFill {
		t.Errorf("notification kind: got %s, want re_fill", n.Kind)
	}
	if n.CommandID != "cmd_r0b_notif" {
		t.Errorf("notification command_id: got %s, want cmd_r0b_notif", n.CommandID)
	}
}

// TestDeferredNotification_R4_ReEvaluate verifies that R4 (canComplete error)
// produces a "re_evaluate" deferred notification with the correct Reason.
func TestDeferredNotification_R4_ReEvaluate(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	canCompleteErr := "tasks not terminal: task_a pending"
	rec.SetCanComplete(func(state *model.CommandState) (model.PlanStatus, error) {
		return "", fmt.Errorf("%s", canCompleteErr)
	})

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r4_ntf", CommandID: "cmd_r4_ntf", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf); err != nil {
		t.Fatalf("write result file: %v", err)
	}

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  "cmd_r4_ntf",
		PlanStatus: model.PlanStatusSealed,
		CreatedAt:  now, UpdatedAt: now,
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r4_ntf.yaml"), state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	os.MkdirAll(filepath.Join(maestroDir, "quarantine"), 0755)

	repairs, notifications := rec.Reconcile()

	r4 := filterRepairs(repairs, reconcile.PatternR4)
	if len(r4) != 1 {
		t.Fatalf("expected 1 R4 repair, got %d", len(r4))
	}

	// Verify DeferredNotification content
	if len(notifications) != 1 {
		t.Fatalf("expected 1 deferred notification from R4, got %d", len(notifications))
	}
	n := notifications[0]
	if n.Kind != reconcile.NotifyReEvaluate {
		t.Errorf("notification kind: got %s, want re_evaluate", n.Kind)
	}
	if n.CommandID != "cmd_r4_ntf" {
		t.Errorf("notification command_id: got %s, want cmd_r4_ntf", n.CommandID)
	}
	if n.Reason != canCompleteErr {
		t.Errorf("notification reason: got %q, want %q", n.Reason, canCompleteErr)
	}
}

// TestDeferredNotification_R6_FillTimeout verifies that R6 produces a
// "fill_timeout" deferred notification with the correct TimedOutPhases.
func TestDeferredNotification_R6_FillTimeout(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconcilerWithFactory(maestroDir, func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	})

	now := time.Now().UTC().Format(time.RFC3339)
	pastDeadline := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  "cmd_r6_ntf",
		PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{
				PhaseID: "p1", Name: "research", Type: "concrete",
				Status: model.PhaseStatusCompleted,
			},
			{
				PhaseID: "p2", Name: "implementation", Type: "deferred",
				Status:          model.PhaseStatusAwaitingFill,
				DependsOnPhases: []string{"research"},
				FillDeadlineAt:  &pastDeadline,
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_ntf.yaml"), state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	repairs, notifications := rec.Reconcile()

	r6 := filterRepairs(repairs, reconcile.PatternR6)
	if len(r6) != 1 {
		t.Fatalf("expected 1 R6 repair, got %d: %+v", len(r6), r6)
	}

	// Verify DeferredNotification content
	if len(notifications) != 1 {
		t.Fatalf("expected 1 deferred notification from R6, got %d", len(notifications))
	}
	n := notifications[0]
	if n.Kind != reconcile.NotifyFillTimeout {
		t.Errorf("notification kind: got %s, want fill_timeout", n.Kind)
	}
	if n.CommandID != "cmd_r6_ntf" {
		t.Errorf("notification command_id: got %s, want cmd_r6_ntf", n.CommandID)
	}
	if n.TimedOutPhases == nil {
		t.Fatal("notification timed_out_phases should not be nil")
	}
	if !n.TimedOutPhases["implementation"] {
		t.Errorf("timed_out_phases should contain 'implementation', got %v", n.TimedOutPhases)
	}
	if len(n.TimedOutPhases) != 1 {
		t.Errorf("expected 1 timed out phase, got %d: %v", len(n.TimedOutPhases), n.TimedOutPhases)
	}
}
