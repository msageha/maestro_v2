package formation

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
)

// UpOptions holds configuration for the 'maestro up' command.
type UpOptions struct {
	MaestroDir string
	Config     model.Config
	Boost      bool
	Continuous bool
	Force      bool // Force recreation even if session already exists

	// Explicit flags: only overwrite config when explicitly set by CLI
	BoostSet      bool
	ContinuousSet bool
}

// ErrSessionExists is returned when a maestro session already exists and --force is not set.
var ErrSessionExists = fmt.Errorf("maestro session already exists (use --force to recreate)")

// RunUp executes the 'maestro up' command.
func RunUp(opts UpOptions) error {
	// Set tmux session name from project config
	tmux.SetSessionName("maestro-" + opts.Config.Project.Name)

	// Initialize tmux debug logger for session lifecycle diagnostics.
	tmuxLogPath := filepath.Join(opts.MaestroDir, "logs", "tmux_debug.log")
	if err := os.MkdirAll(filepath.Dir(tmuxLogPath), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create tmux debug log directory: %v\n", err)
	} else if tmuxLogFile, err := os.OpenFile(tmuxLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to open tmux debug log: %v\n", err)
	} else {
		tmuxLogger := log.New(tmuxLogFile, "", log.LstdFlags|log.Lmicroseconds)
		tmux.SetDebugLogger(tmuxLogger)
		defer func() {
			tmux.SetDebugLogger(nil)
			tmuxLogFile.Close()
		}()
	}

	// Guard: refuse to destroy a running session unless --force is set
	if tmux.SessionExists() && !opts.Force {
		return ErrSessionExists
	}

	// Always reset state on startup
	if err := resetFormation(opts.MaestroDir); err != nil {
		return fmt.Errorf("reset: %w", err)
	}

	// Reflect flags into config
	if err := reflectFlags(opts); err != nil {
		return fmt.Errorf("reflect flags: %w", err)
	}

	// Reload config after flag reflection
	cfg, err := model.LoadConfig(opts.MaestroDir)
	if err != nil {
		return fmt.Errorf("reload config: %w", err)
	}

	// Transition continuous mode to running if enabled
	if cfg.Continuous.Enabled {
		if err := activateContinuousMode(opts.MaestroDir, cfg); err != nil {
			return fmt.Errorf("activate continuous: %w", err)
		}
	}

	// Startup recovery
	if err := startupRecovery(opts.MaestroDir); err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	}

	// Save current tmux server options before hardening overrides them.
	if err := saveServerOptions(opts.MaestroDir); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save server options: %v\n", err)
	}

	// Create tmux formation
	if err := createFormation(cfg); err != nil {
		return fmt.Errorf("create formation: %w", err)
	}

	// Start daemon in background
	if err := startDaemon(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Wait for daemon to become ready (UDS ping)
	socketPath := filepath.Join(opts.MaestroDir, uds.DefaultSocketName)
	if err := waitDaemonReady(socketPath, 10*time.Second); err != nil {
		// Daemon failed to start — clean up
		_ = stopDaemon(opts.MaestroDir)
		if tmux.SessionExists() {
			_ = tmux.KillSession()
		}
		return fmt.Errorf("daemon not ready: %w", err)
	}

	fmt.Println("Maestro formation is up.")
	return nil
}

// CleanupOnFailure performs best-effort cleanup of resources that may have been
// partially created during a failed RunUp. Safe to call even if nothing was created.
func CleanupOnFailure(maestroDir string) {
	_ = stopDaemon(maestroDir)
	if tmux.SessionExists() {
		_ = tmux.KillSession()
	}
}
