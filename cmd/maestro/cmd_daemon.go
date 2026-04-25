package main

import (
	"fmt"
	"log/slog"
	"os"

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
	// (repair_pending). Here we always inject either:
	//   - RealVerifyRunner: when verify.enabled is true (default) — loads
	//     .maestro/verify.yaml or DefaultVerifyConfig and executes each
	//     command sequentially.
	//   - SkipVerifyRunner: when verify.enabled is explicitly false —
	//     emergency rollback path for operators with a faulty verify.yaml
	//     (still emits a per-task audit log entry so the rollback is visible).
	verifyLogger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With(
		"component", "verify_runner",
	)
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
		verifyLogger.Warn("verify_runner_skip_explicit",
			"reason", "verify.enabled=false in config.yaml — verification is disabled per operator opt-out")
		d.SetVerifyRunner(daemon.NewSkipVerifyRunner())
	}

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
