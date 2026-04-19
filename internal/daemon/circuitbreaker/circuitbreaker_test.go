package circuitbreaker

import (
	"bytes"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
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
			MaxConsecutiveFailures: ptr.Int(maxFailures),
			ProgressTimeoutMinutes: ptr.Int(timeoutMin),
		},
	}
	cb := NewHandler(cfg, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)
	return cb
}

func TestUpdateCounterOnResult_Disabled(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(false, 3, 30)
	state := &model.CommandState{CommandID: "cmd1"}

	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusFailed, "t1", "r1", time.Now())
	if tripped {
		t.Error("expected no trip when circuit breaker is disabled")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures when disabled, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
}

func TestUpdateCounterOnResult_IncrementOnFailure(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	state := &model.CommandState{CommandID: "cmd1"}

	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusFailed, "t1", "r1", time.Now())
	if tripped {
		t.Error("expected no trip on first failure")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 1 {
		t.Errorf("expected 1 failure, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
}

func TestUpdateCounterOnResult_DecrementOnSuccess(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			ConsecutiveFailures: 2,
		},
	}

	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusCompleted, "t1", "r1", time.Now())
	if tripped {
		t.Error("expected no trip on success")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 1 {
		t.Errorf("expected 1 failure after decrement, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
	if state.CircuitBreaker.LastProgressAt == nil {
		t.Error("expected LastProgressAt to be set")
	}

	// Second success decrements to 0
	tripped, _ = cb.UpdateCounterOnResult(state, model.StatusCompleted, "t2", "r2", time.Now())
	if tripped {
		t.Error("expected no trip on success")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures after second decrement, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}

	// Third success stays at 0 (floor)
	tripped, _ = cb.UpdateCounterOnResult(state, model.StatusCompleted, "t3", "r3", time.Now())
	if tripped {
		t.Error("expected no trip on success")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures (floor), got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
}

func TestUpdateCounterOnResult_TripOnThreshold(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			ConsecutiveFailures: 2,
		},
	}

	tripped, reason := cb.UpdateCounterOnResult(state, model.StatusFailed, "t3", "r3", time.Now())
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
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	state := &model.CommandState{
		CommandID: "cmd1",
		TaskTracking: model.TaskTracking{
			AppliedResultIDs: map[string]string{
				"task1": "r_existing",
			},
		},
	}

	// Try to apply same result ID that already exists for this task
	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusFailed, "task1", "r_existing", time.Now())
	if tripped {
		t.Error("expected no trip on idempotent result")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures on idempotent, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
}

func TestTripBreaker_SetsStateFields(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	cb := newTestHandler(false, 3, 30)
	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip when disabled")
	}
}

func TestCheckProgressTimeout_NoStateReader(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip when state reader is nil")
	}
}

func TestCheckProgressTimeout_NilUsesDefault(t *testing.T) {
	t.Parallel()
	// nil ProgressTimeoutMinutes should default to 30 minutes
	cfg := model.Config{
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:                true,
			MaxConsecutiveFailures: ptr.Int(3),
			ProgressTimeoutMinutes: nil, // unset → default 30
		},
	}
	cb := NewHandler(cfg, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)
	reader := &mockCircuitBreakerStateReader{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {LastProgressAt: ptr.String(time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339))},
		},
	}
	cb.SetStateReader(reader)

	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if !shouldTrip {
		t.Error("expected trip when timeout is nil (defaults to 30) and last progress was 1 hour ago")
	}
}

func TestCheckProgressTimeout_NotExpired(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	reader := &mockCircuitBreakerStateReader{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {LastProgressAt: ptr.String(time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339))},
		},
	}
	cb.SetStateReader(reader)

	shouldTrip, _ := cb.CheckProgressTimeout("cmd1")
	if shouldTrip {
		t.Error("expected no trip within timeout")
	}
}

