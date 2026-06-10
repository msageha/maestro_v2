package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// leaseInvalidReason checks the core lease invariants shared between Phase C
// fencing and heartbeat validation: task must be in_progress with a matching
// epoch. Returns "" if valid, or "status"/"epoch" describing the failure.
func leaseInvalidReason(status model.Status, leaseEpoch, expectedEpoch int) string {
	if status != model.StatusInProgress {
		return "status"
	}
	if leaseEpoch != expectedEpoch {
		return "epoch"
	}
	return ""
}

// isFenceStale checks whether a queue entry has been modified since Phase A
// by comparing lease epoch, status, and expiry. Used by Phase C apply methods
// for both dispatch results and busy-check results.
//
// The expiry component is intentionally a STRICT string equality, even though
// lease extensions (worker heartbeat, grace TTL) change the expiry without
// bumping the epoch. That is the point: an expiry that moved between the
// Phase A snapshot and the Phase C apply means some concurrent actor touched
// the lease while Phase B ran without the scan lock. The only such actor at
// a matching epoch is the heartbeat handler — and a current-epoch heartbeat
// is positive evidence that the worker holds this dispatch and is alive.
// Dropping the snapshot-based result then defers to that fresher evidence:
//
//   - busy-check "non-busy → release" verdict: releasing a lease the worker
//     just heartbeat-extended would re-dispatch a live task. Dropping the
//     verdict is the safe outcome.
//   - dispatch failure verdict: a genuine delivery failure means the worker
//     never received the envelope and cannot heartbeat at this epoch, so a
//     concurrent extension contradicts the verdict; keep the lease and let
//     lease-expiry recovery decide on a later scan.
//   - dispatch success verdict: the queue entry is already in_progress and
//     persisted from Phase A; the only loss is a lagging state-side
//     lifecycle marker, which result_write / reconcilers catch up.
//
// Do NOT "fix" this by allowing forward-extended expiries to pass the fence —
// that would reintroduce the release-after-heartbeat race on the busy-check
// path.
//
//nolint:unused // exercised from queue_scan_helpers_test.go (golangci-lint runs with tests:false)
func isFenceStale(status model.Status, leaseEpoch int, leaseExpiresAt *string, expectedEpoch int, expectedExpiresAt string) bool {
	if leaseInvalidReason(status, leaseEpoch, expectedEpoch) != "" {
		return true
	}
	return leaseExpiresAt == nil || *leaseExpiresAt != expectedExpiresAt
}

// FenceRejection describes why a Phase C fence check rejected an apply.
// A zero value (Reason == "") means the fence is valid.
type FenceRejection struct {
	Reason string // "status", "epoch", "expiry", or "" (valid)
}

// Stale returns true if the fence was rejected for any reason.
func (fr FenceRejection) Stale() bool { return fr.Reason != "" }

// String returns a human-readable description suitable for structured logs.
func (fr FenceRejection) String() string {
	if fr.Reason == "" {
		return "valid"
	}
	return fr.Reason
}

// checkResultFencing performs the same fence check as isFenceStale but returns
// a FenceRejection that indicates the specific reason for rejection. This
// allows callers to produce more actionable log messages. See the isFenceStale
// comment for why the expiry comparison is a strict equality and must stay so.
func checkResultFencing(status model.Status, leaseEpoch int, leaseExpiresAt *string, expectedEpoch int, expectedExpiresAt string) FenceRejection {
	if reason := leaseInvalidReason(status, leaseEpoch, expectedEpoch); reason != "" {
		return FenceRejection{Reason: reason}
	}
	if leaseExpiresAt == nil || *leaseExpiresAt != expectedExpiresAt {
		return FenceRejection{Reason: "expiry"}
	}
	return FenceRejection{}
}

// isEpochStale performs epoch-only validation for cases where only the epoch
// matters (e.g., lightweight pre-checks that don't need full fence validation).
//
//nolint:unused // exercised from queue_scan_helpers_test.go (golangci-lint runs with tests:false)
func isEpochStale(leaseEpoch, expectedEpoch int) bool {
	return leaseEpoch != expectedEpoch
}

// timeParseCache caches time.Parse(time.RFC3339, ...) results within a scan
// cycle to avoid repeated parsing of the same timestamp strings on hot paths.
// A nil receiver falls back to time.Parse without caching.
type timeParseCache struct {
	m map[string]time.Time
}

func newTimeParseCache() *timeParseCache {
	return &timeParseCache{m: make(map[string]time.Time)}
}

// ParseRFC3339 parses an RFC3339 timestamp, returning a cached result if
// available. A nil receiver falls back to time.Parse(time.RFC3339, s).
func (c *timeParseCache) ParseRFC3339(s string) (time.Time, error) {
	if c == nil {
		return time.Parse(time.RFC3339, s)
	}
	if t, ok := c.m[s]; ok {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return t, err
	}
	c.m[s] = t
	return t, nil
}

// Reset clears the cache for a new scan cycle.
func (c *timeParseCache) Reset() {
	if c == nil {
		return
	}
	clear(c.m)
}

// isMaxInProgressTimeout checks whether the elapsed time since the given
// RFC3339 timestamp exceeds maxMin minutes. Returns false if the timestamp
// cannot be parsed (scan-safe: parse errors are treated as "not timed out").
func isMaxInProgressTimeout(now time.Time, timestampRFC3339 string, maxMin int, tc *timeParseCache) bool {
	t, err := tc.ParseRFC3339(timestampRFC3339)
	if err != nil {
		return false
	}
	return now.Sub(t) >= time.Duration(maxMin)*time.Minute
}

// taskElapsedSinceDispatch returns a human-readable string describing how
// long the task has been actively held since its in_progress timestamp,
// falling back to the last UpdatedAt when in_progress_at is missing. Used
// purely for log surface to make hang-detection diagnostics tractable —
// the caller never branches on the value, so any parse failure quietly
// degrades to "?".
func taskElapsedSinceDispatch(task *model.Task, now time.Time, tc *timeParseCache) string {
	stamp := ""
	if task.InProgressAt != nil && *task.InProgressAt != "" {
		stamp = *task.InProgressAt
	} else {
		stamp = task.UpdatedAt
	}
	if stamp == "" {
		return "?"
	}
	t, err := tc.ParseRFC3339(stamp)
	if err != nil {
		return "?"
	}
	return now.Sub(t).Round(time.Second).String()
}

// maxGraceLeaseDuration returns the maximum cumulative duration for grace lease
// extensions. Computed as max_in_progress_min / 3, with a floor of
// scanInterval * 3 to allow at least a few scan cycles.
func maxGraceLeaseDuration(maxInProgressMin, scanIntervalSec int) time.Duration {
	graceMin := maxInProgressMin / 3
	grace := time.Duration(graceMin) * time.Minute
	floor := time.Duration(scanIntervalSec*3) * time.Second
	if grace < floor {
		return floor
	}
	return grace
}

