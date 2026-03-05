package daemon

import (
	"bytes"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

func newTestCircuitBreakerHandler(enabled bool, maxFailures, timeoutMin int) *CircuitBreakerHandler {
	cfg := model.Config{
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:                enabled,
			MaxConsecutiveFailures: maxFailures,
			ProgressTimeoutMinutes: timeoutMin,
		},
	}
	cb := NewCircuitBreakerHandler(cfg, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	return cb
}

func TestUpdateCounterOnResult_Disabled(t *testing.T) {
	cb := newTestCircuitBreakerHandler(false, 3, 30)
	state := &model.CommandState{CommandID: "cmd1"}

	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusFailed, "r1", time.Now())
	if tripped {
		t.Error("expected no trip when circuit breaker is disabled")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures when disabled, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
}

func TestUpdateCounterOnResult_IncrementOnFailure(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	state := &model.CommandState{CommandID: "cmd1"}

	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusFailed, "r1", time.Now())
	if tripped {
		t.Error("expected no trip on first failure")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 1 {
		t.Errorf("expected 1 failure, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
}

func TestUpdateCounterOnResult_ResetOnSuccess(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			ConsecutiveFailures: 2,
		},
	}

	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusCompleted, "r1", time.Now())
	if tripped {
		t.Error("expected no trip on success")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures after success, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
	if state.CircuitBreaker.LastProgressAt == nil {
		t.Error("expected LastProgressAt to be set")
	}
}

func TestUpdateCounterOnResult_TripOnThreshold(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			ConsecutiveFailures: 2,
		},
	}

	tripped, reason := cb.UpdateCounterOnResult(state, model.StatusFailed, "r3", time.Now())
	if !tripped {
		t.Error("expected trip at threshold")
	}
	if reason == "" {
		t.Error("expected non-empty trip reason")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 3 {
		t.Errorf("expected 3 failures, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
}

func TestUpdateCounterOnResult_IdempotentSkip(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	state := &model.CommandState{
		CommandID: "cmd1",
		AppliedResultIDs: map[string]string{
			"task1": "r_existing",
		},
	}

	// Try to apply same result ID that already exists
	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusFailed, "r_existing", time.Now())
	if tripped {
		t.Error("expected no trip on idempotent result")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures on idempotent, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
}

func TestTripBreaker_SetsStateFields(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	state := &model.CommandState{CommandID: "cmd1"}

	now := time.Now()
	cb.TripBreaker(state, "test reason", now)

	if !state.CircuitBreaker.Tripped {
		t.Error("expected Tripped=true")
	}
	if state.CircuitBreaker.TrippedAt == nil {
		t.Error("expected TrippedAt to be set")
	}
	if state.CircuitBreaker.TripReason == nil || *state.CircuitBreaker.TripReason != "test reason" {
		t.Errorf("expected TripReason='test reason', got %v", state.CircuitBreaker.TripReason)
	}
	if !state.Cancel.Requested {
		t.Error("expected Cancel.Requested=true")
	}
	if state.Cancel.RequestedBy == nil || *state.Cancel.RequestedBy != "circuit_breaker" {
		t.Error("expected Cancel.RequestedBy='circuit_breaker'")
	}
}

func TestTripBreaker_IdempotentWhenAlreadyTripped(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	firstTrippedAt := "2025-01-01T00:00:00Z"
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			Tripped:   true,
			TrippedAt: &firstTrippedAt,
		},
	}

	cb.TripBreaker(state, "second reason", time.Now())

	// Should not change the original trip time
	if *state.CircuitBreaker.TrippedAt != firstTrippedAt {
		t.Errorf("expected original TrippedAt to be preserved, got %s", *state.CircuitBreaker.TrippedAt)
	}
}

func TestTripBreaker_DoesNotOverwriteExistingCancel(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	existingBy := "user"
	existingReason := "user cancelled"
	existingAt := "2025-01-01T00:00:00Z"
	state := &model.CommandState{
		CommandID: "cmd1",
		Cancel: model.CancelState{
			Requested:   true,
			RequestedBy: &existingBy,
			Reason:      &existingReason,
			RequestedAt: &existingAt,
		},
	}

	cb.TripBreaker(state, "circuit breaker reason", time.Now())

	if *state.Cancel.RequestedBy != "user" {
		t.Errorf("expected existing cancel to be preserved, got %s", *state.Cancel.RequestedBy)
	}
}

