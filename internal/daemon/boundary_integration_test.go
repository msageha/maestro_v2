package daemon

import (
	"fmt"
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

// newBoundaryTestDaemon creates a fully-wired integration daemon for boundary tests.
// Uses the same pattern as newIntegrationDaemon but with configurable options.
func newBoundaryTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := newTestDaemon(t)
	lockMap := lock.NewMutexMap()
	reader := &integrationStateReader{maestroDir: d.maestroDir, lockMap: lockMap}
	d.handler = NewQueueHandler(d.maestroDir, d.config, lockMap, d.logger, d.logLevel)
	d.handler.SetStateReader(reader)
	d.handler.SetCanComplete(testCanComplete)
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	})
	d.handler.SetBusyChecker(func(string) bool { return false })
	for _, sub := range []string{"dead_letters", "quarantine", "state"} {
		os.MkdirAll(filepath.Join(d.maestroDir, sub), 0755)
	}
	t.Cleanup(func() {
		d.handler.scanRunMu.Lock()
		d.handler.scanRunMu.Unlock()
		os.RemoveAll(d.maestroDir)
	})
	return d
}

// =============================================================================
// Command Dispatch Guard — Boundary Tests
// =============================================================================

// TestGuard_AllTerminalCommands_PendingDispatched verifies that when all
// existing commands are terminal, a new pending command IS dispatched.
func TestGuard_AllTerminalCommands_PendingDispatched(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{ID: "cmd_done1", Content: "completed cmd", Status: model.StatusCompleted, CreatedAt: now, UpdatedAt: now},
			{ID: "cmd_done2", Content: "failed cmd", Status: model.StatusFailed, CreatedAt: now, UpdatedAt: now},
			{ID: "cmd_done3", Content: "cancelled cmd", Status: model.StatusCancelled, CreatedAt: now, UpdatedAt: now},
			{ID: "cmd_new", Content: "new pending", Priority: 1, Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
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

	var newCmd *model.Command
	for i := range result.Commands {
		if result.Commands[i].ID == "cmd_new" {
			newCmd = &result.Commands[i]
		}
	}
	if newCmd == nil {
		t.Fatal("cmd_new not found")
	}
	if newCmd.Status != model.StatusInProgress {
		t.Errorf("cmd_new: got %s, want in_progress (should dispatch when all others terminal)", newCmd.Status)
	}
}

// TestGuard_EmptyQueue_NoPanic verifies empty queue doesn't cause issues.
func TestGuard_EmptyQueue_NoPanic(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	cq := model.CommandQueue{SchemaVersion: 1, FileType: "command_queue"}
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	yamlutil.AtomicWrite(plannerPath, cq)

	// Should not panic
	qh.PeriodicScan()
}

// TestGuard_MultipleInProgressCommands_NoneDispatched verifies the at-most-one
// guard: even if the queue is corrupted with 2 in_progress commands, no new
// dispatches occur.
func TestGuard_MultipleInProgressCommands_NoneDispatched(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	owner := qh.leaseOwnerID()
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{ID: "cmd_ip1", Content: "ip1", Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &future, LeaseEpoch: 1, CreatedAt: now, UpdatedAt: now},
			{ID: "cmd_ip2", Content: "ip2", Status: model.StatusInProgress, LeaseOwner: &owner, LeaseExpiresAt: &future, LeaseEpoch: 1, CreatedAt: now, UpdatedAt: now},
			{ID: "cmd_pending", Content: "waiting", Status: model.StatusPending, Priority: 1, CreatedAt: now, UpdatedAt: now},
		},
	}
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	yamlutil.AtomicWrite(plannerPath, cq)

	qh.PeriodicScan()

	data, _ := os.ReadFile(plannerPath)
	var result model.CommandQueue
	parseYAML(data, &result)

	for _, cmd := range result.Commands {
		if cmd.ID == "cmd_pending" && cmd.Status != model.StatusPending {
			t.Errorf("cmd_pending: got %s, want pending (blocked by in_progress guard)", cmd.Status)
		}
	}
}

// TestGuard_DeadLetterCommandNotBlockingDispatch verifies that dead_letter
// status does not block dispatch (it's terminal).
func TestGuard_DeadLetterCommandNotBlockingDispatch(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{ID: "cmd_dl", Content: "dead lettered", Status: model.StatusDeadLetter, CreatedAt: now, UpdatedAt: now},
			{ID: "cmd_new", Content: "new", Priority: 1, Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	yamlutil.AtomicWrite(plannerPath, cq)

	qh.PeriodicScan()

	data, _ := os.ReadFile(plannerPath)
	var result model.CommandQueue
	parseYAML(data, &result)

	for _, cmd := range result.Commands {
		if cmd.ID == "cmd_new" && cmd.Status != model.StatusInProgress {
			t.Errorf("cmd_new: got %s, want in_progress (dead_letter should not block)", cmd.Status)
		}
	}
}

// =============================================================================
// Command Lease Auto-Extend — Boundary Tests
// =============================================================================

// TestCommandLeaseAutoExtend_ExactMaxTimeout verifies that a command at exactly
// the max_in_progress_min boundary is released (not extended).
func TestCommandLeaseAutoExtend_ExactMaxTimeout(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)
	qh.config.Watcher.MaxInProgressMin = 30

	owner := qh.leaseOwnerID()
	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	// UpdatedAt exactly at max_in_progress_min ago
	updatedAt := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID: "cmd_exact", Content: "exact timeout", Priority: 1,
				Status: model.StatusInProgress, LeaseOwner: &owner,
				LeaseExpiresAt: &expired, LeaseEpoch: 3,
				CreatedAt: updatedAt, UpdatedAt: updatedAt,
			},
		},
	}
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	yamlutil.AtomicWrite(plannerPath, cq)

	qh.PeriodicScan()

	data, _ := os.ReadFile(plannerPath)
	var result model.CommandQueue
	parseYAML(data, &result)

	cmd := result.Commands[0]
	// At exact boundary, time.Since(updatedAt) >= 30m should be true → release
	if cmd.Status != model.StatusPending {
		t.Errorf("cmd_exact: got %s, want pending (released at max_in_progress_min boundary)", cmd.Status)
	}
}

