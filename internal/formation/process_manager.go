package formation

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"syscall"
	"time"

	"github.com/msageha/maestro_v2/internal/uds"
)

// processManager abstracts OS-level process operations for testability.
type processManager interface {
	// Alive reports whether the process with the given PID is running.
	// For pid <= 0, Alive returns false without issuing a system call.
	Alive(pid int) bool
	// StartTime returns a token representing when pid was started.
	// Returns "" if the process info is unavailable (e.g. process
	// already exited or pid is invalid).
	StartTime(pid int) string
	// Signal sends a signal to the process with the given PID.
	// Returns an error if the underlying syscall fails (e.g. ESRCH
	// when the process does not exist, EPERM when permission is denied).
	Signal(pid int, sig syscall.Signal) error
}

// udsSender abstracts UDS client operations for testability.
type udsSender interface {
	SendCommand(command string, params any) (*uds.Response, error)
}

// Config holds the dependencies and tuning parameters for daemon
// lifecycle operations. Using a struct instead of package-level globals
// enables parallel tests and explicit dependency injection.
type Config struct {
	// NewUDSClient creates a UDS client for the given socket path with the
	// specified timeout.
	NewUDSClient func(socketPath string, timeout time.Duration) udsSender

	// ProcMgr provides OS-level process operations (alive check, signal, etc.).
	ProcMgr processManager

	// Timing configuration for daemon lifecycle operations.
	DaemonPollTimeout       time.Duration
	DaemonPollInterval      time.Duration
	ProcessExitPollInterval time.Duration
	PostSignalWait          time.Duration
	WaitReadyPollInterval   time.Duration
}

// DefaultConfig returns a Config with production defaults.
func DefaultConfig() *Config {
	return &Config{
		NewUDSClient: func(socketPath string, timeout time.Duration) udsSender {
			c := uds.NewClient(socketPath)
			c.SetTimeout(timeout)
			return c
		},
		ProcMgr:                 &osProcessManager{},
		DaemonPollTimeout:       10 * time.Second,
		DaemonPollInterval:      500 * time.Millisecond,
		ProcessExitPollInterval: 500 * time.Millisecond,
		PostSignalWait:          500 * time.Millisecond,
		WaitReadyPollInterval:   200 * time.Millisecond,
	}
}

// defaultConfig is the package-level configuration used by daemon lifecycle
// functions. Tests replace this via withTestConfig to inject mocks and fast timings.
var defaultConfig = DefaultConfig()

// osProcessManager implements processManager using real OS system calls.
type osProcessManager struct{}

func (m *osProcessManager) Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}

func (m *osProcessManager) StartTime(pid int) string {
	return platformProcessStartTime(pid)
}

func (m *osProcessManager) Signal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// ---------------------------------------------------------------------------
// PromptDetector — retry-capable prompt detection with health tracking
// ---------------------------------------------------------------------------

// AgentHealth represents the health status of an agent.
type AgentHealth string

const (
	// AgentHealthy indicates the agent is responding to prompt checks normally.
	AgentHealthy AgentHealth = "healthy"
	// AgentUnhealthy indicates prompt detection has failed repeatedly.
	AgentUnhealthy AgentHealth = "unhealthy"
)

const (
	defaultPromptMaxRetries    = 3
	defaultPromptRetryInterval = 1 * time.Second
)

// PromptChecker checks whether an agent is showing a prompt (idle state).
// Returns true if a prompt was detected, false otherwise.
type PromptChecker func(agentID string) (bool, error)

// PromptDetector provides retry-capable prompt detection with per-agent health
// tracking. Agents that fail all retry attempts are marked unhealthy and excluded
// from dispatch until a subsequent recheck recovers them.
type PromptDetector struct {
	mu            sync.RWMutex
	healthStatus  map[string]AgentHealth
	maxRetries    int
	retryInterval time.Duration
	checkPrompt   PromptChecker
	sleepFn       func(time.Duration)
}

// PromptDetectorOption configures a PromptDetector.
type PromptDetectorOption func(*PromptDetector)

// WithPromptMaxRetries overrides the maximum retry count.
func WithPromptMaxRetries(n int) PromptDetectorOption {
	return func(d *PromptDetector) { d.maxRetries = n }
}

// WithPromptRetryInterval overrides the interval between retries.
func WithPromptRetryInterval(d time.Duration) PromptDetectorOption {
	return func(pd *PromptDetector) { pd.retryInterval = d }
}

// WithPromptSleepFn overrides time.Sleep for testing.
func WithPromptSleepFn(fn func(time.Duration)) PromptDetectorOption {
	return func(d *PromptDetector) { d.sleepFn = fn }
}

