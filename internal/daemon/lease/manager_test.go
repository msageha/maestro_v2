package lease

import (
	"bytes"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

func newTestManager() *Manager {
	return New(model.WatcherConfig{
		DispatchLeaseSec: 300,
		MaxInProgressMin: ptr.Int(60),
	}, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)
}

func TestAcquireCommandLease(t *testing.T) {
	t.Parallel()
	lm := newTestManager()
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
	t.Parallel()
	lm := newTestManager()
	cmd := &model.Command{
		ID:     "cmd_001",
		Status: model.StatusCompleted,
	}

	if err := lm.AcquireCommandLease(cmd, "planner"); err == nil {
		t.Error("expected error for completed→in_progress")
	}
}

func TestReleaseCommandLease(t *testing.T) {
	t.Parallel()
	lm := newTestManager()
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
	t.Parallel()
	lm := newTestManager()
	cmd := &model.Command{
		ID:        "cmd_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	lm.AcquireCommandLease(cmd, "planner")
	originalUpdatedAt := cmd.UpdatedAt

	// Set the lease expiry to a past time so the extended expiry will be strictly after it.
	pastExpires := time.Now().Add(-time.Hour).Format(time.RFC3339)
	cmd.LeaseExpiresAt = &pastExpires
	oldExpires, _ := time.Parse(time.RFC3339, *cmd.LeaseExpiresAt)

	if err := lm.ExtendCommandLease(cmd); err != nil {
		t.Fatalf("extend: %v", err)
	}

	newExpires, _ := time.Parse(time.RFC3339, *cmd.LeaseExpiresAt)
	if !newExpires.After(oldExpires) {
		t.Errorf("new expiry %v should be after old expiry %v", newExpires, oldExpires)
	}

	// QA-008: UpdatedAt must NOT change on lease extension
	if cmd.UpdatedAt != originalUpdatedAt {
		t.Errorf("UpdatedAt changed after ExtendCommandLease: got %s, want %s", cmd.UpdatedAt, originalUpdatedAt)
	}
}

func TestExtendTaskLease_UpdatedAtUnchanged(t *testing.T) {
	t.Parallel()
	lm := newTestManager()
	task := &model.Task{
		ID:        "task_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := lm.AcquireTaskLease(task, "worker1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	originalUpdatedAt := task.UpdatedAt

	// Set the lease expiry to a past time to avoid sleeping for the clock to advance.
	pastExpires := time.Now().Add(-time.Hour).Format(time.RFC3339)
	task.LeaseExpiresAt = &pastExpires

	if err := lm.ExtendTaskLease(task); err != nil {
		t.Fatalf("extend: %v", err)
	}

	// QA-008: UpdatedAt must NOT change on lease extension
	if task.UpdatedAt != originalUpdatedAt {
		t.Errorf("UpdatedAt changed after ExtendTaskLease: got %s, want %s", task.UpdatedAt, originalUpdatedAt)
	}
}

func TestExtendCommandLease_NotInProgress(t *testing.T) {
	t.Parallel()
	lm := newTestManager()
	cmd := &model.Command{ID: "cmd_001", Status: model.StatusPending}

	if err := lm.ExtendCommandLease(cmd); err == nil {
		t.Error("expected error for extending non in_progress")
	}
}

func TestAcquireTaskLease(t *testing.T) {
	t.Parallel()
	lm := newTestManager()
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
	t.Parallel()
	lm := newTestManager()
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
	t.Parallel()
	lm := newTestManager()
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
	t.Parallel()
	lm := newTestManager()

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
	t.Parallel()
	lm := newTestManager()

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
	t.Parallel()
	lm := newTestManager()

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

func TestLeaseEpochIncrement(t *testing.T) {
	t.Parallel()
	lm := newTestManager()
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

func TestIsLeaseNearExpiry(t *testing.T) {
	t.Parallel()
	lm := newTestManager()

	// nil → near expiry (treated as expired)
	if !lm.IsLeaseNearExpiry(nil, 60) {
		t.Error("nil lease should be near expiry")
	}

	// Expires in 30s, buffer 60s → near expiry
	soon := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339)
	if !lm.IsLeaseNearExpiry(&soon, 60) {
		t.Error("lease expiring in 30s with 60s buffer should be near expiry")
	}

	// Expires in 2 minutes, buffer 60s → NOT near expiry
	later := time.Now().UTC().Add(2 * time.Minute).Format(time.RFC3339)
	if lm.IsLeaseNearExpiry(&later, 60) {
		t.Error("lease expiring in 2m with 60s buffer should NOT be near expiry")
	}

	// Already expired → near expiry
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if !lm.IsLeaseNearExpiry(&past, 60) {
		t.Error("already expired lease should be near expiry")
	}

	// Malformed date → near expiry
	bad := "not-a-date"
	if !lm.IsLeaseNearExpiry(&bad, 60) {
		t.Error("malformed date should be treated as near expiry")
	}
}

func TestExtendTaskLease_NotInProgress(t *testing.T) {
	t.Parallel()
	lm := newTestManager()

	statuses := []struct {
		name   string
		status model.Status
	}{
		{"pending", model.StatusPending},
		{"completed", model.StatusCompleted},
		{"failed", model.StatusFailed},
		{"cancelled", model.StatusCancelled},
		{"dead_letter", model.StatusDeadLetter},
	}
	for _, tc := range statuses {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			task := &model.Task{ID: "task_001", Status: tc.status}
			if err := lm.ExtendTaskLease(task); err == nil {
				t.Errorf("expected error for extending %s task lease", tc.status)
			}
		})
	}
}

func TestExtendTaskLeaseGrace_NotInProgress(t *testing.T) {
	t.Parallel()
	lm := newTestManager()

	statuses := []struct {
		name   string
		status model.Status
	}{
		{"pending", model.StatusPending},
		{"completed", model.StatusCompleted},
		{"failed", model.StatusFailed},
		{"cancelled", model.StatusCancelled},
	}
	for _, tc := range statuses {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			task := &model.Task{ID: "task_001", Status: tc.status}
			if err := lm.ExtendTaskLeaseGrace(task, 30*time.Second); err == nil {
				t.Errorf("expected error for grace-extending %s task lease", tc.status)
			}
		})
	}
}

