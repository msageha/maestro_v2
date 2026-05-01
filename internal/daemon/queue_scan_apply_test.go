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
	"github.com/msageha/maestro_v2/internal/daemon/dispatch"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// fixedClock implements Clock with a fixed time for deterministic tests.
type fixedClock struct {
	now time.Time
}

func (fc *fixedClock) Now() time.Time { return fc.now }

func TestApplyBusyCheckCore_GraceLimitExceeded(t *testing.T) {
	t.Parallel()

	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	expiresAt := now.Add(-1 * time.Second).Format(time.RFC3339) // expired lease

	// Config: maxInProgressMin=60, dispatchLeaseSec=300 (5min), scanIntervalSec=10
	// Grace limit = maxGraceLeaseDuration(60, 10) = 60/3 = 20 minutes
	// Grace starts at updatedAt + dispatchLease
	tests := []struct {
		name              string
		updatedAt         string
		wantLeaseReleases int
		wantExtensions    int
	}{
		{
			name: "grace_within_limit_extends",
			// updatedAt = now - 10min; graceStart = now - 10min + 5min = now - 5min
			// elapsed grace = 5min < 20min limit => extend
			updatedAt:         now.Add(-10 * time.Minute).Format(time.RFC3339),
			wantLeaseReleases: 0,
			wantExtensions:    1,
		},
		{
			name: "grace_exceeded_releases",
			// updatedAt = now - 30min; graceStart = now - 30min + 5min = now - 25min
			// elapsed grace = 25min >= 20min limit => release
			updatedAt:         now.Add(-30 * time.Minute).Format(time.RFC3339),
			wantLeaseReleases: 1,
			wantExtensions:    0,
		},
		{
			name: "grace_at_boundary_releases",
			// updatedAt = now - 25min; graceStart = now - 25min + 5min = now - 20min
			// elapsed grace = 20min >= 20min limit => release
			updatedAt:         now.Add(-25 * time.Minute).Format(time.RFC3339),
			wantLeaseReleases: 1,
			wantExtensions:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			maestroDir := testutil.SetupDirFixPerms(t)
			cfg := model.Config{
				Agents: model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
				Watcher: model.WatcherConfig{
					DispatchLeaseSec: 300,
					ScanIntervalSec:  10,
					MaxInProgressMin: ptr.Int(60),
				},
				Queue: model.QueueConfig{PriorityAgingSec: 60},
			}
			qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
			qh.clock = &fixedClock{now: now}
			qh.scanExecutor.scanCounters = metrics.ScanCounters{}

			released := false
			extended := false
			dirty := false

			bc := busyCheckResult{
				Item: busyCheckItem{
					Kind:      "task",
					EntryID:   "task_001",
					AgentID:   "worker1",
					Epoch:     3,
					UpdatedAt: tt.updatedAt,
					ExpiresAt: expiresAt,
				},
				Busy:      false,
				Undecided: true,
			}

			ops := busyCheckOps{
				kind:       "task",
				ownerLabel: "worker=worker1",
				releaseLease: func() error {
					released = true
					return nil
				},
				extendLease: func() error {
					return nil
				},
				extendGrace: func(ttl time.Duration) error {
					extended = true
					return nil
				},
				markDirty: func() {
					dirty = true
				},
			}

			qh.applyBusyCheckCore(bc, "task_001", model.StatusInProgress, 3, &expiresAt, ops)

			if qh.scanExecutor.scanCounters.LeaseReleases != tt.wantLeaseReleases {
				t.Errorf("LeaseReleases = %d, want %d", qh.scanExecutor.scanCounters.LeaseReleases, tt.wantLeaseReleases)
			}
			if qh.scanExecutor.scanCounters.LeaseExtensions != tt.wantExtensions {
				t.Errorf("LeaseExtensions = %d, want %d", qh.scanExecutor.scanCounters.LeaseExtensions, tt.wantExtensions)
			}
			if tt.wantLeaseReleases > 0 && !released {
				t.Error("expected releaseLease to be called")
			}
			if tt.wantExtensions > 0 && !extended {
				t.Error("expected extendGrace to be called")
			}
			if !dirty {
				t.Error("expected dirty to be set")
			}
		})
	}
}

