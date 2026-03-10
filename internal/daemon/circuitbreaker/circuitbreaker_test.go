package circuitbreaker

import (
	"bytes"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// mockStateReader implements core.StateReader for testing.
type mockStateReader struct {
	taskStates        map[string]model.Status
	phases            map[string][]core.PhaseInfo
	deps              map[string][]string
	systemCommitReady map[string][2]bool
}

func (m *mockStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	key := commandID + ":" + taskID
	s, ok := m.taskStates[key]
	if !ok {
		return "", fmt.Errorf("task %s not found in command %s", taskID, commandID)
	}
	return s, nil
}

func (m *mockStateReader) GetCommandPhases(commandID string) ([]core.PhaseInfo, error) {
	p, ok := m.phases[commandID]
	if !ok {
		return nil, fmt.Errorf("command %s not found", commandID)
	}
	return p, nil
}

func (m *mockStateReader) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	key := commandID + ":" + taskID
	return m.deps[key], nil
}

func (m *mockStateReader) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	return nil
}

func (m *mockStateReader) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	if m.taskStates == nil {
		m.taskStates = make(map[string]model.Status)
	}
	m.taskStates[commandID+":"+taskID] = newStatus
	return nil
}

func (m *mockStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	return false, nil
}

func (m *mockStateReader) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	return &model.CircuitBreakerState{}, nil
}

func (m *mockStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	return nil
}

func (m *mockStateReader) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	if m.systemCommitReady == nil {
		return false, false, nil
	}
	key := commandID + ":" + taskID
	v, ok := m.systemCommitReady[key]
	if !ok {
		return false, false, nil
	}
	return v[0], v[1], nil
}

// mockCircuitBreakerStateReader provides controllable circuit breaker state for testing.
type mockCircuitBreakerStateReader struct {
	mockStateReader
	cbStates    map[string]*model.CircuitBreakerState
	trippedCmds map[string]string
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

func newTestHandler(enabled bool, maxFailures, timeoutMin int) *Handler {
	cfg := model.Config{
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:                enabled,
			MaxConsecutiveFailures: model.IntPtr(maxFailures),
			ProgressTimeoutMinutes: model.IntPtr(timeoutMin),
		},
	}
	cb := NewHandler(cfg, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)
	return cb
}

func TestUpdateCounterOnResult_Disabled(t *testing.T) {
	cb := newTestHandler(false, 3, 30)
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
	cb := newTestHandler(true, 3, 30)
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
	cb := newTestHandler(true, 3, 30)
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
	cb := newTestHandler(true, 3, 30)
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
	cb := newTestHandler(true, 3, 30)
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
	cb := newTestHandler(true, 3, 30)
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
	cb := newTestHandler(true, 3, 30)
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
	cb := newTestHandler(true, 3, 30)
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

func TestCheckProgressTimeout_Disabled(t *testing.T) {
	cb := newTestHandler(false, 3, 30)
	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip when disabled")
	}
}

func TestCheckProgressTimeout_NoStateReader(t *testing.T) {
	cb := newTestHandler(true, 3, 30)
	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip when state reader is nil")
	}
}

func TestCheckProgressTimeout_NilUsesDefault(t *testing.T) {
	// nil ProgressTimeoutMinutes should default to 30 minutes
	cfg := model.Config{
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:                true,
			MaxConsecutiveFailures: model.IntPtr(3),
			ProgressTimeoutMinutes: nil, // unset → default 30
		},
	}
	cb := NewHandler(cfg, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)
	reader := &mockCircuitBreakerStateReader{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {LastProgressAt: strPtr(time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339))},
		},
	}
	cb.SetStateReader(reader)

	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if !shouldTrip {
		t.Error("expected trip when timeout is nil (defaults to 30) and last progress was 1 hour ago")
	}
}

func TestCheckProgressTimeout_NotExpired(t *testing.T) {
	cb := newTestHandler(true, 3, 30)
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
	cb := newTestHandler(true, 3, 30)
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
	cb := newTestHandler(true, 3, 30)
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
	cb := newTestHandler(true, 3, 30)
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
		value    *int
		expected int
	}{
		{nil, 3}, // default
		{model.IntPtr(1), 1},
		{model.IntPtr(5), 5},
	}
	for _, tt := range tests {
		cfg := model.CircuitBreakerConfig{MaxConsecutiveFailures: tt.value}
		got := cfg.EffectiveMaxConsecutiveFailures()
		if got != tt.expected {
			t.Errorf("EffectiveMaxConsecutiveFailures(%v) = %d, want %d", tt.value, got, tt.expected)
		}
	}
}

func TestConfigEffectiveProgressTimeoutMinutes(t *testing.T) {
	tests := []struct {
		value    *int
		expected int
	}{
		{nil, 30},              // nil returns default
		{model.IntPtr(30), 30},
		{model.IntPtr(60), 60},
	}
	for _, tt := range tests {
		cfg := model.CircuitBreakerConfig{ProgressTimeoutMinutes: tt.value}
		got := cfg.EffectiveProgressTimeoutMinutes()
		if got != tt.expected {
			t.Errorf("EffectiveProgressTimeoutMinutes(%v) = %d, want %d", tt.value, got, tt.expected)
		}
	}
}

func strPtr(s string) *string { return &s }
