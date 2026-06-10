package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/bridge"
	"github.com/msageha/maestro_v2/internal/daemon"
	"github.com/msageha/maestro_v2/internal/formation"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
)

// runDaemon starts the maestro daemon process.
func runDaemon(args []string) error {
	cmd := NewCommand("maestro daemon", "maestro daemon")
	if err := cmd.Parse(args); err != nil {
		return err
	}

	maestroDir, err := requireMaestroDir("daemon")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return fmt.Errorf("maestro daemon: load config: %w", err)
	}

	if err := setupTmuxSession("daemon", maestroDir, cfg); err != nil {
		return err
	}

	d, err := daemon.New(maestroDir, cfg)
	if err != nil {
		return fmt.Errorf("maestro daemon: create daemon: %w", err)
	}

	// Wire Phase 6 state reader for dependency resolution (shared lockMap)
	sharedLockMap := d.LockMap()
	sm := plan.NewStateManager(maestroDir, sharedLockMap)
	if sm == nil {
		return fmt.Errorf("maestro daemon: failed to create state manager")
	}
	reader := plan.NewPlanStateReader(sm)
	if reader == nil {
		return fmt.Errorf("maestro daemon: failed to create plan state reader")
	}
	d.SetStateReader(reader)
	d.SetCanComplete(plan.CanComplete)
	d.SetDeferredPlanCompleter(func(commandID string) (bool, error) {
		result, err := plan.CompleteDeferredPublish(plan.CompleteOptions{
			CommandID:  commandID,
			MaestroDir: maestroDir,
			Config:     cfg,
			LockMap:    sharedLockMap,
		})
		if err != nil {
			return false, err
		}
		if result == nil {
			return false, nil // no deferred intent
		}
		return true, nil
	})

	// Wire phase diagnoser to avoid daemon→plan import cycle
	d.SetPhaseDiagnoser(func(phase model.Phase, tasks []model.Task, results []model.TaskResult) string {
		diag := plan.DiagnosePhase(phase, tasks, results)
		if diag == nil {
			return ""
		}
		return plan.FormatDiagnosisPrompt(diag)
	})

	// Wire plan executor for UDS plan operations (shared lockMap)
	executor := &bridge.PlanExecutorImpl{
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    sharedLockMap,
	}
	d.SetPlanExecutor(executor)

	// §S1-1 Verification Runner wiring. The result-write handler is created
	// with verifyRunner=nil so that a wiring miss surfaces as fail-closed
	// (repair_pending). Production injects RealVerifyRunner by default. The
	// skip runner is only available behind an explicit emergency env gate.
	//
	// Route the verify runner's slog output to <maestroDir>/logs/daemon.log
	// instead of os.Stderr. When `maestro up` starts the daemon in the
	// background, internal/formation/daemon.go discards stdout/stderr so
	// stderr-routed logs vanish, leaving operators unable to confirm that
	// e.g. `gosec ./...` actually executed during verification. A second
	// append-mode handle to daemon.log is safe: POSIX guarantees atomic
	// appends below PIPE_BUF, so verify lines interleave cleanly with the
	// daemon's primary logger.
	verifyLogPath := filepath.Join(maestroDir, "logs", "daemon.log")
	verifyLogFile, err := os.OpenFile(verifyLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600) //nolint:gosec // verifyLogPath is constructed from a controlled application log directory
	if err != nil {
		return fmt.Errorf("maestro daemon: open verify runner log: %w", err)
	}
	defer func() {
		// Best-effort close on runDaemon return. d.Run() blocks until shutdown,
		// so by this point no verify goroutine is still writing. Closing here
		// flushes the final log line on graceful shutdown and avoids fd-leak;
		// the OS would reclaim the fd at process exit anyway, but explicit
		// close matches the daemon's own log file lifecycle.
		_ = verifyLogFile.Close()
	}()
	verifyLogger := slog.New(slog.NewTextHandler(verifyLogFile, nil)).With(
		"component", "verify_runner",
	)
	// Route slog.Default to the same daemon.log file. Background-launched
	// daemons discard stdout/stderr (formation/daemon.go), so slog calls
	// outside the explicit logger plumbing (e.g. paneactivity's
	// `pane_blocked_prompt_detected` warning) are otherwise silently
	// dropped. Reports of 2026-05-04 confirmed `lease_extend_pane_blocked`
	// fired on the daemon's structured logger but the corresponding
	// slog.Warn from the tracker never reached daemon.log. Wiring
	// slog.Default to the file produces a single observable surface.
	//
	// No baked-in component attribute: daemon-resident packages label their
	// own lines via call-time `slog.Default().With("component", ...)`
	// helpers (plan/paneactivity/events/formation-acceptor). A baked-in
	// "default" label would duplicate the component key on those lines;
	// records WITHOUT component= are exactly the not-yet-plumbed call sites.
	slog.SetDefault(slog.New(slog.NewTextHandler(verifyLogFile, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	if cfg.Verify.EffectiveEnabled() {
		projectDir := cfg.Maestro.ProjectRoot
		if projectDir == "" {
			// Fall back to the daemon's CWD when project_root is not pinned in
			// config.yaml — preserves existing behaviour for older workspaces.
			if cwd, err := os.Getwd(); err == nil {
				projectDir = cwd
			}
		}
		d.SetVerifyRunner(daemon.NewRealVerifyRunner(maestroDir, projectDir, verifyLogger))
	} else {
		// verify.enabled=false is a normal operating mode for projects that
		// are not software-development workflows (research, documentation,
		// note-taking, …). The autonomous LLM Orchestration design treats
		// "no machine-checkable verify step" as the default: it is the
		// operator's responsibility to opt in by writing `.maestro/verify.yaml`
		// and flipping `verify.enabled: true`. Surface the disabled state at
		// INFO so daemon.log makes the choice visible without forcing an
		// emergency env gate.
		verifyLogger.Info("verify_runner_disabled",
			"reason", "verify.enabled=false in config.yaml — daemon will skip per-task verification (normal mode for non-software-dev workflows)")
		d.SetVerifyRunner(daemon.NewSkipVerifyRunner())
	}
	d.SetVerifyAsync(true)

	// Auto-accept Claude Code workspace trust dialog in this long-lived process.
	// The CLI process (which calls createFormation) exits shortly after formation
	// is complete, killing the CLI-side goroutine. This call picks up where the
	// CLI left off, covering the full window after daemon startup.
	formation.StartTrustDialogAcceptor(maestroDir)

	if err := d.Run(); err != nil {
		return fmt.Errorf("maestro daemon: %w", err)
	}
	return nil
}
