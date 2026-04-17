package formation

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// SessionRecoveryState represents the current state of session recovery.
type SessionRecoveryState string

const (
	// SessionRecoveryIdle indicates no recovery is in progress.
	SessionRecoveryIdle SessionRecoveryState = "idle"
	// SessionRecoveryRecovering indicates recovery is actively being attempted.
	SessionRecoveryRecovering SessionRecoveryState = "recovering"
	// SessionRecoveryFailed indicates all recovery attempts have been exhausted.
	SessionRecoveryFailed SessionRecoveryState = "failed"
)

const (
	defaultMaxRecoveryRetries = 3
)

// SessionRecoverer handles automatic recovery of lost tmux sessions.
// It attempts to recreate the session with exponential backoff and exposes
// the current recovery state for external monitoring.
type SessionRecoverer struct {
	mu    sync.RWMutex
	state SessionRecoveryState

	maxRetries     int
	sessionCreator func(string) error // creates a new tmux session
	sessionChecker func() bool        // checks if session exists
	sleepFn        func(time.Duration) // for testing: overrides time.Sleep
}

// SessionRecovererOption configures a SessionRecoverer.
type SessionRecovererOption func(*SessionRecoverer)

// WithMaxRetries overrides the maximum number of recovery attempts.
func WithMaxRetries(n int) SessionRecovererOption {
	return func(r *SessionRecoverer) { r.maxRetries = n }
}

// WithSessionCreator overrides the session creation function (for testing).
func WithSessionCreator(fn func(string) error) SessionRecovererOption {
	return func(r *SessionRecoverer) { r.sessionCreator = fn }
}

// WithSessionChecker overrides the session existence check function (for testing).
func WithSessionChecker(fn func() bool) SessionRecovererOption {
	return func(r *SessionRecoverer) { r.sessionChecker = fn }
}

// WithSleepFn overrides time.Sleep for testing.
func WithSleepFn(fn func(time.Duration)) SessionRecovererOption {
	return func(r *SessionRecoverer) { r.sleepFn = fn }
}

// NewSessionRecoverer creates a SessionRecoverer with the given options.
// By default it uses tmux operations from the tmux package; callers should
// provide WithSessionCreator and WithSessionChecker for unit testing.
func NewSessionRecoverer(opts ...SessionRecovererOption) *SessionRecoverer {
	r := &SessionRecoverer{
		state:      SessionRecoveryIdle,
		maxRetries: defaultMaxRecoveryRetries,
		sleepFn:    time.Sleep,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// State returns the current recovery state (thread-safe).
func (r *SessionRecoverer) State() SessionRecoveryState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state
}

func (r *SessionRecoverer) setState(s SessionRecoveryState) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
}

// RecoverSession attempts to recreate a lost tmux session with exponential
// backoff (1s, 2s, 4s). Returns nil on success or an error if all retries
// are exhausted, in which case the state transitions to SessionRecoveryFailed
// and an slog.Error is emitted for user intervention.
func (r *SessionRecoverer) RecoverSession(windowName string) error {
	r.setState(SessionRecoveryRecovering)

	var lastErr error
	for attempt := 1; attempt <= r.maxRetries; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(1<<(attempt-2)) * time.Second // 1s, 2s, 4s
			slog.Info("session_recovery_backoff", "attempt", attempt, "backoff", backoff)
			r.sleepFn(backoff)
		}

		// Check if session recovered externally between retries
		if r.sessionChecker != nil && r.sessionChecker() {
			r.setState(SessionRecoveryIdle)
			slog.Info("session_recovered_external", "attempt", attempt)
			return nil
		}

		if r.sessionCreator == nil {
			lastErr = fmt.Errorf("sessionCreator not configured")
			continue
		}

		err := r.sessionCreator(windowName)
		if err != nil {
			lastErr = err
			slog.Warn("session_recovery_attempt_failed", "attempt", attempt, "max", r.maxRetries, "error", err)
			continue
		}

		r.setState(SessionRecoveryIdle)
		slog.Info("session_recovered_success", "attempt", attempt)
		return nil
	}

	r.setState(SessionRecoveryFailed)
	slog.Error("session_recovery_exhausted",
		"max_retries", r.maxRetries,
		"last_error", lastErr,
		"action_required", "manual intervention needed to restore tmux session",
	)
	return fmt.Errorf("session recovery failed after %d attempts: %w", r.maxRetries, lastErr)
}

// Reset transitions the recoverer back to idle state, e.g. after external
// recovery or before a new recovery cycle.
func (r *SessionRecoverer) Reset() {
	r.setState(SessionRecoveryIdle)
}
