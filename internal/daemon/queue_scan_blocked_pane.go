package daemon

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/paneactivity"
	"github.com/msageha/maestro_v2/internal/model"
)

// stepBlockedPaneTimeout fails in-progress tasks whose worker pane has
// been wedged on a confirmation prompt longer than blockedPaneFailAfter()
// OR whose pane is showing a runtime terminal-error frame (Claude API
// 4xx, content filter, invalid_request_error, …). Runs every scan tick
// (default 60s) regardless of lease expiry — the previous implementation
// only checked at lease expiry (default 5 min) so the 3-min threshold
// was effectively rounded up to 5 min in production (Report 2026-05-05
// P0-B), and terminal-error fast-fail was only wired to the lease-expiry
// path so it never fired for tasks whose lease was still valid (Report
// 2026-05-06 P0).
//
// Side-effects:
//   - calls ObserveVerdict for each in-progress task's worker pane,
//     which refreshes Tracker.blockedSince. This MUST run before
//     stepDispatchOrRecovery's collectExpiredTaskBusyChecks so
//     downstream callers see a fresh same-scan snapshot.
//   - for tasks that exceed the threshold or hit a terminal error,
//     sets task.Status = Failed, writes a synthetic failed result,
//     and forgets the agent's pane snapshot so the next dispatch
//     starts from a clean baseline.
func (qh *QueueHandler) stepBlockedPaneTimeout(s *scanState) {
	if qh.paneActivity == nil {
		return
	}
	threshold := blockedPaneFailAfter()
	now := qh.clock.Now()
	for queueFile, tq := range s.tasks {
		agentID := workerIDFromPath(queueFile)
		if agentID == "" {
			continue
		}
		hasInProgress := false
		for i := range tq.Queue.Tasks {
			if tq.Queue.Tasks[i].Status == model.StatusInProgress {
				hasInProgress = true
				break
			}
		}
		if !hasInProgress {
			continue
		}
		// Refresh pane verdict so blockedSince is current. This is the
		// canonical scan-tick observation: the verdict is cached in
		// scanState so the downstream lease-expiry path reuses it instead
		// of re-observing. A second ObserveVerdict in the same tick lands
		// within minPrevAge of this one and degrades to a same-scan
		// VerdictUncertain, which the grace-extension path then extends on
		// every expiry up to the 30-minute hard cap (E2E 2026-06-11).
		verdict := qh.observePaneVerdictForAgent(agentID)
		if s.paneVerdicts != nil {
			s.paneVerdicts[agentID] = verdict
		}

		// Terminal-error fast-fail: the runtime emitted a non-recoverable
		// error frame. The task cannot recover without operator action,
		// so failing immediately (no timeout) is strictly better than
		// extending the lease until max_in_progress_min.
		if verdict == paneactivity.VerdictTerminalError {
			for i := range tq.Queue.Tasks {
				task := &tq.Queue.Tasks[i]
				if task.Status != model.StatusInProgress {
					continue
				}
				qh.log(LogLevelWarn,
					"task_failed_terminal_error id=%s worker=%s epoch=%d "+
						"(scan-tick path; runtime emitted terminal error frame, failing immediately)",
					task.ID, agentID, task.LeaseEpoch)
				if qh.failTaskTerminalError(task, queueFile, agentID) {
					s.taskDirty[queueFile] = true
					if recovErr := qh.recoverWorkerPaneAfterBlocked(agentID); recovErr != nil {
						qh.log(LogLevelWarn,
							"terminal_error_recovery_failed worker=%s task=%s error=%v "+
								"(pane may still show the error; next dispatch will still re-launch the agent)",
							agentID, task.ID, recovErr)
					}
				}
			}
			continue
		}

		if threshold <= 0 {
			continue
		}
		if verdict != paneactivity.VerdictBlocked {
			continue
		}
		since, ok := qh.paneActivity.BlockedSince(agentID)
		if !ok {
			continue
		}
		blockedFor := now.Sub(since)
		if blockedFor < threshold {
			qh.log(LogLevelDebug,
				"blocked_pane_below_threshold worker=%s blocked_for=%s threshold=%s",
				agentID, blockedFor.Round(time.Second), threshold)
			continue
		}
		for i := range tq.Queue.Tasks {
			task := &tq.Queue.Tasks[i]
			if task.Status != model.StatusInProgress {
				continue
			}
			qh.log(LogLevelWarn,
				"task_failed_blocked_pane_timeout id=%s worker=%s epoch=%d blocked_for=%s threshold=%s "+
					"(scan-tick path; not waiting for lease expiry)",
				task.ID, agentID, task.LeaseEpoch, blockedFor.Round(time.Second), threshold)
			if qh.failTaskBlockedPane(task, queueFile, agentID, blockedFor) {
				s.taskDirty[queueFile] = true
				if recovErr := qh.recoverWorkerPaneAfterBlocked(agentID); recovErr != nil {
					qh.log(LogLevelWarn,
						"blocked_pane_recovery_failed worker=%s task=%s error=%v "+
							"(pane may still show the prompt; next dispatch may collide)",
						agentID, task.ID, recovErr)
				}
			}
		}
	}
}