// --- applyNotificationDispatchResult tests ---

func TestApplyNotificationDispatchResult_Success(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	expiresAt := now.Add(5 * time.Minute).Format(time.RFC3339)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}

	nq := &model.NotificationQueue{
		Notifications: []model.Notification{
			{
				ID:             "ntf1",
				Status:         model.StatusInProgress,
				LeaseEpoch:     2,
				LeaseExpiresAt: &expiresAt,
			},
		},
	}
	dirty := false

	dr := dispatchResult{
		Item: dispatchItem{
			Kind:         "notification",
			Notification: &model.Notification{ID: "ntf1"},
			Epoch:        2,
			ExpiresAt:    expiresAt,
		},
		Success: true,
	}

	qh.applyNotificationDispatchResult(dr, nq, &dirty)

	if !dirty {
		t.Error("expected dirty=true after successful notification dispatch")
	}
	if nq.Notifications[0].Status != model.StatusCompleted {
		t.Errorf("notification status = %s, want completed", nq.Notifications[0].Status)
	}
}

func TestApplyNotificationDispatchResult_NotFound(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	expiresAt := now.Add(5 * time.Minute).Format(time.RFC3339)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}

	nq := &model.NotificationQueue{
		Notifications: []model.Notification{
			{ID: "ntf_other", Status: model.StatusInProgress, LeaseEpoch: 1, LeaseExpiresAt: &expiresAt},
		},
	}
	dirty := false

	dr := dispatchResult{
		Item: dispatchItem{
			Kind:         "notification",
			Notification: &model.Notification{ID: "ntf_missing"},
			Epoch:        1,
			ExpiresAt:    expiresAt,
		},
		Success: true,
	}

	// Should not panic when notification is not found
	qh.applyNotificationDispatchResult(dr, nq, &dirty)

	if dirty {
		t.Error("expected dirty=false when notification not found")
	}
	if nq.Notifications[0].Status != model.StatusInProgress {
		t.Errorf("other notification should be unchanged, got %s", nq.Notifications[0].Status)
	}
}

// --- applyTaskDispatchResult tests ---

func TestApplyTaskDispatchResult_Success(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	expiresAt := now.Add(5 * time.Minute).Format(time.RFC3339)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}

	queueFile := "/fake/worker1.yaml"
	taskQueues := map[string]*taskQueueEntry{
		queueFile: {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{
						ID:             "t1",
						Status:         model.StatusInProgress,
						LeaseEpoch:     3,
						LeaseExpiresAt: &expiresAt,
					},
				},
			},
		},
	}
	taskDirty := map[string]bool{}

	dr := dispatchResult{
		Item: dispatchItem{
			Kind:      "task",
			Task:      &model.Task{ID: "t1"},
			Epoch:     3,
			ExpiresAt: expiresAt,
		},
		Success: true,
	}

	qh.applyTaskDispatchResult(dr, taskQueues, taskDirty)

	if !taskDirty[queueFile] {
		t.Error("expected taskDirty[queueFile]=true")
	}
	if qh.scanExecutor.scanCounters.TasksDispatched != 1 {
		t.Errorf("TasksDispatched = %d, want 1", qh.scanExecutor.scanCounters.TasksDispatched)
	}
}