func TestIsTaskMaxInProgressTimeout(t *testing.T) {
	t.Parallel()
	lm := newTestManager() // maxInProgressMin = 60

	now := time.Now().UTC()
	recent := now.Add(-30 * time.Minute).Format(time.RFC3339)
	old := now.Add(-61 * time.Minute).Format(time.RFC3339)
	exact := now.Add(-60 * time.Minute).Format(time.RFC3339)

	tests := []struct {
		name         string
		inProgressAt *string
		updatedAt    string
		want         bool
	}{
		{
			name:         "InProgressAt_recent_not_timeout",
			inProgressAt: &recent,
			updatedAt:    old, // should be ignored; InProgressAt takes priority
			want:         false,
		},
		{
			name:         "InProgressAt_old_timeout",
			inProgressAt: &old,
			updatedAt:    recent, // should be ignored
			want:         true,
		},
		{
			name:         "InProgressAt_exact_boundary_timeout",
			inProgressAt: &exact,
			updatedAt:    recent,
			want:         true,
		},
		{
			name:         "InProgressAt_nil_fallback_to_UpdatedAt_recent",
			inProgressAt: nil,
			updatedAt:    recent,
			want:         false,
		},
		{
			name:         "InProgressAt_nil_fallback_to_UpdatedAt_old",
			inProgressAt: nil,
			updatedAt:    old,
			want:         true,
		},
		{
			name:         "InProgressAt_empty_fallback_to_UpdatedAt",
			inProgressAt: ptr.String(""),
			updatedAt:    old,
			want:         true,
		},
		{
			name:         "InProgressAt_malformed_not_timeout",
			inProgressAt: ptr.String("not-a-date"),
			updatedAt:    old,
			want:         false,
		},
		{
			name:         "UpdatedAt_malformed_not_timeout",
			inProgressAt: nil,
			updatedAt:    "not-a-date",
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			task := &model.Task{
				ID:           "task_test",
				Status:       model.StatusInProgress,
				InProgressAt: tt.inProgressAt,
				UpdatedAt:    tt.updatedAt,
			}
			got := lm.IsTaskMaxInProgressTimeout(task)
			if got != tt.want {
				t.Errorf("IsTaskMaxInProgressTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsTaskMaxInProgressTimeout_Disabled(t *testing.T) {
	t.Parallel()
	// maxInProgressMin = 0 → disabled
	lm := New(model.WatcherConfig{
		DispatchLeaseSec: 300,
		MaxInProgressMin: ptr.Int(0),
	}, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)

	old := time.Now().UTC().Add(-120 * time.Minute).Format(time.RFC3339)
	task := &model.Task{
		ID:           "task_test",
		Status:       model.StatusInProgress,
		InProgressAt: &old,
		UpdatedAt:    old,
	}
	if lm.IsTaskMaxInProgressTimeout(task) {
		t.Error("expected false when maxInProgressMin is 0 (disabled)")
	}
}

func TestAcquireTaskLease_SetsInProgressAt(t *testing.T) {
	t.Parallel()
	lm := newTestManager()
	task := &model.Task{
		ID:        "task_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if err := lm.AcquireTaskLease(task, "worker1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if task.InProgressAt == nil {
		t.Fatal("InProgressAt should be set after AcquireTaskLease")
	}
	// InProgressAt should be close to now
	ts, err := time.Parse(time.RFC3339, *task.InProgressAt)
	if err != nil {
		t.Fatalf("parse InProgressAt: %v", err)
	}
	if time.Since(ts) > 5*time.Second {
		t.Errorf("InProgressAt too far from now: %v", ts)
	}
}

func TestReleaseTaskLease_ClearsInProgressAt(t *testing.T) {
	t.Parallel()
	lm := newTestManager()
	task := &model.Task{
		ID:        "task_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	lm.AcquireTaskLease(task, "worker1")
	if task.InProgressAt == nil {
		t.Fatal("InProgressAt should be set after acquire")
	}

	lm.ReleaseTaskLease(task)
	if task.InProgressAt != nil {
		t.Error("InProgressAt should be nil after ReleaseTaskLease (H5)")
	}
}

func TestRenewableCommands(t *testing.T) {
	t.Parallel()
	lm := newTestManager()

	soon := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339)
	later := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	owner := "daemon:12345"

	commands := []model.Command{
		{ID: "c1", Status: model.StatusInProgress, LeaseExpiresAt: &soon, LeaseOwner: &owner, LeaseEpoch: 1},
		{ID: "c2", Status: model.StatusInProgress, LeaseExpiresAt: &later, LeaseOwner: &owner, LeaseEpoch: 1},
		{ID: "c3", Status: model.StatusPending},
		{ID: "c4", Status: model.StatusInProgress, LeaseExpiresAt: &past, LeaseOwner: &owner, LeaseEpoch: 1},
	}

	renewable := lm.RenewableCommands(commands, 60)
	if len(renewable) != 1 {
		t.Fatalf("expected 1 renewable, got %d", len(renewable))
	}
	if renewable[0] != 0 {
		t.Errorf("expected index 0 (c1), got %d", renewable[0])
	}
}
