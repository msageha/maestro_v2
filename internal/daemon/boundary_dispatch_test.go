package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil/mocks"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// newBoundaryTestDaemon creates a fully-wired integration daemon for boundary tests.
// Uses the same pattern as newIntegrationDaemon but with configurable options.
func newBoundaryTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := newTestDaemon(t)
	lockMap := lock.NewMutexMap()
	reader := &integrationStateReader{maestroDir: d.maestroDir, lockMap: lockMap}
	d.handler = NewQueueHandler(d.maestroDir, d.config, lockMap, d.logger, d.logLevel,
		WithBusyChecker(BusyCheckerFunc(func(string) bool { return false })))
	d.handler.SetStateReader(reader)
	d.handler.SetCanComplete(testCanComplete)
	d.handler.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	})
	for _, sub := range []string{"dead_letters", "quarantine", "state"} {
		os.MkdirAll(filepath.Join(d.maestroDir, sub), 0755)
	}
	t.Cleanup(func() {
		d.handler.scanRunMu.Lock()
		os.RemoveAll(d.maestroDir)
		d.handler.scanRunMu.Unlock()
	})
	return d
}

// =============================================================================
// Command Dispatch Guard — Boundary Tests
// =============================================================================

// TestGuard_AllTerminalCommands_PendingDispatched verifies that when all
// existing commands are terminal, a new pending command IS dispatched.
func TestGuard_AllTerminalCommands_PendingDispatched(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)
	qh.config.Watcher.MaxInProgressMin = model.IntPtr(30)

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
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)
	qh.config.Watcher.MaxInProgressMin = model.IntPtr(30)

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

	// Create state file so R0-dispatch doesn't revert to pending
	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_extend", PlanStatus: model.PlanStatusSealed,
		CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_extend.yaml"), state)

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
	t.Parallel()
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
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)
	qh.config.Watcher.MaxInProgressMin = nil // should default to 60

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

	// Create state file so R0-dispatch doesn't revert to pending
	stateDir := filepath.Join(maestroDir, "state", "commands")
	os.MkdirAll(stateDir, 0755)
	state := model.CommandState{
		SchemaVersion: 1, FileType: "state_command",
		CommandID: "cmd_default", PlanStatus: model.PlanStatusSealed,
		CreatedAt: updatedAt, UpdatedAt: updatedAt,
	}
	yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_default.yaml"), state)

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
	t.Parallel()
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
	t.Parallel()
	d := newBoundaryTestDaemon(t)
	d.config.Watcher.MaxInProgressMin = model.IntPtr(30)
	// Recreate handler with updated config
	lockMap := d.handler.lockMap
	reader := &integrationStateReader{maestroDir: d.maestroDir, lockMap: lockMap}
	d.handler = NewQueueHandler(d.maestroDir, d.config, lockMap, d.logger, d.logLevel,
		WithBusyChecker(BusyCheckerFunc(func(string) bool { return true }))) // always busy
	d.handler.SetStateReader(reader)
	d.handler.SetCanComplete(testCanComplete)
	d.handler.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	})

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
	t.Parallel()
	d := newBoundaryTestDaemon(t)
	d.config.Watcher.MaxInProgressMin = model.IntPtr(60)
	lockMap := d.handler.lockMap
	reader := &integrationStateReader{maestroDir: d.maestroDir, lockMap: lockMap}
	d.handler = NewQueueHandler(d.maestroDir, d.config, lockMap, d.logger, d.logLevel,
		WithBusyChecker(BusyCheckerFunc(func(string) bool { return true })))
	d.handler.SetStateReader(reader)
	d.handler.SetCanComplete(testCanComplete)
	d.handler.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}, nil
	})

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
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Mock executor that fails dispatch
	qh.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mocks.MockExecutor{Result: agent.ExecResult{
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
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	dispatchCount := 0
	qh.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		dispatchCount++
		return &mocks.MockExecutor{Result: agent.ExecResult{
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
