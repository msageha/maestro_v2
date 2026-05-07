package formation

import (
	"fmt"
	"log/slog"
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
		slog.Warn("removeIfExists failed", "path", path, "error", err)
	}
}

// cleanupWithLock removes stale PID and socket files while holding the daemon
// lock, then releases the lock. Used by stopDaemon to deduplicate the
// lock-acquire-then-cleanup pattern.
func cleanupWithLock(fl *lock.FileLock, pidPath string, socketPaths []string) error {
	removeIfExists(pidPath)
	for _, socketPath := range socketPaths {
		removeIfExists(socketPath)
	}
	if err := fl.Unlock(); err != nil {
		slog.Warn("daemon lock unlock failed", "error", err)
	}
	return nil
}

// daemonStartupLogPath returns the path to the daemon-startup capture log
// inside maestroDir. See startDaemon for why this file exists.
func daemonStartupLogPath(maestroDir string) string {
	return filepath.Join(maestroDir, "logs", "daemon_startup.log")
}

// startDaemon starts the maestro daemon as a background process. Child
// stdout/stderr are appended to <maestroDir>/logs/daemon_startup.log so
// early-return errors from runDaemon (load config, create daemon, wire
// reader, etc.) — which fire before the structured logger is wired —
// stay observable; without this, `maestro up` could only print the generic
// "daemon not ready: ping timeout" (Report 2026-05-05). Append mode;
// rotation is the operator's responsibility.
func startDaemon(maestroDir string) error {
	execPath, err := os.Executable()
	if err != nil {
		execPath = "maestro" // fallback to PATH lookup
	}
	cmd := exec.Command(execPath, "daemon") //nolint:gosec // execPath is the current binary path from os.Executable
	logPath := daemonStartupLogPath(maestroDir)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		return fmt.Errorf("ensure logs dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // logPath is constructed from maestroDir, an internal-controlled path
	if err != nil {
		return fmt.Errorf("open daemon startup log %s: %w", logPath, err)
	}
	// Mark each `maestro up` invocation with a separator so multiple startups
	// in the same workspace are easy to walk through. Best-effort write —
	// failure here would only lose visual padding, not the actual log
	// content, and the daemon child still inherits a working file handle.
	_, _ = fmt.Fprintf(logFile, "\n--- daemon spawn at %s (pid will follow) ---\n", time.Now().UTC().Format(time.RFC3339))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Create a new session so the daemon survives terminal closure (no SIGHUP).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start daemon: %w", err)
	}
	_, _ = fmt.Fprintf(logFile, "spawned pid=%d\n", cmd.Process.Pid)
	// Reap the child process in the background. cmd.Wait() returns when the
	// daemon process exits, so this goroutine is guaranteed to terminate.
	// The fd stays open until Wait returns so any final stderr flush from
	// the child still lands in the file.
	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		if err != nil {
			slog.Warn("daemon process exited with error",
				"error", err,
				"startup_log", logPath,
				"hint", "tail the startup log for the real cause; see daemonStartupTail() output that maestro up prints on ping timeout")
		}
	}()
	return nil
}

// daemonStartupTail returns the last `tailBytes` of the daemon startup
// log so callers (typically `maestro up`'s ping-timeout error path) can
// print the actual cause of a failed startup instead of the generic
// "daemon did not respond" message. Returns "" on any read failure —
// quiet degradation since the log is purely diagnostic.
func daemonStartupTail(maestroDir string, tailBytes int) string {
	logPath := daemonStartupLogPath(maestroDir)
	data, err := os.ReadFile(logPath) //nolint:gosec // logPath is derived from maestroDir; internal-controlled
	if err != nil {
		return ""
	}
	if tailBytes <= 0 || len(data) <= tailBytes {
		return strings.TrimSpace(string(data))
	}
	return "...\n" + strings.TrimSpace(string(data[len(data)-tailBytes:]))
}

// stopDaemon stops the daemon via UDS shutdown, then verifies it exited using
// the PID file. Falls back to SIGTERM → SIGKILL if the daemon does not exit.
// Returns an error if daemon death could not be confirmed.
func (c *Config) stopDaemon(maestroDir string) error {
	socketPath, err := uds.SocketPath(maestroDir)
	if err != nil {
		return fmt.Errorf("resolve daemon socket path: %w", err)
	}
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	socketCleanupPaths := uds.SocketCleanupPaths(maestroDir)

	// Quick check: if neither socket nor PID file exists, no daemon to stop.
	socketExists := false
	for _, path := range socketCleanupPaths {
		if _, err := os.Stat(path); err == nil {
			socketExists = true
			break
		} else if !os.IsNotExist(err) {
			socketExists = true
			break
		}
	}
	_, pidErr := os.Stat(pidPath)
	if !socketExists && os.IsNotExist(pidErr) {
		return nil
	}

	// Step 1: Request graceful shutdown via UDS
	client := c.NewUDSClient(socketPath, 5*time.Second)
	if _, err := client.SendCommand("shutdown", nil); err != nil {
		slog.Warn("shutdown command failed", "error", err)
	}

	// Step 2: Read and validate PID from daemon.pid (cross-check with lock file)
	pid := validateDaemonPID(maestroDir)

	if isValidPID(pid) {
		// Capture process start time so we can detect PID reuse: if the OS
		// recycles this PID for an unrelated process during shutdown, the
		// start time will differ and terminateProcess will refuse to kill it.
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

		// Identity check pairs the PID with origStartTime so terminateProcess
		// only signals a process that is still the same daemon we observed
		// above — guarding against PID reuse during the poll window.
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
			for _, path := range socketCleanupPaths {
				removeIfExists(path)
			}
			return nil
		}
		return fmt.Errorf("stat locks directory: %w", err)
	}

	fl := lock.NewFileLock(lockPath)
	if err := fl.TryLock(); err == nil {
		// Lock acquired → no daemon holds it.
		return cleanupWithLock(fl, pidPath, socketCleanupPaths)
	}

	// Lock held but no valid PID: poll for daemon exit via lock release
	deadline := time.Now().Add(c.DaemonPollTimeout)
	for time.Now().Before(deadline) {
		if err := fl.TryLock(); err == nil {
			return cleanupWithLock(fl, pidPath, socketCleanupPaths)
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
//
//nolint:unused // exercised from up_test.go (golangci-lint runs with tests:false)
func processAlive(pid int) bool {
	return defaultConfig.ProcMgr.Alive(pid)
}
