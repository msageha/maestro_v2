package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// TestCommandDispatchGuard_ExpiredLease verifies that when an in_progress command
// has an expired lease, new pending commands are still NOT dispatched.
// The at-most-one guard blocks dispatch regardless of lease validity.
func TestCommandDispatchGuard_ExpiredLease(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	expiredTime := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	owner := qh.leaseOwnerID()

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID:             "cmd_expired",
				Content:        "In-progress with expired lease",
				Priority:       1,
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiredTime,
				LeaseEpoch:     1,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "cmd_pending",
				Content:   "Pending command waiting to dispatch",
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

	if len(result.Commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(result.Commands))
	}

	// cmd_expired should remain in_progress (auto-extended, not released)
	expired := result.Commands[0]
	if expired.ID != "cmd_expired" {
		t.Fatalf("expected cmd_expired at index 0, got %s", expired.ID)
	}
	if expired.Status != model.StatusInProgress {
		t.Errorf("cmd_expired status: got %s, want in_progress (auto-extended)", expired.Status)
	}

	// cmd_pending must remain pending — the at-most-one guard blocks dispatch
	pending := result.Commands[1]
	if pending.ID != "cmd_pending" {
		t.Fatalf("expected cmd_pending at index 1, got %s", pending.ID)
	}
	if pending.Status != model.StatusPending {
		t.Errorf("cmd_pending status: got %s, want pending (blocked by at-most-one guard)", pending.Status)
	}
	if pending.LeaseOwner != nil {
		t.Errorf("cmd_pending should have no lease_owner, got %v", *pending.LeaseOwner)
	}
}

// TestCommandDispatchGuard_ValidLease verifies the baseline at-most-one guard:
// an in_progress command with a valid (non-expired) lease blocks new dispatches.
func TestCommandDispatchGuard_ValidLease(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	futureTime := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	owner := qh.leaseOwnerID()

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID:             "cmd_active",
				Content:        "In-progress with valid lease",
				Priority:       1,
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &futureTime,
				LeaseEpoch:     1,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "cmd_waiting",
				Content:   "Pending command",
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

	if len(result.Commands) != 2 {
		t.Fatalf("expected 2 commands, got %d", len(result.Commands))
	}

	// cmd_active stays in_progress
	if result.Commands[0].Status != model.StatusInProgress {
		t.Errorf("cmd_active status: got %s, want in_progress", result.Commands[0].Status)
	}

	// cmd_waiting must remain pending — blocked by the at-most-one guard
	if result.Commands[1].Status != model.StatusPending {
		t.Errorf("cmd_waiting status: got %s, want pending (blocked by at-most-one guard)", result.Commands[1].Status)
	}
	if result.Commands[1].LeaseOwner != nil {
		t.Errorf("cmd_waiting should have no lease_owner, got %v", *result.Commands[1].LeaseOwner)
	}
}

// TestCommandLeaseAutoExtend verifies that an expired command lease is auto-extended
// in Phase A (collectExpiredCommandBusyChecks) rather than released.
// After a scan, the command should remain in_progress with a new future lease expiry.
func TestCommandLeaseAutoExtend(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	expiredTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	owner := qh.leaseOwnerID()
	recentUpdate := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID:             "cmd_extend",
				Content:        "Command with expired lease",
				Priority:       1,
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiredTime,
				LeaseEpoch:     3,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:      recentUpdate, // recent enough to not hit max_in_progress_min
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

	cmd := result.Commands[0]

	// Must remain in_progress (auto-extended, not released to pending)
	if cmd.Status != model.StatusInProgress {
		t.Errorf("status: got %s, want in_progress (auto-extended)", cmd.Status)
	}

	// Lease epoch should be unchanged (ExtendCommandLease does not increment epoch)
	if cmd.LeaseEpoch != 3 {
		t.Errorf("lease_epoch: got %d, want 3 (unchanged by extend)", cmd.LeaseEpoch)
	}

	// LeaseExpiresAt must be updated to a future time
	if cmd.LeaseExpiresAt == nil {
		t.Fatal("lease_expires_at should not be nil after auto-extend")
	}
	newExpiry, err := time.Parse(time.RFC3339, *cmd.LeaseExpiresAt)
	if err != nil {
		t.Fatalf("parse new lease_expires_at: %v", err)
	}
	if !newExpiry.After(time.Now().UTC()) {
		t.Errorf("lease_expires_at should be in the future after auto-extend, got %s", *cmd.LeaseExpiresAt)
	}

	// LeaseOwner should be preserved
	if cmd.LeaseOwner == nil || *cmd.LeaseOwner != owner {
		t.Errorf("lease_owner: got %v, want %s (preserved)", cmd.LeaseOwner, owner)
	}
}

