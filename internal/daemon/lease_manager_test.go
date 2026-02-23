package daemon

import (
	"bytes"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

func newTestLeaseManager() *LeaseManager {
	return NewLeaseManager(model.WatcherConfig{
		DispatchLeaseSec: 300,
		MaxInProgressMin: 60,
	}, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
}

func TestAcquireCommandLease(t *testing.T) {
	lm := newTestLeaseManager()
	cmd := &model.Command{
		ID:        "cmd_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := lm.AcquireCommandLease(cmd, "planner"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if cmd.Status != model.StatusInProgress {
		t.Errorf("status: got %s, want in_progress", cmd.Status)
	}
	if cmd.LeaseOwner == nil || *cmd.LeaseOwner != "planner" {
		t.Error("lease_owner not set")
	}
	if cmd.LeaseExpiresAt == nil {
		t.Error("lease_expires_at not set")
	}
	if cmd.LeaseEpoch != 1 {
		t.Errorf("lease_epoch: got %d, want 1", cmd.LeaseEpoch)
	}
}

func TestAcquireCommandLease_InvalidTransition(t *testing.T) {
	lm := newTestLeaseManager()
	cmd := &model.Command{
		ID:     "cmd_001",
		Status: model.StatusCompleted,
	}

	if err := lm.AcquireCommandLease(cmd, "planner"); err == nil {
		t.Error("expected error for completed→in_progress")
	}
}

func TestReleaseCommandLease(t *testing.T) {
	lm := newTestLeaseManager()
	cmd := &model.Command{
		ID:        "cmd_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	lm.AcquireCommandLease(cmd, "planner")
	if err := lm.ReleaseCommandLease(cmd); err != nil {
		t.Fatalf("release: %v", err)
	}

	if cmd.Status != model.StatusPending {
		t.Errorf("status: got %s, want pending", cmd.Status)
	}
	if cmd.LeaseOwner != nil {
		t.Error("lease_owner should be nil")
	}
	if cmd.LeaseExpiresAt != nil {
		t.Error("lease_expires_at should be nil")
	}
	// LeaseEpoch should be preserved
	if cmd.LeaseEpoch != 1 {
		t.Errorf("lease_epoch should be preserved: got %d", cmd.LeaseEpoch)
	}
}

func TestExtendCommandLease(t *testing.T) {
	lm := newTestLeaseManager()
	cmd := &model.Command{
		ID:        "cmd_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	lm.AcquireCommandLease(cmd, "planner")
	oldExpires, _ := time.Parse(time.RFC3339, *cmd.LeaseExpiresAt)

	// Wait just enough for the new expiry to be after the old one
	time.Sleep(1100 * time.Millisecond)
	if err := lm.ExtendCommandLease(cmd); err != nil {
		t.Fatalf("extend: %v", err)
	}

	newExpires, _ := time.Parse(time.RFC3339, *cmd.LeaseExpiresAt)
	if !newExpires.After(oldExpires) {
		t.Errorf("new expiry %v should be after old expiry %v", newExpires, oldExpires)
	}
}

func TestExtendCommandLease_NotInProgress(t *testing.T) {
	lm := newTestLeaseManager()
	cmd := &model.Command{ID: "cmd_001", Status: model.StatusPending}

	if err := lm.ExtendCommandLease(cmd); err == nil {
		t.Error("expected error for extending non in_progress")
	}
}

func TestAcquireTaskLease(t *testing.T) {
	lm := newTestLeaseManager()
	task := &model.Task{
		ID:        "task_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := lm.AcquireTaskLease(task, "worker1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if task.Status != model.StatusInProgress {
		t.Errorf("status: got %s, want in_progress", task.Status)
	}
	if task.LeaseOwner == nil || *task.LeaseOwner != "worker1" {
		t.Error("lease_owner not set")
	}
	if task.LeaseEpoch != 1 {
		t.Errorf("lease_epoch: got %d, want 1", task.LeaseEpoch)
	}
}

func TestReleaseTaskLease(t *testing.T) {
	lm := newTestLeaseManager()
	task := &model.Task{
		ID:        "task_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	lm.AcquireTaskLease(task, "worker1")
	if err := lm.ReleaseTaskLease(task); err != nil {
		t.Fatalf("release: %v", err)
	}

	if task.Status != model.StatusPending {
		t.Errorf("status: got %s, want pending", task.Status)
	}
}

func TestAcquireNotificationLease(t *testing.T) {
	lm := newTestLeaseManager()
	ntf := &model.Notification{
		ID:        "ntf_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := lm.AcquireNotificationLease(ntf, "orchestrator"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if ntf.Status != model.StatusInProgress {
		t.Errorf("status: got %s, want in_progress", ntf.Status)
	}
}

func TestIsLeaseExpired(t *testing.T) {
	lm := newTestLeaseManager()

	// nil → expired
	if !lm.IsLeaseExpired(nil) {
		t.Error("nil lease should be expired")
	}

	// Past time → expired
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	if !lm.IsLeaseExpired(&past) {
		t.Error("past lease should be expired")
	}

	// Future time → not expired
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	if lm.IsLeaseExpired(&future) {
		t.Error("future lease should not be expired")
	}
}

func TestExpireTasks(t *testing.T) {
	lm := newTestLeaseManager()

	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	w := "worker1"

	tasks := []model.Task{
		{ID: "t1", Status: model.StatusInProgress, LeaseExpiresAt: &past, LeaseOwner: &w},
		{ID: "t2", Status: model.StatusInProgress, LeaseExpiresAt: &future, LeaseOwner: &w},
		{ID: "t3", Status: model.StatusPending},
		{ID: "t4", Status: model.StatusInProgress, LeaseExpiresAt: &past, LeaseOwner: &w},
	}

	expired := lm.ExpireTasks(tasks)
	if len(expired) != 2 {
		t.Errorf("expected 2 expired, got %d", len(expired))
	}
	if expired[0] != 0 || expired[1] != 3 {
		t.Errorf("expected indices [0,3], got %v", expired)
	}
}

func TestExpireCommands(t *testing.T) {
	lm := newTestLeaseManager()

	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	w := "planner"

	commands := []model.Command{
		{ID: "c1", Status: model.StatusInProgress, LeaseExpiresAt: &past, LeaseOwner: &w},
		{ID: "c2", Status: model.StatusPending},
	}

	expired := lm.ExpireCommands(commands)
	if len(expired) != 1 {
		t.Errorf("expected 1 expired, got %d", len(expired))
	}
}

func TestHasInFlightForOwner(t *testing.T) {
	lm := newTestLeaseManager()

	w1 := "worker1"
	w2 := "worker2"

	tasks := []model.Task{
		{ID: "t1", Status: model.StatusInProgress, LeaseOwner: &w1},
		{ID: "t2", Status: model.StatusPending},
	}

	if !lm.HasInFlightForOwner(tasks, "worker1") {
		t.Error("worker1 should have in-flight")
	}
	if lm.HasInFlightForOwner(tasks, "worker2") {
		t.Error("worker2 should not have in-flight")
	}

	_ = w2
}

func TestLeaseEpochIncrement(t *testing.T) {
	lm := newTestLeaseManager()
	task := &model.Task{
		ID:        "task_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// First acquire: epoch 0→1
	lm.AcquireTaskLease(task, "worker1")
	if task.LeaseEpoch != 1 {
		t.Errorf("first acquire epoch: got %d, want 1", task.LeaseEpoch)
	}

	// Release back to pending
	lm.ReleaseTaskLease(task)
	if task.LeaseEpoch != 1 {
		t.Errorf("after release epoch: got %d, want 1", task.LeaseEpoch)
	}

	// Second acquire: epoch 1→2
	lm.AcquireTaskLease(task, "worker2")
	if task.LeaseEpoch != 2 {
		t.Errorf("second acquire epoch: got %d, want 2", task.LeaseEpoch)
	}
}
