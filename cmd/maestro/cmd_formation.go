package main

import (
	"fmt"
	"os"

	"github.com/msageha/maestro_v2/internal/formation"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runUp starts the formation (daemon + agents) and optionally attaches to tmux.
func runUp(args []string) error {
	fs := newFlagSet("maestro up")
	var boost, continuous, detach, force bool
	fs.BoolVar(&boost, "boost", false, "")
	fs.BoolVar(&continuous, "continuous", false, "")
	fs.BoolVar(&detach, "detach", false, "")
	fs.BoolVar(&detach, "d", false, "")
	fs.BoolVar(&force, "force", false, "")
	fs.BoolVar(&force, "f", false, "")
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro up: %v\nusage: maestro up [--boost] [--continuous] [--detach|-d] [--force|-f]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro up: unexpected argument: %s\nusage: maestro up [--boost] [--continuous] [--detach|-d] [--force|-f]", fs.Arg(0))}
	}

	maestroDir, err := requireMaestroDir("up")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return fmt.Errorf("maestro up: load config: %w", err)
	}

	// Validate project name before use in tmux session name
	if err := validate.ValidateProjectName(cfg.Project.Name); err != nil {
		return fmt.Errorf("maestro up: invalid project name: %w", err)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	opts := formation.UpOptions{
		MaestroDir:    maestroDir,
		Config:        cfg,
		Boost:         boost,
		Continuous:    continuous,
		Force:         force,
		BoostSet:      boost,
		ContinuousSet: continuous,
	}

	if err := formation.RunUp(opts); err != nil {
		// Clean up any partially-created resources (tmux session, daemon)
		fmt.Fprintln(os.Stderr, "Cleaning up after setup failure...")
		formation.CleanupOnFailure(maestroDir)
		return fmt.Errorf("maestro up: %w", err)
	}

	if !detach {
		if os.Getenv("TMUX") != "" {
			fmt.Printf("Already inside tmux. Attach with: tmux switch-client -t %s\n", tmux.GetSessionName())
		} else {
			if err := tmux.AttachSession(); err != nil {
				return fmt.Errorf("maestro up: attach: %w", err)
			}
		}
	}
	return nil
}

// runDown gracefully shuts down the formation.
func runDown(args []string) error {
	fs := newFlagSet("maestro down")
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro down: %v\nusage: maestro down", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro down: unexpected argument: %s\nusage: maestro down", fs.Arg(0))}
	}

	maestroDir, err := requireMaestroDir("down")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		// Config may be corrupt, but 'down' must still be able to stop the daemon.
		// Proceed with zero config — UDS/PID-based shutdown works without it.
		fmt.Fprintf(os.Stderr, "Warning: could not load config: %v\nProceeding with default config.\n", err)
	}

	if err := formation.RunDown(maestroDir, cfg); err != nil {
		return fmt.Errorf("maestro down: %w", err)
	}
	return nil
}
