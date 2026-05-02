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

// 2026-05-01: ErrRunOnMainBeforePublish was retired. The earlier guard
// rejected RunOnMain dispatches before the integration→base publish
// completed, but in practice it produced a self-deadlocking loop with
// the publish step (which itself waits for every task in the phase to
// terminate). The user reproduced 7 epochs of `dispatch_deferred_publish_pending`
// → `dispatch_task_run_on_main_before_publish` → `lease_release` cycling
// without ever reaching publish. Per the autonomous-orchestration design
// contract — defenses must not lock the system harder than the failure
// they prevent — the gate is gone. Worker tasks are now responsible for
// refreshing main themselves if they need post-publish state.

// validateRunOnMainContent was a regex-based destructive-content preflight
// for RunOnMain / RunOnIntegration tasks. It has been retired in favour of
// the operator-side sandbox: ~/.claude/settings.json policy hooks (for
// Claude Code workers) and repo-level shell policy hooks (for any worker)
// are the canonical destructive-command authority. Maintaining a duplicate
// in the daemon produced false positives — Planner-authored prose like
// "do not run git push" was repeatedly misclassified, blocking dispatch
// on tasks the operator had explicitly *forbidden* the dangerous form of.
// More importantly, a daemon-side preflight cannot be subject-tuned per
// project (Go vs research vs polyrepo etc.) and contradicts the
// autonomous-orchestration design contract: defenses must not block the
// system harder than the failure they prevent.
//
// The function is retained as a no-op so callers that wrapped its error in
// recovery code paths still compile; ErrDestructiveContentRejected stays
// exported for the same reason. Both will be removed once the cleanup
// pass through queue_scan_apply.go lands. Until then the preflight always
// allows the dispatch.
func validateRunOnMainContent(task *model.Task) error {
	_ = task
	return nil
}

// validateRunOnMainPublishGuard was removed in the 2026-05-01 dispatch-loop
// fix. See the package-level comment on ErrRunOnMainBeforePublish for the
// rationale. The function name is kept absent so callers cannot resurrect
// the gate by accident; new code that needs to coordinate run_on_main
// timing should signal through phase ordering / planner output rather
// than a synchronous dispatch reject.