// isGraceLeaseExceeded checks whether the cumulative grace lease extension
// period has exceeded the given limit. Grace period starts approximately at
// updatedAt + dispatchLease (when the original lease first expired).
func isGraceLeaseExceeded(now time.Time, updatedAtRFC3339 string, dispatchLease, graceLimit time.Duration, tc *timeParseCache) bool {
	t, err := tc.ParseRFC3339(updatedAtRFC3339)
	if err != nil {
		return false
	}
	graceStart := t.Add(dispatchLease)
	return now.Sub(graceStart) >= graceLimit
}

// buildGlobalInFlightSet scans ALL task queues to find workers with in_progress
// tasks that have valid (non-expired) leases. The returned map is keyed by
// worker ID (derived from queue file path); a true value means the worker has
// at least one non-expired in_progress task and should not receive new dispatches.
//
// Preconditions:
//   - Callers must hold scanMu.Lock or ensure taskQueues is a consistent
//     snapshot (e.g., loaded atomically before the call). The function reads
//     from the provided map without acquiring any locks itself.
//
// This function is read-only and does not modify taskQueues or acquire any locks.
func (qh *QueueHandler) buildGlobalInFlightSet(taskQueues map[string]*taskQueueEntry) map[string]bool {
	inFlight := make(map[string]bool)
	for queueFile, tq := range taskQueues {
		workerID := workerIDFromPath(queueFile)
		if workerID == "" {
			continue
		}
		for _, task := range tq.Queue.Tasks {
			if task.Status == model.StatusInProgress && !qh.leaseManager.IsLeaseExpired(task.LeaseExpiresAt) {
				inFlight[workerID] = true
				break
			}
		}
	}
	return inFlight
}

// phaseTasksAllTerminal reports whether every task of the given phase has
// reached a terminal effective status (any of completed/failed/cancelled/
// dead_letter/aborted/timed_out). Used by the merge collector to decide
// whether a phase has converged enough to push worker output onto
// integration — even a phase that ended in PhaseStatusFailed should
// have its completed-task outputs landed once every task finished, so
// publish/cleanup paths can proceed without leaving worker worktrees
// permanently at WorktreeStatusActive (Report 2026-05-04 stall).
//
// Lineage-aware via GetEffectiveTaskStatus: a task whose lineage
// successor finished is treated as if it had reached the successor's
// status, the same view checkActivePhaseCompletion uses. Tasks the
// state reader cannot resolve (missing entry, transient error) fall
// back to "non-terminal" so a degraded read does not race the gate.
func phaseTasksAllTerminal(stateReader StateReader, commandID string, phase PhaseInfo) bool {
	if stateReader == nil {
		return false
	}
	taskIDs := phase.TaskIDs
	if len(taskIDs) == 0 {
		taskIDs = phase.RequiredTaskIDs
	}
	if len(taskIDs) == 0 {
		return false
	}
	for _, taskID := range taskIDs {
		status, err := stateReader.GetEffectiveTaskStatus(commandID, taskID)
		if err != nil {
			return false
		}
		if !model.IsTerminal(status) {
			return false
		}
	}
	return true
}

// collectWorktreePhaseMerges detects phases that just completed and collects
// merge work items for Phase B execution. Runs in Phase A under scanMu.Lock.
// Only performs fast in-memory checks — all git I/O is deferred to Phase B.
// Skips phases that have already been merged (tracked in worktree command state).
func (qh *QueueHandler) collectWorktreePhaseMerges(commandID string, taskQueues map[string]*taskQueueEntry) []worktreeMergeItem {
	if !qh.dependencyResolver.HasStateReader() || qh.worktreeManager == nil {
		return nil
	}

	phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(commandID)
	if err != nil {
		qh.log(LogLevelDebug,
			"phase_merge_skip command=%s reason=get_phases_error error=%v",
			commandID, err)
		return nil
	}

	// Load worktree state to check already-merged phases
	cmdState, err := qh.worktreeManager.GetCommandState(commandID)
	if err != nil {
		qh.log(LogLevelDebug,
			"phase_merge_skip command=%s reason=load_state_error error=%v",
			commandID, err)
		return nil
	}

	// H10: do not collect any merge work for quarantined integrations.
	// Quarantined is terminal and requires manual operator intervention; without
	// this gate Phase B would re-enter MergeToIntegration on every scan and
	// either spin on the early-return error or attempt mutating git ops.
	if cmdState.Integration.Status == model.IntegrationStatusQuarantined {
		return nil
	}

	// Conflict / partial_merge gate: while a worker is in Conflict or
	// Resolving status, the resume-merge pipeline owns the integration
	// branch. Re-entering Phase B's MergeToIntegration with the *remaining*
	// (non-conflict) workers can:
	//
	//   - Re-attempt phase merges that already succeeded once (the conflict
	//     blocked MarkPhaseMerged so the phase is still collected on every
	//     scan), pinning each fresh merge_conflict signal to the *first*
	//     unmerged phase rather than the phase the worker belongs to.
	//   - Re-emit duplicate merge_conflict signals after the previous one was
	//     delivered and dropped, because Phase C upserts against the in-memory
	//     queue only.
	//
	// AutoRecover / ResumeMerge / AutoRecoverAfterResolution are still able
	// to drive recovery through wm.signalStore + tryMergeWorker — Phase B's
	// general auto-merge collector is the wrong path while the integration
	// is in a recovery state, so we step out and let the resolver pipeline
	// run uncontested.
	if (cmdState.Integration.Status == model.IntegrationStatusConflict ||
		cmdState.Integration.Status == model.IntegrationStatusPartialMerge) &&
		hasConflictOrResolvingWorker(cmdState.Workers) {
		return nil
	}

	// Build workerID → task purpose map from task queues
	workerPurposes := buildWorkerPurposes(commandID, taskQueues)

	// Fallback for commands that declare no phases: worker worktree writes
	// can still occur, so collect a single implicit-phase merge once all
	// tasks terminate without failures.
	if len(phases) == 0 {
		return qh.collectImplicitWorktreeMerge(commandID, cmdState, taskQueues, workerPurposes)
	}

	// Build workerIDs once outside the phase loop.
	//
	// Skip workers in Conflict/Resolving state: those are owned by the
	// resume-merge pipeline (ResumeMerge → attemptResolvedMerges →
	// commitResolvedWorkerChanges), which bypasses the normal transition
	// machine to commit the resolution edits. Including them here causes the
	// Phase B auto-commit path to call CommitWorkerChanges, which fails the
	// `resolving → committed` transition guard, records a `commit_failed`
	// signal, and blocks publishing even after the resolution task succeeded.
	workerIDs := eligibleWorkerIDsForAutoCommit(cmdState.Workers)
	workerIDs = qh.filterWorkersAwaitingCommitRecovery(commandID, workerIDs, cmdState, taskQueues)
	if len(workerIDs) == 0 {
		// Surface the empty-worker-set case so operators inspecting a
		// stalled command can tell whether the merge collector even saw
		// anything to commit. Reports of 2026-05-04 saw a stall where
		// no worker_committed log fired and there was no diagnostic to
		// distinguish "merge collector did not run" from "no eligible
		// workers".
		var workerStatuses []string
		for _, ws := range cmdState.Workers {
			workerStatuses = append(workerStatuses, ws.WorkerID+":"+string(ws.Status))
		}
		qh.log(LogLevelDebug,
			"phase_merge_skip command=%s reason=no_eligible_workers integration_status=%s workers=%v",
			commandID, cmdState.Integration.Status, workerStatuses)
		return nil
	}

	items := make([]worktreeMergeItem, 0, len(phases))
	stateReader := qh.dependencyResolver.GetStateReader()
	for _, phase := range phases {
		// Skip phases already merged.
		if cmdState.MergedPhases != nil {
			if _, merged := cmdState.MergedPhases[phase.ID]; merged {
				qh.log(LogLevelDebug,
					"phase_merge_skip command=%s phase=%s reason=already_merged",
					commandID, phase.ID)
				continue
			}
		}
		// Only merge if this phase has tasks. We accept either TaskIDs
		// (full set) or RequiredTaskIDs (legacy callers / fixtures that
		// only populate the required slice). All-optional phases would
		// otherwise have an empty RequiredTaskIDs but a populated
		// TaskIDs and were silently skipped by the previous gate.
		if len(phase.TaskIDs) == 0 && len(phase.RequiredTaskIDs) == 0 {
			qh.log(LogLevelDebug,
				"phase_merge_skip command=%s phase=%s reason=no_tasks",
				commandID, phase.ID)
			continue
		}
		// Phase status is no longer a hard gate. Earlier versions skipped
		// failed/cancelled/timed_out phases entirely so partial outputs
		// would not leak onto integration, but Report 2026-05-04 pinned
		// the resulting deadlock: a phase with 6 completed tasks + 2 failed
		// goes to PhaseStatusFailed → merge collector skips it → worker
		// worktrees stay at WorktreeStatusActive with uncommitted output →
		// publish gate's pendingWorker check defers cleanup forever (the
		// worker output is "real work"), and the daemon loops emitting
		// `worktree_publish_skip_no_commits_deferred` indefinitely.
		// Operator intervention was the only escape.
		//
		// New policy: collect the merge regardless of phase outcome. The
		// underlying autonomous-LLM-orchestration contract is "trust worker
		// output verbatim and let the next stage decide" — failed plans
		// already have a separate publish-gate check that blocks main
		// publish on PlanStatusFailed/Cancelled, so partial integration
		// state never reaches main. What the merge buys is forward
		// progress: worker.Status advances to Committed/Integrated, the
		// publish gate stops blocking on Active workers, and cleanup can
		// run cleanly. Operators inspecting integration after a failed
		// plan see exactly what the workers produced.
		//
		// Phase merge is still gated on "all required tasks have
		// terminated" — a partially-completed phase whose tasks are still
		// in flight should not be force-merged (a successor task may
		// produce more work for the same worker).
		if !phaseTasksAllTerminal(stateReader, commandID, phase) {
			qh.log(LogLevelDebug,
				"phase_merge_skip command=%s phase=%s phase_status=%s reason=tasks_not_all_terminal",
				commandID, phase.ID, phase.Status)
			continue
		}

		qh.log(LogLevelInfo,
			"phase_merge_collected command=%s phase=%s phase_status=%s workers=%v",
			commandID, phase.ID, phase.Status, workerIDs)
		items = append(items, worktreeMergeItem{
			CommandID:      commandID,
			PhaseID:        phase.ID,
			WorkerIDs:      workerIDs,
			WorkerPurposes: workerPurposes,
		})
	}

	return items
}

