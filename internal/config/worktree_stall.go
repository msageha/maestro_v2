// Package config provides thin schema-level helpers around model.Config that
// are convenient to test in isolation from the daemon. The canonical config
// structures themselves still live in internal/model.
package config

import "github.com/msageha/maestro_v2/internal/model"

// DefaultWorktreeStallTimeoutMinutes is the fallback used by
// model.WorktreeConfig.EffectiveStallTimeoutMinutes when the user has not set
// worktree.stall_timeout_minutes in config.yaml.
const DefaultWorktreeStallTimeoutMinutes = 30

// WorktreeStallTimeoutMinutes returns the effective stall-detection timeout
// for the given worktree config in minutes.
func WorktreeStallTimeoutMinutes(w model.WorktreeConfig) int {
	return w.EffectiveStallTimeoutMinutes()
}
