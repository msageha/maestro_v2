// Package formation manages tmux session formation (up/down) for agents.
package formation

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
)

// RunDown executes the 'maestro down' command.
func RunDown(maestroDir string, cfg model.Config) error {
	tmux.SetSessionName("maestro-" + cfg.Project.Name)
	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)

	// Check if daemon socket exists
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		// Socket gone but PID file may linger from a crashed daemon
		cleanupStalePID(maestroDir)
		if tmux.SessionExists() {
			_ = tmux.KillSession()
		}
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
			_ = tmux.KillSession()
		}
		fmt.Println("Maestro formation stopped.")
		return nil
	}

	if !resp.Success {
		return fmt.Errorf("shutdown request rejected by daemon")
	}

	fmt.Println("Shutdown accepted. Waiting for daemon to stop...")

	// Wait for daemon to stop (poll socket removal or lock release)
	timeout := 100 * time.Second
	deadline := time.Now().Add(timeout)
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
		if err := tmux.KillSession(); err != nil {
			return fmt.Errorf("kill tmux session: %w", err)
		}
	}

	fmt.Println("Maestro formation stopped.")
	return nil
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
