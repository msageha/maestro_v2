package main

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/bridge"
	"github.com/msageha/maestro_v2/internal/daemon"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runDaemon starts the maestro daemon process.
func runDaemon(args []string) error {
	fs := newFlagSet("maestro daemon")
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro daemon: %v\nusage: maestro daemon", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro daemon: unexpected argument: %s\nusage: maestro daemon", fs.Arg(0))}
	}

	maestroDir, err := requireMaestroDir("daemon")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return fmt.Errorf("maestro daemon: load config: %w", err)
	}

	// HIGH-16: Validate project name before use in tmux session name
	if err := validate.ValidateProjectName(cfg.Project.Name); err != nil {
		return fmt.Errorf("maestro daemon: invalid project name: %w", err)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	d, err := daemon.New(maestroDir, cfg)
	if err != nil {
		return fmt.Errorf("maestro daemon: create daemon: %w", err)
	}

	// Wire Phase 6 state reader for dependency resolution (shared lockMap)
	sharedLockMap := d.LockMap()
	sm := plan.NewStateManager(maestroDir, sharedLockMap)
	reader := plan.NewPlanStateReader(sm)
	d.SetStateReader(reader)
	d.SetCanComplete(plan.CanComplete)

	// Wire plan executor for UDS plan operations (shared lockMap)
	d.SetPlanExecutor(&bridge.PlanExecutorImpl{
		MaestroDir: maestroDir,
		Config:     cfg,
		LockMap:    sharedLockMap,
	})

	if err := d.Run(); err != nil {
		return fmt.Errorf("maestro daemon: %w", err)
	}
	return nil
}
