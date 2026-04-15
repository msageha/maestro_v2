package formation

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
)

// RunDown executes the 'maestro down' command.
func RunDown(maestroDir string, cfg model.Config) error {
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	// Initialize tmux debug logger for down process
	tmuxLog, tmuxLogErr := initTmuxDebugLog(maestroDir)
	if tmuxLogErr != nil {
		log.Printf("[WARN] RunDown: %v", tmuxLogErr)
	}
	defer func() {
		if err := tmuxLog.Close(); err != nil {
			log.Printf("[WARN] RunDown: close tmux log file: %v", err)
		}
	}()

	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)

	// Check if daemon socket exists
	if _, err := os.Stat(socketPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat daemon socket: %w", err)
		}
		// Socket gone but PID file may linger from a crashed daemon
		cleanupStalePID(maestroDir)
		log.Printf("[DEBUG] RunDown: killing session (daemon socket missing)")
		if err := tmux.KillSession(); err != nil {
			log.Printf("[WARN] KillSession (daemon socket missing): %v", err)
		}
		restoreServerOptions(maestroDir)
		fmt.Println("Daemon is not running. Formation stopped.")
		return nil
	}

	// Send shutdown request to daemon
	client := defaultConfig.NewUDSClient(socketPath, 5*time.Second)

	resp, err := client.SendCommand("shutdown", nil)
	if err != nil {
		// Connection error — daemon may still be alive (hung, shutting down, broken socket)
		fmt.Printf("Warning: could not connect to daemon: %v\n", err)
		fmt.Println("Cleaning up daemon and tmux session...")
		cleanupStalePID(maestroDir)
		removeIfExists(socketPath)
		log.Printf("[DEBUG] RunDown: killing session (daemon connection failed)")
		if err := tmux.KillSession(); err != nil {
			log.Printf("[WARN] KillSession (daemon connection failed): %v", err)
		}
		restoreServerOptions(maestroDir)
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
		if isValidPID(pid) {
			origStartTime := defaultConfig.ProcMgr.StartTime(pid)
			sameProcess := daemonIdentityChecker(maestroDir, pid, origStartTime)
			result, termErr := terminateProcess(pid, sameProcess, 5*time.Second)
			if termErr != nil {
				log.Printf("[WARN] terminateProcess failed for pid %d: %v", pid, termErr)
			}
			if result == terminateStopped {
				removeIfExists(pidPath)
			}
		}
		// Clean up socket if daemon left it behind
		removeIfExists(socketPath)
	}

	// Kill tmux session (idempotent — safe even if already gone)
	log.Printf("[DEBUG] RunDown: killing session (normal shutdown path)")
	if err := tmux.KillSession(); err != nil {
		return fmt.Errorf("kill tmux session: %w", err)
	}

	restoreServerOptions(maestroDir)
	fmt.Println("Maestro formation stopped.")
	return nil
}

// restoreServerOptions restores tmux server options from the backup file
// saved during 'maestro up'. Falls back to tmux defaults if the backup
// file is missing or unreadable. Skips restoration if other maestro
// sessions are still running on the same tmux server.
func restoreServerOptions(maestroDir string) {
	if hasOtherMaestroSessions() {
		return
	}

	// Defaults matching tmux upstream defaults
	options := map[string]string{
		"exit-empty":      "on",
		"exit-unattached": "off",
	}

	backupPath := filepath.Join(maestroDir, serverOptionsBackupFile)
	data, err := os.ReadFile(backupPath) //nolint:gosec // backupPath is derived from maestroDir; caller controls path
	if err == nil {
		var saved map[string]string
		if err := yamlv3.Unmarshal(data, &saved); err == nil {
			for k, v := range saved {
				options[k] = v
			}
		}
		removeIfExists(backupPath)
	}

	for name, value := range options {
		if err := tmux.SetServerOption(name, value); err != nil {
			log.Printf("[WARN] restoreServerOptions: set %s=%s: %v", name, value, err)
		}
	}
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
func (c *Config) cleanupStalePID(maestroDir string) {
	pid := validateDaemonPID(maestroDir)
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	if !isValidPID(pid) {
		removeIfExists(pidPath) // Remove stale/invalid PID file
		return
	}
	if !c.ProcMgr.Alive(pid) {
		removeIfExists(pidPath)
		return
	}

	origStartTime := c.ProcMgr.StartTime(pid)
	sameProcess := c.daemonIdentityChecker(maestroDir, pid, origStartTime)
	result, _ := c.terminateProcess(pid, sameProcess, 5*time.Second)
	if result == terminateStopped {
		removeIfExists(pidPath)
	}
}

func cleanupStalePID(maestroDir string) {
	defaultConfig.cleanupStalePID(maestroDir)
}
