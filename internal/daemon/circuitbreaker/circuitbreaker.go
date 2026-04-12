// Package circuitbreaker provides circuit breaker logic for command failure detection.
package circuitbreaker

import (
	"fmt"
	"log"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// Handler evaluates circuit breaker conditions for commands.
type Handler struct {
	core.LogMixin
	config       model.Config
	logger       *log.Logger
	logLevel     core.LogLevel
	clock        core.Clock
	stateManager core.StateManager
}

// NewHandler creates a new Handler.
func NewHandler(cfg model.Config, logger *log.Logger, logLevel core.LogLevel) *Handler {
	return &Handler{
		LogMixin: core.LogMixin{DL: core.NewDaemonLoggerFromLegacy("circuit_breaker", logger, logLevel)},
		config:   cfg,
		logger:   logger,
		logLevel: logLevel,
		clock:    core.RealClock{},
	}
}

// SetStateReader wires the state manager for circuit breaker state queries.
func (cb *Handler) SetStateReader(reader core.StateManager) {
	cb.stateManager = reader
}

// Enabled returns whether the circuit breaker is enabled in config.
func (cb *Handler) Enabled() bool {
	return cb.config.CircuitBreaker.Enabled
}

// UpdateCounterOnResult updates the circuit breaker counter based on a task result.
// Called from resultWritePhaseB where the state:{commandID} lock is already held.
// The state pointer is mutated in-place and saved by the caller.
// Returns (tripped, tripReason) if the breaker should trip.
func (cb *Handler) UpdateCounterOnResult(
	state *model.CommandState,
	resultStatus model.Status,
	resultID string,
	now time.Time,
) (bool, string) {
	if !cb.config.CircuitBreaker.Enabled {
		return false, ""
	}

	// Idempotency guard: skip if this result was already applied
	if state.AppliedResultIDs != nil {
		for _, appliedID := range state.AppliedResultIDs {
			if appliedID == resultID {
				return false, ""
			}
		}
	}

	nowStr := now.UTC().Format(time.RFC3339)

	switch resultStatus {
	case model.StatusCompleted:
		if state.CircuitBreaker.ConsecutiveFailures > 0 {
			state.CircuitBreaker.ConsecutiveFailures--
		}
		state.CircuitBreaker.LastProgressAt = &nowStr
		return false, ""

	case model.StatusFailed:
		state.CircuitBreaker.ConsecutiveFailures++
		cb.Log(core.LogLevelInfo, "circuit_breaker_counter command=%s consecutive_failures=%d",
			state.CommandID, state.CircuitBreaker.ConsecutiveFailures)

		threshold := cb.config.CircuitBreaker.EffectiveMaxConsecutiveFailures()
		if state.CircuitBreaker.ConsecutiveFailures >= threshold {
			reason := fmt.Sprintf("consecutive_failures=%d reached threshold=%d",
				state.CircuitBreaker.ConsecutiveFailures, threshold)
			return true, reason
		}
		return false, ""
	}

	return false, ""
}

// TripBreaker sets the circuit breaker to tripped state and issues cancel on the command state.
// Called from resultWritePhaseB where the state:{commandID} lock is already held.
// The state pointer is mutated in-place and saved by the caller.
func (cb *Handler) TripBreaker(state *model.CommandState, reason string, now time.Time) {
	if state.CircuitBreaker.Tripped {
		return // already tripped
	}

	nowStr := now.UTC().Format(time.RFC3339)
	state.CircuitBreaker.Tripped = true
	state.CircuitBreaker.TrippedAt = &nowStr
	state.CircuitBreaker.TripReason = &reason

	// Set cancel request so the existing cancel flow handles task cancellation
	if !state.Cancel.Requested {
		by := "circuit_breaker"
		state.Cancel.Requested = true
		state.Cancel.RequestedAt = &nowStr
		state.Cancel.RequestedBy = &by
		state.Cancel.Reason = &reason
	}

	cb.Log(core.LogLevelWarn, "circuit_breaker_tripped command=%s reason=%s", state.CommandID, reason)
}

// CheckProgressTimeout checks if the progress timeout has been exceeded for a command.
// Called from periodicScanPhaseA. Returns (shouldTrip, reason).
func (cb *Handler) CheckProgressTimeout(commandID string) (bool, string) {
	if !cb.config.CircuitBreaker.Enabled {
		return false, ""
	}
	if cb.stateManager == nil {
		return false, ""
	}

	timeoutMin := cb.config.CircuitBreaker.EffectiveProgressTimeoutMinutes()
	if timeoutMin <= 0 {
		return false, ""
	}

	cbState, err := cb.stateManager.GetCircuitBreakerState(commandID)
	if err != nil {
		cb.Log(core.LogLevelWarn, "circuit_breaker_state_read command=%s error=%v", commandID, err)
		return false, ""
	}

	if cbState.Tripped {
		return false, "" // already tripped
	}

	if cbState.LastProgressAt == nil {
		return false, "" // no progress recorded yet; don't trip on new commands
	}

	lastProgress, err := time.Parse(time.RFC3339, *cbState.LastProgressAt)
	if err != nil {
		cb.Log(core.LogLevelWarn, "circuit_breaker_parse_time command=%s error=%v", commandID, err)
		return false, ""
	}

	elapsed := cb.clock.Now().Sub(lastProgress)
	if elapsed >= time.Duration(timeoutMin)*time.Minute {
		reason := fmt.Sprintf("progress_timeout=%dm elapsed=%s last_progress=%s",
			timeoutMin, elapsed.Truncate(time.Second), *cbState.LastProgressAt)
		return true, reason
	}

	return false, ""
}

// ProgressTimeoutMinutes returns the effective progress timeout from config.
func (cb *Handler) ProgressTimeoutMinutes() int {
	return cb.config.CircuitBreaker.EffectiveProgressTimeoutMinutes()
}

// StateReader returns the configured state reader for read-only access (may be nil).
func (cb *Handler) StateReader() core.StateReader {
	return cb.stateManager
}

// StateManager returns the configured state manager for read/write access (may be nil).
func (cb *Handler) StateManager() core.StateManager {
	return cb.stateManager
}

