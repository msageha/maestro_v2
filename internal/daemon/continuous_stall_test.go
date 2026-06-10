package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func stallTestConfig() model.Config {
	return model.Config{
		Continuous: model.ContinuousConfig{Enabled: true, MaxIterations: 10},
	}
}

func writeStalledState(t *testing.T, maestroDir string, stalledFor time.Duration) {
	t.Helper()
	last := "cmd_stall_1"
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 3,
		MaxIterations:    10,
		Status:           model.ContinuousStatusRunning,
		LastCommandID:    &last,
		UpdatedAt:        time.Now().UTC().Add(-stalledFor).Format(time.RFC3339),
	})
}

func writeStallCommandQueue(t *testing.T, maestroDir string, statuses ...model.Status) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755); err != nil {
		t.Fatal(err)
	}
	cq := model.CommandQueue{SchemaVersion: 1, FileType: "queue_command"}
	for i, st := range statuses {
		cq.Commands = append(cq.Commands, model.Command{
			ID:     "cmd_q_" + string(rune('a'+i)),
			Status: st,
		})
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", "planner.yaml"), &cq); err != nil {
		t.Fatal(err)
	}
}

func TestContinuousStall_NotifiesOncePerStall(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	if err := os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755); err != nil {
		t.Fatal(err)
	}
	ch := newTestContinuousHandler(maestroDir, stallTestConfig())
	writeStalledState(t, maestroDir, 20*time.Minute)

	ch.CheckStall()

	nq := readNotificationQueue(t, maestroDir)
	if nq == nil || len(nq.Notifications) != 1 {
		t.Fatalf("expected 1 stall notification, got %+v", nq)
	}
	n := nq.Notifications[0]
	if n.Type != model.NotificationTypeContinuousStalled {
		t.Errorf("type: got %s, want %s", n.Type, model.NotificationTypeContinuousStalled)
	}
	if n.CommandID != "cmd_stall_1" {
		t.Errorf("commandID: got %s, want cmd_stall_1", n.CommandID)
	}

	// Same stall on the next tick must not enqueue a duplicate.
	ch.CheckStall()
	nq = readNotificationQueue(t, maestroDir)
	if len(nq.Notifications) != 1 {
		t.Errorf("expected once-per-stall notification, got %d entries", len(nq.Notifications))
	}
}

func TestContinuousStall_ActiveCommand_NoNotify(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	ch := newTestContinuousHandler(maestroDir, stallTestConfig())
	writeStalledState(t, maestroDir, 20*time.Minute)
	writeStallCommandQueue(t, maestroDir, model.StatusPending)

	ch.CheckStall()

	nq := readNotificationQueue(t, maestroDir)
	if nq != nil && len(nq.Notifications) != 0 {
		t.Errorf("pending command means no stall; got %+v", nq.Notifications)
	}
}

func TestContinuousStall_TerminalCommandsOnly_Notifies(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	ch := newTestContinuousHandler(maestroDir, stallTestConfig())
	writeStalledState(t, maestroDir, 20*time.Minute)
	writeStallCommandQueue(t, maestroDir, model.StatusCompleted, model.StatusFailed)

	ch.CheckStall()

	nq := readNotificationQueue(t, maestroDir)
	if nq == nil || len(nq.Notifications) != 1 {
		t.Fatalf("terminal-only queue should stall-notify, got %+v", nq)
	}
}

func TestContinuousStall_BelowThreshold_NoNotify(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	if err := os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755); err != nil {
		t.Fatal(err)
	}
	ch := newTestContinuousHandler(maestroDir, stallTestConfig())
	writeStalledState(t, maestroDir, 1*time.Minute) // default threshold is 600s

	ch.CheckStall()

	nq := readNotificationQueue(t, maestroDir)
	if nq != nil && len(nq.Notifications) != 0 {
		t.Errorf("below threshold must not notify; got %+v", nq.Notifications)
	}
}

func TestContinuousStall_ExplicitlyDisabled_NoNotify(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	if err := os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := stallTestConfig()
	cfg.Continuous.StallNotificationSec = -1
	ch := newTestContinuousHandler(maestroDir, cfg)
	writeStalledState(t, maestroDir, 20*time.Minute)

	ch.CheckStall()

	nq := readNotificationQueue(t, maestroDir)
	if nq != nil && len(nq.Notifications) != 0 {
		t.Errorf("negative stall_notification_sec disables the watchdog; got %+v", nq.Notifications)
	}
}

func TestContinuousStall_NotRunning_NoNotify(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	if err := os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755); err != nil {
		t.Fatal(err)
	}
	ch := newTestContinuousHandler(maestroDir, stallTestConfig())
	last := "cmd_stall_1"
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 3,
		Status:           model.ContinuousStatusPaused,
		LastCommandID:    &last,
		UpdatedAt:        time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339),
	})

	ch.CheckStall()

	nq := readNotificationQueue(t, maestroDir)
	if nq != nil && len(nq.Notifications) != 0 {
		t.Errorf("paused mode must not stall-notify; got %+v", nq.Notifications)
	}
}

func TestEffectiveStallNotificationSec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   int
		want int
	}{
		{0, 600},
		{-1, 0},
		{120, 120},
	}
	for _, tt := range tests {
		c := model.ContinuousConfig{StallNotificationSec: tt.in}
		if got := c.EffectiveStallNotificationSec(); got != tt.want {
			t.Errorf("EffectiveStallNotificationSec(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}