// Removed: buildWorkerExpectedPaths. The expected_paths gate that policed
// worker outputs against a Planner-declared file list was retired together
// with the rest of the commit-policy gates — workers' diffs are now
// committed verbatim and downstream verify catches anomalies. Re-introducing
// the builder would re-enable that gate by side effect.

// buildWorkerPurposes builds a workerID → task purpose map from task queues.
// Uses the most recently dispatched task's purpose for each worker.
// taskQueues is keyed by queue file path, so we iterate all entries.
func buildWorkerPurposes(_ string, taskQueues map[string]*taskQueueEntry) map[string]string {
	purposes := make(map[string]string)
	for _, tqEntry := range taskQueues {
		for _, task := range tqEntry.Queue.Tasks {
			if task.LeaseOwner != nil && *task.LeaseOwner != "" && task.Purpose != "" {
				purposes[*task.LeaseOwner] = task.Purpose
			}
		}
	}
	if len(purposes) == 0 {
		return nil
	}
	return purposes
}

// hasExpiredLeases checks whether any queue entry has an expired lease.
// Used to decide whether to prioritize recovery over dispatch (spec §5.8.1).
func (qh *QueueHandler) hasExpiredLeases(
	taskQueues map[string]*taskQueueEntry,
	cq *model.CommandQueue,
	nq *model.NotificationQueue,
) bool {
	for _, cmd := range cq.Commands {
		if cmd.Status == model.StatusInProgress && qh.leaseManager.IsLeaseExpired(cmd.LeaseExpiresAt) {
			return true
		}
	}
	for _, tq := range taskQueues {
		for _, task := range tq.Queue.Tasks {
			if task.Status == model.StatusInProgress && qh.leaseManager.IsLeaseExpired(task.LeaseExpiresAt) {
				return true
			}
		}
	}
	for _, ntf := range nq.Notifications {
		if ntf.Status == model.StatusInProgress && qh.leaseManager.IsLeaseExpired(ntf.LeaseExpiresAt) {
			return true
		}
	}
	return false
}