// TestApplyTaskDispatchResult_SubmitUncertain_RetainsLeaseAndMarksRunning is
// the regression guard for the 2026-04-27 single-worker E2E run. After a
// long task1 (~70s) completed, task2 dispatch raised
// ErrSubmitConfirmUncertain because the deliverer's 6-second probe
// exhausted without seeing a Claude UI marker — even though the worker had
// already received the prompt and was actively writing the task's expected
// output to its worktree. The previous code released the lease on every
// non-retryable failure, which let the next scan re-dispatch the same task
// (epoch 2, then epoch 3) and corrupted the run: the worker's eventual
// result_write hit FENCING_REJECT because the queue entry had bounced back
// to "pending" during a lease_release window. The fix treats
// ErrSubmitConfirmUncertain as an "assumed delivered" outcome on the task
// path: lease retained, task remains in_progress, lease epoch unchanged,
// markTaskRunning advances the extended state machine, and
// TasksDispatchedUncertain (rather than TasksDispatched or LeaseReleases)
// is incremented so operators can monitor probe false-negative rates
// separately. If the worker really didn't receive (rare), the dispatch
// lease TTL is the recovery boundary — hasExpiredLeases picks up
// in_progress tasks past their TTL via the standard expired-lease path.
func TestApplyTaskDispatchResult_SubmitUncertain_RetainsLeaseAndMarksRunning(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	expiresAt := now.Add(5 * time.Minute).Format(time.RFC3339)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}

	owner := "worker1"
	queueFile := "/fake/worker1.yaml"
	taskQueues := map[string]*taskQueueEntry{
		queueFile: {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{
						ID:             "t1",
						CommandID:      "cmd1",
						Status:         model.StatusInProgress,
						LeaseEpoch:     3,
						LeaseOwner:     &owner,
						LeaseExpiresAt: &expiresAt,
					},
				},
			},
		},
	}
	taskDirty := map[string]bool{}

	dr := dispatchResult{
		Item: dispatchItem{
			Kind:      "task",
			Task:      &model.Task{ID: "t1"},
			Epoch:     3,
			ExpiresAt: expiresAt,
		},
		Success: false,
		Error:   fmt.Errorf("dispatch wrapped: %w", agent.ErrSubmitConfirmUncertain),
	}

	qh.applyTaskDispatchResult(dr, taskQueues, taskDirty)

	got := taskQueues[queueFile].Queue.Tasks[0]
	if got.Status != model.StatusInProgress {
		t.Errorf("task.Status = %s, want in_progress (uncertain dispatch must NOT bounce back to pending — would FENCING_REJECT the worker's eventual result_write)", got.Status)
	}
	if got.LeaseOwner == nil || *got.LeaseOwner != owner {
		t.Errorf("task.LeaseOwner = %v, want %q (lease MUST be retained to suppress same-task re-dispatch on the next scan)", got.LeaseOwner, owner)
	}
	if got.LeaseExpiresAt == nil || *got.LeaseExpiresAt != expiresAt {
		t.Errorf("task.LeaseExpiresAt = %v, want %q (lease TTL preserved so hasExpiredLeases recovery still triggers if the worker truly didn't receive)", got.LeaseExpiresAt, expiresAt)
	}
	if got.LeaseEpoch != 3 {
		t.Errorf("task.LeaseEpoch = %d, want 3 unchanged", got.LeaseEpoch)
	}
	if !taskDirty[queueFile] {
		t.Error("expected taskDirty[queueFile]=true so the assumed-running state persists across the scan flush")
	}
	if qh.scanExecutor.scanCounters.LeaseReleases != 0 {
		t.Errorf("LeaseReleases = %d, want 0 (lease retained, not released)", qh.scanExecutor.scanCounters.LeaseReleases)
	}
	if qh.scanExecutor.scanCounters.TasksDispatched != 0 {
		t.Errorf("TasksDispatched = %d, want 0 (assumed dispatches use the separate TasksDispatchedUncertain counter)", qh.scanExecutor.scanCounters.TasksDispatched)
	}
	if qh.scanExecutor.scanCounters.TasksDispatchedUncertain != 1 {
		t.Errorf("TasksDispatchedUncertain = %d, want 1", qh.scanExecutor.scanCounters.TasksDispatchedUncertain)
	}
}

// TestApplyTaskDispatchResult_PublishPending_PreservesRetryBudget was
// removed in the 2026-05-01 dispatch-loop fix. The publish guard it
// covered (ErrRunOnMainBeforePublish) was retired because the gate
// produced a self-deadlocking dispatch loop. See
// internal/daemon/dispatch/validate_run_on_main.go for the rationale.