// TestTaskLeaseExpiry_ReleaseToPending verifies that task leases (unlike commands)
// are released back to pending when expired and the agent is not busy.
// This confirms the asymmetric behavior: commands auto-extend, tasks release.
func TestTaskLeaseExpiry_ReleaseToPending(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Override busyChecker to return false (agent not busy)
	qh.busyChecker = func(agentID string) bool { return false }

	expiredTime := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	owner := "worker1"
	recentUpdate := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "task_queue",
		Tasks: []model.Task{
			{
				ID:             "task_expired",
				CommandID:      "cmd_001",
				Purpose:        "Task with expired lease",
				Content:        "Some work",
				Priority:       1,
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiredTime,
				LeaseEpoch:     2,
				CreatedAt:      time.Now().UTC().Format(time.RFC3339),
				UpdatedAt:      recentUpdate,
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

	if len(result.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(result.Tasks))
	}

	task := result.Tasks[0]

	// With busyChecker=false, the task goes through:
	// Phase A: expired lease detected → busy check deferred
	// Phase B: busy check returns false
	// Phase C: lease released → pending
	// The task is NOT re-dispatched in the same scan because expired leases
	// exist → hasExpiredLeases returns true → dispatch is skipped.
	if task.Status != model.StatusPending {
		t.Fatalf("task status: got %s, want pending (released after non-busy expired lease)", task.Status)
	}
	if task.LeaseOwner != nil {
		t.Errorf("lease_owner should be nil after release, got %v", *task.LeaseOwner)
	}
	if task.LeaseExpiresAt != nil {
		t.Errorf("lease_expires_at should be nil after release, got %v", *task.LeaseExpiresAt)
	}
}

// TestCommandDispatchError_LeaseRetained verifies that when a command dispatch
// fails in Phase B, the lease is NOT released in Phase C.
// This prevents duplicate dispatch: the command stays in_progress and will be
// auto-extended on the next scan cycle.
func TestCommandDispatchError_LeaseRetained(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	qh := newTestQueueHandler(maestroDir)

	// Mock executor that fails dispatch
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{
			Success:   false,
			Error:     fmt.Errorf("planner tmux pane not responding"),
			Retryable: true,
		}}, nil
	})

	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID:        "cmd_fail",
				Content:   "Command that will fail dispatch",
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

	cmd := result.Commands[0]

	// Must stay in_progress — lease is retained on dispatch error for commands
	if cmd.Status != model.StatusInProgress {
		t.Errorf("status: got %s, want in_progress (lease retained on dispatch error)", cmd.Status)
	}

	// Lease owner should still be set
	if cmd.LeaseOwner == nil {
		t.Error("lease_owner should be set (lease retained)")
	}

	// LeaseExpiresAt should still be set
	if cmd.LeaseExpiresAt == nil {
		t.Error("lease_expires_at should be set (lease retained)")
	}

	// LeaseEpoch should be 1 (acquired once)
	if cmd.LeaseEpoch != 1 {
		t.Errorf("lease_epoch: got %d, want 1", cmd.LeaseEpoch)
	}

	// Attempts should be 1
	if cmd.Attempts != 1 {
		t.Errorf("attempts: got %d, want 1", cmd.Attempts)
	}
}
