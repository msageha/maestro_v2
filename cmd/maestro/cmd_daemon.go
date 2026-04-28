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

const allowVerifySkipEnv = "MAESTRO_ALLOW_VERIFY_SKIP"

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
	verifyLogger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With(
		"component", "verify_runner",
	)
	if cfg.Verify.EffectiveEnabled() {
		if err := clearVerifyStatusWarning(maestroDir); err != nil {
			verifyLogger.Warn("verify_status_warning_clear_failed", "error", err)
		}
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
		if os.Getenv(allowVerifySkipEnv) != "1" {
			return fmt.Errorf("maestro daemon: verify.enabled=false requires %s=1; refusing to start with silent verification skip", allowVerifySkipEnv)
		}
		verifyLogger.Warn("verify_runner_skip_explicit",
			"reason", "verify.enabled=false with MAESTRO_ALLOW_VERIFY_SKIP=1 — verification is disabled by explicit emergency opt-out")
		if err := writeVerifyStatusWarning(maestroDir); err != nil {
			verifyLogger.Warn("verify_status_warning_write_failed", "error", err)
		}
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

func writeVerifyStatusWarning(maestroDir string) error {
	stateDir := filepath.Join(maestroDir, "state")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return err
	}
	data := []byte("schema_version: 1\nfile_type: verify_status\nmode: skipped\nreason: verify.enabled=false with MAESTRO_ALLOW_VERIFY_SKIP=1\n")
	return os.WriteFile(filepath.Join(stateDir, "verify_status.yaml"), data, 0o600)
}

func clearVerifyStatusWarning(maestroDir string) error {
	err := os.Remove(filepath.Join(maestroDir, "state", "verify_status.yaml"))
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	return err
}