// TestCommandLeaseAutoExtend_JustBeforeMaxTimeout verifies that a command
// just before max_in_progress_min is auto-extended.
func TestCommandLeaseAutoExtend_JustBeforeMaxTimeout(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)
	qh.config.Watcher.MaxInProgressMin = 30

	owner := qh.leaseOwnerID()
	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	// UpdatedAt 29 minutes ago (1 minute before threshold)
	updatedAt := time.Now().Add(-29 * time.Minute).UTC().Format(time.RFC3339)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID: "cmd_extend", Content: "still within limit", Priority: 1,
				Status: model.StatusInProgress, LeaseOwner: &owner,
				LeaseExpiresAt: &expired, LeaseEpoch: 2,
				CreatedAt: updatedAt, UpdatedAt: updatedAt,
			},
		},
	}
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	yamlutil.AtomicWrite(plannerPath, cq)

	qh.PeriodicScan()

	data, _ := os.ReadFile(plannerPath)
	var result model.CommandQueue
	parseYAML(data, &result)

	cmd := result.Commands[0]
	if cmd.Status != model.StatusInProgress {
		t.Errorf("cmd_extend: got %s, want in_progress (auto-extended before max timeout)", cmd.Status)
	}
	if cmd.LeaseExpiresAt == nil {
		t.Fatal("lease_expires_at should be set after auto-extend")
	}
	newExpiry, _ := time.Parse(time.RFC3339, *cmd.LeaseExpiresAt)
	if !newExpiry.After(time.Now()) {
		t.Error("lease_expires_at should be in the future after auto-extend")
	}
}

// TestCommandLeaseAutoExtend_NilLeaseExpiresAt verifies that malformed entries
// (nil lease_expires_at) are repaired via auto-extend.
func TestCommandLeaseAutoExtend_NilLeaseExpiresAt(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	owner := qh.leaseOwnerID()
	recentUpdate := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID: "cmd_nil_lease", Content: "malformed", Priority: 1,
				Status: model.StatusInProgress, LeaseOwner: &owner,
				LeaseExpiresAt: nil, // malformed
				LeaseEpoch:     1,
				CreatedAt:      recentUpdate, UpdatedAt: recentUpdate,
			},
		},
	}
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	yamlutil.AtomicWrite(plannerPath, cq)

	qh.PeriodicScan()

	data, _ := os.ReadFile(plannerPath)
	var result model.CommandQueue
	parseYAML(data, &result)

	cmd := result.Commands[0]
	// Malformed nil lease_expires_at → IsLeaseExpired returns true → auto-extend
	if cmd.Status != model.StatusInProgress {
		t.Errorf("cmd_nil_lease: got %s, want in_progress (repaired by auto-extend)", cmd.Status)
	}
	if cmd.LeaseExpiresAt == nil {
		t.Error("lease_expires_at should be repaired (non-nil)")
	}
}

// TestCommandLeaseAutoExtend_DefaultMaxInProgressMin verifies the default
// max_in_progress_min (60) is used when config value is 0.
func TestCommandLeaseAutoExtend_DefaultMaxInProgressMin(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)
	qh.config.Watcher.MaxInProgressMin = 0 // should default to 60

	owner := qh.leaseOwnerID()
	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	// 59 minutes ago — within default 60m limit
	updatedAt := time.Now().Add(-59 * time.Minute).UTC().Format(time.RFC3339)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID: "cmd_default", Content: "default timeout", Priority: 1,
				Status: model.StatusInProgress, LeaseOwner: &owner,
				LeaseExpiresAt: &expired, LeaseEpoch: 1,
				CreatedAt: updatedAt, UpdatedAt: updatedAt,
			},
		},
	}
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	yamlutil.AtomicWrite(plannerPath, cq)

	qh.PeriodicScan()

	data, _ := os.ReadFile(plannerPath)
	var result model.CommandQueue
	parseYAML(data, &result)

	if result.Commands[0].Status != model.StatusInProgress {
		t.Errorf("got %s, want in_progress (within default 60m max)", result.Commands[0].Status)
	}
}

// =============================================================================
// Task Lease Expiry — Boundary Tests
// =============================================================================

// TestTaskLeaseExpiry_NilLeaseExpiresAt verifies malformed task entries
// (nil lease_expires_at) are released immediately in Phase A.
func TestTaskLeaseExpiry_NilLeaseExpiresAt(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	owner := "worker1"
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID: "task_nil", CommandID: "cmd_001",
				Status: model.StatusInProgress, LeaseOwner: &owner,
				LeaseExpiresAt: nil, // malformed
				LeaseEpoch:     1,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	workerPath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	yamlutil.AtomicWrite(workerPath, tq)

	qh.PeriodicScan()

	data, _ := os.ReadFile(workerPath)
	var result model.TaskQueue
	parseYAML(data, &result)

	task := result.Tasks[0]
	if task.Status != model.StatusPending {
		t.Errorf("got %s, want pending (nil lease_expires_at → immediate release)", task.Status)
	}
	if task.LeaseOwner != nil {
		t.Error("lease_owner should be nil after release")
	}
}

// TestTaskLeaseExpiry_BusyAgent_MaxTimeout verifies that even a busy agent
// is released when max_in_progress_min is exceeded.
func TestTaskLeaseExpiry_BusyAgent_MaxTimeout(t *testing.T) {
	d := newBoundaryTestDaemon(t)
	d.config.Watcher.MaxInProgressMin = 30
	// Recreate handler with updated config
	lockMap := d.handler.lockMap
	reader := &integrationStateReader{maestroDir: d.maestroDir, lockMap: lockMap}
	d.handler = NewQueueHandler(d.maestroDir, d.config, lockMap, d.logger, d.logLevel)
	d.handler.SetStateReader(reader)
	d.handler.SetCanComplete(testCanComplete)
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	})
	d.handler.SetBusyChecker(func(string) bool { return true }) // always busy

	owner := "worker1"
	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	// Updated 31 minutes ago → exceeds max_in_progress_min
	oldUpdate := time.Now().Add(-31 * time.Minute).UTC().Format(time.RFC3339)

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID: "task_busy_max", CommandID: "cmd_001",
				Status: model.StatusInProgress, LeaseOwner: &owner,
				LeaseExpiresAt: &expired, LeaseEpoch: 2,
				CreatedAt: oldUpdate, UpdatedAt: oldUpdate,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	d.handler.PeriodicScan()

	tqAfter := readTaskQueue(t, d, "worker1")
	if tqAfter.Tasks[0].Status != model.StatusPending {
		t.Errorf("got %s, want pending (busy but max_in_progress exceeded)", tqAfter.Tasks[0].Status)
	}
}

