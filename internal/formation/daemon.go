package formation

import (
	"fmt"
	"log"
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

// removeIfExists removes the file at path, ignoring "file not found" errors.
// Non-fatal errors (e.g. permission denied) are logged as warnings.
func removeIfExists(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("[WARN] removeIfExists: %s: %v", path, err)
	}
}

// cleanupWithLock removes stale PID and socket files while holding the daemon
// lock, then releases the lock. Used by stopDaemon to deduplicate the
// lock-acquire-then-cleanup pattern.
func cleanupWithLock(fl *lock.FileLock, pidPath, socketPath string) error {
	removeIfExists(pidPath)
	removeIfExists(socketPath)
	if err := fl.Unlock(); err != nil {
		log.Printf("[WARN] daemon lock unlock: %v", err)
	}
	return nil
}

// startDaemon starts the maestro daemon as a background process.
func startDaemon() error {
	execPath, err := os.Executable()
	if err != nil {
		execPath = "maestro" // fallback to PATH lookup
	}
	cmd := exec.Command(execPath, "daemon") //nolint:gosec // execPath is the current binary path from os.Executable
	cmd.Stdout = nil
	cmd.Stderr = nil
	// Create a new session so the daemon survives terminal closure (no SIGHUP).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Reap the child process in the background. cmd.Wait() returns when the
	// daemon process exits, so this goroutine is guaranteed to terminate.
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("[WARN] daemon process exited with error: %v", err)
		}
	}()
	return nil
}

// stopDaemon stops the daemon via UDS shutdown, then verifies it exited using
// the PID file. Falls back to SIGTERM → SIGKILL if the daemon does not exit.
// Returns an error if daemon death could not be confirmed.
func (c *Config) stopDaemon(maestroDir string) error {
	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	pidPath := filepath.Join(maestroDir, "daemon.pid")

	// Quick check: if neither socket nor PID file exists, no daemon to stop
	_, socketErr := os.Stat(socketPath)
	_, pidErr := os.Stat(pidPath)
	if os.IsNotExist(socketErr) && os.IsNotExist(pidErr) {
		return nil
	}

	// Step 1: Request graceful shutdown via UDS
	client := c.NewUDSClient(socketPath, 5*time.Second)
	if _, err := client.SendCommand("shutdown", nil); err != nil {
		log.Printf("[WARN] shutdown command failed: %v", err)
	}

	// Step 2: Read and validate PID from daemon.pid (cross-check with lock file)
	pid := validateDaemonPID(maestroDir)

	if isValidPID(pid) {
		// Capture process start time to detect PID reuse (Fix #7)
		origStartTime := c.ProcMgr.StartTime(pid)

		// PID-based monitoring: poll process exit, then escalate
		deadline := time.Now().Add(c.DaemonPollTimeout)
		for time.Now().Before(deadline) {
			if !c.ProcMgr.Alive(pid) {
				removeIfExists(pidPath)
				return nil
			}
			time.Sleep(c.DaemonPollInterval)
		}

		// Use terminateProcess with PID + start time identity check (Fix #7, #8)
		sameProcess := c.daemonIdentityChecker(maestroDir, pid, origStartTime)
		result, err := c.terminateProcess(pid, sameProcess, 5*time.Second)
		if result == terminateNotTarget {
			// PID was reused by another process — don't clean up
			return nil
		}
		if err != nil {
			return err
		}
		removeIfExists(pidPath)
		return nil
	}

	// No valid PID: use lock acquisition to verify daemon is gone
	lockDir := filepath.Join(maestroDir, "locks")
	lockPath := filepath.Join(lockDir, "daemon.lock")

	// If locks directory doesn't exist, no daemon has ever run
	if _, err := os.Stat(lockDir); err != nil {
		if os.IsNotExist(err) {
			removeIfExists(pidPath)
			removeIfExists(socketPath)
			return nil
		}
		return fmt.Errorf("stat locks directory: %w", err)
	}

	fl := lock.NewFileLock(lockPath)
	if err := fl.TryLock(); err == nil {
		// Lock acquired → no daemon holds it.
		return cleanupWithLock(fl, pidPath, socketPath)
	}

	// Lock held but no valid PID: poll for daemon exit via lock release
	deadline := time.Now().Add(c.DaemonPollTimeout)
	for time.Now().Before(deadline) {
		if err := fl.TryLock(); err == nil {
			return cleanupWithLock(fl, pidPath, socketPath)
		}
		time.Sleep(c.DaemonPollInterval)
	}

	removeIfExists(pidPath)
	return fmt.Errorf("could not confirm daemon stopped (no valid PID, lock still held after timeout)")
}

func stopDaemon(maestroDir string) error {
	return defaultConfig.stopDaemon(maestroDir)
}

// daemonIdentityChecker returns a function that verifies a PID still belongs
// to the original daemon by checking both the PID file cross-reference and
// the process start time.
func (c *Config) daemonIdentityChecker(maestroDir string, originalPID int, origStartTime string) func(int) bool {
	return func(pid int) bool {
		// Check PID file still matches
		if currentPID := validateDaemonPID(maestroDir); currentPID != originalPID {
			return false
		}
		// Check process start time hasn't changed (PID reuse detection)
		if origStartTime != "" {
			currentStartTime := c.ProcMgr.StartTime(pid)
			if currentStartTime == "" || currentStartTime != origStartTime {
				return false
			}
		}
		return true
	}
}

func daemonIdentityChecker(maestroDir string, originalPID int, origStartTime string) func(int) bool {
	return defaultConfig.daemonIdentityChecker(maestroDir, originalPID, origStartTime)
}

// waitDaemonReady polls the daemon's UDS ping endpoint until it responds
// successfully or the timeout is reached.
func (c *Config) waitDaemonReady(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := c.NewUDSClient(socketPath, 1*time.Second)

	for time.Now().Before(deadline) {
		resp, err := client.SendCommand("ping", nil)
		if err == nil && resp.Success {
			return nil
		}
		time.Sleep(c.WaitReadyPollInterval)
	}
	return fmt.Errorf("daemon did not respond to ping within %s", timeout)
}

func waitDaemonReady(socketPath string, timeout time.Duration) error {
	return defaultConfig.waitDaemonReady(socketPath, timeout)
}

// isValidPID reports whether pid is a valid process ID (positive integer).
func isValidPID(pid int) bool {
	return pid > 0
}

// readDaemonPID reads the daemon PID from the PID file. Returns 0 if unreadable or invalid.
func readDaemonPID(pidPath string) int {
	data, err := os.ReadFile(pidPath) //nolint:gosec // pidPath is derived from maestroDir; caller controls path
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !isValidPID(pid) {
		return 0
	}
	return pid
}

// validateDaemonPID cross-checks the PID from daemon.pid against
// the PID stored in locks/daemon.lock. Returns the PID if valid, 0 otherwise.
func validateDaemonPID(maestroDir string) int {
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	pid := readDaemonPID(pidPath)
	if !isValidPID(pid) {
		return 0
	}
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	lockPID := lock.ReadLockPID(lockPath)
	// Lock file must be readable and match daemon.pid for the PID to be trusted.
	if !isValidPID(lockPID) || lockPID != pid {
		removeIfExists(pidPath)
		return 0
	}
	return pid
}

// processAlive checks whether a process with the given PID is still running.
// Delegates to the package-level processManager for testability.
func processAlive(pid int) bool {
	return defaultConfig.ProcMgr.Alive(pid)
}