func TestCheckProgressTimeout_Expired(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	reader := &mockCircuitBreakerStateReader{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {LastProgressAt: ptr.String(time.Now().Add(-31 * time.Minute).UTC().Format(time.RFC3339))},
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
	t.Parallel()
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
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	reader := &mockCircuitBreakerStateReader{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {
				Tripped:        true,
				LastProgressAt: ptr.String(time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)),
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
	t.Parallel()
	tests := []struct {
		value    *int
		expected int
	}{
		{nil, 3}, // default
		{ptr.Int(1), 1},
		{ptr.Int(5), 5},
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
	t.Parallel()
	tests := []struct {
		value    *int
		expected int
	}{
		{nil, 30},              // nil returns default
		{ptr.Int(30), 30},
		{ptr.Int(60), 60},
	}
	for _, tt := range tests {
		cfg := model.CircuitBreakerConfig{ProgressTimeoutMinutes: tt.value}
		got := cfg.EffectiveProgressTimeoutMinutes()
		if got != tt.expected {
			t.Errorf("EffectiveProgressTimeoutMinutes(%v) = %d, want %d", tt.value, got, tt.expected)
		}
	}
}

func TestUpdateCounterOnResult_NilState(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)

	// Must not panic with nil state.
	tripped, reason := cb.UpdateCounterOnResult(nil, model.StatusFailed, "t1", "r1", time.Now())
	if tripped {
		t.Error("expected no trip on nil state")
	}
	if reason != "" {
		t.Errorf("expected empty reason on nil state, got %q", reason)
	}
}

func TestTripBreaker_NilState(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)

	// Must not panic with nil state.
	cb.TripBreaker(nil, "test reason", time.Now())
}

func newTestHandlerWithHalfOpenDelay(enabled bool, maxFailures, timeoutMin, halfOpenDelaySec int) *Handler {
	cfg := model.Config{
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:                enabled,
			MaxConsecutiveFailures: ptr.Int(maxFailures),
			ProgressTimeoutMinutes: ptr.Int(timeoutMin),
			HalfOpenDelaySec:       ptr.Int(halfOpenDelaySec),
		},
	}
	return NewHandler(cfg, log.New(&bytes.Buffer{}, "", 0), core.LogLevelDebug)
}

func TestCheckHalfOpenTransition_NotTripped(t *testing.T) {
	t.Parallel()
	cb := newTestHandlerWithHalfOpenDelay(true, 3, 30, 10)
	state := &model.CommandState{CommandID: "cmd1"}

	transitioned := cb.CheckHalfOpenTransition(state, time.Now())
	if transitioned {
		t.Error("expected no transition when breaker is not tripped")
	}
}

func TestCheckHalfOpenTransition_TooEarly(t *testing.T) {
	t.Parallel()
	cb := newTestHandlerWithHalfOpenDelay(true, 3, 30, 60)
	now := time.Now()
	trippedAt := now.Add(-30 * time.Second).UTC().Format(time.RFC3339) // 30s ago, delay is 60s
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			Tripped:   true,
			TrippedAt: &trippedAt,
		},
	}

	transitioned := cb.CheckHalfOpenTransition(state, now)
	if transitioned {
		t.Error("expected no transition before half-open delay expires")
	}
}

func TestCheckHalfOpenTransition_Success(t *testing.T) {
	t.Parallel()
	cb := newTestHandlerWithHalfOpenDelay(true, 3, 30, 10)
	now := time.Now()
	trippedAt := now.Add(-15 * time.Second).UTC().Format(time.RFC3339) // 15s ago, delay is 10s
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			Tripped:   true,
			TrippedAt: &trippedAt,
		},
	}

	transitioned := cb.CheckHalfOpenTransition(state, now)
	if !transitioned {
		t.Error("expected transition to half-open after delay")
	}
	if !state.CircuitBreaker.HalfOpen {
		t.Error("expected HalfOpen=true")
	}
	if state.CircuitBreaker.HalfOpenAt == nil {
		t.Error("expected HalfOpenAt to be set")
	}
	if state.CircuitBreaker.HalfOpenProbeActive {
		t.Error("expected HalfOpenProbeActive=false initially")
	}
}

func TestCheckHalfOpenTransition_AlreadyHalfOpen(t *testing.T) {
	t.Parallel()
	cb := newTestHandlerWithHalfOpenDelay(true, 3, 30, 10)
	now := time.Now()
	trippedAt := now.Add(-15 * time.Second).UTC().Format(time.RFC3339)
	halfOpenAt := now.Add(-5 * time.Second).UTC().Format(time.RFC3339)
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			Tripped:   true,
			TrippedAt: &trippedAt,
			HalfOpen:  true,
			HalfOpenAt: &halfOpenAt,
		},
	}

	transitioned := cb.CheckHalfOpenTransition(state, now)
	if transitioned {
		t.Error("expected no transition when already half-open")
	}
}