// TestTaskLeaseExpiry_BusyAgent_WithinLimit verifies that a busy agent within
// max_in_progress_min gets its lease extended.
func TestTaskLeaseExpiry_BusyAgent_WithinLimit(t *testing.T) {
	d := newBoundaryTestDaemon(t)
	d.config.Watcher.MaxInProgressMin = 60
	lockMap := d.handler.lockMap
	reader := &integrationStateReader{maestroDir: d.maestroDir, lockMap: lockMap}
	d.handler = NewQueueHandler(d.maestroDir, d.config, lockMap, d.logger, d.logLevel)
	d.handler.SetStateReader(reader)
	d.handler.SetCanComplete(testCanComplete)
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	})
	d.handler.SetBusyChecker(func(string) bool { return true })

	owner := "worker1"
	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	recentUpdate := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID: "task_busy_ok", CommandID: "cmd_001",
				Status: model.StatusInProgress, LeaseOwner: &owner,
				LeaseExpiresAt: &expired, LeaseEpoch: 1,
				CreatedAt: recentUpdate, UpdatedAt: recentUpdate,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	d.handler.PeriodicScan()

	tqAfter := readTaskQueue(t, d, "worker1")
	task := tqAfter.Tasks[0]
	if task.Status != model.StatusInProgress {
		t.Errorf("got %s, want in_progress (busy agent within limit → extend)", task.Status)
	}
	if task.LeaseExpiresAt == nil {
		t.Fatal("lease_expires_at should be set after extend")
	}
	newExpiry, _ := time.Parse(time.RFC3339, *task.LeaseExpiresAt)
	if !newExpiry.After(time.Now()) {
		t.Error("lease_expires_at should be in the future after extend")
	}
}

// =============================================================================
// Dispatch Error Handling — Asymmetric Command vs Task Behavior
// =============================================================================

// TestTaskDispatchError_LeaseReleased verifies that when a TASK dispatch fails,
// the lease IS released (unlike commands where it is retained).
func TestTaskDispatchError_LeaseReleased(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Mock executor that fails dispatch
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{
			Success:   false,
			Error:     fmt.Errorf("worker tmux pane not found"),
			Retryable: true,
		}}, nil
	})

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID: "task_fail", CommandID: "cmd_001",
				Purpose: "test", Content: "work",
				Priority: 1, Status: model.StatusPending,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	workerPath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	yamlutil.AtomicWrite(workerPath, tq)

	qh.PeriodicScan()

	data, _ := os.ReadFile(workerPath)
	var result model.TaskQueue
	parseYAML(data, &result)

	task := result.Tasks[0]
	// Task dispatch failure → lease released → back to pending
	if task.Status != model.StatusPending {
		t.Errorf("task_fail: got %s, want pending (task lease released on dispatch error)", task.Status)
	}
	if task.LeaseOwner != nil {
		t.Error("lease_owner should be nil (released)")
	}
}

// TestCommandDispatchError_SecondScan_StaysInProgress verifies that after a
// command dispatch fails, the next scan still keeps it in_progress (auto-extend).
func TestCommandDispatchError_SecondScan_StaysInProgress(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	dispatchCount := 0
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		dispatchCount++
		return &mockExecutor{result: agent.ExecResult{
			Success:   false,
			Error:     fmt.Errorf("planner not responding"),
			Retryable: true,
		}}, nil
	})

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID: "cmd_retry", Content: "retry test", Priority: 1,
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

	// First scan: dispatch fails → lease retained
	qh.PeriodicScan()

	data, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner queue after scan 1: %v", err)
	}
	var after1 model.CommandQueue
	if err := parseYAML(data, &after1); err != nil {
		t.Fatalf("parse planner queue after scan 1: %v", err)
	}
	if after1.Commands[0].Status != model.StatusInProgress {
		t.Fatalf("after scan 1: got %s, want in_progress", after1.Commands[0].Status)
	}

	// dispatchCount should be 1 after first scan (dispatch attempted once)
	if dispatchCount != 1 {
		t.Errorf("after scan 1: dispatchCount=%d, want 1", dispatchCount)
	}

	// Second scan: command still in_progress → guard blocks re-dispatch, lease auto-extended if expired
	qh.PeriodicScan()

	data, err = os.ReadFile(plannerPath)
	if err != nil {
		t.Fatalf("read planner queue after scan 2: %v", err)
	}
	var after2 model.CommandQueue
	if err := parseYAML(data, &after2); err != nil {
		t.Fatalf("parse planner queue after scan 2: %v", err)
	}
	if after2.Commands[0].Status != model.StatusInProgress {
		t.Errorf("after scan 2: got %s, want in_progress (still retained)", after2.Commands[0].Status)
	}

	// dispatchCount must still be 1: the at-most-one guard prevents re-dispatch
	if dispatchCount != 1 {
		t.Errorf("after scan 2: dispatchCount=%d, want 1 (guard should prevent re-dispatch)", dispatchCount)
	}
}

// =============================================================================
// Epoch Fencing — Phase C Staleness Detection
// =============================================================================