// collectWorktreePublishAndCleanup checks if a command is ready for worktree
// publish-to-base or cleanup. Returns publish and cleanup items for Phase B.
// Runs in Phase A under scanMu.Lock — only fast checks and YAML reads.
func (qh *QueueHandler) collectWorktreePublishAndCleanup(
	commandID string,
	commandContent string,
	taskQueues map[string]*taskQueueEntry,
) ([]worktreePublishItem, []worktreeCleanupItem) {
	// Load worktree state
	cmdState, err := qh.worktreeManager.GetCommandState(commandID)
	if err != nil {
		return nil, nil
	}

	// Check if all tasks for this command are terminal
	allTerminal, taskHasFailed := qh.checkCommandTasksTerminal(commandID, taskQueues)
	if !allTerminal {
		return nil, nil
	}

	// State-side gate: even when every queue task has reached a terminal
	// status, the command-state TaskStates view can still hold non-terminal
	// entries (verify_pending, repair_pending, paused_for_replan, or a
	// freshly registered repair task at planned). Without this gate, a
	// verify command mid-flight against the worker worktree could be
	// masked when the worktree is torn down on the publish-driven cleanup
	// pass. Refusing to publish until the state side has also drained
	// means a verify failure has time to schedule its repair task before
	// main accepts the commit. Errors here fail closed (skip publish).
	if hasNonTerminal, err := qh.dependencyResolver.GetStateReader().HasNonTerminalTaskState(commandID); err != nil {
		if !errors.Is(err, ErrStateNotFound) {
			qh.log(LogLevelWarn, "worktree_publish_state_check_failed command=%s error=%v", commandID, err)
			return nil, nil
		}
		// State file not found is benign for non-phased commands that never
		// produced one — fall through and let phase / integration gates decide.
	} else if hasNonTerminal {
		qh.log(LogLevelDebug,
			"worktree_publish_blocked_state_non_terminal command=%s "+
				"(verify/repair/replan still in flight; deferring publish)",
			commandID)
		return nil, nil
	}

	// For phased commands, also verify all phases are terminal.
	// Errors fail closed (skip publish) to avoid premature publishing.
	phases, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(commandID)
	if err != nil {
		if !errors.Is(err, ErrStateNotFound) {
			qh.log(LogLevelWarn, "worktree_publish_phase_check_failed command=%s error=%v", commandID, err)
		}
		return nil, nil
	}
	// Determine "did the command finish unsuccessfully?" via phase status rather
	// than raw task status. Once Planner has replaced a failed task with a retry
	// via add_retry_task, the phase reopens and — on retry success — transitions
	// back to PhaseStatusCompleted. Judging by raw task failures instead would
	// permanently poison the command because the original Failed task stays in
	// the queue's history even after retry succeeds, making publish unreachable.
	// For commands with no phases (implicit path), fall back to the task-level
	// signal.
	hasFailed := false
	if len(phases) == 0 {
		hasFailed = taskHasFailed
	} else {
		for _, phase := range phases {
			if !model.IsPhaseTerminal(phase.Status) {
				return nil, nil
			}
			if phase.Status != model.PhaseStatusCompleted {
				hasFailed = true
			}
		}
	}

	var publishes []worktreePublishItem
	var cleanups []worktreeCleanupItem

	if hasFailed {
		// Race-safe reconciliation: a phase may have failed transiently
		// (e.g. verify_repair scheduled, retry-task replaced the failed
		// task) and by the time we reach here the lineage may have
		// already produced a successful retry. The persisted plan_status
		// field is updated by R4PlanStatus on a separate scan cycle, so
		// reading it here would race the reconciler — synthetic_failure
		// would land before R4 had a chance to flip plan_status to
		// completed, poisoning a recovered command. Instead, derive the
		// authoritative status directly from the live state via
		// plan.DeriveStatus, which is lineage-aware (a cancelled-then-
		// superseded task whose retry succeeded does not count as a
		// failure). On parse/read errors we fail open — defer to the
		// next scan rather than write a synthetic_failure that might be
		// wrong.
		state, ok := qh.loadCommandStateForDerivation(commandID)
		if !ok {
			qh.log(LogLevelDebug,
				"worktree_publish_state_load_deferred command=%s "+
					"(unable to derive authoritative status; deferring publish/synthetic_failure to next scan)",
				commandID)
			return publishes, cleanups
		}
		derived, deriveErr := plan.DeriveStatus(state)
		if deriveErr != nil {
			qh.log(LogLevelDebug,
				"worktree_publish_derive_failed command=%s error=%v "+
					"(deferring publish/synthetic_failure to next scan)",
				commandID, deriveErr)
			return publishes, cleanups
		}
		switch derived {
		case model.PlanStatusCompleted:
			qh.log(LogLevelInfo,
				"worktree_publish_proceeding_after_recovery command=%s derived_status=%s "+
					"(dead phases superseded; honoring derived status)",
				commandID, derived)
			// Fall through to integration-status switch below — let the
			// command publish normally.
		default:
			// Don't publish if the plan genuinely failed — partial results
			// stay on integration branch for operator inspection.
			//
			// Writing a synthetic failed CommandResult here lets
			// R3PlannerQueue walk the queue command from in_progress →
			// failed and R4PlanStatus reconcile state.PlanStatus on the
			// next scan, so the command terminates cleanly. We pass
			// the derived status into the synthetic result so that
			// continuous_handler / Orchestrator agree on the outcome.
			wrote := qh.writeSyntheticFailedPlannerResult(commandID, "phase_failed_publish_blocked")
			if wrote {
				qh.log(LogLevelInfo, "worktree_publish_skip_failed command=%s derived_status=%s",
					commandID, derived)
			} else {
				qh.log(LogLevelDebug,
					"worktree_publish_skip_failed_already_terminal command=%s derived_status=%s",
					commandID, derived)
			}
			if qh.config.Worktree.CleanupOnFailure {
				cleanups = append(cleanups, worktreeCleanupItem{
					CommandID: commandID,
					Reason:    "failure",
				})
			}
			return publishes, cleanups
		}
	}

	// No failures — check integration status to decide action
	switch cmdState.Integration.Status {
	case model.IntegrationStatusQuarantined:
		// Quarantined integrations must not be published; operator
		// intervention required. Emit WARN exactly once per
		// (command, reason) pair to avoid scan-tick log spam — the
		// state is durable and cannot autonomously progress past this
		// point, so re-warning every scan adds no information. Subsequent
		// observations are demoted to DEBUG.
		reasonKey := commandID + "::" + cmdState.Integration.QuarantineReason
		level := LogLevelDebug
		if _, alreadyLogged := qh.publishQuarantineLogged.LoadOrStore(reasonKey, struct{}{}); !alreadyLogged {
			level = LogLevelWarn
		}
		qh.log(level, "worktree_publish_quarantined command=%s reason=%s",
			commandID, cmdState.Integration.QuarantineReason)
		if qh.worktreeManager != nil {
			qh.worktreeManager.CleanupTempPublishBranch(commandID)
		}
		return publishes, cleanups
	case model.IntegrationStatusMerged:
		// Block publish if any worker had an unresolved auto-commit failure.
		// Otherwise, partially-committed phases would publish unmerged worker changes.
		if len(cmdState.CommitFailedWorkers) > 0 {
			qh.log(LogLevelWarn, "worktree_publish_blocked_commit_failed command=%s workers=%v",
				commandID, cmdState.CommitFailedWorkers)
			return publishes, cleanups
		}
		// Ready to publish
		publishes = append(publishes, worktreePublishItem{
			CommandID:      commandID,
			PublishMessage: commandContent,
		})
		qh.log(LogLevelInfo, "worktree_publish_collected command=%s status=%s", commandID, cmdState.Integration.Status)
	case model.IntegrationStatusPublishFailed:
		// Block publish if any worker had an unresolved auto-commit failure.
		if len(cmdState.CommitFailedWorkers) > 0 {
			qh.log(LogLevelWarn, "worktree_publish_blocked_commit_failed command=%s workers=%v",
				commandID, cmdState.CommitFailedWorkers)
			return publishes, cleanups
		}
		// Respect exponential backoff — do not re-queue until the backoff period has elapsed.
		if cmdState.Integration.NextPublishRetryAt != "" {
			nextRetry, err := qh.timeCache.ParseRFC3339(cmdState.Integration.NextPublishRetryAt)
			if err == nil && qh.clock.Now().Before(nextRetry) {
				qh.log(LogLevelDebug, "worktree_publish_backoff command=%s next_retry_at=%s",
					commandID, cmdState.Integration.NextPublishRetryAt)
				return publishes, cleanups
			}
		}
		// Backoff elapsed — retry publish
		publishes = append(publishes, worktreePublishItem{
			CommandID:      commandID,
			PublishMessage: commandContent,
		})
		qh.log(LogLevelInfo, "worktree_publish_retry_collected command=%s retry_count=%d",
			commandID, cmdState.Integration.PublishFailureCount)
	case model.IntegrationStatusPublished:
		// Already published — collect cleanup if configured and not yet cleaned
		if qh.config.Worktree.CleanupOnSuccess {
			cleanups = append(cleanups, worktreeCleanupItem{
				CommandID: commandID,
				Reason:    "success",
			})
		}
	case model.IntegrationStatusFailed:
		// An integration marked Failed (typically by fast_track_cleanup's
		// MarkIntegrationFailed) is a dead-end for the publish gate —
		// there's no path forward other than synthesising a planner
		// result so R3PlannerQueue can walk the queue command terminal
		// and Orchestrator can be notified. Without this branch the
		// command would sit in_progress forever and block Continuous
		// Mode from advancing iteration.
		//
		// If the failure marker landed while there are still unresolved
		// commit_failed workers, defer the synthetic failure so the
		// daemon's auto-commit retry path has a chance to clear the
		// failure on the next scan and reopen the publish gate. Without
		// this gate, a transient .git/index.lock would render the command
		// terminal even though the next scan would have succeeded.
		// CommitFailedWorkers is the durable signal.
		if len(cmdState.CommitFailedWorkers) > 0 {
			qh.log(LogLevelWarn,
				"worktree_publish_integration_failed_commit_retry_pending command=%s workers=%v "+
					"(deferring synthetic failure; auto-commit retry may succeed and reopen publish)",
				commandID, cmdState.CommitFailedWorkers)
			return publishes, cleanups
		}
		// Idempotent: writeSyntheticFailedPlannerResult is a no-op when a
		// planner result already exists for the command, so a recurring
		// scan only emits the loud "skip" log once.
		wrote := qh.writeSyntheticFailedPlannerResult(commandID, "integration_failed_publish_blocked")
		if wrote {
			qh.log(LogLevelWarn,
				"worktree_publish_integration_failed_synthetic command=%s "+
					"(integration marked failed; emitting synthetic planner result so the queue can close)",
				commandID)
		} else {
			qh.log(LogLevelDebug,
				"worktree_publish_integration_failed_already_synthesised command=%s",
				commandID)
		}
		if qh.config.Worktree.CleanupOnFailure {
			cleanups = append(cleanups, worktreeCleanupItem{
				CommandID: commandID,
				Reason:    "failure",
			})
		}
	case model.IntegrationStatusCreated:
		// No commits were ever made on the integration branch — typically a
		// command that resolved as a no-op (sleep verification, audit-only
		// task, research write-up that produced no diff). Without this case
		// the worktree sat at status=created forever, the publish gate kept
		// emitting `worktree_publish_not_ready integration_status=created`
		// every scan, and the Dashboard never observed cleanup. Treat the
		// derived plan status as authoritative: if the plan finished
		// (completed/cancelled/failed), schedule a cleanup and stop logging.
		// Pending plans are still skipped so we don't tear down a worktree
		// before any task has had a chance to commit.
		state, ok := qh.loadCommandStateForDerivation(commandID)
		planTerminal := false
		if ok {
			if derived, derr := plan.DeriveStatus(state); derr == nil {
				switch derived {
				case model.PlanStatusCompleted, model.PlanStatusFailed, model.PlanStatusCancelled:
					planTerminal = true
				}
			}
		}
		if !planTerminal {
			qh.log(LogLevelDebug, "worktree_publish_not_ready command=%s integration_status=%s",
				commandID, cmdState.Integration.Status)
			return publishes, cleanups
		}
		// Defer no_op_terminal cleanup if any worker still has uncommitted
		// output. The previous version of this gate only consulted the
		// cached WorktreeStatusActive flag, which mis-fires when a worker's
		// status field has drifted out of sync with the actual git state
		// (e.g. setWorkerStatus crash window, status overwritten by an
		// unrelated transition). Falling back to a live git probe via
		// IsWorkerAheadOrDirty closes that hole — Report 2026-05-03 issue-3
		// observed a single-task command whose worker had real diff in its
		// worktree but a non-Active status, so the gate let cleanup run
		// and silently destroyed the worker's output.
		var pendingWorker string
		var pendingReason string
		for _, ws := range cmdState.Workers {
			if ws.Status == model.WorktreeStatusActive {
				pendingWorker = ws.WorkerID
				pendingReason = "status_active"
				break
			}
			// Live git probe for any non-terminal worker state. Conflict /
			// Resolving are owned by the resume-merge pipeline and must
			// not block cleanup here. Cleanup-* / Failed are terminal from
			// our perspective so don't require a probe.
			switch ws.Status {
			case model.WorktreeStatusCreated,
				model.WorktreeStatusCommitted,
				model.WorktreeStatusIntegrated:
				dirty, perr := qh.worktreeManager.IsWorkerAheadOrDirty(commandID, ws.WorkerID)
				if perr != nil {
					// Probe failure → fail-safe (defer cleanup) so we never
					// destroy work because we couldn't confirm cleanliness.
					pendingWorker = ws.WorkerID
					pendingReason = "probe_failed:" + perr.Error()
					continue
				}
				if dirty {
					pendingWorker = ws.WorkerID
					pendingReason = "git_status_dirty_or_ahead"
				}
			}
			if pendingWorker != "" {
				break
			}
		}
		if pendingWorker != "" {
			qh.log(LogLevelWarn,
				"worktree_publish_skip_no_commits_deferred command=%s active_worker=%s reason=%s integration_status=created plan_terminal=true "+
					"(uncommitted worker output present; deferring no_op_terminal cleanup until incremental merge runs)",
				commandID, pendingWorker, pendingReason)
			return publishes, cleanups
		}
		qh.log(LogLevelInfo,
			"worktree_publish_skip_no_commits command=%s integration_status=created plan_terminal=true "+
				"(no diff to publish; scheduling cleanup)",
			commandID)
		// CleanupOnSuccess covers the no-op case too — the command finished
		// without producing changes, which is a healthy outcome.
		if qh.config.Worktree.CleanupOnSuccess {
			cleanups = append(cleanups, worktreeCleanupItem{
				CommandID: commandID,
				Reason:    "no_op_terminal",
			})
			// Mirror the orphan-cleanup log shape so observers that key off
			// "orphan_worktree_cleanup_triggered ... reason=no_op_terminal"
			// see the no-op publish path uniformly. The pre-existing
			// worktree_publish_skip_no_commits line stays for backward-compat.
			qh.log(LogLevelInfo,
				"orphan_worktree_cleanup_triggered command=%s cmd_status=in_progress integration_status=created elapsed=0s threshold=bypass(no_op) reason=no_op_terminal "+
					"(publish gate path; no diff to publish, terminal plan)",
				commandID)
		}
	default:
		// Not ready (merging, conflict, publishing)
		qh.log(LogLevelDebug, "worktree_publish_not_ready command=%s integration_status=%s",
			commandID, cmdState.Integration.Status)
	}

	return publishes, cleanups
}

