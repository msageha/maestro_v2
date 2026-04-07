package daemon

import (
	"bytes"
	"log"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/model"
)

// TestStepCircuitBreaker_NilStateReader verifies that stepCircuitBreaker
// does not panic when the circuit breaker is enabled but no StateReader
// has been wired (e.g. during early startup or in degraded configurations).
func TestStepCircuitBreaker_NilStateReader(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	// Wire a circuit breaker that is Enabled but has no StateReader.
	cfg := model.Config{
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:                true,
			ProgressTimeoutMinutes: model.IntPtr(30),
		},
	}
	cb := circuitbreaker.NewHandler(cfg, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	if cb.StateReader() != nil {
		t.Fatalf("precondition: StateReader should be nil")
	}
	qh.SetCircuitBreaker(cb)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
					{ID: "cmd2", Status: model.StatusPending},
				},
			},
		},
	}

	// Must not panic on nil StateReader().
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("stepCircuitBreaker panicked with nil StateReader: %v", r)
		}
	}()
	qh.stepCircuitBreaker(&s)
}
