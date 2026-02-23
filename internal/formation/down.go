// Package formation manages tmux session formation (up/down) for agents.
package formation

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
)

// RunDown executes the 'maestro down' command.
func RunDown(maestroDir string) error {
	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)

	// Check if daemon socket exists
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		// Daemon not running — just kill tmux session
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
		// Connection error — daemon may have already stopped
		fmt.Printf("Warning: could not connect to daemon: %v\n", err)
		fmt.Println("Cleaning up tmux session...")
		if tmux.SessionExists() {
			_ = tmux.KillSession()
		}
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

	// Check if daemon stopped
	if _, err := os.Stat(socketPath); err == nil {
		fmt.Println("Warning: daemon did not stop within timeout.")
		return fmt.Errorf("shutdown timeout after %v", timeout)
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