// loadCommandStateForDerivation reads state/commands/<commandID>.yaml so the
// caller can run plan.DeriveStatus directly. Distinct from loadCommandPlanStatus,
// which only reads the persisted plan_status field — that field lags behind
// reality between the time a phase failed and the time R4PlanStatus catches
// up. publish-gate decisions need the live derivation to avoid persisting a
// stale synthetic_failure. Returns (nil, false) on any read/parse error.
func (qh *QueueHandler) loadCommandStateForDerivation(commandID string) (*model.CommandState, bool) {
	statePath := commandStatePath(qh.maestroDir, commandID)
	data, err := os.ReadFile(statePath) //nolint:gosec // controlled application state path
	if err != nil {
		return nil, false
	}
	if len(data) == 0 {
		return nil, false
	}
	var cs model.CommandState
	if err := yamlv3.Unmarshal(data, &cs); err != nil {
		return nil, false
	}
	return &cs, true
}

// isCommandPlannerIdle reports whether an in_progress command is currently
// not occupying the Planner pane. Used by collectPendingCommandDispatches
// to allow a fresh command to dispatch while a previously-dispatched command
// is in the "deferred to daemon recovery" wait state (every required task
// at paused_for_replan, or terminal-but-blocked-on-publish). The Planner
// pane is genuinely free in those scenarios, so blocking new dispatches
// stalls the entire queue for up to the R10 deadletter window.
//
// Planner-idle is a *conservative* predicate: when in doubt, return false so
// the legacy single-in-flight guard kicks in. False is also returned when
// the state file is missing/unreadable — a brand-new in_progress command
// that has not yet written state is definitely Planner-engaged.
func (qh *QueueHandler) isCommandPlannerIdle(commandID string) bool {
	cs, ok := qh.loadCommandStateForDerivation(commandID)
	if !ok || cs == nil {
		return false
	}
	// Pre-sealed plans are by definition Planner-engaged: the Planner is
	// still authoring tasks for the command.
	if cs.PlanStatus == model.PlanStatusPlanning {
		return false
	}
	// Filling/awaiting_fill phases mean the Planner has been asked to fill
	// the next phase — definitely not idle.
	for _, phase := range cs.Phases {
		if phase.Status == model.PhaseStatusFilling || phase.Status == model.PhaseStatusAwaitingFill {
			return false
		}
	}
	if len(cs.RequiredTaskIDs) == 0 {
		// No required tasks declared yet — Planner is mid-authoring.
		return false
	}
	// Walk required tasks. If any is not terminal AND not paused_for_replan,
	// the command is still "live": there is or will be a worker dispatch
	// the Planner needs to observe. Only when every required task is
	// terminal-or-paused do we declare Planner-idle.
	for _, id := range cs.RequiredTaskIDs {
		status, ok := cs.TaskStates[id]
		if !ok {
			return false
		}
		if model.IsTerminal(status) {
			continue
		}
		if status == model.StatusPausedForReplan {
			continue
		}
		return false
	}
	return true
}

