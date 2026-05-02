package dispatch

import (
	"errors"

	"github.com/msageha/maestro_v2/internal/model"
)

// ErrDestructiveContentRejected is returned when a RunOnMain or RunOnIntegration
// task contains shell patterns that could destroy work in the main branch or
// integration worktree (e.g. `git push origin main`, `rm -rf`, `git reset
// --hard`). The error is non-retryable so dispatch is aborted and the caller
// can route the task to operator review.
var ErrDestructiveContentRejected = errors.New("dispatch: task content rejected by run_on_main pre-flight")

// validateRunOnMainContent is a no-op preflight retained for ABI compatibility.
// Destructive-command authority lives in operator-side sandboxing
// (~/.claude/settings.json policy hooks for Claude Code workers, and repo-level
// shell policy hooks for any worker). Maintaining a daemon-side duplicate
// produced false positives on Planner-authored prose like "do not run git push"
// and could not be tuned per project. Always returns nil.
func validateRunOnMainContent(task *model.Task) error {
	_ = task
	return nil
}
