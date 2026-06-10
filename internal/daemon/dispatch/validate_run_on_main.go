package dispatch

import (
	"errors"
	"fmt"
	"os"

	"github.com/msageha/maestro_v2/internal/model"
)

// ErrRunOnMainPreflightRejected is returned when a run_on_main task fails the
// dispatch pre-flight: the assigned worker does not run claude-code, or the
// command's integration branch has not been published to base yet. Both are
// non-retryable for this queue entry — re-dispatching would fail identically
// on every scan (the publish gate cannot advance while the task itself is
// pending), so the queue apply path terminates the entry as failed and
// surfaces the reason to the Planner.
var ErrRunOnMainPreflightRejected = errors.New("dispatch: run_on_main pre-flight rejected")

// integrationStatusReader is the slice of WorktreeResolver the pre-flight
// needs; declared separately so tests can stub just this method.
type integrationStatusReader interface {
	GetIntegrationStatus(commandID string) (model.IntegrationStatus, error)
}

// validateRunOnMainPreflight enforces the two mechanical invariants for
// run_on_main tasks at the last gate before pane delivery. The plan API
// (submit / add-task) enforces the same rules earlier with friendlier
// errors; this pre-flight is defense-in-depth for queue entries that
// predate those validations or were produced by bypass paths.
//
//  1. Runtime: the worker must run claude-code. The read-only guard on the
//     main working directory (@run_on_main pane variable consumed by the
//     PreToolUse policy hook) only exists for claude-code; codex / gemini
//     workers launch with sandbox-bypass flags and would have no technical
//     barrier against mutating main.
//
//  2. Publish ordering: the command's integration branch must be published
//     (or never have produced integration state). Verifying main before this
//     command's outputs land there reads stale code and fails spuriously,
//     while the pending task simultaneously blocks the publish gate — a
//     deadlock (Report cmd_1777330979).
//
// Status semantics for the publish gate:
//   - state file absent → allow: the command never created worktree state
//     and there is nothing of this command's to wait for.
//   - created → allow: integration exists but holds no merged worker output
//     (run_on_main-only verification command); main already reflects every
//     published predecessor.
//   - published → allow: the intended post-publish verification window.
//   - anything else (merging/merged/conflict/partial_merge/publishing/
//     publish_failed/quarantined/failed) → reject: worker output exists that
//     has not reached main.
func validateRunOnMainPreflight(task *model.Task, workerID string, workerCfg model.WorkerConfig, wm integrationStatusReader) error {
	if task == nil || !task.RunOnMain {
		return nil
	}

	workerModel := workerCfg.ModelFor(workerID)
	if runtime, _ := model.ParseRuntimeFromModel(workerModel); runtime != model.RuntimeClaudeCode {
		return fmt.Errorf("%w: worker %s runs model %q (runtime %s); run_on_main tasks require a claude-code worker because only claude-code enforces the read-only main guard",
			ErrRunOnMainPreflightRejected, workerID, workerModel, runtime)
	}

	if wm == nil {
		// Worktree mode disabled: no integration/publish pipeline exists, so
		// there is no ordering to enforce.
		return nil
	}
	status, err := wm.GetIntegrationStatus(task.CommandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// Transient read failure: do not terminate the task over an IO blip.
		// Returning a plain (non-sentinel) error keeps the lease, and the
		// standard lease-expiry recovery re-attempts on a later scan.
		return fmt.Errorf("run_on_main pre-flight: read integration status for %s: %w", task.CommandID, err)
	}
	switch status {
	case model.IntegrationStatusPublished, model.IntegrationStatusCreated:
		return nil
	default:
		return fmt.Errorf("%w: integration status for command %s is %q, not published; run_on_main tasks verify the published main branch (submit them as a separate command after publish, or use run_on_integration for pre-publish verification)",
			ErrRunOnMainPreflightRejected, task.CommandID, status)
	}
}
