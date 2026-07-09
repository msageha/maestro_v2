package formation

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
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
//
// Exported so the CLI can distinguish it from genuine setup failures: it is
// returned BEFORE any resource is created, so the caller must NOT run
// CleanupOnFailure for it — doing so would tear down the already-running
// formation this guard exists to protect (a plain re-run of `maestro up`
// would destroy the live session + daemon).
var ErrSessionExists = fmt.Errorf("maestro session already exists (use --force to recreate)")

// RunUp executes the 'maestro up' command.
func RunUp(opts UpOptions) (err error) {
	// Set tmux session name from project config. The session name is the
	// human-readable "maestro-<projectName>"; per-checkout isolation comes
	// from the per-instance socket below, not the session name.
	tmux.SetSessionName(tmux.BuildMaestroSessionName(opts.Config.Project.Name))
	// Configure a per-project tmux socket (`tmux -L <socket>`). Sharing
	// the default socket lets concurrent maestro instances of different
	// projects pile sessions onto the same tmux server, producing
	// SESSION_LOST races, ID collisions, and stray autoAcceptTrustDialog
	// keypresses (Report 2026-05-06 P0). One socket per instance also
	// means one tmux server per instance, so coexisting instances stay
	// fully isolated and the shared-server races vanish structurally.
	tmux.SetTmuxSocket(tmux.BuildMaestroSocketName(opts.Config.Project.Name, opts.MaestroDir))

	// Guard: refuse to destroy a running session unless --force is set.
	// The check must not fail open: if tmux cannot be queried at all
	// (e.g. a sandbox denying socket access), proceeding would tear down
	// and recreate state on top of a possibly-live formation.
	sessionAlive, err := tmux.SessionExistsChecked()
	if err != nil {
		return fmt.Errorf("check existing session: %w", err)
	}
	if sessionAlive && !opts.Force {
		return ErrSessionExists
	}

	if err := preflightEnvironment(opts.MaestroDir); err != nil {
		return err
	}

	// Initialize tmux debug logger for session lifecycle diagnostics.
	tmuxLog, tmuxLogErr := initTmuxDebugLog(opts.MaestroDir)
	if tmuxLogErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", tmuxLogErr)
	}
	defer func() {
		if cerr := tmuxLog.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close tmux debug log: %w", cerr))
		}
	}()

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
	if err := createFormation(opts.MaestroDir, cfg); err != nil {
		return fmt.Errorf("create formation: %w", err)
	}

	// Start daemon in background
	if err := startDaemon(opts.MaestroDir); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Wait for daemon to become ready (UDS ping)
	socketPath, err := uds.SocketPath(opts.MaestroDir)
	if err != nil {
		_ = stopDaemon(opts.MaestroDir)
		if killErr := tmux.KillSession(); killErr != nil {
			slog.Warn("KillSession failed during socket path cleanup", "error", killErr)
		}
		return fmt.Errorf("resolve daemon socket path: %w", err)
	}
	if err := waitDaemonReady(socketPath, 10*time.Second); err != nil {
		// Daemon failed to start — clean up. Surface the tail of the
		// daemon startup log so operators see the actual cause (e.g. a
		// config validation error from runDaemon's early return) instead
		// of the bare "ping timeout" message that was the symptom.
		tail := daemonStartupTail(opts.MaestroDir, 4096)
		_ = stopDaemon(opts.MaestroDir)
		if err := tmux.KillSession(); err != nil {
			slog.Warn("KillSession failed during daemon not ready cleanup", "error", err)
		}
		if tail != "" {
			return fmt.Errorf("daemon not ready: %w\n--- daemon startup log (last 4 KiB of %s) ---\n%s\n--- end startup log ---",
				err, daemonStartupLogPath(opts.MaestroDir), tail)
		}
		return fmt.Errorf("daemon not ready: %w (startup log empty at %s)", err, daemonStartupLogPath(opts.MaestroDir))
	}

	fmt.Println("Maestro formation is up.")
	return nil
}

// CleanupOnFailure performs best-effort cleanup of resources that may have been
// partially created during a failed RunUp. Safe to call even if nothing was created.
// Returns an error if any cleanup operation fails.
func CleanupOnFailure(maestroDir string) error {
	var errs []error
	if err := stopDaemon(maestroDir); err != nil {
		errs = append(errs, fmt.Errorf("stop daemon: %w", err))
	}
	if err := tmux.KillSession(); err != nil {
		errs = append(errs, fmt.Errorf("kill tmux session: %w", err))
	}
	return errors.Join(errs...)
}
