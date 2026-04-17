package formation

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// PromptDetector tests
// ---------------------------------------------------------------------------

func TestPromptDetector_DetectsOnFirstAttempt(t *testing.T) {
	t.Parallel()
	d := NewPromptDetector(
		func(agentID string) (bool, error) { return true, nil },
		WithPromptSleepFn(func(time.Duration) {}),
	)

	detected, err := d.DetectPrompt("worker1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !detected {
		t.Error("expected prompt detected")
	}
	if !d.IsHealthy("worker1") {
		t.Error("expected worker1 to be healthy")
	}
}

func TestPromptDetector_DetectsOnRetry(t *testing.T) {
	t.Parallel()
	var attempt int32
	d := NewPromptDetector(
		func(agentID string) (bool, error) {
			a := atomic.AddInt32(&attempt, 1)
			if a < 3 {
				return false, fmt.Errorf("timeout")
			}
			return true, nil
		},
		WithPromptSleepFn(func(time.Duration) {}),
	)

	detected, err := d.DetectPrompt("worker1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !detected {
		t.Error("expected prompt detected on retry")
	}
	if got := atomic.LoadInt32(&attempt); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if !d.IsHealthy("worker1") {
		t.Error("expected worker1 healthy after retry success")
	}
}

func TestPromptDetector_AllRetriesFail_MarksUnhealthy(t *testing.T) {
	t.Parallel()
	d := NewPromptDetector(
		func(agentID string) (bool, error) { return false, fmt.Errorf("timeout") },
		WithPromptSleepFn(func(time.Duration) {}),
	)

	detected, err := d.DetectPrompt("worker1")
	if detected {
		t.Error("expected no detection")
	}
	if err == nil {
		t.Error("expected error")
	}
	if d.IsHealthy("worker1") {
		t.Error("expected worker1 to be unhealthy after all retries failed")
	}
	if h := d.Health("worker1"); h != AgentUnhealthy {
		t.Errorf("health = %s, want unhealthy", h)
	}
}

func TestPromptDetector_RecheckRecovery(t *testing.T) {
	t.Parallel()
	var checkCount int32
	d := NewPromptDetector(
		func(agentID string) (bool, error) {
			c := atomic.AddInt32(&checkCount, 1)
			// Fail for first 3 (DetectPrompt) then succeed (RecheckHealth)
			if c <= 3 {
				return false, fmt.Errorf("timeout")
			}
			return true, nil
		},
		WithPromptSleepFn(func(time.Duration) {}),
	)

	// First: all retries fail → unhealthy
	d.DetectPrompt("worker1")
	if d.IsHealthy("worker1") {
		t.Fatal("expected unhealthy after failed detect")
	}

	// Recheck: now prompt detected → healthy again
	d.RecheckHealth("worker1")
	if !d.IsHealthy("worker1") {
		t.Error("expected worker1 recovered to healthy")
	}
}

func TestPromptDetector_RecheckSkipsHealthyAgent(t *testing.T) {
	t.Parallel()
	var checkCount int32
	d := NewPromptDetector(
		func(agentID string) (bool, error) {
			atomic.AddInt32(&checkCount, 1)
			return true, nil
		},
		WithPromptSleepFn(func(time.Duration) {}),
	)

	// Detect successfully (healthy)
	d.DetectPrompt("worker1")
	before := atomic.LoadInt32(&checkCount)

	// Recheck should skip healthy agent (no additional check call)
	d.RecheckHealth("worker1")
	after := atomic.LoadInt32(&checkCount)
	if after != before {
		t.Errorf("RecheckHealth called checkPrompt for healthy agent: before=%d, after=%d", before, after)
	}
}

func TestPromptDetector_UnknownAgentIsHealthy(t *testing.T) {
	t.Parallel()
	d := NewPromptDetector(
		func(string) (bool, error) { return true, nil },
	)

	if !d.IsHealthy("unknown-agent") {
		t.Error("unknown agent should be considered healthy")
	}
	if h := d.Health("unknown-agent"); h != AgentHealthy {
		t.Errorf("Health(unknown) = %s, want healthy", h)
	}
}

func TestPromptDetector_RetryInterval(t *testing.T) {
	t.Parallel()
	var sleepDurations []time.Duration
	d := NewPromptDetector(
		func(string) (bool, error) { return false, fmt.Errorf("fail") },
		WithPromptRetryInterval(500*time.Millisecond),
		WithPromptSleepFn(func(d time.Duration) { sleepDurations = append(sleepDurations, d) }),
	)

	d.DetectPrompt("worker1")
	// 3 attempts: sleep before 2nd and 3rd
	if len(sleepDurations) != 2 {
		t.Fatalf("expected 2 sleeps, got %d", len(sleepDurations))
	}
	for i, d := range sleepDurations {
		if d != 500*time.Millisecond {
			t.Errorf("sleep[%d] = %v, want 500ms", i, d)
		}
	}
}