func TestApplyTaskDispatchResult_EpochMismatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	expiresAt := now.Add(5 * time.Minute).Format(time.RFC3339)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}

	queueFile := "/fake/worker1.yaml"
	taskQueues := map[string]*taskQueueEntry{
		queueFile: {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{
						ID:             "t1",
						Status:         model.StatusInProgress,
						LeaseEpoch:     5, // different from dispatch epoch
						LeaseExpiresAt: &expiresAt,
					},
				},
			},
		},
	}
	taskDirty := map[string]bool{}

	dr := dispatchResult{
		Item: dispatchItem{
			Kind:      "task",
			Task:      &model.Task{ID: "t1"},
			Epoch:     3, // stale epoch
			ExpiresAt: expiresAt,
		},
		Success: true,
	}

	qh.applyTaskDispatchResult(dr, taskQueues, taskDirty)

	// Stale result should be skipped — dirty should not be set
	if taskDirty[queueFile] {
		t.Error("expected taskDirty to be false for stale epoch")
	}
	if qh.scanExecutor.scanCounters.TasksDispatched != 0 {
		t.Errorf("TasksDispatched = %d, want 0 for stale result", qh.scanExecutor.scanCounters.TasksDispatched)
	}
}

// TestApplyTaskDispatchResult_DestructiveContent_Terminates pins down the
// non-retryable termination path. validateRunOnMainContent on the dispatch
// side returns ErrDestructiveContentRejected for run_on_main/integration
// tasks containing destructive shell snippets (`git push`, `rm -rf`, etc.);
// without the special-case below, the queue's lease-release fallback would
// flip the task back to pending and the next scan cycle would re-dispatch
// it forever. The terminal Failed transition stops the loop and surfaces
// the violation in operator logs.
func TestApplyTaskDispatchResult_DestructiveContent_Terminates(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	expiresAt := now.Add(5 * time.Minute).Format(time.RFC3339)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}

	owner := "worker1"
	queueFile := "/fake/worker1.yaml"
	taskQueues := map[string]*taskQueueEntry{
		queueFile: {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{
						ID:             "t1",
						CommandID:      "cmd1",
						Status:         model.StatusInProgress,
						LeaseEpoch:     3,
						LeaseOwner:     &owner,
						LeaseExpiresAt: &expiresAt,
						RunOnMain:      true,
					},
				},
			},
		},
	}
	taskDirty := map[string]bool{}

	dr := dispatchResult{
		Item: dispatchItem{
			Kind:      "task",
			Task:      &model.Task{ID: "t1"},
			Epoch:     3,
			ExpiresAt: expiresAt,
		},
		Success: false,
		Error:   fmt.Errorf("dispatch wrapped: %w", dispatch.ErrDestructiveContentRejected),
	}

	qh.applyTaskDispatchResult(dr, taskQueues, taskDirty)

	got := taskQueues[queueFile].Queue.Tasks[0]
	if got.Status != model.StatusFailed {
		t.Errorf("task.Status = %s, want failed (terminal — must not be re-dispatched)", got.Status)
	}
	if got.LeaseOwner != nil {
		t.Errorf("task.LeaseOwner = %v, want nil after terminal failure", got.LeaseOwner)
	}
	if got.LeaseExpiresAt != nil {
		t.Errorf("task.LeaseExpiresAt = %v, want nil after terminal failure", got.LeaseExpiresAt)
	}
	if !taskDirty[queueFile] {
		t.Error("expected taskDirty[queueFile]=true so the queue write persists the terminal status")
	}
	if qh.scanExecutor.scanCounters.LeaseReleases != 1 {
		t.Errorf("LeaseReleases = %d, want 1 (counter shared with terminal failure)",
			qh.scanExecutor.scanCounters.LeaseReleases)
	}
}

