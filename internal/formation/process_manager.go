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
	Alive(pid int) bool
	// StartTime returns a token representing when pid was started.
	// Returns "" if the process info is unavailable.
	StartTime(pid int) string
	// Signal sends a signal to the process with the given PID.
	Signal(pid int, sig syscall.Signal) error
}

// udsSender abstracts UDS client operations for testability.
type udsSender interface {
	SendCommand(command string, params any) (*uds.Response, error)
}

// newUDSClient creates a UDS client for the given socket path with the
// specified timeout. Tests replace this to inject mocks.
var newUDSClient = func(socketPath string, timeout time.Duration) udsSender {
	c := uds.NewClient(socketPath)
	c.SetTimeout(timeout)
	return c
}

// procMgr is the package-level process manager used by daemon lifecycle
// functions. Tests replace this to inject mocks.
var procMgr processManager = &osProcessManager{}

// Timing configuration for daemon lifecycle operations.
// Tests override these for faster execution.
var (
	daemonPollTimeout       = 10 * time.Second
	daemonPollInterval      = 500 * time.Millisecond
	processExitPollInterval = 500 * time.Millisecond
	postSignalWait          = 500 * time.Millisecond
	waitReadyPollInterval   = 200 * time.Millisecond
)

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