// TestDispatchResult_NormalPath verifies the normal dispatch→result→completed flow.
func TestDispatchResult_NormalPath(t *testing.T) {
	d := newBoundaryTestDaemon(t)

	now := time.Now().UTC().Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID: "task_fence", CommandID: "cmd_fence",
				Purpose: "fencing", Content: "test", Priority: 1,
				Status:    model.StatusPending,
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	// Create the command state file so IsSystemCommitReady can load it
	stateDir := filepath.Join(d.maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID:  "cmd_fence",
		PlanStatus: model.PlanStatusSealed,
		TaskStates: map[string]model.Status{
			"task_fence": model.StatusPending,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_fence.yaml"), state); err != nil {
		t.Fatalf("write command state: %v", err)
	}

	// First scan dispatches the task
	d.handler.PeriodicScan()

	tqAfter := readTaskQueue(t, d, "worker1")
	if tqAfter.Tasks[0].Status != model.StatusInProgress {
		t.Fatalf("expected in_progress after dispatch, got %s", tqAfter.Tasks[0].Status)
	}
	epoch1 := tqAfter.Tasks[0].LeaseEpoch

	// Write result with correct epoch
	writeResult(t, d, "worker1", "task_fence", "cmd_fence", "completed", "done", epoch1)

	// Verify task is now completed
	tqFinal := readTaskQueue(t, d, "worker1")
	if tqFinal.Tasks[0].Status != model.StatusCompleted {
		t.Errorf("task should be completed, got %s", tqFinal.Tasks[0].Status)
	}
}

// TestEpochFencing_StaleDispatchResult verifies that applyTaskDispatchResult
// rejects a dispatch result when epoch/lease fields don't match (stale fencing).
func TestEpochFencing_StaleDispatchResult(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	owner := qh.leaseOwnerID()
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)

	taskQueues := map[string]*taskQueueEntry{
		"worker1.yaml": {
			Queue: model.TaskQueue{
				SchemaVersion: 1,
				FileType:      "task_queue",
				Tasks: []model.Task{
					{
						ID: "task_stale", CommandID: "cmd_stale",
						Status: model.StatusInProgress, LeaseOwner: &owner,
						LeaseExpiresAt: &future, LeaseEpoch: 3,
						CreatedAt: now, UpdatedAt: now,
					},
				},
			},
		},
	}
	taskDirty := map[string]bool{}

	// Stale dispatch result: epoch mismatch (result has epoch=1, queue has epoch=3)
	staleResult := dispatchResult{
		Item: dispatchItem{
			Kind:      "task",
			Task:      &model.Task{ID: "task_stale", CommandID: "cmd_stale"},
			WorkerID:  "worker1",
			Epoch:     1, // stale epoch
			ExpiresAt: future,
		},
		Success: true,
	}
	qh.applyTaskDispatchResult(staleResult, taskQueues, taskDirty)

	// Task should NOT be modified (fencing rejected the stale result)
	task := taskQueues["worker1.yaml"].Queue.Tasks[0]
	if task.Status != model.StatusInProgress {
		t.Errorf("task should remain in_progress after stale rejection, got %s", task.Status)
	}
	if taskDirty["worker1.yaml"] {
		t.Error("taskDirty should be false — stale result should not mark queue dirty")
	}
}

// TestEpochFencing_StaleCommandDispatchResult verifies that
// applyCommandDispatchResult rejects stale results for commands.
func TestEpochFencing_StaleCommandDispatchResult(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	owner := qh.leaseOwnerID()
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID: "cmd_stale", Content: "stale test",
				Status: model.StatusInProgress, LeaseOwner: &owner,
				LeaseExpiresAt: &future, LeaseEpoch: 5,
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	dirty := false

	// Stale: epoch mismatch
	staleResult := dispatchResult{
		Item: dispatchItem{
			Kind:      "command",
			Command:   &model.Command{ID: "cmd_stale"},
			Epoch:     2, // stale
			ExpiresAt: future,
		},
		Success: true,
	}
	qh.applyCommandDispatchResult(staleResult, &cq, &dirty)

	if dirty {
		t.Error("dirty should be false — stale result rejected")
	}

	// Stale: lease_expires_at mismatch
	differentExpiry := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	staleResult2 := dispatchResult{
		Item: dispatchItem{
			Kind:      "command",
			Command:   &model.Command{ID: "cmd_stale"},
			Epoch:     5, // correct epoch
			ExpiresAt: differentExpiry,
		},
		Success: true,
	}
	qh.applyCommandDispatchResult(staleResult2, &cq, &dirty)

	if dirty {
		t.Error("dirty should be false — stale result rejected (lease mismatch)")
	}
}

// TestEpochFencing_StaleNotificationDispatchResult verifies stale fencing for notifications.
func TestEpochFencing_StaleNotificationDispatchResult(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	owner := qh.leaseOwnerID()
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)

	nq := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "queue_notification",
		Notifications: []model.Notification{
			{
				ID: "ntf_stale", CommandID: "cmd1", Type: "command_completed",
				Status: model.StatusInProgress, LeaseOwner: &owner,
				LeaseExpiresAt: &future, LeaseEpoch: 4,
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	dirty := false

	staleResult := dispatchResult{
		Item: dispatchItem{
			Kind:         "notification",
			Notification: &model.Notification{ID: "ntf_stale"},
			Epoch:        1, // stale
			ExpiresAt:    future,
		},
		Success: true,
	}
	qh.applyNotificationDispatchResult(staleResult, &nq, &dirty)

	if dirty {
		t.Error("dirty should be false — stale notification result rejected")
	}
	if nq.Notifications[0].Status != model.StatusInProgress {
		t.Error("notification should remain in_progress after stale rejection")
	}
}

// =============================================================================
// Reconciler Boundary Tests — R0 Threshold
// =============================================================================

// TestReconciler_R0_ExactThreshold verifies that R0 DOES trigger when
// planning age is exactly at the threshold boundary (dispatch_lease_sec * 2).
func TestReconciler_R0_ExactThreshold(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	// dispatch_lease_sec=60, threshold=120s. Created exactly 120s ago.
	// time.Since(createdAt).Seconds() >= threshold should trigger.
	createdAt := time.Now().UTC().Add(-120 * time.Second).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0_exact", PlanStatus: model.PlanStatusPlanning,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0_exact.yaml"), state)

	repairs, _ := rec.Reconcile()
	r0 := filterRepairs(repairs, "R0")
	if len(r0) != 1 {
		t.Fatalf("expected 1 R0 repair at exact threshold, got %d", len(r0))
	}
}

