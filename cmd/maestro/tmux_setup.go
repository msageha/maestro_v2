package main

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/validate"
)

// setupTmuxSession validates the project name and configures the tmux session name.
// This is shared across all commands that need a tmux session (up, daemon, status, agent exec).
//
// The session name is just "maestro-<projectName>" for attach UX. Per-checkout
// isolation is handled at the socket layer (BuildMaestroSocketName), so two
// checkouts of the same project still get distinct tmux servers and cannot
// step on each other.
func setupTmuxSession(cmd, maestroDir string, cfg model.Config) error {
	if err := validate.ProjectName(cfg.Project.Name); err != nil {
		return fmt.Errorf("maestro %s: invalid project name: %w", cmd, err)
	}
	tmux.SetSessionName(tmux.BuildMaestroSessionName(cfg.Project.Name))
	// Use a per-instance tmux socket. Sharing the default socket lets
	// concurrent maestro instances race on SESSION_LOST and
	// autoAcceptTrustDialog (Report 2026-05-06 P0).
	tmux.SetTmuxSocket(tmux.BuildMaestroSocketName(cfg.Project.Name, maestroDir))
	return nil
}
