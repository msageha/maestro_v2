package main

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/validate"
)

// setupTmuxSession validates the project name and configures the tmux session name.
// This is shared across all commands that need a tmux session (up, daemon, status, agent exec).
func setupTmuxSession(cmd string, cfg model.Config) error {
	if err := validate.ProjectName(cfg.Project.Name); err != nil {
		return fmt.Errorf("maestro %s: invalid project name: %w", cmd, err)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)
	return nil
}
