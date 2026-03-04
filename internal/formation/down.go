// Package formation manages tmux session formation (up/down) for agents.
package formation

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
)

// RunDown executes the 'maestro down' command.
func RunDown(maestroDir string, cfg model.Config) error {
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	// Initialize tmux debug logger for down process
	logsDir := filepath.Join(maestroDir, "logs")
	_ = os.MkdirAll(logsDir, 0o755)
	tmuxLogPath := filepath.Join(logsDir, "tmux_debug.log")
	if tmuxLogFile, err := os.OpenFile(tmuxLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		tmuxLogger := log.New(tmuxLogFile, "", log.LstdFlags|log.Lmicroseconds)
		tmux.SetDebugLogger(tmuxLogger)
		defer func() {
			tmux.SetDebugLogger(nil)
			tmuxLogFile.Close()
		}()
	}

	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)

	// Check if daemon socket exists
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		// Socket gone but PID file may linger from a crashed daemon
		cleanupStalePID(maestroDir)
		if tmux.SessionExists() {
			fmt.Println("[debug] RunDown: killing session (daemon socket missing)")
			_ = tmux.KillSession()
		}
		restoreServerOptions()
		fmt.Println("Daemon is not running. Formation stopped.")
		return nil
	}

	// Send shutdown request to daemon
	client := uds.NewClient(socketPath)
	client.SetTimeout(5 * time.Second)

	resp, err := client.SendCommand("shutdown", nil)
	if err != nil {
		// Connection error — daemon may still be alive (hung, shutting down, broken socket)
		fmt.Printf("Warning: could not connect to daemon: %v\n", err)
		fmt.Println("Cleaning up daemon and tmux session...")
		cleanupStalePID(maestroDir)
		_ = os.Remove(socketPath)
		if tmux.SessionExists() {
			fmt.Println("[debug] RunDown: killing session (daemon connection failed)")
			_ = tmux.KillSession()
		}
		restoreServerOptions()
		fmt.Println("Maestro formation stopped.")
		return nil
	}

	if !resp.Success {
		return fmt.Errorf("shutdown request rejected by daemon")
	}

	fmt.Println("Shutdown accepted. Waiting for daemon to stop...")

	// Wait for daemon to stop (poll socket removal or lock release)
	const daemonShutdownTimeout = 30 * time.Second
	deadline := time.Now().Add(daemonShutdownTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); os.IsNotExist(err) {
			break
		}
		time.Sleep(1 * time.Second)
	}

	// Check if daemon stopped; fall back to PID-based kill
	if _, err := os.Stat(socketPath); err == nil {
		fmt.Println("Warning: daemon did not stop via UDS. Attempting PID-based kill...")
		pid := validateDaemonPID(maestroDir)
		pidPath := filepath.Join(maestroDir, "daemon.pid")
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			termDeadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(termDeadline) {
				if !processAlive(pid) {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if processAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				time.Sleep(500 * time.Millisecond)
			}
			_ = os.Remove(pidPath)
		}
		// Clean up socket if daemon left it behind
		_ = os.Remove(socketPath)
	}

	// Kill tmux session
	if tmux.SessionExists() {
		fmt.Println("[debug] RunDown: killing session (normal shutdown path)")
		if err := tmux.KillSession(); err != nil {
			return fmt.Errorf("kill tmux session: %w", err)
		}
	}

	restoreServerOptions()
	fmt.Println("Maestro formation stopped.")
	return nil
}

// restoreServerOptions restores tmux server options to their defaults after
// session cleanup so the tmux server exits naturally when the user has no
// other sessions. Skips restoration if other maestro sessions are still
// running on the same tmux server to avoid killing them.
func restoreServerOptions() {
	if hasOtherMaestroSessions() {
		return
	}
	_ = tmux.SetServerOption("exit-empty", "on")
	_ = tmux.SetServerOption("exit-unattached", "off")
}

// hasOtherMaestroSessions checks if any other maestro-* sessions exist
// on the current tmux server (excluding the session being torn down).
// On transient errors (timeout, IPC failure) it returns true to err on the
// safe side and avoid accidentally killing other sessions.
func hasOtherMaestroSessions() bool {
	sessions, err := tmux.ListSessions()
	if err != nil {
		// Server not running — nothing to protect, safe to restore.
		if errors.Is(err, tmux.ErrTmuxServer) {
			return false
		}
		// Transient error (timeout, etc.) — assume other sessions may exist.
		return true
	}
	currentSession := tmux.GetSessionName()
	for _, s := range sessions {
		if strings.HasPrefix(s, "maestro-") && s != currentSession {
			return true
		}
	}
	return false
}

// cleanupStalePID kills a lingering daemon process if daemon.pid exists.
func cleanupStalePID(maestroDir string) {
	pid := validateDaemonPID(maestroDir)
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	if pid <= 0 {
		_ = os.Remove(pidPath) // Remove stale/invalid PID file
		return
	}
	if !processAlive(pid) {
		_ = os.Remove(pidPath)
		return
	}

	_ = syscall.Kill(pid, syscall.SIGTERM)
	termDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(termDeadline) {
		if !processAlive(pid) {
			_ = os.Remove(pidPath)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if processAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(500 * time.Millisecond)
	}
	_ = os.Remove(pidPath)
}