func TestCheckHalfOpenTransition_Disabled(t *testing.T) {
	t.Parallel()
	cb := newTestHandlerWithHalfOpenDelay(false, 3, 30, 10)
	state := &model.CommandState{CommandID: "cmd1"}

	transitioned := cb.CheckHalfOpenTransition(state, time.Now())
	if transitioned {
		t.Error("expected no transition when disabled")
	}
}

func TestCheckHalfOpenTransition_NilState(t *testing.T) {
	t.Parallel()
	cb := newTestHandlerWithHalfOpenDelay(true, 3, 30, 10)

	transitioned := cb.CheckHalfOpenTransition(nil, time.Now())
	if transitioned {
		t.Error("expected no transition on nil state")
	}
}

func TestAllowProbe_HalfOpen(t *testing.T) {
	t.Parallel()
	cb := newTestHandlerWithHalfOpenDelay(true, 3, 30, 10)
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			HalfOpen: true,
		},
	}

	allowed := cb.AllowProbe(state)
	if !allowed {
		t.Error("expected probe allowed in half-open state")
	}
	if !state.CircuitBreaker.HalfOpenProbeActive {
		t.Error("expected HalfOpenProbeActive=true after AllowProbe")
	}

	// Second call should be rejected (probe already active)
	allowed = cb.AllowProbe(state)
	if allowed {
		t.Error("expected probe rejected when already active")
	}
}

func TestAllowProbe_NotHalfOpen(t *testing.T) {
	t.Parallel()
	cb := newTestHandlerWithHalfOpenDelay(true, 3, 30, 10)
	state := &model.CommandState{CommandID: "cmd1"}

	allowed := cb.AllowProbe(state)
	if allowed {
		t.Error("expected probe not allowed when not half-open")
	}
}

func TestAllowProbe_NilState(t *testing.T) {
	t.Parallel()
	cb := newTestHandlerWithHalfOpenDelay(true, 3, 30, 10)

	allowed := cb.AllowProbe(nil)
	if allowed {
		t.Error("expected probe not allowed on nil state")
	}
}

func TestHalfOpenProbe_Success_ClosesBreaker(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	now := time.Now()
	trippedAt := now.Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	halfOpenAt := now.Add(-5 * time.Second).UTC().Format(time.RFC3339)
	reason := "consecutive_failures=3 reached threshold=3"
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			Tripped:             true,
			TrippedAt:           &trippedAt,
			TripReason:          &reason,
			ConsecutiveFailures: 3,
			HalfOpen:            true,
			HalfOpenAt:          &halfOpenAt,
			HalfOpenProbeActive: true,
		},
	}

	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusCompleted, "probe_task", "r_probe", now)
	if tripped {
		t.Error("expected no trip on probe success")
	}
	if state.CircuitBreaker.Tripped {
		t.Error("expected Tripped=false after probe success")
	}
	if state.CircuitBreaker.HalfOpen {
		t.Error("expected HalfOpen=false after probe success")
	}
	if state.CircuitBreaker.HalfOpenProbeActive {
		t.Error("expected HalfOpenProbeActive=false after probe success")
	}
	if state.CircuitBreaker.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures after probe success, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}
	if state.CircuitBreaker.TrippedAt != nil {
		t.Error("expected TrippedAt=nil after probe success")
	}
	if state.CircuitBreaker.TripReason != nil {
		t.Error("expected TripReason=nil after probe success")
	}
}

func TestHalfOpenProbe_Failure_ReopensBreaker(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	now := time.Now()
	trippedAt := now.Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	halfOpenAt := now.Add(-5 * time.Second).UTC().Format(time.RFC3339)
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			Tripped:             true,
			TrippedAt:           &trippedAt,
			ConsecutiveFailures: 3,
			HalfOpen:            true,
			HalfOpenAt:          &halfOpenAt,
			HalfOpenProbeActive: true,
		},
	}

	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusFailed, "probe_task", "r_probe", now)
	if tripped {
		t.Error("expected no trip signal on probe failure (already tripped)")
	}
	if !state.CircuitBreaker.Tripped {
		t.Error("expected Tripped=true (breaker stays tripped)")
	}
	if state.CircuitBreaker.HalfOpen {
		t.Error("expected HalfOpen=false after probe failure (re-opened)")
	}
	if state.CircuitBreaker.HalfOpenProbeActive {
		t.Error("expected HalfOpenProbeActive=false after probe failure")
	}
	// TrippedAt should be reset to enable next half-open timer
	if state.CircuitBreaker.TrippedAt == nil {
		t.Error("expected TrippedAt to be reset for next half-open cycle")
	}
	if state.CircuitBreaker.TripReason == nil || *state.CircuitBreaker.TripReason != "half_open_probe_failed" {
		t.Errorf("expected TripReason='half_open_probe_failed', got %v", state.CircuitBreaker.TripReason)
	}
}