// defaultBlockedPaneFailAfter is the threshold for the consecutive-blocked
// run after which the queue scanner force-fails an in-flight task. Tighter
// than circuit_breaker.progress_timeout_minutes (30 min default) on
// purpose: a runtime confirmation prompt that a Worker pane is wedged on
// (e.g. claude-code refusing `--dangerously-skip-permissions` for files
// under `.claude/`) cannot be recovered without operator intervention, so
// holding the iteration open for half an hour just stalls the whole
// command. Three minutes gives the operator's auto-approval / allowlist
// settings (configured on the ~/.claude side) a fair window to clear the
// prompt before the daemon decides to route around the worker.
const defaultBlockedPaneFailAfter = 3 * time.Minute

// blockedPaneFailAfter returns the blocked-pane fail timeout, honouring
// the MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC environment variable (positive
// integer seconds). Returns 0 to disable the early-fail behaviour entirely
// — the original lease-extend-until-progress_timeout path is preserved as
// a fallback when callers explicitly set the value to 0.
//
// The threshold is intentionally NOT a config.yaml field: the blocked
// path should be rare in a properly configured operator setup (the right
// fix is auto-approval on the runtime side), and keeping the surface area
// of `config.yaml` lean is a project goal.
func blockedPaneFailAfter() time.Duration {
	raw := os.Getenv("MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC")
	if raw == "" {
		return defaultBlockedPaneFailAfter
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 0 {
		return defaultBlockedPaneFailAfter
	}
	return time.Duration(secs) * time.Second
}

// failTaskBlockedPane transitions an in-progress task to Failed because
// its worker pane has been wedged on a confirmation/approval prompt for
// longer than blockedPaneFailAfter(). Returns true when the transition
// succeeded (the caller should mark the queue dirty and continue), false
// when the state machine drifted and the task should fall through to the
// usual lease-handling path.
//
// Mirrors the destructive-content rejection path in
// applyTaskDispatchResult: directly update the task entry, write a
// synthetic failed result so reconcilers see the terminal status, and
// forget the agent's pane snapshot so the next observation establishes a
// fresh baseline once the worker (eventually) exits the prompt.
func (qh *QueueHandler) failTaskBlockedPane(task *model.Task, queueFile, agentID string, blockedFor time.Duration) bool {
	if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusFailed); err != nil {
		qh.log(LogLevelError,
			"task_failed_blocked_pane_invalid_transition task=%s from=%s to=failed reason=%v",
			task.ID, task.Status, err)
		return false
	}
	task.Status = model.StatusFailed
	task.LeaseOwner = nil
	task.LeaseExpiresAt = nil
	task.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
	qh.scanExecutor.scanCounters.LeaseReleases++
	workerID := workerIDFromPath(queueFile)
	qh.writeSyntheticBlockedPaneResult(workerID, task.ID, task.CommandID, agentID, blockedFor)
	if qh.paneActivity != nil {
		qh.paneActivity.ForgetAgent(agentID)
	}
	return true
}