// NewPromptDetector creates a PromptDetector that uses checkFn to probe agents.
func NewPromptDetector(checkFn PromptChecker, opts ...PromptDetectorOption) *PromptDetector {
	d := &PromptDetector{
		healthStatus:  make(map[string]AgentHealth),
		maxRetries:    defaultPromptMaxRetries,
		retryInterval: defaultPromptRetryInterval,
		checkPrompt:   checkFn,
		sleepFn:       time.Sleep,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// DetectPrompt attempts to detect an agent's prompt with retries.
// On success, the agent is marked healthy. If all retries fail, the agent
// is marked unhealthy and excluded from dispatch.
func (d *PromptDetector) DetectPrompt(agentID string) (bool, error) {
	var lastErr error
	for attempt := 1; attempt <= d.maxRetries; attempt++ {
		if attempt > 1 {
			d.sleepFn(d.retryInterval)
		}

		detected, err := d.checkPrompt(agentID)
		if err != nil {
			lastErr = err
			slog.Debug("prompt_detect_retry", "agent", agentID, "attempt", attempt, "error", err)
			continue
		}
		if detected {
			d.setHealth(agentID, AgentHealthy)
			return true, nil
		}
		// No error but not detected — retry
		lastErr = fmt.Errorf("prompt not detected for %s", agentID)
	}

	// All retries exhausted
	d.setHealth(agentID, AgentUnhealthy)
	slog.Warn("prompt_detect_exhausted", "agent", agentID, "max_retries", d.maxRetries)
	return false, lastErr
}

// IsHealthy reports whether the agent is in healthy state.
// Unknown agents are considered healthy (no negative information).
func (d *PromptDetector) IsHealthy(agentID string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	h, ok := d.healthStatus[agentID]
	if !ok {
		return true
	}
	return h == AgentHealthy
}

// RecheckHealth re-runs prompt detection for an unhealthy agent and transitions
// it back to healthy if the prompt is now detected. Healthy agents are skipped.
func (d *PromptDetector) RecheckHealth(agentID string) {
	if d.IsHealthy(agentID) {
		return
	}
	detected, _ := d.checkPrompt(agentID)
	if detected {
		d.setHealth(agentID, AgentHealthy)
		slog.Info("agent_health_recovered", "agent", agentID)
	}
}

// Health returns the current health status of an agent.
func (d *PromptDetector) Health(agentID string) AgentHealth {
	d.mu.RLock()
	defer d.mu.RUnlock()
	h, ok := d.healthStatus[agentID]
	if !ok {
		return AgentHealthy
	}
	return h
}

func (d *PromptDetector) setHealth(agentID string, h AgentHealth) {
	d.mu.Lock()
	d.healthStatus[agentID] = h
	d.mu.Unlock()
}

// ---------------------------------------------------------------------------
// AgentPIDTracker — PID recording and restart detection
// ---------------------------------------------------------------------------

// pidRecord stores the last-known identity of an agent process.
type pidRecord struct {
	PID       int
	StartTime string
}

// PIDQuerier retrieves the current PID and start time for an agent.
type PIDQuerier func(agentID string) (pid int, startTime string, err error)

// AgentPIDTracker records agent process PIDs and detects restarts by
// comparing current PIDs and start times against stored records.
type AgentPIDTracker struct {
	mu       sync.RWMutex
	records  map[string]pidRecord
	getPID   PIDQuerier
	procMgr  processManager
}

// AgentPIDTrackerOption configures an AgentPIDTracker.
type AgentPIDTrackerOption func(*AgentPIDTracker)

// WithProcMgr overrides the processManager used for StartTime lookups.
func WithProcMgr(pm processManager) AgentPIDTrackerOption {
	return func(t *AgentPIDTracker) { t.procMgr = pm }
}

// NewAgentPIDTracker creates a tracker that uses getPIDFn to query agent PIDs.
func NewAgentPIDTracker(getPIDFn PIDQuerier, opts ...AgentPIDTrackerOption) *AgentPIDTracker {
	t := &AgentPIDTracker{
		records: make(map[string]pidRecord),
		getPID:  getPIDFn,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Record stores the PID and start time for an agent.
func (t *AgentPIDTracker) Record(agentID string, pid int, startTime string) {
	t.mu.Lock()
	t.records[agentID] = pidRecord{PID: pid, StartTime: startTime}
	t.mu.Unlock()
}

// CheckForRestart queries the current PID for agentID and compares it against
// the stored record. Returns true if a restart was detected (PID or start time
// changed). For agents with no stored record, the current PID is recorded and
// false is returned.
func (t *AgentPIDTracker) CheckForRestart(agentID string) (bool, error) {
	currentPID, currentStart, err := t.getPID(agentID)
	if err != nil {
		return false, fmt.Errorf("get pid for %s: %w", agentID, err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	prev, exists := t.records[agentID]
	if !exists {
		// First observation: record and report no restart
		t.records[agentID] = pidRecord{PID: currentPID, StartTime: currentStart}
		return false, nil
	}

	// Compare PID
	if currentPID != prev.PID {
		slog.Warn("agent_restart_detected_pid", "agent", agentID,
			"old_pid", prev.PID, "new_pid", currentPID)
		t.records[agentID] = pidRecord{PID: currentPID, StartTime: currentStart}
		return true, nil
	}

	// Same PID but different start time → PID reuse (restart)
	if prev.StartTime != "" && currentStart != "" && currentStart != prev.StartTime {
		slog.Warn("agent_restart_detected_starttime", "agent", agentID,
			"pid", currentPID, "old_start", prev.StartTime, "new_start", currentStart)
		t.records[agentID] = pidRecord{PID: currentPID, StartTime: currentStart}
		return true, nil
	}

	return false, nil
}

// LastRecord returns the last-known PID record for an agent. Returns (0, "", false)
// if no record exists.
func (t *AgentPIDTracker) LastRecord(agentID string) (pid int, startTime string, exists bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.records[agentID]
	if !ok {
		return 0, "", false
	}
	return r.PID, r.StartTime, true
}