// TestApplyTaskDispatchResult_DestructiveContent_WritesSyntheticResult ensures
// the synthetic failed result file write closes the queue→result→state loop
// for run_on_main / run_on_integration destructive-content rejections.
// Without it, queue is failed but result file is empty, so R2ResultState
// (which only syncs from results) cannot move TaskStates off in_progress —
// leaving the command state file permanently out of sync with the queue.
func TestApplyTaskDispatchResult_DestructiveContent_WritesSyntheticResult(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)
	expiresAt := now.Add(5 * time.Minute).Format(time.RFC3339)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}

	owner := "worker1"
	// queueFile only needs to provide the worker basename — the synthetic
	// write derives the worker ID via filepath.Base + TrimSuffix.
	queueFile := filepath.Join(maestroDir, "queue", "worker1.yaml")
	taskQueues := map[string]*taskQueueEntry{
		queueFile: {
			Queue: model.TaskQueue{
				Tasks: []model.Task{
					{
						ID:             "t1",
						CommandID:      "cmd1",
						Status:         model.StatusInProgress,
						LeaseEpoch:     3,
						LeaseOwner:     &owner,
						LeaseExpiresAt: &expiresAt,
						RunOnMain:      true,
					},
				},
			},
		},
	}
	taskDirty := map[string]bool{}

	dr := dispatchResult{
		Item: dispatchItem{
			Kind:      "task",
			Task:      &model.Task{ID: "t1"},
			Epoch:     3,
			ExpiresAt: expiresAt,
		},
		Success: false,
		Error:   fmt.Errorf("dispatch wrapped: %w", dispatch.ErrDestructiveContentRejected),
	}

	qh.applyTaskDispatchResult(dr, taskQueues, taskDirty)

	// Read the synthetic result file written by the helper.
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read synthetic result file: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatalf("unmarshal synthetic result file: %v", err)
	}
	if len(rf.Results) != 1 {
		t.Fatalf("synthetic results count = %d, want 1", len(rf.Results))
	}
	got := rf.Results[0]
	if got.TaskID != "t1" {
		t.Errorf("TaskID = %q, want t1", got.TaskID)
	}
	if got.CommandID != "cmd1" {
		t.Errorf("CommandID = %q, want cmd1", got.CommandID)
	}
	if got.Status != model.StatusFailed {
		t.Errorf("Status = %s, want failed", got.Status)
	}
	if got.PartialChangesPossible {
		t.Errorf("PartialChangesPossible = true; destructive rejection never started a Worker so no partial changes are possible")
	}
	if got.RetrySafe {
		t.Errorf("RetrySafe = true; policy violation must not be auto-retried")
	}
	if got.ID == "" {
		t.Errorf("synthetic result ID empty; reconcilers need a stable id")
	}
}

// --- applySignalResults tests ---

func TestApplySignalResults_SuccessfulDelivery(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}

	sq := &model.PlannerSignalQueue{
		Signals: []model.PlannerSignal{
			{Kind: "awaiting_fill", CommandID: "cmd1", PhaseID: "p1", CreatedAt: "2025-01-01T00:00:00Z", UpdatedAt: "2025-01-01T00:00:00Z"},
			{Kind: "other_signal", CommandID: "cmd2", PhaseID: "p2", CreatedAt: "2025-01-01T00:00:00Z", UpdatedAt: "2025-01-01T00:00:00Z"},
		},
	}
	dirty := false

	results := []signalDeliveryResult{
		{
			Item:    signalDeliveryItem{CommandID: "cmd1", PhaseID: "p1", Kind: "awaiting_fill", Message: "fill"},
			Success: true,
		},
	}

	qh.applySignalResults(results, sq, &dirty)

	if !dirty {
		t.Error("expected dirty=true after signal delivery")
	}
	// Successfully delivered signal should be removed; other signal retained
	if len(sq.Signals) != 1 {
		t.Fatalf("expected 1 signal remaining, got %d", len(sq.Signals))
	}
	if sq.Signals[0].Kind != "other_signal" {
		t.Errorf("remaining signal kind = %s, want other_signal", sq.Signals[0].Kind)
	}
	if qh.scanExecutor.scanCounters.SignalDeliveries != 1 {
		t.Errorf("SignalDeliveries = %d, want 1", qh.scanExecutor.scanCounters.SignalDeliveries)
	}
}

