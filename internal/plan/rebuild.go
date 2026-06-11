package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// RebuildOptions holds the configuration for rebuilding command state from worker results.
type RebuildOptions struct {
	CommandID  string
	MaestroDir string
	LockMap    *lock.MutexMap
}

// Rebuild reconstructs the command state by scanning worker result files and applying the latest status for each task.
func Rebuild(opts RebuildOptions) error {
	if opts.LockMap == nil {
		return ErrLockMapRequired
	}
	sm := NewStateManager(opts.MaestroDir, opts.LockMap)

	sm.LockCommand(opts.CommandID)
	defer sm.UnlockCommand(opts.CommandID)

	state, err := sm.LoadState(opts.CommandID)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// Defensive init: AppliedResultIDs may be nil in old/corrupted state files
	if state.AppliedResultIDs == nil {
		state.AppliedResultIDs = make(map[string]string)
	}

	// Prune stale entries: AppliedResultIDs may contain task_ids that no longer
	// exist in TaskStates (e.g. tasks removed from the plan after a previous
	// reconcile). Leaving them in place would let result_write idempotency
	// checks key off ghost task_ids and incorrectly drop legitimate writes.
	for taskID := range state.AppliedResultIDs {
		if _, ok := state.TaskStates[taskID]; !ok {
			slogc().Info("rebuild: pruning stale applied_result_id for unknown task", "task_id", taskID)
			delete(state.AppliedResultIDs, taskID)
		}
	}

	// Scan all results/worker{N}.yaml files for tasks belonging to this command
	resultsDir := filepath.Join(opts.MaestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return fmt.Errorf("read results dir: %w", err)
	}

	// Collect the latest result per task_id across all worker files.
	// Results may span multiple files and file read order (os.ReadDir) does not
	// guarantee chronological ordering, so we compare CreatedAt timestamps.
	type latestResult struct {
		status    model.Status
		resultID  string
		createdAt time.Time
	}
	latestByTask := make(map[string]latestResult)
	skippedFiles := 0

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		path := filepath.Join(resultsDir, name)
		data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application results directory
		if err != nil {
			slogc().Warn("rebuild: skipping unreadable result file", "file", name, "error", err)
			skippedFiles++
			continue
		}

		var rf model.TaskResultFile
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			slogc().Warn("rebuild: skipping corrupt result file", "file", name, "error", err)
			skippedFiles++
			continue
		}

		for _, r := range rf.Results {
			if r.CommandID != opts.CommandID {
				continue
			}

			if _, ok := state.TaskStates[r.TaskID]; !ok {
				continue // unknown task
			}

			createdAt, err := time.Parse(time.RFC3339, r.CreatedAt)
			if err != nil {
				continue // skip results with unparseable timestamps
			}

			if existing, ok := latestByTask[r.TaskID]; ok {
				if createdAt.Before(existing.createdAt) {
					continue // older result, skip
				}
				if createdAt.Equal(existing.createdAt) && r.ID < existing.resultID {
					continue // same timestamp, use deterministic tie-break by ID
				}
			}

			latestByTask[r.TaskID] = latestResult{
				status:    r.Status,
				resultID:  r.ID,
				createdAt: createdAt,
			}
		}
	}

	if skippedFiles > 0 {
		slogc().Warn("rebuild: result files skipped due to read/parse errors", "skipped_count", skippedFiles)
	}

	// Apply the latest result for each task with transition validation.
	// Rebuild allows non-terminal → terminal (crash recovery: pending/in_progress → completed/failed)
	// but rejects terminal → any (prevents overwriting already-settled state).
	for taskID, lr := range latestByTask {
		currentStatus := state.TaskStates[taskID]
		if model.IsTerminal(currentStatus) {
			slogc().Info("rebuild: skipping task with terminal status", "task_id", taskID, "current_status", string(currentStatus), "target_status", string(lr.status))
			continue
		}
		state.TaskStates[taskID] = lr.status
		state.AppliedResultIDs[taskID] = lr.resultID
	}

	// Phantom-task pass: a retry/repair task can land in TaskStates as
	// `planned` while the corresponding queue write silently failed (or
	// was rolled back without the RetryEnqueueFailed bookkeeping). Without
	// intervention the entry stays `planned` indefinitely, blocking phase
	// termination and leaving `plan rebuild` reporting "no-op success"
	// while the command is actually wedged. Detect such entries by
	// cross-referencing every worker queue file and flip them to failed
	// so the next scan can advance the phase.
	//
	// Conservative scope: we only act on `planned` status because that is
	// the deterministic post-RegisterRetryTaskInState state. Any other
	// non-terminal status (`pending`, `in_progress`, `verify_pending`,
	// `repair_pending`, `paused_for_replan`) is owned by another
	// reconciler / dispatch path; force-failing those would race those
	// owners and false-fail healthy work.
	phantomFailures, queueScanErr := detectPhantomPlannedTasks(opts.MaestroDir, opts.CommandID, state.TaskStates)
	if queueScanErr != nil {
		// Queue read failures are surfaced as a warning rather than a
		// hard error: the rebuild result-application work above is
		// already valuable, and the operator can rerun rebuild after
		// fixing the queue read issue.
		slogc().Warn("rebuild: phantom-task queue scan failed", "error", queueScanErr)
	}
	for _, taskID := range phantomFailures {
		slogc().Warn("rebuild: phantom task in state has no queue entry, marking failed",
			"command_id", opts.CommandID, "task_id", taskID)
		state.TaskStates[taskID] = model.StatusFailed
		if state.CancelledReasons == nil {
			state.CancelledReasons = make(map[string]string)
		}
		state.CancelledReasons[taskID] = "phantom_task_no_queue_entry: detected by plan rebuild"
	}

	now := nowUTC()
	state.LastReconciledAt = &now
	state.UpdatedAt = now

	return sm.SaveState(state)
}

// detectPhantomPlannedTasks scans every worker queue file under
// <maestroDir>/queue and returns the task IDs that are at status
// `planned` in taskStates but absent from every queue. Returns the
// (sorted, for deterministic test ordering) phantom IDs and any error
// encountered while reading the queue directory.
func detectPhantomPlannedTasks(maestroDir, commandID string, taskStates map[string]model.Status) ([]string, error) {
	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no queue dir means no queues; treat as scan-clean
		}
		return nil, fmt.Errorf("read queue dir: %w", err)
	}

	queueIDs := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		path := filepath.Join(queueDir, name)
		data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application queue directory
		if err != nil {
			// Fail-safe: a queue we cannot read may contain exactly the
			// planned tasks we are checking for. Continuing the scan would
			// classify them as phantoms and force-fail healthy tasks over a
			// transient I/O error / single corrupt file. Mirror the
			// directory-read failure: abort the whole phantom pass.
			return nil, fmt.Errorf("read queue %s (aborting phantom scan; a hidden queue could hold the scanned tasks): %w", name, err)
		}
		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			return nil, fmt.Errorf("parse queue %s (aborting phantom scan; a hidden queue could hold the scanned tasks): %w", name, err)
		}
		for _, t := range tq.Tasks {
			if t.CommandID != commandID {
				continue
			}
			queueIDs[t.ID] = true
		}
	}

	var phantoms []string
	for taskID, status := range taskStates {
		if status != model.StatusPlanned {
			continue
		}
		if queueIDs[taskID] {
			continue
		}
		phantoms = append(phantoms, taskID)
	}
	sort.Strings(phantoms)
	return phantoms, nil
}
