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
}

// Manager tracks consecutive failures and manages mode transitions
// between normal, degraded, and recovering states.
type Manager struct {
	mode                 Mode
	config               Config
	consecutiveFailures  int
	lastSuccessAt        time.Time
	recoveringStartedAt  time.Time
	mu                   sync.Mutex
	nowFunc              func() time.Time
}

// NewManager creates a new Manager initialized in ModeNormal.
func NewManager(cfg Config) *Manager {
	return &Manager{
		mode:    ModeNormal,
		config:  cfg,
		nowFunc: time.Now,
	}
}

// IsWorkerAllowed reports whether the given worker is allowed to operate
// in the current mode. In ModeNormal all workers are allowed. In
// ModeDegraded and ModeRecovering only "worker1" is permitted.
func (m *Manager) IsWorkerAllowed(workerID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.mode == ModeNormal {
		return true
	}
	return workerID == "worker1"
}

// RecordSuccess records a successful operation for the given worker.
// It resets the consecutive failure counter, updates lastSuccessAt,
// and handles mode transitions from degraded to recovering and from
// recovering to normal.
func (m *Manager) RecordSuccess(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.consecutiveFailures = 0
	m.lastSuccessAt = m.nowFunc()

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
// It increments the consecutive failure counter and handles mode
// transitions from normal to degraded and from recovering to degraded.
func (m *Manager) RecordFailure(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.consecutiveFailures++

	switch m.mode {
	case ModeNormal:
		if m.consecutiveFailures >= m.config.ConsecutiveFailureThreshold {
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