// TestReconciler_R0_JustBelowThreshold verifies that R0 does NOT trigger when
// planning age is just below the threshold.
func TestReconciler_R0_JustBelowThreshold(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	// Created 119 seconds ago (1 second before threshold)
	createdAt := time.Now().UTC().Add(-119 * time.Second).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0_below", PlanStatus: model.PlanStatusPlanning,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0_below.yaml"), state)

	repairs, _ := rec.Reconcile()
	r0 := filterRepairs(repairs, "R0")
	if len(r0) != 0 {
		t.Fatalf("expected 0 R0 repairs just below threshold, got %d", len(r0))
	}
}

// TestReconciler_R0_WithWorkerTasks verifies that R0 also cleans up worker tasks.
func TestReconciler_R0_WithWorkerTasks(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	queueDir := filepath.Join(maestroDir, "queue")

	oldTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0_tasks", PlanStatus: model.PlanStatusPlanning,
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0_tasks.yaml"), state)

	// Create worker queues with tasks for this command + another command
	now := time.Now().UTC().Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1, FileType: "queue_task",
		Tasks: []model.Task{
			{ID: "task_r0_1", CommandID: "cmd_r0_tasks", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
			{ID: "task_other", CommandID: "cmd_other", Status: model.StatusPending, CreatedAt: now, UpdatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(queueDir, "worker1.yaml"), tq)

	repairs, _ := rec.Reconcile()
	r0 := filterRepairs(repairs, "R0")
	if len(r0) != 1 {
		t.Fatalf("expected 1 R0 repair, got %d", len(r0))
	}

	// Verify only the target command's tasks were removed
	data, _ := os.ReadFile(filepath.Join(queueDir, "worker1.yaml"))
	var updatedTQ model.TaskQueue
	yamlv3.Unmarshal(data, &updatedTQ)
	if len(updatedTQ.Tasks) != 1 {
		t.Fatalf("expected 1 remaining task (other command), got %d", len(updatedTQ.Tasks))
	}
	if updatedTQ.Tasks[0].ID != "task_other" {
		t.Errorf("expected task_other to remain, got %s", updatedTQ.Tasks[0].ID)
	}
}

// =============================================================================
// Reconciler Boundary Tests — R0b Filling Stuck
// =============================================================================

// TestReconciler_R0b_NoTasks verifies R0b handles empty TaskIDs gracefully.
func TestReconciler_R0b_NoTasks(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	oldTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0b_empty", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "empty-phase", Status: model.PhaseStatusFilling, TaskIDs: nil},
		},
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0b_empty.yaml"), state)

	repairs, _ := rec.Reconcile()
	r0b := filterRepairs(repairs, "R0b")
	if len(r0b) != 1 {
		t.Fatalf("expected 1 R0b repair with empty TaskIDs, got %d", len(r0b))
	}
}

// TestReconciler_R0b_MultiplePhases verifies R0b handles multiple stuck phases.
func TestReconciler_R0b_MultiplePhases(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	oldTime := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r0b_multi", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "phase-1", Status: model.PhaseStatusFilling, TaskIDs: []string{"t1"}},
			{PhaseID: "p2", Name: "phase-2", Status: model.PhaseStatusCompleted}, // not filling
			{PhaseID: "p3", Name: "phase-3", Status: model.PhaseStatusFilling, TaskIDs: []string{"t2", "t3"}},
		},
		CreatedAt: oldTime, UpdatedAt: oldTime,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r0b_multi.yaml"), state)

	repairs, _ := rec.Reconcile()
	r0b := filterRepairs(repairs, "R0b")
	if len(r0b) != 2 {
		t.Fatalf("expected 2 R0b repairs (2 filling phases), got %d", len(r0b))
	}
}

// =============================================================================
// Reconciler Boundary Tests — R4 CanComplete Various Statuses
// =============================================================================