func TestApplySignalResults_FailedDeliveryRetry(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300, ScanIntervalSec: 10},
		Retry:   model.RetryConfig{SignalDispatch: 5},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}

	sq := &model.PlannerSignalQueue{
		Signals: []model.PlannerSignal{
			{Kind: "awaiting_fill", CommandID: "cmd1", PhaseID: "p1", Attempts: 0, CreatedAt: "2025-01-01T00:00:00Z", UpdatedAt: "2025-01-01T00:00:00Z"},
		},
	}
	dirty := false

	results := []signalDeliveryResult{
		{
			Item:    signalDeliveryItem{CommandID: "cmd1", PhaseID: "p1", Kind: "awaiting_fill"},
			Success: false,
			Error:   fmt.Errorf("planner unreachable"),
		},
	}

	qh.applySignalResults(results, sq, &dirty)

	if !dirty {
		t.Error("expected dirty=true after failed delivery")
	}
	// Failed signal should be retained for retry
	if len(sq.Signals) != 1 {
		t.Fatalf("expected 1 signal retained for retry, got %d", len(sq.Signals))
	}
	sig := sq.Signals[0]
	if sig.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", sig.Attempts)
	}
	if sig.LastError == nil {
		t.Error("expected LastError to be set")
	}
	if sig.NextAttemptAt == nil {
		t.Error("expected NextAttemptAt to be set for retry backoff")
	}
	if qh.scanExecutor.scanCounters.SignalRetries != 1 {
		t.Errorf("SignalRetries = %d, want 1", qh.scanExecutor.scanCounters.SignalRetries)
	}
}

func TestApplySignalResults_DeadLetter(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300, ScanIntervalSec: 10},
		Retry:   model.RetryConfig{SignalDispatch: 3}, // max 3 attempts
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}

	sq := &model.PlannerSignalQueue{
		Signals: []model.PlannerSignal{
			{Kind: "awaiting_fill", CommandID: "cmd1", PhaseID: "p1", Attempts: 2, CreatedAt: "2025-01-01T00:00:00Z", UpdatedAt: "2025-01-01T00:00:00Z"},
		},
	}
	dirty := false

	results := []signalDeliveryResult{
		{
			Item:    signalDeliveryItem{CommandID: "cmd1", PhaseID: "p1", Kind: "awaiting_fill"},
			Success: false,
			Error:   fmt.Errorf("still failing"),
		},
	}

	qh.applySignalResults(results, sq, &dirty)

	if !dirty {
		t.Error("expected dirty=true after dead letter")
	}
	// Signal at max attempts should be removed (dead-lettered)
	if len(sq.Signals) != 0 {
		t.Fatalf("expected 0 signals (dead-lettered), got %d", len(sq.Signals))
	}
	if qh.scanExecutor.scanCounters.SignalDeadLetters != 1 {
		t.Errorf("SignalDeadLetters = %d, want 1", qh.scanExecutor.scanCounters.SignalDeadLetters)
	}
}

func TestApplySignalResults_NoResults(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}

	sq := &model.PlannerSignalQueue{
		Signals: []model.PlannerSignal{
			{Kind: "awaiting_fill", CommandID: "cmd1", PhaseID: "p1"},
		},
	}
	dirty := false

	qh.applySignalResults(nil, sq, &dirty)

	// No results means all signals should be retained
	if len(sq.Signals) != 1 {
		t.Fatalf("expected 1 signal retained, got %d", len(sq.Signals))
	}
	if dirty {
		t.Error("expected dirty=false with no results")
	}
}

// --- recoverExpiredNotificationLeases tests ---