// mockCircuitBreakerStateReader provides controllable circuit breaker state for testing.
type mockCircuitBreakerStateReader struct {
	mockStateReader
	cbStates      map[string]*model.CircuitBreakerState
	trippedCmds   map[string]string
}

func (m *mockCircuitBreakerStateReader) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	if s, ok := m.cbStates[commandID]; ok {
		return s, nil
	}
	return &model.CircuitBreakerState{}, nil
}

func (m *mockCircuitBreakerStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	if m.trippedCmds == nil {
		m.trippedCmds = make(map[string]string)
	}
	m.trippedCmds[commandID] = reason
	return nil
}

func TestCheckProgressTimeout_Disabled(t *testing.T) {
	cb := newTestCircuitBreakerHandler(false, 3, 30)
	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip when disabled")
	}
}

func TestCheckProgressTimeout_NoStateReader(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip when state reader is nil")
	}
}

func TestCheckProgressTimeout_TimeoutDisabled(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 0)
	reader := &mockCircuitBreakerStateReader{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {LastProgressAt: strPtr(time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339))},
		},
	}
	cb.SetStateReader(reader)

	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip when timeout is disabled (0)")
	}
}

func TestCheckProgressTimeout_NotExpired(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	reader := &mockCircuitBreakerStateReader{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {LastProgressAt: strPtr(time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339))},
		},
	}
	cb.SetStateReader(reader)

	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip within timeout")
	}
}

func TestCheckProgressTimeout_Expired(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	reader := &mockCircuitBreakerStateReader{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {LastProgressAt: strPtr(time.Now().Add(-31 * time.Minute).UTC().Format(time.RFC3339))},
		},
	}
	cb.SetStateReader(reader)

	shouldTrip, reason := cb.CheckProgressTimeout("cmd1")
	if !shouldTrip {
		t.Error("expected trip when timeout expired")
	}
	if reason == "" {
		t.Error("expected non-empty trip reason")
	}
}

func TestCheckProgressTimeout_NilLastProgressAt(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	reader := &mockCircuitBreakerStateReader{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {},
		},
	}
	cb.SetStateReader(reader)

	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip when LastProgressAt is nil (new command)")
	}
}

func TestCheckProgressTimeout_AlreadyTripped(t *testing.T) {
	cb := newTestCircuitBreakerHandler(true, 3, 30)
	reader := &mockCircuitBreakerStateReader{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {
				Tripped:        true,
				LastProgressAt: strPtr(time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)),
			},
		},
	}
	cb.SetStateReader(reader)

	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip when already tripped")
	}
}

func TestConfigEffectiveMaxConsecutiveFailures(t *testing.T) {
	tests := []struct {
		value    int
		expected int
	}{
		{0, 3},  // default
		{1, 1},
		{5, 5},
	}
	for _, tt := range tests {
		cfg := model.CircuitBreakerConfig{MaxConsecutiveFailures: tt.value}
		got := cfg.EffectiveMaxConsecutiveFailures()
		if got != tt.expected {
			t.Errorf("EffectiveMaxConsecutiveFailures(%d) = %d, want %d", tt.value, got, tt.expected)
		}
	}
}

func TestConfigEffectiveProgressTimeoutMinutes(t *testing.T) {
	tests := []struct {
		value    int
		expected int
	}{
		{0, 0},   // disabled
		{30, 30},
		{60, 60},
	}
	for _, tt := range tests {
		cfg := model.CircuitBreakerConfig{ProgressTimeoutMinutes: tt.value}
		got := cfg.EffectiveProgressTimeoutMinutes()
		if got != tt.expected {
			t.Errorf("EffectiveProgressTimeoutMinutes(%d) = %d, want %d", tt.value, got, tt.expected)
		}
	}
}

func strPtr(s string) *string { return &s }
