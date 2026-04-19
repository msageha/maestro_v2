// Package fallback implements the degraded-mode fallback manager that limits
// worker dispatch when consecutive failures exceed a configured threshold.
package fallback

import (
	"sync"
	"time"
)

// Mode represents the operational mode of the fallback manager.
type Mode int

const (
	// ModeNormal indicates all workers are allowed.
	ModeNormal Mode = iota
	// ModeDegraded indicates only essential workers are allowed.
	ModeDegraded
	// ModeRecovering indicates the system is recovering from degraded mode.
	ModeRecovering
)

// String returns a human-readable representation of the Mode.
func (m Mode) String() string {
	switch m {
	case ModeNormal:
		return "normal"
	case ModeDegraded:
		return "degraded"
	case ModeRecovering:
		return "recovering"
	default:
		return "unknown"
	}
}

// Config holds the configuration for the fallback Manager.
type Config struct {
	Enabled                     bool
	ConsecutiveFailureThreshold int
	RecoveryCheckIntervalSec    int
	MinHealthyDurationSec       int
	EssentialWorkerID           string
}

// effectiveEssentialWorkerID returns the configured essential worker ID,
// defaulting to "worker1" if unset.
func (c Config) effectiveEssentialWorkerID() string {
	if c.EssentialWorkerID != "" {
		return c.EssentialWorkerID
	}
	return "worker1"
}

// workerState holds per-worker failure and success tracking state.
type workerState struct {
	consecutiveFailures int
	lastSuccessAt       time.Time
}

// Manager tracks consecutive failures per worker and manages mode transitions
// between normal, degraded, and recovering states.
type Manager struct {
	mode                Mode
	config              Config
	workers             map[string]*workerState
	recoveringStartedAt time.Time
	mu                  sync.Mutex
	nowFunc             func() time.Time
}

// NewManager creates a new Manager initialized in ModeNormal.
func NewManager(cfg Config) *Manager {
	return &Manager{
		mode:    ModeNormal,
		config:  cfg,
		workers: make(map[string]*workerState),
		nowFunc: time.Now,
	}
}

// getOrCreateWorkerState returns the workerState for the given workerID,
// creating a new one if it does not exist. Must be called with mu held.
func (m *Manager) getOrCreateWorkerState(workerID string) *workerState {
	ws, ok := m.workers[workerID]
	if !ok {
		ws = &workerState{}
		m.workers[workerID] = ws
	}
	return ws
}

// IsWorkerAllowed reports whether the given worker is allowed to operate
// in the current mode. Returns true unconditionally when fallback is disabled.
// In ModeNormal all workers are allowed. In ModeDegraded and ModeRecovering
// only the configured essential worker is permitted.
func (m *Manager) IsWorkerAllowed(workerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.config.Enabled {
		return true
	}
	if m.mode == ModeNormal {
		return true
	}
	return workerID == m.config.effectiveEssentialWorkerID()
}

// RecordSuccess records a successful operation for the given worker.
// It resets that worker's consecutive failure counter, updates lastSuccessAt,
// and handles mode transitions from degraded to recovering and from
// recovering to normal.
func (m *Manager) RecordSuccess(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.config.Enabled {
		return
	}

	ws := m.getOrCreateWorkerState(workerID)
	ws.consecutiveFailures = 0
	ws.lastSuccessAt = m.nowFunc()

	switch m.mode {
	case ModeDegraded:
		m.mode = ModeRecovering
		m.recoveringStartedAt = m.nowFunc()
	case ModeRecovering:
		elapsed := m.nowFunc().Sub(m.recoveringStartedAt)
		if elapsed >= time.Duration(m.config.MinHealthyDurationSec)*time.Second {
			m.mode = ModeNormal
		}
	}
}

// RecordFailure records a failed operation for the given worker.
// It increments that worker's consecutive failure counter and handles mode
// transitions from normal to degraded and from recovering to degraded.
func (m *Manager) RecordFailure(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.config.Enabled {
		return
	}

	ws := m.getOrCreateWorkerState(workerID)
	ws.consecutiveFailures++

	switch m.mode {
	case ModeNormal:
		if ws.consecutiveFailures >= m.config.ConsecutiveFailureThreshold {
			m.mode = ModeDegraded
		}
	case ModeRecovering:
		m.mode = ModeDegraded
	}
}

// Mode returns the current operational mode. It is safe for concurrent use.
func (m *Manager) Mode() Mode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode
}