func TestTripBreaker_ResetsHalfOpenState(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	halfOpenAt := time.Now().UTC().Format(time.RFC3339)
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			HalfOpen:            true,
			HalfOpenAt:          &halfOpenAt,
			HalfOpenProbeActive: true,
		},
	}

	cb.TripBreaker(state, "new trip reason", time.Now())

	if state.CircuitBreaker.HalfOpen {
		t.Error("expected HalfOpen=false after trip")
	}
	if state.CircuitBreaker.HalfOpenAt != nil {
		t.Error("expected HalfOpenAt=nil after trip")
	}
	if state.CircuitBreaker.HalfOpenProbeActive {
		t.Error("expected HalfOpenProbeActive=false after trip")
	}
}

func TestCheckHalfOpenTransition_DefaultDelay(t *testing.T) {
	t.Parallel()
	// nil HalfOpenDelaySec should use default (60s)
	cb := newTestHandler(true, 3, 30) // uses nil HalfOpenDelaySec
	now := time.Now()
	trippedAt := now.Add(-61 * time.Second).UTC().Format(time.RFC3339)
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			Tripped:   true,
			TrippedAt: &trippedAt,
		},
	}

	transitioned := cb.CheckHalfOpenTransition(state, now)
	if !transitioned {
		t.Error("expected transition with default 60s delay (61s elapsed)")
	}
}

func TestCheckHalfOpenTransition_DefaultDelay_TooEarly(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30) // uses nil HalfOpenDelaySec → default 60s
	now := time.Now()
	trippedAt := now.Add(-30 * time.Second).UTC().Format(time.RFC3339)
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			Tripped:   true,
			TrippedAt: &trippedAt,
		},
	}

	transitioned := cb.CheckHalfOpenTransition(state, now)
	if transitioned {
		t.Error("expected no transition before default 60s delay (30s elapsed)")
	}
}

func TestEffectiveHalfOpenDelaySec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value    *int
		expected int
	}{
		{nil, 60},            // default
		{ptr.Int(10), 10},
		{ptr.Int(120), 120},
	}
	for _, tt := range tests {
		cfg := model.CircuitBreakerConfig{HalfOpenDelaySec: tt.value}
		got := cfg.EffectiveHalfOpenDelaySec()
		if got != tt.expected {
			t.Errorf("EffectiveHalfOpenDelaySec(%v) = %d, want %d", tt.value, got, tt.expected)
		}
	}
}

func TestUpdateCounterOnResult_WritesAppliedResultIDs(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)

	tests := []struct {
		name     string
		status   model.Status
		taskID   string
		resultID string
	}{
		{"completed", model.StatusCompleted, "t_completed", "r_completed"},
		{"failed", model.StatusFailed, "t_failed", "r_failed"},
		{"cancelled", model.StatusCancelled, "t_cancelled", "r_cancelled"},
		{"pending", model.StatusPending, "t_pending", "r_pending"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &model.CommandState{CommandID: "cmd1"}

			cb.UpdateCounterOnResult(state, tt.status, tt.taskID, tt.resultID, time.Now())

			if state.AppliedResultIDs == nil {
				t.Fatal("expected AppliedResultIDs to be initialized")
			}
			if got, ok := state.AppliedResultIDs[tt.taskID]; !ok || got != tt.resultID {
				t.Errorf("AppliedResultIDs[%s] = %q, want %q", tt.taskID, got, tt.resultID)
			}
		})
	}
}