// recoverWorkerPaneAfterBlocked respawns the worker pane to the project
// root, killing the wedged agent process and clearing any pending
// confirmation prompt left in the TUI. The pane stays alive (tmux pane
// persists) but the agent process is terminated; the next dispatch will
// re-launch claude/codex/gemini via the standard ensureWorkingDir +
// process-manager path.
//
// Without this recovery the pane is left displaying the prompt: an
// operator who eventually approves it would re-run a now-stale Bash
// invocation against a different lease epoch (FENCING_REJECT race), or
// the next dispatched task would type its prompt straight into the
// pending choice menu (input collision). Killing the agent eliminates
// both.
//
// Errors are returned for diagnostic logging but the caller MUST NOT
// treat them as fatal — the task is already failed at this point and
// orchestration must proceed regardless.
func (qh *QueueHandler) recoverWorkerPaneAfterBlocked(workerID string) error {
	if qh.execProvider == nil {
		return nil
	}
	exec, err := qh.execProvider.GetExecutor()
	if err != nil {
		// Daemon shutting down or test harness without an executor —
		// nothing to do. Returning nil keeps callers' control flow simple.
		qh.log(LogLevelDebug,
			"blocked_pane_recovery_executor_unavailable worker=%s error=%v",
			workerID, err)
		return nil
	}
	// Unconditional eviction: the pane is known to be stuck on a blocked
	// prompt, so there is no healthy process to preserve.
	if err := exec.RespawnPaneToProjectRoot(workerID, ""); err != nil {
		return fmt.Errorf("respawn worker pane %s: %w", workerID, err)
	}
	qh.log(LogLevelInfo,
		"blocked_pane_recovery_respawned worker=%s "+
			"(agent process killed; next dispatch re-launches claude)",
		workerID)
	return nil
}

// terminalErrorSummaryFmt is the operator-/Planner-facing remediation
// guidance written into the synthetic result Summary when a worker pane
// emits a runtime-level terminal error frame. The %s placeholder is
// the worker (agent) ID. Kept as a package constant so changes are made
// in one place rather than hunting through fmt.Sprintf call sites.
const terminalErrorSummaryFmt = "runtime_terminal_error: worker %s pane emitted a non-recoverable error frame from the agent runtime " +
	"(e.g. `API Error: 4xx`, `Output blocked by content filtering policy`, `invalid_request_error`, `authentication_error`). " +
	"The runtime cannot self-recover, so the daemon failed the task immediately and respawned the pane to clear the stale TUI. " +
	"Most likely causes and remediations: (a) the prompt content tripped a content filter — replan the task with the same goal " +
	"phrased differently or split the work into smaller steps so the model has a clearer focus; (b) revoked / rate-limited / " +
	"mis-scoped API key — rotate the credential on the operator side (this is a ~/.claude / runtime configuration concern, not " +
	"a Maestro fix); (c) malformed request structure (very rare in normal operation). DO NOT confuse this with a blocked-pane / " +
	"protected-path failure — those have a different summary key."

// blockedPaneTimeoutSummaryFmt is the operator-/Planner-facing remediation
// guidance written into the synthetic result Summary when the daemon
// force-fails a task because its worker pane has been wedged on a
// confirmation/approval prompt past blockedPaneFailAfter(). The
// placeholders are the worker (agent) ID and the elapsed blocked-for
// duration.
const blockedPaneTimeoutSummaryFmt = "blocked_pane_timeout: worker %s pane has been wedged on a runtime confirmation/approval " +
	"prompt for %s, longer than the daemon's blocked-pane timeout. Auto-failed to keep the iteration moving. Most common causes: " +
	"(a) a task touched a runtime-protected path (e.g. .claude/, ~/.claude/, .codex/, .gemini/, .vscode/, .idea/) which the " +
	"underlying CLI insists on confirming before each edit, even with --dangerously-skip-permissions (managed settings can " +
	"disable bypassPermissions entirely, downgrading the worker to default mode); (b) a command failed inside the OS sandbox " +
	"(EPERM on a protected glob such as **/.vscode/**) and the runtime asked for approval to retry it unsandboxed. Fix: drop " +
	"the protected path from the task's expected_paths and edit content, OR add auto-approval / allowlists in the operator's " +
	"~/.claude side. Package-manager installs are already auto-escaped from the sandbox by the worker policy hook."

// failTaskTerminalError transitions an in-progress task to Failed because
// the worker pane displayed a runtime terminal-error frame (Claude API 4xx,
// content filter, invalid_request_error, …). Mirrors failTaskBlockedPane
// but writes a terminal-error-specific synthetic summary so the Planner /
// Orchestrator does not get the wrong remediation hint (Report 2026-05-06
// P-1: terminal-error tasks were getting the blocked-pane summary which
// suggested "drop protected path from expected_paths" — completely
// unrelated to a content-filter rejection).
func (qh *QueueHandler) failTaskTerminalError(task *model.Task, queueFile, agentID string) bool {
	if err := model.ValidateCommandTaskQueueTransition(task.Status, model.StatusFailed); err != nil {
		qh.log(LogLevelError,
			"task_failed_terminal_error_invalid_transition task=%s from=%s to=failed reason=%v",
			task.ID, task.Status, err)
		return false
	}
	task.Status = model.StatusFailed
	task.LeaseOwner = nil
	task.LeaseExpiresAt = nil
	task.UpdatedAt = qh.clock.Now().UTC().Format(time.RFC3339)
	qh.scanExecutor.scanCounters.LeaseReleases++
	workerID := workerIDFromPath(queueFile)
	qh.writeSyntheticTerminalErrorResult(workerID, task.ID, task.CommandID, agentID)
	if qh.paneActivity != nil {
		qh.paneActivity.ForgetAgent(agentID)
	}
	return true
}