// writeSyntheticFailedPlannerResult appends a synthetic failed
// CommandResult to results/planner.yaml on behalf of a Planner whose
// command cannot publish (failed phase blocks the publish gate) and who
// would otherwise leave the queue command in_progress forever.
// R3PlannerQueue then walks the queue command terminal on the next scan
// and R4PlanStatus reconciles state.PlanStatus, so a single write drives
// the whole "command goes terminal" propagation chain.
//
// The synthetic result is populated from state.TaskStates so the
// Orchestrator sees the actual task outcomes (which tasks completed,
// which failed) rather than an empty `tasks: []` payload (Report
// 2026-05-05 P1 v5). Worker attribution is left blank — only the
// state file is consulted to keep this recovery path resilient to
// missing or partial queue files.
//
// Returns true if a synthetic result was written, false if the call was
// a no-op (an existing result for commandID was found, an I/O error
// occurred, or the lock body returned early). Callers can use the bool
// to distinguish first-time emission (log at info) from subsequent
// recurring scans (log at debug) — keeps daemon.log readable when a
// terminal-failed command lingers because cleanup_on_failure=false.
func (qh *QueueHandler) writeSyntheticFailedPlannerResult(commandID, reason string) bool {
	resultPath := filepath.Join(qh.maestroDir, "results", "planner.yaml")
	wrote := false
	qh.lockMap.WithLock("result:planner", func() {
		var rf model.CommandResultFile
		data, err := os.ReadFile(resultPath) //nolint:gosec // controlled application result path
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			qh.log(LogLevelWarn,
				"synthetic_planner_result_load_failed command=%s reason=%s error=%v",
				commandID, reason, err)
			return
		}
		if len(data) > 0 {
			if err := yamlv3.Unmarshal(data, &rf); err != nil {
				qh.log(LogLevelWarn,
					"synthetic_planner_result_parse_failed command=%s reason=%s error=%v",
					commandID, reason, err)
				return
			}
		}
		for _, r := range rf.Results {
			if r.CommandID == commandID {
				return // idempotent: already have a result, wrote stays false
			}
		}
		resultID, err := model.GenerateID(model.IDTypeResult)
		if err != nil {
			qh.log(LogLevelWarn,
				"synthetic_planner_result_id_failed command=%s reason=%s error=%v",
				commandID, reason, err)
			return
		}
		nowStr := qh.clock.Now().UTC().Format(time.RFC3339)
		if rf.SchemaVersion == 0 {
			rf.SchemaVersion = 1
		}
		if rf.FileType == "" {
			rf.FileType = "result_command"
		}

		tasks, stats, summary := qh.deriveSyntheticPlannerPayload(commandID, reason)

		rf.Results = append(rf.Results, model.CommandResult{
			ID:        resultID,
			CommandID: commandID,
			Status:    model.StatusFailed,
			Summary:   summary,
			TaskStats: stats,
			Tasks:     tasks,
			CreatedAt: nowStr,
		})
		if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
			qh.log(LogLevelError,
				"synthetic_planner_result_write_failed command=%s reason=%s error=%v",
				commandID, reason, err)
			return
		}
		qh.log(LogLevelInfo,
			"synthetic_planner_result command=%s status=failed reason=%s tasks=%d completed=%d failed=%d "+
				"(R3/R4 will walk planner queue + state.plan_status to failed on next scan)",
			commandID, reason, stats.Total, stats.Completed, stats.Failed)
		wrote = true
	})
	return wrote
}