func TestPromptDetector_NoErrorButNotDetected(t *testing.T) {
	t.Parallel()
	d := NewPromptDetector(
		func(string) (bool, error) { return false, nil },
		WithPromptSleepFn(func(time.Duration) {}),
	)

	detected, err := d.DetectPrompt("worker1")
	if detected {
		t.Error("expected not detected")
	}
	if err == nil {
		t.Error("expected error for not detected")
	}
	if d.IsHealthy("worker1") {
		t.Error("expected unhealthy")
	}
}

// ---------------------------------------------------------------------------
// AgentPIDTracker tests
// ---------------------------------------------------------------------------

func TestAgentPIDTracker_FirstObservation(t *testing.T) {
	t.Parallel()
	tr := NewAgentPIDTracker(
		func(agentID string) (int, string, error) {
			return 1234, "start-1", nil
		},
	)

	restarted, err := tr.CheckForRestart("worker1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if restarted {
		t.Error("first observation should not report restart")
	}

	pid, st, exists := tr.LastRecord("worker1")
	if !exists || pid != 1234 || st != "start-1" {
		t.Errorf("record = (%d, %q, %v), want (1234, start-1, true)", pid, st, exists)
	}
}

func TestAgentPIDTracker_NoPIDChange(t *testing.T) {
	t.Parallel()
	tr := NewAgentPIDTracker(
		func(agentID string) (int, string, error) {
			return 1234, "start-1", nil
		},
	)

	// First call: record
	tr.CheckForRestart("worker1")

	// Second call: same PID and start time → no restart
	restarted, err := tr.CheckForRestart("worker1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if restarted {
		t.Error("same PID should not report restart")
	}
}

func TestAgentPIDTracker_DetectsRestart_PIDChange(t *testing.T) {
	t.Parallel()
	var callCount int32
	tr := NewAgentPIDTracker(
		func(agentID string) (int, string, error) {
			c := atomic.AddInt32(&callCount, 1)
			if c == 1 {
				return 1234, "start-1", nil
			}
			return 5678, "start-2", nil // different PID
		},
	)

	// First call: record
	tr.CheckForRestart("worker1")

	// Second call: PID changed → restart detected
	restarted, err := tr.CheckForRestart("worker1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !restarted {
		t.Error("PID change should be detected as restart")
	}

	// Record should be updated
	pid, _, _ := tr.LastRecord("worker1")
	if pid != 5678 {
		t.Errorf("updated PID = %d, want 5678", pid)
	}
}

func TestAgentPIDTracker_DetectsRestart_StartTimeChange(t *testing.T) {
	t.Parallel()
	var callCount int32
	tr := NewAgentPIDTracker(
		func(agentID string) (int, string, error) {
			c := atomic.AddInt32(&callCount, 1)
			if c == 1 {
				return 1234, "Mon Jan 1 00:00:00 2024", nil
			}
			return 1234, "Tue Jan 2 00:00:00 2024", nil // same PID, different start
		},
	)

	tr.CheckForRestart("worker1")

	restarted, err := tr.CheckForRestart("worker1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !restarted {
		t.Error("start time change should be detected as restart")
	}
}

func TestAgentPIDTracker_ErrorFromQuerier(t *testing.T) {
	t.Parallel()
	tr := NewAgentPIDTracker(
		func(agentID string) (int, string, error) {
			return 0, "", fmt.Errorf("process not found")
		},
	)

	_, err := tr.CheckForRestart("worker1")
	if err == nil {
		t.Error("expected error from querier")
	}
}

func TestAgentPIDTracker_UnknownAgent(t *testing.T) {
	t.Parallel()
	tr := NewAgentPIDTracker(
		func(string) (int, string, error) { return 1234, "s", nil },
	)

	_, _, exists := tr.LastRecord("nonexistent")
	if exists {
		t.Error("expected no record for unknown agent")
	}
}

func TestAgentPIDTracker_ManualRecord(t *testing.T) {
	t.Parallel()
	tr := NewAgentPIDTracker(
		func(string) (int, string, error) { return 9999, "new-start", nil },
	)

	// Manually record a PID
	tr.Record("worker1", 1234, "old-start")

	pid, st, exists := tr.LastRecord("worker1")
	if !exists || pid != 1234 || st != "old-start" {
		t.Errorf("record = (%d, %q, %v), want (1234, old-start, true)", pid, st, exists)
	}

	// CheckForRestart should detect the change
	restarted, err := tr.CheckForRestart("worker1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !restarted {
		t.Error("expected restart: PID changed from 1234 to 9999")
	}
}

func TestAgentPIDTracker_EmptyStartTimesSkipComparison(t *testing.T) {
	t.Parallel()
	tr := NewAgentPIDTracker(
		func(string) (int, string, error) {
			return 1234, "", nil // empty start time
		},
	)

	tr.CheckForRestart("worker1")

	// Same PID, both empty start times → no restart
	restarted, err := tr.CheckForRestart("worker1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if restarted {
		t.Error("empty start times should not trigger restart detection")
	}
}