// writeSyntheticTerminalErrorResult appends a synthetic failed result for
// a task whose worker pane emitted a runtime terminal-error frame. The
// summary names the actual cause space (Claude API / content filter /
// authentication) and points at the right remediation (rephrase prompt,
// rotate API key, etc.) so the Planner picks an appropriate replan path.
func (qh *QueueHandler) writeSyntheticTerminalErrorResult(workerID, taskID, commandID, agentID string) {
	if workerID == "" {
		qh.log(LogLevelError,
			"synthetic_terminal_error_result_skipped task=%s command=%s reason=missing_worker_id",
			taskID, commandID)
		return
	}

	lockKey := "result:" + workerID
	qh.lockMap.Lock(lockKey)
	defer qh.lockMap.Unlock(lockKey)

	resultPath := resultFilePath(qh.maestroDir, workerID)
	if err := updateYAMLFile(resultPath, func(rf *model.TaskResultFile) error {
		if rf.SchemaVersion == 0 {
			rf.SchemaVersion = 1
			rf.FileType = "result_task"
		}
		resultID, err := model.GenerateID(model.IDTypeResult)
		if err != nil {
			return fmt.Errorf("generate synthetic terminal-error result id: %w", err)
		}
		rf.Results = append(rf.Results, model.TaskResult{
			ID:                     resultID,
			TaskID:                 taskID,
			CommandID:              commandID,
			Status:                 model.StatusFailed,
			Summary:                fmt.Sprintf(terminalErrorSummaryFmt, agentID),
			PartialChangesPossible: false,
			RetrySafe:              true,
			CreatedAt:              qh.clock.Now().UTC().Format(time.RFC3339),
		})
		return nil
	}); err != nil {
		qh.log(LogLevelError,
			"synthetic_terminal_error_result_write task=%s command=%s worker=%s error=%v",
			taskID, commandID, workerID, err)
	}
}

// writeSyntheticBlockedPaneResult appends a synthetic failed result entry
// to the worker's result file when the daemon force-failed a task because
// its pane was wedged on a runtime confirmation prompt. Mirrors
// writeSyntheticPreflightResult: same lock ordering, same
// "result:<workerID>" key, same downstream pipeline so R1/R2 reconcilers
// propagate the terminal status without depending on the worker ever
// writing its own result (it cannot — its pane is still wedged).
func (qh *QueueHandler) writeSyntheticBlockedPaneResult(workerID, taskID, commandID, agentID string, blockedFor time.Duration) {
	if workerID == "" {
		qh.log(LogLevelError,
			"synthetic_blocked_pane_result_skipped task=%s command=%s reason=missing_worker_id",
			taskID, commandID)
		return
	}

	lockKey := "result:" + workerID
	qh.lockMap.Lock(lockKey)
	defer qh.lockMap.Unlock(lockKey)

	resultPath := resultFilePath(qh.maestroDir, workerID)
	if err := updateYAMLFile(resultPath, func(rf *model.TaskResultFile) error {
		if rf.SchemaVersion == 0 {
			rf.SchemaVersion = 1
			rf.FileType = "result_task"
		}
		resultID, err := model.GenerateID(model.IDTypeResult)
		if err != nil {
			return fmt.Errorf("generate synthetic blocked-pane result id: %w", err)
		}
		rf.Results = append(rf.Results, model.TaskResult{
			ID:                     resultID,
			TaskID:                 taskID,
			CommandID:              commandID,
			Status:                 model.StatusFailed,
			Summary:                fmt.Sprintf(blockedPaneTimeoutSummaryFmt, agentID, blockedFor.Round(time.Second)),
			PartialChangesPossible: true,
			RetrySafe:              false,
			CreatedAt:              qh.clock.Now().UTC().Format(time.RFC3339),
		})
		return nil
	}); err != nil {
		qh.log(LogLevelError,
			"synthetic_blocked_pane_result_write task=%s command=%s worker=%s error=%v "+
				"(queue terminal but state will lag until reconciler retries)",
			taskID, commandID, workerID, err)
	}
}