// deriveSyntheticPlannerPayload reads state.TaskStates for commandID and
// returns a Tasks slice, aggregated TaskStats, and a human-readable
// summary that surfaces actual completed / failed task counts and the
// IDs of failed tasks (so an Orchestrator scanning results/planner.yaml
// is not misled into thinking the command produced zero work). Best-
// effort: if the state file cannot be read, the tasks slice is empty
// and the summary mentions only the synthetic reason.
//
// The summary also surfaces integration merge state. When a command is
// failed at the publish gate but worker_merged events have already
// rolled per-worker commits onto the integration branch (data-loss
// avoidance — see failed_phase_merge_no_skip.md), the summary records
// `integration_state=merged (partial — main publish skipped)` so the
// Orchestrator (and a human reading results/planner.yaml) can see
// upfront that re-running the failed task may collide with already-
// integrated changes (Report 2026-05-05 P1 v5).
func (qh *QueueHandler) deriveSyntheticPlannerPayload(commandID, reason string) ([]model.CommandResultTask, model.TaskStats, string) {
	baseSummary := "synthetic_failure: " + reason + " (daemon recovery — planner did not finalise)"

	statePath := commandStatePath(qh.maestroDir, commandID)
	data, err := os.ReadFile(statePath) //nolint:gosec // controlled application state path
	if err != nil {
		return nil, model.TaskStats{}, baseSummary + integrationStateSummarySuffix(qh.maestroDir, commandID)
	}
	var cs model.CommandState
	if err := yamlv3.Unmarshal(data, &cs); err != nil {
		return nil, model.TaskStats{}, baseSummary + integrationStateSummarySuffix(qh.maestroDir, commandID)
	}
	if len(cs.TaskStates) == 0 {
		return nil, model.TaskStats{}, baseSummary + integrationStateSummarySuffix(qh.maestroDir, commandID)
	}

	taskIDs := make([]string, 0, len(cs.TaskStates))
	for id := range cs.TaskStates {
		taskIDs = append(taskIDs, id)
	}
	sort.Strings(taskIDs)

	tasks := make([]model.CommandResultTask, 0, len(taskIDs))
	failedIDs := make([]string, 0)
	for _, id := range taskIDs {
		status := cs.TaskStates[id]
		taskSummary := ""
		if reason, ok := cs.CancelledReasons[id]; ok && reason != "" {
			taskSummary = reason
		}
		tasks = append(tasks, model.CommandResultTask{
			TaskID:  id,
			Status:  status,
			Summary: taskSummary,
		})
		if status == model.StatusFailed || status == model.StatusDeadLetter {
			failedIDs = append(failedIDs, id)
		}
	}
	stats := model.ComputeTaskStats(tasks)

	summary := baseSummary
	summary += fmt.Sprintf(" — task_stats total=%d completed=%d failed=%d cancelled=%d",
		stats.Total, stats.Completed, stats.Failed, stats.Cancelled)
	if len(failedIDs) > 0 {
		summary += " — failed_tasks=" + strings.Join(failedIDs, ",")
	}
	summary += integrationStateSummarySuffix(qh.maestroDir, commandID)
	return tasks, stats, summary
}

// integrationStateSummarySuffix reads state/worktrees/<commandID>.yaml
// and returns a " — integration_state=..." suffix describing the
// integration branch lifecycle, including a partial-merge indicator
// when worker commits have already been merged into the integration
// branch despite the command failing at the publish gate. Returns an
// empty string when the worktree state file is unavailable.
func integrationStateSummarySuffix(maestroDir, commandID string) string {
	path := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	data, err := os.ReadFile(path) //nolint:gosec // controlled application state path
	if err != nil {
		return ""
	}
	var ws model.WorktreeCommandState
	if err := yamlv3.Unmarshal(data, &ws); err != nil {
		return ""
	}
	status := string(ws.Integration.Status)
	if status == "" {
		return ""
	}
	if ws.Integration.Status == model.IntegrationStatusMerged {
		return fmt.Sprintf(" — integration_state=%s (partial — worker commits integrated; main publish skipped due to failed phase)", status)
	}
	return " — integration_state=" + status
}

// checkCommandTasksTerminal checks if all tasks for a command across all task
// queues are in terminal state. Returns (allTerminal, hasFailed).
// Runs in Phase A under scanMu.Lock — iterates already-loaded in-memory queues.
func (qh *QueueHandler) checkCommandTasksTerminal(
	commandID string,
	taskQueues map[string]*taskQueueEntry,
) (bool, bool) {
	taskCount := 0
	hasFailed := false

	for _, tq := range taskQueues {
		for _, task := range tq.Queue.Tasks {
			if task.CommandID != commandID {
				continue
			}
			taskCount++
			if !model.IsTerminal(task.Status) {
				return false, false
			}
			if task.Status == model.StatusFailed || task.Status == model.StatusDeadLetter {
				hasFailed = true
			}
		}
	}

	if taskCount == 0 {
		return false, false // No tasks found — command not ready
	}
	return true, hasFailed
}

// commandHasAnyCompletedTask reports whether a command has at least one task
// at StatusCompleted across all worker queues. Used by the implicit-phase
// merge collector so a long-running command whose first task has finished
// can begin an incremental merge — without waiting for every task to
// terminate. This is the autonomous recovery for Planner patterns that
// emit dependent tasks across multiple workers without declaring phases:
// worker2 needs worker1's output, which would otherwise be invisible on
// the integration branch until the whole command finishes.
//
// hasFailed mirrors checkCommandTasksTerminal: a failed task short-circuits
// the merge collection so partial outputs are not pushed onto integration
// alongside a task the Planner is going to repair.
func commandHasAnyCompletedTask(
	commandID string,
	taskQueues map[string]*taskQueueEntry,
) (bool, bool) {
	hasCompleted := false
	hasFailed := false
	for _, tq := range taskQueues {
		for _, task := range tq.Queue.Tasks {
			if task.CommandID != commandID {
				continue
			}
			if task.Status == model.StatusCompleted {
				hasCompleted = true
			}
			if task.Status == model.StatusFailed || task.Status == model.StatusDeadLetter {
				hasFailed = true
			}
		}
	}
	return hasCompleted, hasFailed
}

