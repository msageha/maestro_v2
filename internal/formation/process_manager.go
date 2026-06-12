package formation

import (
	"errors"
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
