package plan

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// CompleteOptions holds the configuration for completing a command.
type CompleteOptions struct {
	CommandID  string
	Summary    string
	MaestroDir string
	Config     model.Config
	LockMap    *lock.MutexMap
}

// CompleteResult contains the outcome of a command completion operation.
type CompleteResult struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`
}

// staleTaskResultsError indicates that the intent's task results snapshot is
// stale relative to the current on-disk results. Returned from the H3 conflict
// path so the caller can refresh results and retry.
type staleTaskResultsError struct {
	IntentVersion  uint64
	CurrentVersion uint64
}

func (e *staleTaskResultsError) Error() string {
	return fmt.Sprintf("stale task results in H3 conflict path: intent_version=%d, current_version=%d", e.IntentVersion, e.CurrentVersion)
}

// advancePhasesInline walks every active phase and transitions it to a
// terminal status when every task in the phase has reached a terminal
// effective status. Mirrors the daemon-side checkActivePhaseCompletion
// rules so plan_complete + auto-cancel produce the same convergence
// without waiting for the next scan cycle.
//
// Transition priority: failed > cancelled > completed. A phase whose
// tasks include any failed → PhaseStatusFailed. Otherwise if any
// cancelled → PhaseStatusCancelled. Otherwise if every task is
// completed → PhaseStatusCompleted.
//
// Idempotent: phases already at a terminal status are skipped.
func advancePhasesInline(state *model.CommandState) {
	if state == nil {
		return
	}
	now := nowUTC()
	// A phase's Failed/Cancelled transition is driven only by its REQUIRED
	// tasks, mirroring DeriveStatus (hasFailed/hasCancelled are computed from
	// RequiredTaskIDs alone). An optional task that failed/was cancelled is
	// terminal but tolerated under CompletionPolicy.OnOptionalFailed (default
	// "ignore"); forcing the whole phase to Failed on an optional failure used
	// to cascade-cancel/block dependent phases for a plan the policy says
	// should still complete. The rare OnOptionalFailed="fail_command" still
	// fails the command via DeriveStatus, so phase-level neutrality is safe.
	requiredSet := make(map[string]bool, len(state.RequiredTaskIDs))
	for _, id := range state.RequiredTaskIDs {
		requiredSet[id] = true
	}
	for i := range state.Phases {
		phase := &state.Phases[i]
		if model.IsPhaseTerminal(phase.Status) {
			continue
		}
		taskIDs := phase.TaskIDs
		if len(taskIDs) == 0 {
			continue
		}
		allTerminal := true
		hasFailed := false
		hasCancelled := false
		for _, tid := range taskIDs {
			ts := EffectiveStatusForCompletion(tid, state)
			if ts == "" || !model.IsTerminal(ts) {
				allTerminal = false
				break
			}
			// RequiredTaskIDs may be empty for an all-optional phase; then every
			// task is optional and the phase still completes when its work is done.
			isRequired := requiredSet[tid]
			switch ts {
			case model.StatusFailed, model.StatusDeadLetter:
				if isRequired {
					hasFailed = true
				}
			case model.StatusCancelled:
				if isRequired {
					hasCancelled = true
				}
			}
		}
		if !allTerminal {
			continue
		}
		var newStatus model.PhaseStatus
		switch {
		case hasFailed:
			newStatus = model.PhaseStatusFailed
		case hasCancelled:
			newStatus = model.PhaseStatusCancelled
		default:
			newStatus = model.PhaseStatusCompleted
		}
		phase.Status = newStatus
		phase.CompletedAt = &now
	}
}

// autoCancelPausedForReplanIfBlocking marks every required task currently
// at StatusPausedForReplan as StatusCancelled, but only when those tasks
// are the SOLE remaining non-terminal blockers. Returns the number of
// tasks transitioned. Used by Complete() to remove the Planner-must-call-
// request-cancel-first friction reported in Report 2026-05-04 issue-4.
//
// Safety:
//   - Tasks at any other non-terminal status (planned/ready/dispatched/
//     in_progress/etc.) abort the auto-cancel; the Planner is calling
//     complete prematurely and should see the existing "required tasks
//     not terminal" error so it can wait or retry.
//   - We only operate on RequiredTaskIDs. Optional tasks have their own
//     terminality semantics and the plan can complete without them.
//   - The cancellation reason is recorded so audit logs still surface
//     what happened.
func autoCancelPausedForReplanIfBlocking(state *model.CommandState) int {
	if state == nil {
		return 0
	}
	pausedIDs := make([]string, 0)
	for _, taskID := range state.RequiredTaskIDs {
		ts, ok := state.TaskStates[taskID]
		if !ok {
			// Unknown required task — let CanComplete surface the error.
			return 0
		}
		if model.IsTerminal(ts) {
			continue
		}
		if ts != model.StatusPausedForReplan {
			// At least one non-paused, non-terminal task — Planner is
			// calling complete prematurely. Don't auto-cancel anything.
			return 0
		}
		pausedIDs = append(pausedIDs, taskID)
	}
	if len(pausedIDs) == 0 {
		return 0
	}
	if state.CancelledReasons == nil {
		state.CancelledReasons = make(map[string]string)
	}
	for _, taskID := range pausedIDs {
		state.TaskStates[taskID] = model.StatusCancelled
		state.CancelledReasons[taskID] = "auto_cancelled_at_plan_complete:paused_for_replan_unblocked"
	}
	return len(pausedIDs)
}

// computeTaskResultsVersion computes a deterministic fingerprint from
// aggregated task results. The version changes whenever the set of results
// or their statuses change. Returns 0 only when called with nil/empty results
// AND no sentinel; however, the sentinel prefix ensures a non-zero value even
// for empty inputs, so TaskResultsVersion == 0 reliably means "no version info"
// (backward-compatible with intents written before this field existed).
func computeTaskResultsVersion(results []model.CommandResultTask) uint64 {
	h := fnv.New64a()
	// Sentinel prefix so empty results produce a non-zero version,
	// distinguishing "computed from empty" from "field not set" (0).
	// hash.Hash.Write never returns a non-nil error, per the io.Writer contract for hashes.
	_, _ = h.Write([]byte("task_results_v1"))

	sorted := make([]model.CommandResultTask, len(results))
	copy(sorted, results)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].TaskID < sorted[j].TaskID
	})
	for _, r := range sorted {
		_, _ = fmt.Fprintf(h, "%s:%s:%s\n", r.TaskID, r.Worker, r.Status) // hash.Hash64 never errors
	}
	return h.Sum64()
}

// Complete finalises a command by writing the result, updating the queue entry,
// and transitioning the state to a terminal plan_status.
//
// Lock ordering (canonical, consistent with the rest of the codebase):
//
//	queue:planner → state:{commandID} → result:planner
//
// All three locks are acquired here so the ordering is visible in one place.
// The helper functions (writeCommandResultLocked, updateCommandQueueEntryLocked)
// assume the caller already holds the required lock.
//
// Crash recovery (CR-019): an intent file is written before the 3-step
// sequence and removed after all steps succeed. On entry, any stale intent
// for this command is recovered by replaying the steps idempotently.
func Complete(opts CompleteOptions) (*CompleteResult, error) {
	if opts.LockMap == nil {
		return nil, ErrLockMapRequired
	}
	// Validate commandID early (before any path construction) to prevent
	// directory traversal via intent file paths.
	if !validate.IsValidBaseName(opts.CommandID) {
		return nil, fmt.Errorf("invalid command ID: %q", opts.CommandID)
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	// Acquire locks in canonical order: queue → state → result
	opts.LockMap.Lock("queue:planner")
	defer opts.LockMap.Unlock("queue:planner")

	// Schedule explicit removal of the per-command state lock entry after
	// the unlock runs. defer is LIFO, so this Remove fires last and drops
	// the now-idle entry from the MutexMap to prevent unbounded growth as
	// commands accumulate over a long-running daemon.
	defer opts.LockMap.Remove("state:" + opts.CommandID)
	sm.LockCommand(opts.CommandID) // state:{commandID}
	defer sm.UnlockCommand(opts.CommandID)

	// --- Intent recovery: replay stale intent if present (CR-019) ---
	intent, intentErr := readCompleteIntent(opts.MaestroDir, opts.CommandID)
	if intentErr != nil {
		// Corrupt/unreadable intent: log and quarantine by removing the broken
		// file so it doesn't block future calls. The normal flow will re-derive
		// the correct status from state.
		slogc().Warn("Complete: corrupt intent file, removing", "command_id", opts.CommandID, "error", intentErr)
		removeCompleteIntent(opts.MaestroDir, opts.CommandID)
	} else if intent != nil {
		// Validate intent matches the current command (defense against file corruption)
		if intent.CommandID != opts.CommandID {
			slogc().Warn("Complete: intent command_id mismatch, removing corrupt intent", "intent_command_id", intent.CommandID, "opts_command_id", opts.CommandID)
			removeCompleteIntent(opts.MaestroDir, opts.CommandID)
		} else {
			slogc().Info("Complete: recovering stale intent", "command_id", opts.CommandID)
			actualStatus, err := replayCompleteIntent(opts, sm, intent)
			if err != nil {
				return nil, fmt.Errorf("intent recovery: %w", err)
			}
			return &CompleteResult{
				CommandID: opts.CommandID,
				Status:    string(actualStatus),
			}, nil
		}
	}

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	// Idempotency: if already completed/failed/cancelled, return existing status
	if state.PlanStatus == model.PlanStatusCompleted || state.PlanStatus == model.PlanStatusFailed || state.PlanStatus == model.PlanStatusCancelled {
		return &CompleteResult{
			CommandID: opts.CommandID,
			Status:    string(state.PlanStatus),
		}, nil
	}

	// Pre-check: when the only blockers preventing completion are tasks at
	// StatusPausedForReplan, auto-cancel them with a synthetic reason so
	// the Planner does not have to make a separate request-cancel call
	// before plan_complete (Report 2026-05-04 issue-4 observed a Planner
	// spinning ~4 minutes retrying plan_complete after max_tasks
	// enforcement forced it to abandon retry-add). The Planner explicitly
	// calling plan_complete is the load-bearing intent — "I have decided
	// to finalise" — so abandoning the stuck tasks is consistent with
	// that decision. R10 deadletter would have done the same thing
	// eventually, just an hour later. We do NOT auto-cancel
	// failed/dead_letter tasks because those carry meaningful failure
	// outcomes the plan-status derivation should still see.
	if autoCancelled := autoCancelPausedForReplanIfBlocking(state); autoCancelled > 0 {
		state.UpdatedAt = nowUTC()
		// Force-advance any phase whose tasks are now terminal. Without
		// this, the auto-cancel writes the task transitions but the
		// phase-level transition (active → cancelled/failed/completed)
		// is left to the next daemon scan. Reports of 2026-05-04
		// observed cases where the scan never re-evaluated the phase
		// (cache TTL, scan-skip, or worker.Status drift) and the
		// command stalled indefinitely with phase=active. Doing the
		// transition inline here makes plan_complete deterministic:
		// once the Planner says "finalise", the phase is moved to its
		// terminal state in the same write.
		advancePhasesInline(state)
		if err := sm.SaveState(state); err != nil {
			return nil, fmt.Errorf("save state after auto-cancel paused_for_replan: %w", err)
		}
		slogc().Info("Complete: auto-cancelled paused_for_replan tasks to unblock plan completion",
			"command_id", opts.CommandID, "count", autoCancelled)
	}

	// can-complete validation
	derivedPlanStatus, err := CanComplete(state)
	if err != nil {
		// Transient daemon-side state (e.g. phase awaiting the
		// merge_recorded gate after every task in it is terminal).
		// Mirror the worktreeNotPublishedError handling below: write a
		// deferred_complete intent so Phase C's deferredPlanCompleter
		// finalises the plan once publish succeeds. Avoids forcing the
		// Planner into an explicit retry loop while the daemon flips
		// phase.Status from active → completed.
		var retErr *retryableError
		if errors.As(err, &retErr) {
			if writeErr := WriteDeferredComplete(opts.MaestroDir, opts.CommandID, opts.Summary); writeErr != nil {
				return nil, fmt.Errorf("write deferred complete: %w", writeErr)
			}
			slogc().Info("Complete: deferred until daemon resolves transient phase state",
				"command_id", opts.CommandID, "reason", retErr.Error())
			return &CompleteResult{
				CommandID: opts.CommandID,
				Status:    "deferred_publish",
			}, nil
		}
		return nil, err
	}

	// Map PlanStatus to Status for result first so the publish guard below
	// can short-circuit for non-success outcomes.
	var resultStatus model.Status
	switch derivedPlanStatus {
	case model.PlanStatusCompleted:
		resultStatus = model.StatusCompleted
	case model.PlanStatusFailed:
		resultStatus = model.StatusFailed
	case model.PlanStatusCancelled:
		resultStatus = model.StatusCancelled
	default:
		return nil, fmt.Errorf("unexpected derived status: %s", derivedPlanStatus)
	}

	// Worktree publish guard: when worktree mode is enabled, the integration
	// branch must be published before the command can complete *successfully*.
	//
	// Failed/cancelled commands bypass this guard so they do not loop on
	// `deferred_publish` forever — `worktree_publish_skip_failed` would
	// otherwise fire every scan (phases stay non-Completed), publish never
	// happens, and deferred_complete is never finalised. Failed commands
	// should NOT be merged to base, so skipping the publish step lets the
	// queue/state transition to terminal failed cleanly. The guard is
	// retained for successful commands so "command completed" never lies
	// about the base branch state.
	if resultStatus == model.StatusCompleted {
		if err := checkWorktreePublished(opts.MaestroDir, opts.CommandID, opts.Config); err != nil {
			// Terminal publish failure (quarantined): non-retryable, do NOT
			// defer. Promote the command to `failed` so plan_complete is
			// finalized, command_failed notification reaches the
			// orchestrator, and the gating loop in result_handler exits
			// cleanly. Tasks are technically completed but the command
			// itself fails because integration → main never reaches
			// publish (Report 2026-05-06 round-3 P0).
			var termErr *worktreePublishTerminalError
			if errors.As(err, &termErr) {
				slogc().Warn("Complete: worktree publish terminally failed; finalizing command as failed",
					"command_id", opts.CommandID,
					"integration_status", termErr.IntegrationStatus,
					"reason", termErr.Reason,
				)
				resultStatus = model.StatusFailed
				derivedPlanStatus = model.PlanStatusFailed
				// Fall through to the normal aggregation / state finalize
				// path below. Task results are still aggregated so the
				// orchestrator notification carries per-task summaries.
			} else {
				var notPub *worktreeNotPublishedError
				if errors.As(err, &notPub) {
					// Publish hasn't completed yet (still merging/publishing or
					// retryable failed). Write a deferred intent so the daemon
					// can auto-complete after publish, and return a non-error
					// "deferred_publish" result to the caller.
					if writeErr := WriteDeferredComplete(opts.MaestroDir, opts.CommandID, opts.Summary); writeErr != nil {
						return nil, fmt.Errorf("write deferred complete: %w", writeErr)
					}
					slogc().Info("Complete: deferred until worktree publish",
						"command_id", opts.CommandID, "integration_status", notPub.IntegrationStatus)
					return &CompleteResult{
						CommandID: opts.CommandID,
						Status:    "deferred_publish",
					}, nil
				}
				return nil, err
			}
		}
	}

	// Aggregate task results from results/worker{N}.yaml (lock-free: AtomicWrite
	// guarantees consistent snapshots; worst case is a slightly stale read).
	taskResults, partialErrors, err := aggregateTaskResults(opts.MaestroDir, opts.CommandID)
	if err != nil {
		return nil, fmt.Errorf("aggregate results: %w", err)
	}
	if len(partialErrors) > 0 {
		return nil, fmt.Errorf("aggregate results: partial task results for command %s: %w", opts.CommandID, errors.Join(partialErrors...))
	}

	// Warn if aggregated task result count diverges from expected_task_count
	// in state. This detects cases where the Planner's summary text may claim
	// a different number of tasks than actually exist.
	//
	// Verify-repair lineage adjustment: when an injected repair task supersedes
	// its predecessor, both the original and the repair write a result file.
	// Counting both inflates aggregated_results past expected_task_count even
	// though only one *logical* task slot exists. Reduce to the lineage-latest
	// view before comparing — see EffectiveStatusForCompletion / LatestDescendant.
	logicalResultCount := lineageLatestResultCount(taskResults, state.RetryLineage)
	if logicalResultCount != state.ExpectedTaskCount {
		slogc().Warn("Complete: task result count does not match expected_task_count",
			"command_id", opts.CommandID,
			"aggregated_results", len(taskResults),
			"logical_result_count", logicalResultCount,
			"expected_task_count", state.ExpectedTaskCount)
	}

	// --- Write intent before the multi-step sequence (CR-019) ---
	taskResultsVersion := computeTaskResultsVersion(taskResults)
	intent = &completeIntent{
		SchemaVersion:      intentSchemaVersion,
		FileType:           "intent_plan_complete",
		CommandID:          opts.CommandID,
		Summary:            opts.Summary,
		ResultStatus:       resultStatus,
		PlanStatus:         derivedPlanStatus,
		TaskResults:        taskResults,
		TaskResultsVersion: taskResultsVersion,
		CreatedAt:          nowUTC(),
	}
	if err := writeCompleteIntent(opts.MaestroDir, intent); err != nil {
		return nil, fmt.Errorf("write intent: %w", err)
	}

	// --- Execute the 3-step sequence (all steps are idempotent) ---
	if err := executeCompleteSteps(opts, sm, state, intent); err != nil {
		// Intent file is kept so the next call can recover.
		return nil, err
	}

	// --- Remove intent after all steps succeeded ---
	removeCompleteIntent(opts.MaestroDir, opts.CommandID)

	return &CompleteResult{
		CommandID: opts.CommandID,
		Status:    string(derivedPlanStatus),
	}, nil
}

// executeCompleteSteps runs the 3-step completion sequence. Each step is
// idempotent so it is safe to replay on recovery.
//
// H3 conflict handling: if the state has been independently transitioned to a
// different terminal status (e.g., dead-letter set it to failed while a
// previous Complete crashed mid-sequence), the result/queue artifacts written
// by the previous attempt will be inconsistent with state. Rather than
// silently skipping, we reconcile result/queue forward to match the actual
// state.PlanStatus so all three stores agree.
func executeCompleteSteps(opts CompleteOptions, sm *StateManager, state *model.CommandState, intent *completeIntent) error {
	// Conflict path: state already terminal AND differs from intent.
	if model.IsPlanTerminal(state.PlanStatus) && state.PlanStatus != intent.PlanStatus {
		actualStatus, err := planStatusToResultStatus(state.PlanStatus)
		if err != nil {
			return fmt.Errorf("conflict reconcile: %w", err)
		}

		// Verify state consistency before reconciling artifacts: all required
		// tasks must be in a terminal state to prevent writing artifacts for
		// a state that is internally inconsistent.
		for _, taskID := range state.RequiredTaskIDs {
			ts, ok := state.TaskStates[taskID]
			if !ok {
				return fmt.Errorf("conflict reconcile: required task %s has no state entry", taskID)
			}
			if !model.IsTerminal(ts) {
				return fmt.Errorf("conflict reconcile: required task %s is non-terminal (%s) but plan_status is %s", taskID, ts, state.PlanStatus)
			}
		}

		// Stale task results detection: re-aggregate fresh results and compare
		// with the intent's snapshot. If they differ, return a retryable error
		// so the caller can refresh the intent and retry.
		if intent.TaskResultsVersion != 0 {
			freshResults, freshPartialErrs, freshErr := aggregateTaskResults(opts.MaestroDir, intent.CommandID)
			if freshErr != nil {
				return fmt.Errorf("conflict reconcile: re-aggregate task results: %w", freshErr)
			}
			if len(freshPartialErrs) > 0 {
				return fmt.Errorf("conflict reconcile: partial task results: %w", errors.Join(freshPartialErrs...))
			}
			freshVersion := computeTaskResultsVersion(freshResults)
			if freshVersion != intent.TaskResultsVersion {
				slogc().Warn("executeCompleteSteps: stale task results detected in H3 conflict path",
					"command_id", intent.CommandID,
					"intent_version", intent.TaskResultsVersion,
					"current_version", freshVersion)
				return &staleTaskResultsError{
					IntentVersion:  intent.TaskResultsVersion,
					CurrentVersion: freshVersion,
				}
			}
		}

		slogc().Warn("executeCompleteSteps: conflict, reconciling result/queue to state", "state_status", string(state.PlanStatus), "intent_status", string(intent.PlanStatus), "command_id", intent.CommandID)

		opts.LockMap.Lock("result:planner")
		rerr := reconcileCommandResultLocked(opts.MaestroDir, intent.CommandID, actualStatus, intent.Summary, intent.TaskResults)
		opts.LockMap.Unlock("result:planner")
		if rerr != nil {
			return fmt.Errorf("reconcile command result: %w", rerr)
		}
		if err := reconcileCommandQueueEntryLocked(opts.MaestroDir, intent.CommandID, actualStatus); err != nil {
			return fmt.Errorf("reconcile command queue: %w", err)
		}
		return nil
	}

	// Step 1: Write to results/planner.yaml (narrow lock scope for result:planner)
	opts.LockMap.Lock("result:planner")
	err := writeCommandResultLocked(opts.MaestroDir, intent.CommandID, intent.ResultStatus, intent.Summary, intent.TaskResults)
	opts.LockMap.Unlock("result:planner")
	if err != nil {
		return fmt.Errorf("write command result: %w", err)
	}

	// Step 2: Update queue/planner.yaml command entry (caller holds queue:planner)
	if err := updateCommandQueueEntryLocked(opts.MaestroDir, intent.CommandID, intent.ResultStatus); err != nil {
		return fmt.Errorf("update command queue: %w", err)
	}

	// Step 3: Update state plan_status (caller holds state:{commandID})
	// Idempotent: state matches intent already, no-op. Otherwise update.
	if state.PlanStatus != intent.PlanStatus {
		state.PlanStatus = intent.PlanStatus
		state.UpdatedAt = nowUTC()
		if err := sm.SaveState(state); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
	}

	return nil
}

// planStatusToResultStatus maps a terminal PlanStatus to the corresponding
// command-level result Status.
func planStatusToResultStatus(ps model.PlanStatus) (model.Status, error) {
	switch ps {
	case model.PlanStatusCompleted:
		return model.StatusCompleted, nil
	case model.PlanStatusFailed:
		return model.StatusFailed, nil
	case model.PlanStatusCancelled:
		return model.StatusCancelled, nil
	default:
		return "", fmt.Errorf("not a terminal plan status: %s", ps)
	}
}

// reconcileCommandResultLocked overwrites an existing command result entry's
// status (preserving the result ID so downstream notification dedup keys
// remain stable) when an H3 conflict is detected. If no entry exists yet,
// a new one is appended.
// Precondition: caller holds "result:planner" lock.
func reconcileCommandResultLocked(maestroDir string, commandID string, status model.Status, summary string, tasks []model.CommandResultTask) error {
	return readModifyWriteResultFile(maestroDir, func(rf *model.CommandResultFile) error {
		now := nowUTC()
		for i := range rf.Results {
			if rf.Results[i].CommandID == commandID {
				// Preserve ID; mutate status/summary/tasks. Reset Notified so
				// the orchestrator notification path can resend the corrected
				// result. Pending notifications referencing the original ID
				// remain valid because the ID itself is unchanged.
				rf.Results[i].Status = status
				rf.Results[i].Summary = summary
				rf.Results[i].TaskStats = model.ComputeTaskStats(tasks)
				rf.Results[i].Tasks = tasks
				rf.Results[i].Notified = false
				rf.Results[i].CreatedAt = now
				return nil
			}
		}

		// No existing entry: fall back to a fresh append.
		resultID, err := model.GenerateID(model.IDTypeResult)
		if err != nil {
			return fmt.Errorf("generate result ID: %w", err)
		}
		rf.Results = append(rf.Results, model.CommandResult{
			ID:        resultID,
			CommandID: commandID,
			Status:    status,
			Summary:   summary,
			TaskStats: model.ComputeTaskStats(tasks),
			Tasks:     tasks,
			CreatedAt: now,
		})
		return nil
	})
}

// reconcileCommandQueueEntryLocked force-updates a command's status in
// queue/planner.yaml even if the entry is already in a terminal state. This
// is the H3 reconciliation counterpart to updateCommandQueueEntryLocked,
// which is intentionally idempotent (no-op on terminal). If the entry is
// missing (already archived), this is a no-op.
// Precondition: caller holds "queue:planner" lock.
func reconcileCommandQueueEntryLocked(maestroDir string, commandID string, status model.Status) error {
	return readModifyWriteCommandQueue(maestroDir, func(cq *model.CommandQueue) bool {
		now := nowUTC()
		for i := range cq.Commands {
			if cq.Commands[i].ID == commandID {
				if cq.Commands[i].Status == status {
					return false
				}
				cq.Commands[i].Status = status
				cq.Commands[i].LeaseOwner = nil
				cq.Commands[i].LeaseExpiresAt = nil
				cq.Commands[i].UpdatedAt = now
				return true
			}
		}
		slogc().Warn("reconcileCommandQueueEntryLocked: command not found in planner queue", "command_id", commandID)
		return false
	})
}

func aggregateTaskResults(maestroDir string, commandID string) ([]model.CommandResultTask, []error, error) {
	resultsDir := filepath.Join(maestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read results dir: %w", err)
	}

	var taskResults []model.CommandResultTask
	var partialErrors []error

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		workerID := strings.TrimSuffix(name, ".yaml")
		path := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
		if err != nil {
			partialErrors = append(partialErrors, fmt.Errorf("read %s: %w", path, err))
			continue
		}

		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			partialErrors = append(partialErrors, fmt.Errorf("parse %s: %w", path, err))
			continue
		}

		for _, r := range rf.Results {
			if r.CommandID == commandID {
				taskResults = append(taskResults, model.CommandResultTask{
					TaskID:  r.TaskID,
					Worker:  workerID,
					Status:  r.Status,
					Summary: r.Summary,
				})
			}
		}
	}

	return taskResults, partialErrors, nil
}

// lineageLatestResultCount counts task results after collapsing each retry
// lineage chain to its single latest descendant. The aggregator
// (aggregateTaskResults) returns one entry per result file, which inflates
// counts when verify-repair has injected a successor for a previously
// finalised task — both the original (now superseded/cancelled) and the
// repair publish their own result. expected_task_count tracks the count of
// logical task slots, so the comparison must collapse lineage chains too.
//
// Algorithm: walk RetryLineage to build the set of "superseded" task IDs
// (any taskID that appears as a predecessor i.e. a *value* in the map), then
// count results whose TaskID is NOT in that set. Result files for tasks
// outside any lineage chain pass through unchanged.
func lineageLatestResultCount(results []model.CommandResultTask, retryLineage map[string]string) int {
	if len(retryLineage) == 0 {
		return len(results)
	}
	superseded := make(map[string]struct{}, len(retryLineage))
	for _, predecessor := range retryLineage {
		superseded[predecessor] = struct{}{}
	}
	count := 0
	for _, r := range results {
		if _, dropped := superseded[r.TaskID]; dropped {
			continue
		}
		count++
	}
	return count
}

// checkWorktreePublished verifies that the worktree integration branch has been
// published before allowing command completion. Returns nil if worktree mode is
// disabled, the worktree state file does not exist, or the integration status is
// "published".
func checkWorktreePublished(maestroDir, commandID string, config model.Config) error {
	if !config.Worktree.Enabled {
		return nil
	}

	path := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read worktree state: %w", err)
	}

	var wcs model.WorktreeCommandState
	if err := yamlv3.Unmarshal(data, &wcs); err != nil {
		return fmt.Errorf("parse worktree state: %w", err)
	}

	// "created" means the integration branch was initialized but no worker
	// branch was ever merged (e.g., a read-only confirmation command where all
	// workers reported no_changes_to_commit). There is nothing to publish, so
	// the publish guard should pass.
	if wcs.Integration.Status == model.IntegrationStatusCreated {
		return nil
	}

	// Quarantine is a terminal-failed state (R8 reconciler has confirmed
	// the publish is non-retryable). Waiting on deferred_publish would
	// wedge plan_complete forever and the orchestrator would never get a
	// notification (Report 2026-05-06 P0). Returning a dedicated error
	// type lets the caller fail-terminal plan_complete cleanly.
	if wcs.Integration.Status == model.IntegrationStatusQuarantined {
		return &worktreePublishTerminalError{
			IntegrationStatus: string(wcs.Integration.Status),
			Reason:            wcs.Integration.QuarantineReason,
		}
	}

	if wcs.Integration.Status != model.IntegrationStatusPublished {
		return &worktreeNotPublishedError{
			IntegrationStatus: string(wcs.Integration.Status),
		}
	}

	if len(wcs.CommitFailedWorkers) > 0 {
		return &planValidationError{
			Msg: fmt.Sprintf("cannot complete command: %d worker(s) have commit failures: %v", len(wcs.CommitFailedWorkers), wcs.CommitFailedWorkers),
		}
	}

	return nil
}

// writeCommandResultLocked writes a command result to results/planner.yaml.
// Precondition: caller holds "result:planner" lock.
func writeCommandResultLocked(maestroDir string, commandID string, status model.Status, summary string, tasks []model.CommandResultTask) error {
	return readModifyWriteResultFile(maestroDir, func(rf *model.CommandResultFile) error {
		// Idempotency: skip if a result for this commandID already exists
		for _, existing := range rf.Results {
			if existing.CommandID == commandID {
				return nil
			}
		}

		resultID, err := model.GenerateID(model.IDTypeResult)
		if err != nil {
			return fmt.Errorf("generate result ID: %w", err)
		}

		now := nowUTC()
		rf.Results = append(rf.Results, model.CommandResult{
			ID:        resultID,
			CommandID: commandID,
			Status:    status,
			Summary:   summary,
			TaskStats: model.ComputeTaskStats(tasks),
			Tasks:     tasks,
			CreatedAt: now,
		})
		return nil
	})
}

// updateCommandQueueEntryLocked updates a command's status in queue/planner.yaml.
// Precondition: caller holds "queue:planner" lock.
// Idempotent: if the command is already in a terminal status, this is a no-op
// (safe for crash-recovery replay).
func updateCommandQueueEntryLocked(maestroDir string, commandID string, status model.Status) error {
	return readModifyWriteCommandQueue(maestroDir, func(cq *model.CommandQueue) bool {
		now := nowUTC()
		for i := range cq.Commands {
			if cq.Commands[i].ID == commandID {
				// Idempotent: already terminal → no-op (recovery replay safe)
				if model.IsTerminal(cq.Commands[i].Status) {
					return false
				}
				cq.Commands[i].Status = status
				cq.Commands[i].LeaseOwner = nil
				cq.Commands[i].LeaseExpiresAt = nil
				cq.Commands[i].UpdatedAt = now
				return true
			}
		}
		// Command may have been archived after a previous partial completion;
		// treat as already handled (recovery safe).
		slogc().Warn("updateCommandQueueEntryLocked: command not found in planner queue", "command_id", commandID)
		return false
	})
}
