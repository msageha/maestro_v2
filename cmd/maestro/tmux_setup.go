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
// The session name combines projectName with an 8-hex-char hash of the
// absolute maestroDir so that two checkouts of the same project (or two
// repos that happen to share project.name) get distinct sessions instead of
// colliding on a single global "maestro-<name>" slot. The collision case
// would otherwise cause `maestro up` in repo B to attach to (and tear down)
// the daemon/agents of repo A.
func setupTmuxSession(cmd, maestroDir string, cfg model.Config) error {
	if err := validate.ProjectName(cfg.Project.Name); err != nil {
		return fmt.Errorf("maestro %s: invalid project name: %w", cmd, err)
	}
	tmux.SetSessionName(tmux.BuildMaestroSessionName(cfg.Project.Name, maestroDir))
	// Use a per-instance tmux socket. Sharing the default socket lets
	// concurrent maestro instances race on SESSION_LOST and
	// autoAcceptTrustDialog (Report 2026-05-06 P0).
	tmux.SetTmuxSocket(tmux.BuildMaestroSocketName(cfg.Project.Name, maestroDir))
	return nil
}