// collectImplicitWorktreeMerge aggregates worktree merge work for commands
// that declare no phases.
//
// Behaviour: incremental merge. The collector emits a merge item as soon
// as at least one task has reached StatusCompleted, instead of waiting
// for every task to terminate. Without this, dependent tasks across
// multiple workers without declared phases would deadlock: worker1's
// completed output would never reach the integration branch, and
// worker2's dispatcher fast-forward to integration HEAD would pull an
// empty branch.
//
// Idempotency under repeated scans: MergeToIntegration's worker iteration
// already skips up-to-date worker branches and the auto-commit pass is a
// no-op when the worktree has nothing staged. The MergedPhases marker for
// __implicit_phase is therefore *not* used as a gate; it is set when a
// merge actually completes and serves as a hint for downstream code, but
// the collector keeps emitting items for new completions until the command
// terminates.
//
// hasFailed short-circuits the merge: a failed task indicates the Planner
// is going to drive a repair, and pushing partial outputs onto integration
// alongside that repair risks publishing half-done work. The publish path
// already blocks on plan-level failures, but holding back the merge keeps
// integration clean while the repair lands.
func (qh *QueueHandler) collectImplicitWorktreeMerge(
	commandID string,
	cmdState *model.WorktreeCommandState,
	taskQueues map[string]*taskQueueEntry,
	workerPurposes map[string]string,
) []worktreeMergeItem {
	if cmdState == nil {
		return nil
	}
	// Allow re-collection for created, partial_merge, conflict, failed, and
	// merged (the latter so an incremental merge can happen after an earlier
	// one already wrote some content). The state transition table already
	// permits these → merging.
	switch cmdState.Integration.Status {
	case model.IntegrationStatusCreated,
		model.IntegrationStatusPartialMerge,
		model.IntegrationStatusConflict,
		model.IntegrationStatusFailed,
		model.IntegrationStatusMerged:
		// eligible for (re-)merge collection
	default:
		return nil
	}

	// Gate: when integration has unresolved conflicts (conflict or partial_merge),
	// skip collection unless there are workers in a mergeable state (created,
	// active, committed, or failed). Workers in conflict/resolving must go
	// through the resolution pipeline (DispatchConflictResolution + resume-merge)
	// before being re-merged. Without this gate, MergeToIntegration would be
	// called on every scan cycle only to skip all conflict/resolving workers,
	// generating spurious logs and wasted git ops.
	if cmdState.Integration.Status == model.IntegrationStatusConflict ||
		cmdState.Integration.Status == model.IntegrationStatusPartialMerge {
		hasMergeableWorker := false
		for _, ws := range cmdState.Workers {
			switch ws.Status {
			case model.WorktreeStatusCreated,
				model.WorktreeStatusActive,
				model.WorktreeStatusCommitted,
				model.WorktreeStatusFailed:
				hasMergeableWorker = true
			}
			if hasMergeableWorker {
				break
			}
		}
		if !hasMergeableWorker {
			return nil
		}
	}

	if len(cmdState.Workers) == 0 {
		return nil
	}

	hasCompleted, hasFailed := commandHasAnyCompletedTask(commandID, taskQueues)
	if !hasCompleted || hasFailed {
		return nil
	}

	// See eligibleWorkerIDsForAutoCommit — Conflict/Resolving workers are owned
	// by ResumeMerge and must not be auto-committed here.
	workerIDs := eligibleWorkerIDsForAutoCommit(cmdState.Workers)
	workerIDs = qh.filterWorkersAwaitingCommitRecovery(commandID, workerIDs, cmdState, taskQueues)
	if len(workerIDs) == 0 {
		return nil
	}

	return []worktreeMergeItem{{
		CommandID:      commandID,
		PhaseID:        "__implicit_phase",
		WorkerIDs:      workerIDs,
		WorkerPurposes: workerPurposes,
	}}
}

// hasConflictOrResolvingWorker reports whether any worker in workers is in
// Conflict or Resolving status — i.e., the resume-merge pipeline currently
// owns the integration branch and Phase B's auto-commit + merge collector
// must yield to avoid duplicate merge attempts and stale-phase signal pinning.
func hasConflictOrResolvingWorker(workers []model.WorktreeState) bool {
	for _, ws := range workers {
		if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
			return true
		}
	}
	return false
}

// eligibleWorkerIDsForAutoCommit returns the worker IDs that Phase B's
// auto-commit + merge path may operate on. Workers in Conflict or Resolving
// status are excluded because they are owned by the resume-merge pipeline,
// which commits their resolution edits via commitResolvedWorkerChanges
// (bypassing the `resolving → committed` transition that the normal
// CommitWorkerChanges would reject). Without this filter, a resolving worker
// with dirty resolution files trips the invalid-transition guard, surfaces a
// spurious `commit_failed` signal, and blocks publish even after the
// resolution itself has succeeded.
func eligibleWorkerIDsForAutoCommit(workers []model.WorktreeState) []string {
	ids := make([]string, 0, len(workers))
	for _, ws := range workers {
		if ws.Status == model.WorktreeStatusConflict || ws.Status == model.WorktreeStatusResolving {
			continue
		}
		ids = append(ids, ws.WorkerID)
	}
	return ids
}

// filterWorkersAwaitingCommitRecovery suppresses duplicate commit_failed
// signals while a planner-driven recovery task is still pending.
//
// A commit policy failure leaves the worker's dirty worktree intact and records
// the worker in CommitFailedWorkers. Without this gate, every scan retries the
// same failed commit, removes the successfully delivered signal, then re-emits
// an identical commit_failed signal on the next scan. Once the Planner adds a
// recovery task and that task completes after the marker timestamp, the worker
// becomes eligible for one fresh auto-commit attempt.
func (qh *QueueHandler) filterWorkersAwaitingCommitRecovery(
	commandID string,
	workerIDs []string,
	cmdState *model.WorktreeCommandState,
	taskQueues map[string]*taskQueueEntry,
) []string {
	if cmdState == nil || len(cmdState.CommitFailedWorkers) == 0 || len(workerIDs) == 0 {
		return workerIDs
	}
	markerTime, err := time.Parse(time.RFC3339, cmdState.UpdatedAt)
	if err != nil {
		// If the marker timestamp is corrupt, keep the old fail-open behavior:
		// retrying is preferable to permanently stalling a command.
		return workerIDs
	}
	failed := make(map[string]struct{}, len(cmdState.CommitFailedWorkers))
	for _, workerID := range cmdState.CommitFailedWorkers {
		failed[workerID] = struct{}{}
	}
	filtered := make([]string, 0, len(workerIDs))
	for _, workerID := range workerIDs {
		if _, isCommitFailed := failed[workerID]; !isCommitFailed {
			filtered = append(filtered, workerID)
			continue
		}
		if qh.workerHasCompletedTaskAfter(commandID, workerID, markerTime, taskQueues) {
			filtered = append(filtered, workerID)
			continue
		}
		qh.log(LogLevelDebug,
			"worktree_commit_retry_suppressed command=%s worker=%s reason=awaiting_commit_recovery",
			commandID, workerID)
	}
	return filtered
}

func (qh *QueueHandler) workerHasCompletedTaskAfter(
	commandID, workerID string,
	after time.Time,
	taskQueues map[string]*taskQueueEntry,
) bool {
	for queueFile, tqEntry := range taskQueues {
		if strings.TrimSuffix(filepath.Base(queueFile), ".yaml") != workerID {
			continue
		}
		for _, task := range tqEntry.Queue.Tasks {
			if task.CommandID != commandID || task.Status != model.StatusCompleted {
				continue
			}
			ts := task.UpdatedAt
			if ts == "" {
				ts = task.CreatedAt
			}
			completedAt, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				continue
			}
			if completedAt.After(after) {
				return true
			}
		}
	}
	return false
}