// TestReconciler_R4_CanCompleteReturnsFailed verifies R4 correctly propagates
// failed status from canComplete.
func TestReconciler_R4_CanCompleteReturnsFailed(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetCanComplete(func(state *model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusFailed, nil
	})

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r4_f", CommandID: "cmd_r4_f", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r4_f", PlanStatus: model.PlanStatusSealed,
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r4_f.yaml"), state)

	repairs, _ := rec.Reconcile()
	r4 := filterRepairs(repairs, "R4")
	if len(r4) != 1 {
		t.Fatalf("expected 1 R4 repair, got %d", len(r4))
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r4_f.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.PlanStatus != model.PlanStatusFailed {
		t.Errorf("plan_status: got %s, want failed", updated.PlanStatus)
	}
}

// TestReconciler_R4_CanCompleteReturnsCancelled verifies R4 correctly propagates
// cancelled status from canComplete.
func TestReconciler_R4_CanCompleteReturnsCancelled(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetCanComplete(func(state *model.CommandState) (model.PlanStatus, error) {
		return model.PlanStatusCancelled, nil
	})

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r4_c", CommandID: "cmd_r4_c", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r4_c", PlanStatus: model.PlanStatusSealed,
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r4_c.yaml"), state)

	repairs, _ := rec.Reconcile()
	r4 := filterRepairs(repairs, "R4")
	if len(r4) != 1 {
		t.Fatalf("expected 1 R4 repair, got %d", len(r4))
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r4_c.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.PlanStatus != model.PlanStatusCancelled {
		t.Errorf("plan_status: got %s, want cancelled", updated.PlanStatus)
	}
}

// TestReconciler_R4_AlreadyTerminal_NoRepair verifies R4 skips commands
// whose plan_status is already terminal.
func TestReconciler_R4_AlreadyTerminal_NoRepair(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetCanComplete(func(state *model.CommandState) (model.PlanStatus, error) {
		t.Fatal("canComplete should not be called for already-terminal states")
		return "", nil
	})

	now := time.Now().UTC().Format(time.RFC3339)

	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{ID: "res_r4_term", CommandID: "cmd_r4_term", Status: model.StatusCompleted, CreatedAt: now},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(resultsDir, "planner.yaml"), rf)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r4_term", PlanStatus: model.PlanStatusCompleted, // already terminal
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r4_term.yaml"), state)

	repairs, _ := rec.Reconcile()
	r4 := filterRepairs(repairs, "R4")
	if len(r4) != 0 {
		t.Fatalf("expected 0 R4 repairs for terminal state, got %d", len(r4))
	}
}

// =============================================================================
// Reconciler Boundary Tests — R6 Complex Cascade
// =============================================================================

// TestReconciler_R6_MultiplePhasesTimedOut verifies R6 with multiple timed-out
// phases and their independent downstream cascades.
func TestReconciler_R6_MultiplePhasesTimedOut(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return &mockExecutorR6{}, nil
	})

	pastDeadline := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	// Two independent branches that both time out:
	// p1 (timed_out) → p3 (cancelled)
	// p2 (timed_out) → p4 (cancelled)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_multi", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "branch1", Type: "deferred", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			{PhaseID: "p2", Name: "branch2", Type: "deferred", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			{PhaseID: "p3", Name: "downstream1", Type: "deferred", Status: model.PhaseStatusPending, DependsOnPhases: []string{"branch1"}},
			{PhaseID: "p4", Name: "downstream2", Type: "deferred", Status: model.PhaseStatusPending, DependsOnPhases: []string{"branch2"}},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_multi.yaml"), state)

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, "R6")
	// Should have 4: p1 timed_out, p2 timed_out, p3 cancelled, p4 cancelled
	if len(r6) != 4 {
		t.Fatalf("expected 4 R6 repairs, got %d: %+v", len(r6), r6)
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r6_multi.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)

	for _, phase := range updated.Phases {
		switch phase.Name {
		case "branch1", "branch2":
			if phase.Status != model.PhaseStatusTimedOut {
				t.Errorf("%s: got %s, want timed_out", phase.Name, phase.Status)
			}
		case "downstream1", "downstream2":
			if phase.Status != model.PhaseStatusCancelled {
				t.Errorf("%s: got %s, want cancelled", phase.Name, phase.Status)
			}
		}
	}
}

// TestReconciler_R6_DiamondDependency verifies R6 cascade with diamond dependency:
// p1 → p2, p1 → p3, p2+p3 → p4
func TestReconciler_R6_DiamondDependency(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return &mockExecutorR6{}, nil
	})

	pastDeadline := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_diamond", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "root", Type: "deferred", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			{PhaseID: "p2", Name: "left", Type: "deferred", Status: model.PhaseStatusPending, DependsOnPhases: []string{"root"}},
			{PhaseID: "p3", Name: "right", Type: "deferred", Status: model.PhaseStatusPending, DependsOnPhases: []string{"root"}},
			{PhaseID: "p4", Name: "merge", Type: "deferred", Status: model.PhaseStatusPending, DependsOnPhases: []string{"left", "right"}},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_diamond.yaml"), state)

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, "R6")
	// root timed_out, left cancelled, right cancelled, merge cancelled
	if len(r6) != 4 {
		t.Fatalf("expected 4 R6 repairs (diamond), got %d: %+v", len(r6), r6)
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r6_diamond.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)

	if updated.Phases[0].Status != model.PhaseStatusTimedOut {
		t.Errorf("root: got %s, want timed_out", updated.Phases[0].Status)
	}
	for i := 1; i <= 3; i++ {
		if updated.Phases[i].Status != model.PhaseStatusCancelled {
			t.Errorf("phase %s: got %s, want cancelled", updated.Phases[i].Name, updated.Phases[i].Status)
		}
	}
}

// TestReconciler_R6_ActivePhaseNotCancelled verifies that active phases
// are NOT cancelled by R6 cascade (only pending/awaiting_fill).
func TestReconciler_R6_ActivePhaseNotCancelled(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return &mockExecutorR6{}, nil
	})

	pastDeadline := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_active", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "timeout", Type: "deferred", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: &pastDeadline},
			{PhaseID: "p2", Name: "active_phase", Type: "concrete", Status: model.PhaseStatusActive, DependsOnPhases: []string{"timeout"}},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_active.yaml"), state)

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, "R6")
	// Only p1 timed_out; p2 is active so NOT cancelled
	if len(r6) != 1 {
		t.Fatalf("expected 1 R6 repair (only timeout), got %d: %+v", len(r6), r6)
	}

	data, _ := os.ReadFile(filepath.Join(stateDir, "cmd_r6_active.yaml"))
	var updated model.CommandState
	yamlv3.Unmarshal(data, &updated)
	if updated.Phases[1].Status != model.PhaseStatusActive {
		t.Errorf("active_phase: got %s, want active (not affected by cascade)", updated.Phases[1].Status)
	}
}

// TestReconciler_R6_NoDeadline_NoTimeout verifies that awaiting_fill phases
// without fill_deadline_at are NOT timed out.
func TestReconciler_R6_NoDeadline_NoTimeout(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)

	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)

	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_r6_nodeadline", PlanStatus: model.PlanStatusSealed,
		Phases: []model.Phase{
			{PhaseID: "p1", Name: "no_deadline", Type: "deferred", Status: model.PhaseStatusAwaitingFill, FillDeadlineAt: nil},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_nodeadline.yaml"), state)

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, "R6")
	if len(r6) != 0 {
		t.Fatalf("expected 0 R6 repairs (no deadline), got %d", len(r6))
	}
}

// =============================================================================
// Dependency Failure Propagation — Complex DAG
// =============================================================================