func TestRecoverExpiredNotificationLeases_ExpiresAndReleases(t *testing.T) {
	t.Parallel()
	// Use real time so that the lease manager's RealClock agrees with our timestamps.
	now := time.Now().UTC()
	expired := now.Add(-1 * time.Minute).Format(time.RFC3339)
	valid := now.Add(5 * time.Minute).Format(time.RFC3339)
	owner := "daemon"

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	nq := &model.NotificationQueue{
		Notifications: []model.Notification{
			{
				ID:             "ntf1",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expired, // expired
				LeaseEpoch:     1,
			},
			{
				ID:             "ntf2",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &valid, // still valid
				LeaseEpoch:     1,
			},
			{
				ID:     "ntf3",
				Status: model.StatusPending, // not in_progress
			},
		},
	}
	dirty := false

	qh.recoverExpiredNotificationLeases(nq, &dirty)

	if !dirty {
		t.Error("expected dirty=true when expired notifications exist")
	}
	// ntf1 should be released (pending)
	if nq.Notifications[0].Status != model.StatusPending {
		t.Errorf("ntf1 status = %s, want pending", nq.Notifications[0].Status)
	}
	// ntf2 should remain in_progress
	if nq.Notifications[1].Status != model.StatusInProgress {
		t.Errorf("ntf2 status = %s, want in_progress", nq.Notifications[1].Status)
	}
}

func TestRecoverExpiredNotificationLeases_NoneExpired(t *testing.T) {
	t.Parallel()
	// Use real time so that the lease manager's RealClock agrees with our timestamps.
	now := time.Now().UTC()
	valid := now.Add(5 * time.Minute).Format(time.RFC3339)
	owner := "daemon"

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)

	nq := &model.NotificationQueue{
		Notifications: []model.Notification{
			{
				ID:             "ntf1",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &valid,
				LeaseEpoch:     1,
			},
		},
	}
	dirty := false

	qh.recoverExpiredNotificationLeases(nq, &dirty)

	if dirty {
		t.Error("expected dirty=false when no notifications expired")
	}
}

func TestApplyBusyCheckCore_MaxTimeoutBeforeGraceLimit(t *testing.T) {
	t.Parallel()

	// Verify that max_in_progress_min timeout takes precedence over the grace limit.
	// If updatedAt is old enough for max_in_progress_min to trigger, we should
	// release due to max timeout, not grace limit.
	now := time.Date(2025, 1, 1, 2, 0, 0, 0, time.UTC)
	expiresAt := now.Add(-1 * time.Second).Format(time.RFC3339)
	// updatedAt = now - 90min. maxInProgressMin=60 => timed out
	updatedAt := now.Add(-90 * time.Minute).Format(time.RFC3339)

	maestroDir := testutil.SetupDirFixPerms(t)
	cfg := model.Config{
		Agents: model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{
			DispatchLeaseSec: 300,
			ScanIntervalSec:  10,
			MaxInProgressMin: ptr.Int(60),
		},
		Queue: model.QueueConfig{PriorityAgingSec: 60},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.clock = &fixedClock{now: now}
	qh.scanExecutor.scanCounters = metrics.ScanCounters{}

	released := false
	dirty := false

	bc := busyCheckResult{
		Item: busyCheckItem{
			Kind:      "task",
			EntryID:   "task_002",
			AgentID:   "worker1",
			Epoch:     5,
			UpdatedAt: updatedAt,
			ExpiresAt: expiresAt,
		},
		Busy:      false,
		Undecided: true,
	}

	ops := busyCheckOps{
		kind:       "task",
		ownerLabel: "worker=worker1",
		releaseLease: func() error {
			released = true
			return nil
		},
		extendLease: func() error { return nil },
		extendGrace: func(ttl time.Duration) error {
			t.Error("extendGrace should not be called when max_in_progress_min is exceeded")
			return nil
		},
		markDirty: func() { dirty = true },
	}

	qh.applyBusyCheckCore(bc, "task_002", model.StatusInProgress, 5, &expiresAt, ops)

	if !released {
		t.Error("expected release due to max_in_progress_min timeout")
	}
	if !dirty {
		t.Error("expected dirty to be set")
	}
	if qh.scanExecutor.scanCounters.LeaseReleases != 1 {
		t.Errorf("LeaseReleases = %d, want 1", qh.scanExecutor.scanCounters.LeaseReleases)
	}
}