func TestUpdateCounterOnResult_WritesAppliedResultIDs_HalfOpenProbe(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	now := time.Now()
	halfOpenAt := now.Add(-5 * time.Second).UTC().Format(time.RFC3339)

	// Half-open probe success
	state := &model.CommandState{
		CommandID: "cmd1",
		CircuitBreaker: model.CircuitBreakerState{
			Tripped:             true,
			HalfOpen:            true,
			HalfOpenAt:          &halfOpenAt,
			HalfOpenProbeActive: true,
		},
	}
	cb.UpdateCounterOnResult(state, model.StatusCompleted, "probe_ok", "r_probe_ok", now)

	if state.AppliedResultIDs == nil {
		t.Fatal("expected AppliedResultIDs to be initialized after half-open probe success")
	}
	if got := state.AppliedResultIDs["probe_ok"]; got != "r_probe_ok" {
		t.Errorf("AppliedResultIDs[probe_ok] = %q, want r_probe_ok", got)
	}

	// Half-open probe failure
	state2 := &model.CommandState{
		CommandID: "cmd2",
		CircuitBreaker: model.CircuitBreakerState{
			Tripped:             true,
			HalfOpen:            true,
			HalfOpenAt:          &halfOpenAt,
			HalfOpenProbeActive: true,
		},
	}
	cb.UpdateCounterOnResult(state2, model.StatusFailed, "probe_fail", "r_probe_fail", now)

	if state2.AppliedResultIDs == nil {
		t.Fatal("expected AppliedResultIDs to be initialized after half-open probe failure")
	}
	if got := state2.AppliedResultIDs["probe_fail"]; got != "r_probe_fail" {
		t.Errorf("AppliedResultIDs[probe_fail] = %q, want r_probe_fail", got)
	}
}

func TestUpdateCounterOnResult_IdempotentAfterWrite(t *testing.T) {
	t.Parallel()
	cb := newTestHandler(true, 3, 30)
	state := &model.CommandState{CommandID: "cmd1"}

	// First call: records the result and increments failure counter
	cb.UpdateCounterOnResult(state, model.StatusFailed, "t1", "r1", time.Now())
	if state.CircuitBreaker.ConsecutiveFailures != 1 {
		t.Fatalf("expected 1 failure after first call, got %d", state.CircuitBreaker.ConsecutiveFailures)
	}

	// Second call with same taskID+resultID: should be idempotent (no double counting)
	cb.UpdateCounterOnResult(state, model.StatusFailed, "t1", "r1", time.Now())
	if state.CircuitBreaker.ConsecutiveFailures != 1 {
		t.Errorf("expected 1 failure after idempotent call, got %d (double counting detected)",
			state.CircuitBreaker.ConsecutiveFailures)
	}
}

func TestFullHalfOpenLifecycle(t *testing.T) {
	t.Parallel()
	cb := newTestHandlerWithHalfOpenDelay(true, 3, 30, 5)
	now := time.Now()

	// Step 1: Build up failures to trip the breaker
	state := &model.CommandState{CommandID: "cmd1"}
	for i := 0; i < 3; i++ {
		tripped, reason := cb.UpdateCounterOnResult(state, model.StatusFailed, fmt.Sprintf("t%d", i), fmt.Sprintf("r%d", i), now)
		if i < 2 && tripped {
			t.Fatalf("unexpected trip at failure %d", i+1)
		}
		if i == 2 {
			if !tripped {
				t.Fatal("expected trip at 3rd failure")
			}
			cb.TripBreaker(state, reason, now)
		}
	}

	// Step 2: Verify tripped state
	if !state.CircuitBreaker.Tripped {
		t.Fatal("expected Tripped=true")
	}

	// Step 3: Too early for half-open
	transitioned := cb.CheckHalfOpenTransition(state, now.Add(3*time.Second))
	if transitioned {
		t.Fatal("expected no transition at 3s (delay=5s)")
	}

	// Step 4: After delay, transition to half-open
	transitioned = cb.CheckHalfOpenTransition(state, now.Add(6*time.Second))
	if !transitioned {
		t.Fatal("expected transition to half-open at 6s")
	}

	// Step 5: Allow probe
	allowed := cb.AllowProbe(state)
	if !allowed {
		t.Fatal("expected probe allowed")
	}

	// Step 6: Probe success → close breaker
	tripped, _ := cb.UpdateCounterOnResult(state, model.StatusCompleted, "probe1", "rp1", now.Add(7*time.Second))
	if tripped {
		t.Fatal("unexpected trip on probe success")
	}
	if state.CircuitBreaker.Tripped || state.CircuitBreaker.HalfOpen {
		t.Fatal("expected breaker closed after probe success")
	}
}
