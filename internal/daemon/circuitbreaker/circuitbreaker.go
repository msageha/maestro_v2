// Package circuitbreaker provides circuit breaker logic for command failure detection.
package circuitbreaker

import (
	"errors"
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
	taskID string,
	resultID string,
	now time.Time,
) (bool, string) {
	if state == nil {
		return false, ""
	}
	if !cb.config.CircuitBreaker.Enabled {
		return false, ""
	}

	// Idempotency guard: skip if this result was already applied for this task (O(1) lookup)
	if appliedID, ok := state.AppliedResultIDs[taskID]; ok && appliedID == resultID {
		return false, ""
	}

	// Record this result ID for idempotency before processing.
	// This ensures all code paths (Completed, Failed, Cancelled, half-open probes)
	// mark the result as applied.
	if state.AppliedResultIDs == nil {
		state.AppliedResultIDs = make(map[string]string)
	}
	state.AppliedResultIDs[taskID] = resultID

	nowStr := now.UTC().Format(time.RFC3339)

	// Handle half-open probe results
	if state.CircuitBreaker.HalfOpen && state.CircuitBreaker.HalfOpenProbeActive {
		switch resultStatus {
		case model.StatusCompleted:
			// Probe succeeded → close the breaker
			state.CircuitBreaker.HalfOpen = false
			state.CircuitBreaker.HalfOpenAt = nil
			state.CircuitBreaker.HalfOpenProbeActive = false
			state.CircuitBreaker.Tripped = false
			state.CircuitBreaker.TrippedAt = nil
			state.CircuitBreaker.TripReason = nil
			state.CircuitBreaker.ConsecutiveFailures = 0
			state.CircuitBreaker.LastProgressAt = &nowStr
			cb.Log(core.LogLevelInfo, "circuit_breaker_half_open_probe_success command=%s closing_breaker", state.CommandID)
			return false, ""

		case model.StatusFailed:
			// Probe failed → re-open (reset timer for next half-open attempt)
			state.CircuitBreaker.HalfOpen = false
			state.CircuitBreaker.HalfOpenAt = nil
			state.CircuitBreaker.HalfOpenProbeActive = false
			state.CircuitBreaker.TrippedAt = &nowStr // reset trip time for next half-open delay
			reason := "half_open_probe_failed"
			state.CircuitBreaker.TripReason = &reason
			cb.Log(core.LogLevelInfo, "circuit_breaker_half_open_probe_failed command=%s reopening_breaker", state.CommandID)
			return false, ""
		}
	}

	switch resultStatus {
	case model.StatusCompleted:
		if state.CircuitBreaker.ConsecutiveFailures > 0 {
			state.CircuitBreaker.ConsecutiveFailures--
		}
		state.CircuitBreaker.LastProgressAt = &nowStr
		return false, ""

	case model.StatusFailed:
		// Lineage-aware count: when a task has a retry_lineage ancestor that
		// is already in a non-success terminal state, treat this as the same
		// root-cause failure and skip the counter increment to avoid tripping
		// the breaker on a single stuck lineage retried multiple times.
		if isLineageAlreadyFailed(taskID, state) {
			cb.Log(core.LogLevelInfo,
				"circuit_breaker_lineage_failure_skipped command=%s task=%s "+
					"(predecessor in retry_lineage already failed; not double-counting)",
				state.CommandID, taskID)
			return false, ""
		}
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

// isLineageAlreadyFailed reports whether any predecessor in the retry
// lineage of taskID is already at a non-success terminal state
// (StatusFailed or StatusCancelled) in TaskStates. Used by the
// failure-counter path to avoid double-counting the same root-cause
// failure across automatic retries. Walks at most len(RetryLineage)
// ancestors with a visited map so corrupted state cannot loop.
//
// Both StatusFailed and StatusCancelled qualify because cascade-recovery
// / verify-repair pipelines can leave the predecessor at either status
// depending on whether it was already terminal at supersede time.
func isLineageAlreadyFailed(taskID string, state *model.CommandState) bool {
	if state == nil || len(state.RetryLineage) == 0 {
		return false
	}
	cur := taskID
	visited := map[string]bool{cur: true}
	for {
		ancestor, ok := state.RetryLineage[cur]
		if !ok || visited[ancestor] {
			return false
		}
		visited[ancestor] = true
		switch state.TaskStates[ancestor] {
		case model.StatusFailed, model.StatusCancelled:
			return true
		}
		cur = ancestor
	}
}

// TripBreaker sets the circuit breaker to tripped state and issues cancel on the command state.
// Called from resultWritePhaseB where the state:{commandID} lock is already held.
// The state pointer is mutated in-place and saved by the caller.
func (cb *Handler) TripBreaker(state *model.CommandState, reason string, now time.Time) {
	if state == nil {
		return
	}
	if state.CircuitBreaker.Tripped {
		return // already tripped
	}

	nowStr := now.UTC().Format(time.RFC3339)
	state.CircuitBreaker.Tripped = true
	state.CircuitBreaker.TrippedAt = &nowStr
	state.CircuitBreaker.TripReason = &reason
	state.CircuitBreaker.HalfOpen = false
	state.CircuitBreaker.HalfOpenAt = nil
	state.CircuitBreaker.HalfOpenProbeActive = false

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
		if errors.Is(err, model.ErrStateNotFound) {
			cb.Log(core.LogLevelDebug, "circuit_breaker_state_read command=%s state_not_found, treating as initial state", commandID)
			return false, ""
		}
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

// CheckHalfOpenTransition checks if a tripped circuit breaker should transition to half-open.
// Called from periodicScanPhaseA. Mutates state in-place; the caller must save.
// Returns true if the state transitioned to half-open.
func (cb *Handler) CheckHalfOpenTransition(state *model.CommandState, now time.Time) bool {
	if state == nil || !cb.config.CircuitBreaker.Enabled {
		return false
	}
	cbs := &state.CircuitBreaker
	if !cbs.Tripped || cbs.HalfOpen {
		return false
	}
	if cbs.TrippedAt == nil {
		return false
	}
	trippedAt, err := time.Parse(time.RFC3339, *cbs.TrippedAt)
	if err != nil {
		return false
	}
	delaySec := cb.config.CircuitBreaker.EffectiveHalfOpenDelaySec()
	if now.Sub(trippedAt) < time.Duration(delaySec)*time.Second {
		return false
	}

	nowStr := now.UTC().Format(time.RFC3339)
	cbs.HalfOpen = true
	cbs.HalfOpenAt = &nowStr
	cbs.HalfOpenProbeActive = false
	cb.Log(core.LogLevelInfo, "circuit_breaker_half_open command=%s after=%ds", state.CommandID, delaySec)
	return true
}

// MarkProgress refreshes the persisted CircuitBreaker.LastProgressAt so the
// progress-timeout path does not trip a command whose worker pane is
// observably alive (e.g. cross-scan tail-hash delta). Without this hook a
// task whose execution legitimately exceeds progress_timeout_minutes would
// be cancelled despite making forward progress, defeating the
// "long-running tasks should never block the system" design goal.
//
// Idempotent and best-effort: when state does not exist yet (e.g. command
// was just created and no result has been written), the call is silently
// dropped. Read/write errors are surfaced to the caller for logging but
// do NOT propagate up the scan loop — pane-active extension is the
// authoritative liveness signal regardless of bookkeeping success.
func (cb *Handler) MarkProgress(commandID string) error {
	if !cb.config.CircuitBreaker.Enabled {
		return nil
	}
	if cb.stateManager == nil {
		return nil
	}
	if err := cb.stateManager.MarkCircuitBreakerProgress(commandID); err != nil {
		if errors.Is(err, model.ErrStateNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// AllowProbe checks whether a single probe task should be dispatched in half-open state.
// Returns true (and marks the probe as active) if a probe is allowed.
// Called with the state lock held; the caller must save.
func (cb *Handler) AllowProbe(state *model.CommandState) bool {
	if state == nil || !cb.config.CircuitBreaker.Enabled {
		return false
	}
	cbs := &state.CircuitBreaker
	if !cbs.HalfOpen || cbs.HalfOpenProbeActive {
		return false
	}
	cbs.HalfOpenProbeActive = true
	cb.Log(core.LogLevelInfo, "circuit_breaker_probe_allowed command=%s", state.CommandID)
	return true
}