// TestDependencyFailure_TransitivePending verifies that when taskA fails,
// taskB (depends on taskA) and taskC (depends on taskB) are both cancelled.
func TestDependencyFailure_TransitivePending(t *testing.T) {
	d := newBoundaryTestDaemon(t)
	commandID := "cmd_dep_trans"
	taskA := "task_dep_a"
	taskB := "task_dep_b"
	taskC := "task_dep_c"

	state := model.CommandState{
		SchemaVersion:   1, FileType: "state_command",
		CommandID:       commandID, PlanStatus: model.PlanStatusSealed,
		RequiredTaskIDs: []string{taskA, taskB, taskC},
		TaskStates:      map[string]model.Status{taskA: model.StatusFailed, taskB: model.StatusPending, taskC: model.StatusPending},
		TaskDependencies: map[string][]string{
			taskB: {taskA},
			taskC: {taskB},
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml"), state)

	tq := model.TaskQueue{
		SchemaVersion: 1, FileType: "queue_task",
		Tasks: []model.Task{
			{ID: taskB, CommandID: commandID, Status: model.StatusPending, BlockedBy: []string{taskA}, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
			{ID: taskC, CommandID: commandID, Status: model.StatusPending, BlockedBy: []string{taskB}, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	// First scan: taskB cancelled (depends on failed taskA)
	d.handler.PeriodicScan()

	tqAfter1 := readTaskQueue(t, d, "worker1")
	taskBStatus := model.StatusPending
	taskCStatus := model.StatusPending
	for _, task := range tqAfter1.Tasks {
		switch task.ID {
		case taskB:
			taskBStatus = task.Status
		case taskC:
			taskCStatus = task.Status
		}
	}
	if taskBStatus != model.StatusCancelled {
		t.Errorf("taskB: got %s, want cancelled", taskBStatus)
	}

	// Second scan: taskC cancelled (depends on now-cancelled taskB)
	d.handler.PeriodicScan()

	tqAfter2 := readTaskQueue(t, d, "worker1")
	for _, task := range tqAfter2.Tasks {
		if task.ID == taskC {
			taskCStatus = task.Status
		}
	}
	if taskCStatus != model.StatusCancelled {
		t.Errorf("taskC: got %s, want cancelled (transitive propagation)", taskCStatus)
	}
}

// TestDependencyFailure_InProgressTaskInterrupted verifies that an in-progress
// task with a failed dependency gets cancelled and an interrupt is deferred.
func TestDependencyFailure_InProgressTaskInterrupted(t *testing.T) {
	d := newBoundaryTestDaemon(t)
	commandID := "cmd_dep_inprog"
	taskA := "task_depip_a"
	taskB := "task_depip_b"

	state := model.CommandState{
		SchemaVersion:   1, FileType: "state_command",
		CommandID:       commandID, PlanStatus: model.PlanStatusSealed,
		RequiredTaskIDs: []string{taskA, taskB},
		TaskStates:      map[string]model.Status{taskA: model.StatusFailed, taskB: model.StatusInProgress},
		TaskDependencies: map[string][]string{
			taskB: {taskA},
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml"), state)

	owner := "worker1"
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1, FileType: "queue_task",
		Tasks: []model.Task{
			{
				ID: taskB, CommandID: commandID, Status: model.StatusInProgress,
				LeaseOwner: &owner, LeaseExpiresAt: &future, LeaseEpoch: 1,
				BlockedBy: []string{taskA},
				CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	d.handler.PeriodicScan()

	tqAfter := readTaskQueue(t, d, "worker1")
	if len(tqAfter.Tasks) == 0 {
		t.Fatal("expected task in queue")
	}
	if tqAfter.Tasks[0].Status != model.StatusCancelled {
		t.Errorf("taskB: got %s, want cancelled (dependency failure)", tqAfter.Tasks[0].Status)
	}
	if tqAfter.Tasks[0].LeaseOwner != nil {
		t.Error("lease should be cleared after cancellation")
	}
}

// =============================================================================
// Signal Processing — Boundary Tests
// =============================================================================

// TestSignalProcessing_BackoffRespected verifies that signals with
// NextAttemptAt in the future are not delivered.
func TestSignalProcessing_BackoffRespected(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	futureAttempt := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	sq := model.PlannerSignalQueue{
		SchemaVersion: 1,
		FileType:      "planner_signal_queue",
		Signals: []model.PlannerSignal{
			{
				Kind: "awaiting_fill", CommandID: "cmd_sig1", PhaseID: "p1", PhaseName: "phase1",
				Message: "test signal", Attempts: 2, NextAttemptAt: &futureAttempt,
				CreatedAt: now, UpdatedAt: now,
			},
		},
	}
	sigPath := filepath.Join(maestroDir, "queue", "planner_signals.yaml")
	yamlutil.AtomicWrite(sigPath, sq)

	qh.PeriodicScan()

	// Signal should still be in queue (not delivered due to backoff)
	data, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatal("signal file should still exist")
	}
	var updated model.PlannerSignalQueue
	yamlv3.Unmarshal(data, &updated)
	if len(updated.Signals) != 1 {
		t.Fatalf("expected 1 signal retained (backoff), got %d", len(updated.Signals))
	}
	if updated.Signals[0].Attempts != 2 {
		t.Errorf("attempts should be unchanged (2), got %d", updated.Signals[0].Attempts)
	}
}

// TestSignalBackoff_Exponential verifies the exponential backoff computation.
func TestSignalBackoff_Exponential(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)
	qh.config.Watcher.ScanIntervalSec = 60

	tests := []struct {
		attempts int
		wantSec  int
	}{
		{0, 5},  // clamped to 1 → 5*(1<<0) = 5
		{1, 5},  // 5*(1<<0) = 5
		{2, 10}, // 5*(1<<1) = 10
		{3, 20}, // 5*(1<<2) = 20
		{4, 40}, // 5*(1<<3) = 40
		{5, 60}, // 5*(1<<4) = 80, capped at ScanIntervalSec=60
		{10, 60},
	}

	for _, tt := range tests {
		d := qh.computeSignalBackoff(tt.attempts)
		gotSec := int(d.Seconds())
		if gotSec != tt.wantSec {
			t.Errorf("attempts=%d: got %ds, want %ds", tt.attempts, gotSec, tt.wantSec)
		}
	}
}

// =============================================================================
// Notification Dispatch — Boundary Tests
// =============================================================================

// TestNotificationDispatch_InProgressBlocks verifies that a valid in-progress
// notification blocks dispatch of pending notifications.
func TestNotificationDispatch_InProgressBlocks(t *testing.T) {
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
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)
	qh.SetBusyChecker(func(string) bool { return false })

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
				Notified: false, // not yet notified
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
	r5 := filterRepairs(repairs, "R5")
	if len(r5) != 0 {
		t.Fatalf("expected 0 R5 repairs for non-notified result, got %d", len(r5))
	}
}

// TestReconciler_R5_NonTerminalResult_NoRepair verifies R5 skips non-terminal results.
func TestReconciler_R5_NonTerminalResult_NoRepair(t *testing.T) {
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
				Notified: true, NotifiedAt: &now,
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
	r5 := filterRepairs(repairs, "R5")
	if len(r5) != 0 {
		t.Fatalf("expected 0 R5 repairs for non-terminal result, got %d", len(r5))
	}
}

// =============================================================================
// Planner Signal Upsert — Deduplication
// =============================================================================

func TestUpsertPlannerSignal_Deduplication(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	sq := model.PlannerSignalQueue{}
	dirty := false

	sig := model.PlannerSignal{
		Kind: "awaiting_fill", CommandID: "cmd_dup", PhaseID: "p1", PhaseName: "phase1",
		Message: "first", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}

	qh.upsertPlannerSignal(&sq, &dirty, sig)
	if !dirty || len(sq.Signals) != 1 {
		t.Fatal("first signal should be added")
	}

	dirty = false
	sig2 := sig
	sig2.Message = "duplicate"
	qh.upsertPlannerSignal(&sq, &dirty, sig2)
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
	qh.upsertPlannerSignal(&sq, &dirty, sig3)
	if !dirty || len(sq.Signals) != 2 {
		t.Errorf("different kind should be added: dirty=%v, len=%d", dirty, len(sq.Signals))
	}
}

// =============================================================================
// hasExpiredLeases — Comprehensive Tests
// =============================================================================

func TestHasExpiredLeases_NoLeases(t *testing.T) {
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
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.canComplete = nil // explicitly unset

	now := time.Now().UTC().Format(time.RFC3339)

	// Set up a result file with terminal result
	resultsDir := filepath.Join(maestroDir, "results")
	os.MkdirAll(resultsDir, 0755)
	rf := model.CommandResultFile{
		SchemaVersion: 1, FileType: "result_command",
		Results: []model.CommandResult{
			{
				ID: "res_r4_nil", CommandID: "cmd_r4_nil", Status: model.StatusCompleted,
				Notified: true, NotifiedAt: &now,
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
	r4 := filterRepairs(repairs, "R4")
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
	r6 := filterRepairs(repairs, "R6")
	if len(r6) != 0 {
		t.Fatalf("expected 0 R6 repairs for unparseable deadline, got %d", len(r6))
	}
}

// TestReconciler_R6_DeadlineExactlyNow verifies R6 at the exact boundary.
// The R6 check is: time.Now().UTC().Before(deadline) → continue (skip).
// When deadline == now, Before returns false → R6 triggers.
// We use time.Now() as the deadline itself to test this boundary precisely.
func TestReconciler_R6_DeadlineExactlyNow(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)

	now := time.Now().UTC().Format(time.RFC3339)
	// Deadline is exactly now (truncated to seconds by RFC3339).
	// time.Now().Before(deadline) will be false (now is not before itself) → R6 triggers.
	deadlineNow := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)

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
				FillDeadlineAt: &deadlineNow,
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_r6_now.yaml"), state); err != nil {
		t.Fatalf("write command state: %v", err)
	}

	// Small sleep to ensure time.Now() is after the deadline (RFC3339 has second precision)
	time.Sleep(1100 * time.Millisecond)

	repairs, _ := rec.Reconcile()
	r6 := filterRepairs(repairs, "R6")
	if len(r6) != 1 {
		t.Fatalf("expected 1 R6 repair for deadline at exact boundary, got %d", len(r6))
	}
}

// =============================================================================
// DeferredNotification Content Verification
// =============================================================================

// TestDeferredNotification_R0b_ReFill verifies that R0b produces a "re_fill"
// deferred notification with the correct CommandID.
func TestDeferredNotification_R0b_ReFill(t *testing.T) {
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

	r0b := filterRepairs(repairs, "R0b")
	if len(r0b) != 1 {
		t.Fatalf("expected 1 R0b repair, got %d", len(r0b))
	}

	// Verify DeferredNotification content
	if len(notifications) != 1 {
		t.Fatalf("expected 1 deferred notification from R0b, got %d", len(notifications))
	}
	n := notifications[0]
	if n.Kind != "re_fill" {
		t.Errorf("notification kind: got %s, want re_fill", n.Kind)
	}
	if n.CommandID != "cmd_r0b_notif" {
		t.Errorf("notification command_id: got %s, want cmd_r0b_notif", n.CommandID)
	}
}

// TestDeferredNotification_R4_ReEvaluate verifies that R4 (canComplete error)
// produces a "re_evaluate" deferred notification with the correct Reason.
func TestDeferredNotification_R4_ReEvaluate(t *testing.T) {
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

	r4 := filterRepairs(repairs, "R4")
	if len(r4) != 1 {
		t.Fatalf("expected 1 R4 repair, got %d", len(r4))
	}

	// Verify DeferredNotification content
	if len(notifications) != 1 {
		t.Fatalf("expected 1 deferred notification from R4, got %d", len(notifications))
	}
	n := notifications[0]
	if n.Kind != "re_evaluate" {
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
	maestroDir := setupTestMaestroDir(t)
	rec := newTestReconciler(maestroDir)
	rec.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
		return &mockExecutorR6{}, nil
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

	r6 := filterRepairs(repairs, "R6")
	if len(r6) != 1 {
		t.Fatalf("expected 1 R6 repair, got %d: %+v", len(r6), r6)
	}

	// Verify DeferredNotification content
	if len(notifications) != 1 {
		t.Fatalf("expected 1 deferred notification from R6, got %d", len(notifications))
	}
	n := notifications[0]
	if n.Kind != "fill_timeout" {
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
