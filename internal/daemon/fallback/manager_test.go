package fallback

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func defaultConfig() Config {
	return Config{
		Enabled:                     true,
		ConsecutiveFailureThreshold: 3,
		RecoveryCheckIntervalSec:    5,
		MinHealthyDurationSec:       10,
	}
}

func TestNormalModeAllowsAllWorkers(t *testing.T) {
	t.Parallel()
	m := NewManager(defaultConfig())

	workers := []string{"worker1", "worker2", "worker3", "worker99"}
	for _, w := range workers {
		assert.True(t, m.IsWorkerAllowed(w), "normal mode should allow %s", w)
	}
	assert.Equal(t, ModeNormal, m.Mode())
}

func TestTransitionNormalToDegradedAfterThresholdFailures(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.ConsecutiveFailureThreshold = 3
	m := NewManager(cfg)

	m.RecordFailure("worker1")
	assert.Equal(t, ModeNormal, m.Mode(), "should stay normal after 1 failure")

	m.RecordFailure("worker1")
	assert.Equal(t, ModeNormal, m.Mode(), "should stay normal after 2 failures")

	m.RecordFailure("worker1")
	assert.Equal(t, ModeDegraded, m.Mode(), "should transition to degraded after 3 failures")
}

func TestDegradedModeOnlyAllowsWorker1(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		workerID string
		allowed  bool
	}{
		{name: "worker1 allowed", workerID: "worker1", allowed: true},
		{name: "worker2 denied", workerID: "worker2", allowed: false},
		{name: "worker3 denied", workerID: "worker3", allowed: false},
		{name: "empty string denied", workerID: "", allowed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := defaultConfig()
			cfg.ConsecutiveFailureThreshold = 1
			m := NewManager(cfg)

			// Push into degraded mode.
			m.RecordFailure("worker1")
			assert.Equal(t, ModeDegraded, m.Mode())

			assert.Equal(t, tt.allowed, m.IsWorkerAllowed(tt.workerID))
		})
	}
}

func TestTransitionDegradedToRecoveringOnSuccess(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.ConsecutiveFailureThreshold = 1
	m := NewManager(cfg)

	m.RecordFailure("worker1")
	assert.Equal(t, ModeDegraded, m.Mode())

	m.RecordSuccess("worker1")
	assert.Equal(t, ModeRecovering, m.Mode())
}

func TestTransitionRecoveringToNormalAfterMinHealthyDuration(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.ConsecutiveFailureThreshold = 1
	cfg.MinHealthyDurationSec = 10
	m := NewManager(cfg)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.nowFunc = func() time.Time { return now }

	// Enter degraded, then recovering.
	m.RecordFailure("worker1")
	assert.Equal(t, ModeDegraded, m.Mode())

	m.RecordSuccess("worker1")
	assert.Equal(t, ModeRecovering, m.Mode())

	// Success before MinHealthyDurationSec should stay recovering.
	now = now.Add(5 * time.Second)
	m.RecordSuccess("worker1")
	assert.Equal(t, ModeRecovering, m.Mode(), "should remain recovering before min healthy duration")

	// Success at exactly MinHealthyDurationSec should transition to normal.
	now = now.Add(5 * time.Second) // total 10s from recoveringStartedAt
	m.RecordSuccess("worker1")
	assert.Equal(t, ModeNormal, m.Mode(), "should transition to normal after min healthy duration")
}

func TestTransitionRecoveringToDegradedOnFailure(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.ConsecutiveFailureThreshold = 1
	m := NewManager(cfg)

	// Enter degraded -> recovering.
	m.RecordFailure("worker1")
	m.RecordSuccess("worker1")
	assert.Equal(t, ModeRecovering, m.Mode())

	// A failure during recovering should go back to degraded.
	m.RecordFailure("worker1")
	assert.Equal(t, ModeDegraded, m.Mode())
}

func TestNowFuncInjectionForDeterministicTests(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.ConsecutiveFailureThreshold = 1
	cfg.MinHealthyDurationSec = 60
	m := NewManager(cfg)

	baseTime := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	m.nowFunc = func() time.Time { return baseTime }

	m.RecordFailure("worker1")
	m.RecordSuccess("worker1")
	assert.Equal(t, ModeRecovering, m.Mode())

	// Advance time by exactly 60 seconds.
	baseTime = baseTime.Add(60 * time.Second)
	m.RecordSuccess("worker1")
	assert.Equal(t, ModeNormal, m.Mode(), "nowFunc injection should allow deterministic time control")
}

func TestConsecutiveFailuresResetOnSuccess(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.ConsecutiveFailureThreshold = 3
	m := NewManager(cfg)

	// Record 2 failures (one short of threshold).
	m.RecordFailure("worker1")
	m.RecordFailure("worker1")
	assert.Equal(t, ModeNormal, m.Mode())

	// A success should reset the counter.
	m.RecordSuccess("worker1")
	assert.Equal(t, ModeNormal, m.Mode())

	// Now 2 more failures should not trigger degraded (counter was reset).
	m.RecordFailure("worker1")
	m.RecordFailure("worker1")
	assert.Equal(t, ModeNormal, m.Mode(), "consecutive failures should have been reset by success")

	// One more failure reaches the threshold again.
	m.RecordFailure("worker1")
	assert.Equal(t, ModeDegraded, m.Mode())
}

func TestRecoveringModeOnlyAllowsWorker1(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.ConsecutiveFailureThreshold = 1
	cfg.MinHealthyDurationSec = 60
	m := NewManager(cfg)

	m.RecordFailure("worker1")
	m.RecordSuccess("worker1")
	assert.Equal(t, ModeRecovering, m.Mode())

	assert.True(t, m.IsWorkerAllowed("worker1"), "recovering mode should allow worker1")
	assert.False(t, m.IsWorkerAllowed("worker2"), "recovering mode should deny worker2")
}

func TestModeStringMethod(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode     Mode
		expected string
	}{
		{ModeNormal, "normal"},
		{ModeDegraded, "degraded"},
		{ModeRecovering, "recovering"},
		{Mode(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.mode.String())
		})
	}
}
