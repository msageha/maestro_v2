package formation

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/uds"
)

// startDaemon starts the maestro daemon as a background process.
func startDaemon() error {
	execPath, err := os.Executable()
	if err != nil {
		execPath = "maestro" // fallback to PATH lookup
	}
	cmd := exec.Command(execPath, "daemon")
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Create a new session so the daemon survives terminal closure (no SIGHUP).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Don't wait — daemon runs in background
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}

// stopDaemon stops the daemon via UDS shutdown, then verifies it exited using
// the PID file. Falls back to SIGTERM → SIGKILL if the daemon does not exit.
// Returns an error if daemon death could not be confirmed.
func stopDaemon(maestroDir string) error {
	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	pidPath := filepath.Join(maestroDir, "daemon.pid")

	// Quick check: if neither socket nor PID file exists, no daemon to stop
	_, socketErr := os.Stat(socketPath)
	_, pidErr := os.Stat(pidPath)
	if os.IsNotExist(socketErr) && os.IsNotExist(pidErr) {
		return nil
	}

	// Step 1: Request graceful shutdown via UDS
	client := uds.NewClient(socketPath)
	client.SetTimeout(5 * time.Second)
	_, _ = client.SendCommand("shutdown", nil)

	// Step 2: Read and validate PID from daemon.pid (cross-check with lock file)
	pid := validateDaemonPID(maestroDir)

	if pid > 0 {
		// PID-based monitoring: poll process exit, then escalate
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if !processAlive(pid) {
				_ = os.Remove(pidPath)
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}

		// SIGTERM fallback — re-validate PID against lock to mitigate PID reuse
		if currentPID := validateDaemonPID(maestroDir); currentPID == pid {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		} else {
			// PID no longer matches lock — original daemon exited, PID may be reused.
			// Don't remove pidPath: a new daemon may own it now.
			return nil
		}
		termDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(termDeadline) {
			if !processAlive(pid) {
				_ = os.Remove(pidPath)
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}

		// SIGKILL as last resort — re-validate PID again
		if currentPID := validateDaemonPID(maestroDir); currentPID == pid {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		} else {
			// Don't remove pidPath: a new daemon may own it now.
			return nil
		}
		time.Sleep(500 * time.Millisecond)
		if processAlive(pid) {
			return fmt.Errorf("daemon pid=%d still alive after SIGKILL", pid)
		}
		_ = os.Remove(pidPath)
		return nil
	}

	// No valid PID: use lock acquisition to verify daemon is gone
	lockDir := filepath.Join(maestroDir, "locks")
	lockPath := filepath.Join(lockDir, "daemon.lock")

	// If locks directory doesn't exist, no daemon has ever run
	if _, err := os.Stat(lockDir); os.IsNotExist(err) {
		_ = os.Remove(pidPath)
		_ = os.Remove(socketPath)
		return nil
	}

	fl := lock.NewFileLock(lockPath)
	if err := fl.TryLock(); err == nil {
		// Lock acquired → no daemon holds it. Clean up stale files.
		_ = fl.Unlock()
		_ = os.Remove(pidPath)
		_ = os.Remove(socketPath)
		return nil
	}

	// Lock held but no valid PID: poll for daemon exit via lock release
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := fl.TryLock(); err == nil {
			_ = fl.Unlock()
			_ = os.Remove(pidPath)
			_ = os.Remove(socketPath)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	_ = os.Remove(pidPath)
	return fmt.Errorf("could not confirm daemon stopped (no valid PID, lock still held after timeout)")
}

// waitDaemonReady polls the daemon's UDS ping endpoint until it responds
// successfully or the timeout is reached.
func waitDaemonReady(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := uds.NewClient(socketPath)
	client.SetTimeout(1 * time.Second)

	for time.Now().Before(deadline) {
		resp, err := client.SendCommand("ping", nil)
		if err == nil && resp.Success {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not respond to ping within %s", timeout)
}

// readDaemonPID reads the daemon PID from the PID file. Returns 0 if unreadable.
func readDaemonPID(pidPath string) int {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// validateDaemonPID cross-checks the PID from daemon.pid against
// the PID stored in locks/daemon.lock. Returns the PID if valid, 0 otherwise.
func validateDaemonPID(maestroDir string) int {
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	pid := readDaemonPID(pidPath)
	if pid <= 0 {
		return 0
	}
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	lockPID := lock.ReadLockPID(lockPath)
	// Lock file must be readable and match daemon.pid for the PID to be trusted.
	if lockPID <= 0 || lockPID != pid {
		_ = os.Remove(pidPath)
		return 0
	}
	return pid
}

// processAlive checks whether a process with the given PID is still running.
// Returns false for pid <= 0.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means the process exists but we lack permission to signal it.
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}
