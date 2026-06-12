package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/reviewer"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// ReviewCoordinator owns the review dispatch pipeline: dispatching reviews for
// completed tasks, monitoring results, and tracking usefulness. Extracting this
// from Daemon groups the four review-related fields and two methods behind a
// single composition boundary.
type ReviewCoordinator struct {
	dispatcher *reviewer.ReviewDispatcher
	tracker    *reviewer.UsefulnessTracker
	requests   map[string]reviewTaskInfo
	mu         sync.Mutex
	maestroDir string
	log        logFunc
	// worktreeManager produces the unified diff sent to the reviewer. nil
	// means worktree mode is disabled or no manager has been wired (legacy
	// tests); in that case the dispatcher falls back to a synthetic
	// summary+files payload, which is degraded but lets the pipeline keep
	// flowing for non-worktree configurations.
	worktreeManager *WorktreeManager
	// diffCache stores precomputed diffs keyed by taskID so the review
	// dispatch path can use a stable snapshot even when the worker worktree
	// has been cleaned up by the time DispatchIfEligible runs.
	// Result-write callers populate it via PrecaptureDiff before
	// relinquishing the worker queue lock; consumers (buildDiffContent)
	// look it up first and fall back to ComputeWorkerDiff only when the
	// cache has nothing for the task. Entries are evicted in
	// DispatchIfEligible to bound memory.
	diffCacheMu sync.Mutex
	diffCache   map[string]string
}

// newReviewCoordinator creates a ReviewCoordinator when review is enabled.
// Returns nil if cfg.Enabled is false.
func newReviewCoordinator(cfg model.ReviewConfig, maestroDir string, log logFunc) *ReviewCoordinator {
	if !cfg.Enabled {
		return nil
	}

	rc := &ReviewCoordinator{
		dispatcher: reviewer.NewReviewDispatcher(cfg),
		requests:   make(map[string]reviewTaskInfo),
		maestroDir: maestroDir,
		log:        log,
		diffCache:  make(map[string]string),
	}

	stateDir := filepath.Join(maestroDir, "state")
	tracker, err := reviewer.NewUsefulnessTracker(stateDir)
	if err != nil {
		log(LogLevelWarn, "usefulness_tracker_init_failed error=%v (reviews will run without tracking)", err)
	} else {
		rc.tracker = tracker
	}

	log(LogLevelInfo, "review_dispatcher enabled models=%v min_bloom=%d max_concurrent=%d",
		cfg.Models,
		cfg.EffectiveMinBloomLevel(),
		cfg.EffectiveMaxConcurrentReviews())

	return rc
}

// Enabled reports whether the coordinator is initialized and reviews are active.
func (rc *ReviewCoordinator) Enabled() bool {
	return rc != nil && rc.dispatcher != nil
}

// SetWorktreeManager wires the WorktreeManager used to compute the unified
// diff for review dispatch. Production startup injects the same Manager that
// owns worktree state files; tests that do not exercise worktree-backed
// review may leave this nil, which forces the legacy summary+files fallback
// payload.
func (rc *ReviewCoordinator) SetWorktreeManager(wm *WorktreeManager) {
	if rc == nil {
		return
	}
	rc.worktreeManager = wm
}

// MonitorResults drains the dispatcher's results channel, persists each
// result to the per-task audit-trail YAML, and records it in the usefulness
// tracker. Runs until the channel is closed (by Close during shutdown).
//
// Reviews are advisory by design: IsAdvisory=true is hard-coded in the
// dispatcher, so findings never gate task completion. Without persistence,
// the full Finding bodies (severity, file path, message, suggested fix)
// would only live in memory and operators would need to scrape daemon.log
// to recover what the reviewer flagged. Each result is mirrored to
// .maestro/state/reviews/<task_id>.yaml before any tracker bookkeeping.
// The persistence path is observability; failures are logged at warn and
// never propagate to the dispatch loop.
func (rc *ReviewCoordinator) MonitorResults() {
	for result := range rc.dispatcher.Results() {
		taskID := extractTaskIDFromRequestID(result.RequestID)
		info, ok := rc.popRequest(taskID)
		var commandID string
		if ok {
			commandID = info.commandID
		}

		auditPath := rc.persistReviewResult(taskID, commandID, result)

		rc.log(LogLevelInfo, "review_result_received request=%s model=%s status=%s findings=%d audit_file=%s",
			result.RequestID, result.ReviewerModel, result.Status, len(result.Findings), auditPath)

		if rc.tracker == nil {
			continue
		}

		if !ok {
			rc.log(LogLevelWarn, "review_result_orphaned request=%s task=%s (no matching dispatch record; audit_file=%s)",
				result.RequestID, taskID, auditPath)
			continue
		}

		trackerResult := reviewer.ReviewResult{
			ReviewerModel: result.ReviewerModel,
			TaskID:        info.taskID,
			CommandID:     info.commandID,
		}
		for _, f := range result.Findings {
			trackerResult.FindingIDs = append(trackerResult.FindingIDs, f.FilePath+":"+f.Message)
		}

		if err := rc.tracker.RecordResult(trackerResult, nil); err != nil {
			rc.log(LogLevelWarn, "usefulness_record_failed request=%s error=%v", result.RequestID, err)
		}
	}
}

// TaskReviewLog is the on-disk audit trail for advisory review results
// associated with a single task. The file lives at
// .maestro/state/reviews/<task_id>.yaml and aggregates one entry per
// reviewer model — re-running the same reviewer overwrites its prior
// entry rather than appending duplicates, so operators see "the latest
// verdict from each reviewer" without having to dedupe manually.
type TaskReviewLog struct {
	SchemaVersion int                  `yaml:"schema_version"`
	FileType      string               `yaml:"file_type"`
	TaskID        string               `yaml:"task_id"`
	CommandID     string               `yaml:"command_id,omitempty"`
	Results       []model.ReviewResult `yaml:"results"`
}

// taskReviewLogPath returns the YAML file used to persist advisory review
// results for a single task. The reviews/ subdirectory under maestro_dir
// is created on demand by persistReviewResult.
func (rc *ReviewCoordinator) taskReviewLogPath(taskID string) string {
	return filepath.Join(rc.maestroDir, "state", "reviews", taskID+".yaml")
}

// persistReviewResult mirrors a single review result into the task's
// audit-trail YAML file. Existing entries for the same reviewer model are
// replaced in place; new reviewer models are appended. The function is
// best-effort: every error is logged at warn and the path is still
// returned (callers use it as a breadcrumb in their own logs).
func (rc *ReviewCoordinator) persistReviewResult(taskID, commandID string, result model.ReviewResult) string {
	path := rc.taskReviewLogPath(taskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		rc.log(LogLevelWarn, "review_persist_mkdir_failed task=%s error=%v", taskID, err)
		return path
	}

	var rec TaskReviewLog
	switch data, err := os.ReadFile(path); { //nolint:gosec // path is constructed from validated taskID under maestroDir
	case err == nil:
		if uerr := yamlv3.Unmarshal(data, &rec); uerr != nil {
			// Treat a malformed pre-existing file as "start fresh" rather
			// than refusing to record the new result. The audit trail is
			// observability; refusing the write because of historical
			// corruption would just hide today's data too.
			rc.log(LogLevelWarn, "review_persist_unmarshal_failed task=%s error=%v (overwriting)", taskID, uerr)
			rec = TaskReviewLog{}
		}
	case os.IsNotExist(err):
		// First result for this task — start fresh.
	default:
		rc.log(LogLevelWarn, "review_persist_read_failed task=%s error=%v", taskID, err)
	}

	if rec.SchemaVersion == 0 {
		rec.SchemaVersion = 1
	}
	if rec.FileType == "" {
		rec.FileType = "task_review_log"
	}
	rec.TaskID = taskID
	if commandID != "" {
		rec.CommandID = commandID
	}

	replaced := false
	for i := range rec.Results {
		if rec.Results[i].ReviewerModel == result.ReviewerModel {
			rec.Results[i] = result
			replaced = true
			break
		}
	}
	if !replaced {
		rec.Results = append(rec.Results, result)
	}

	if err := yamlutil.AtomicWrite(path, &rec); err != nil {
		rc.log(LogLevelWarn, "review_persist_write_failed task=%s error=%v", taskID, err)
	}
	return path
}

// DispatchIfEligible checks whether the completed task qualifies for an
// advisory review and dispatches it asynchronously. Failures are logged but
// never block the caller.
func (rc *ReviewCoordinator) DispatchIfEligible(ctx context.Context, params ResultWriteParams) {
	if rc.dispatcher == nil {
		return
	}
	queuePath := taskQueuePath(rc.maestroDir, params.Reporter)
	data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		rc.log(LogLevelDebug, "review_dispatch_skip task=%s reason=queue_read_error: %v", params.TaskID, err)
		return
	}

	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		rc.log(LogLevelDebug, "review_dispatch_skip task=%s reason=queue_parse_error: %v", params.TaskID, err)
		return
	}

	var task *model.Task
	for i := range tq.Tasks {
		if tq.Tasks[i].ID == params.TaskID {
			task = &tq.Tasks[i]
			break
		}
	}
	if task == nil {
		return
	}

	// A/B candidates skip the advisory review: pre-selection review would
	// be duplicated work against the wrong diff source (the review pipeline
	// reads worker-branch diffs while candidate work lives on candidate
	// branches). Post-intake review of the winner is a later-PR concern.
	if task.ABGroupID != "" {
		rc.log(LogLevelDebug, "review_dispatch_skip task=%s reason=ab_candidate group=%s",
			params.TaskID, task.ABGroupID)
		return
	}
	if !rc.dispatcher.ShouldReview(*task) {
		return
	}

	diffContent := rc.buildDiffContent(params)

	rc.registerRequest(params.TaskID, reviewTaskInfo{
		taskID:    params.TaskID,
		commandID: params.CommandID,
	})

	if err := rc.dispatcher.Dispatch(ctx, *task, diffContent); err != nil {
		rc.log(LogLevelWarn, "review_dispatch_failed task=%s error=%v", params.TaskID, err)
		rc.unregisterRequest(params.TaskID)
		return
	}

	rc.log(LogLevelInfo, "review_dispatched task=%s command=%s bloom_level=%d",
		params.TaskID, params.CommandID, task.BloomLevel)
}

// PrecaptureDiff records a snapshot of the worker diff for taskID. Callers
// invoke this from the result-write path while the worker worktree is
// guaranteed to still exist (i.e., before relinquishing the per-worker queue
// lock or before any cleanup-eligible status transition). DispatchIfEligible
// later prefers the cached value over a fresh ComputeWorkerDiff call so the
// review payload is stable across the cleanup race window.
//
// An empty diff (worktree gone, no commits since the merge base, etc.) is
// intentionally NOT cached: caching "" would suppress the buildDiffContent
// fallback chain (worktreeManager.ComputeWorkerDiff retry → summary +
// files_changed payload), and the dispatcher would record the resulting
// empty payload as status=skipped.
//
// Errors during capture are logged and result in no cache entry, so the
// dispatch path keeps its existing fallback behaviour.
func (rc *ReviewCoordinator) PrecaptureDiff(taskID, commandID, reporter string) {
	if rc == nil || rc.worktreeManager == nil {
		return
	}
	diff, err := rc.worktreeManager.ComputeWorkerDiff(commandID, reporter)
	if err != nil {
		rc.log(LogLevelDebug,
			"review_diff_precapture_failed task=%s command=%s reporter=%s error=%v "+
				"(dispatch path will fall back to summary payload)",
			taskID, commandID, reporter, err)
		return
	}
	if diff == "" {
		rc.log(LogLevelDebug,
			"review_diff_precapture_empty task=%s command=%s reporter=%s "+
				"(not cached; dispatch path will retry ComputeWorkerDiff or fall back to summary payload)",
			taskID, commandID, reporter)
		return
	}
	rc.diffCacheMu.Lock()
	rc.diffCache[taskID] = diff
	rc.diffCacheMu.Unlock()
}

// popPrecapturedDiff returns the cached diff for taskID (if any) and removes
// it from the cache so memory does not grow unbounded for long-running daemons.
// The boolean indicates whether a precapture entry existed; callers can use it
// to decide whether to attempt a fresh ComputeWorkerDiff or treat the empty
// string as "captured but no diff".
func (rc *ReviewCoordinator) popPrecapturedDiff(taskID string) (string, bool) {
	if rc == nil {
		return "", false
	}
	rc.diffCacheMu.Lock()
	defer rc.diffCacheMu.Unlock()
	diff, ok := rc.diffCache[taskID]
	if ok {
		delete(rc.diffCache, taskID)
	}
	return diff, ok
}

// buildDiffContent produces the diff payload sent to the advisory reviewer.
// Prefers, in order:
//
//  1. a precaptured diff from PrecaptureDiff (stable across cleanup races
//     where the worker worktree gets wiped before DispatchIfEligible runs),
//  2. a fresh `git diff <merge-base>` via WorktreeManager.ComputeWorkerDiff,
//  3. a synthetic "summary + files changed" payload (legacy fallback).
//
// The fallback always produces non-empty content (it includes task/command
// IDs and the reporter at minimum) so the dispatcher's "DiffContent empty
// → status=skipped" early-return never fires from the daemon side. If
// the reviewer model still cannot find anything to review, it returns 0
// findings as completed — a meaningfully different signal than "we never
// sent a payload".
func (rc *ReviewCoordinator) buildDiffContent(params ResultWriteParams) string {
	if cached, ok := rc.popPrecapturedDiff(params.TaskID); ok && cached != "" {
		return cached
	}

	if rc.worktreeManager != nil {
		diff, err := rc.worktreeManager.ComputeWorkerDiff(params.CommandID, params.Reporter)
		if err != nil {
			rc.log(LogLevelWarn,
				"review_diff_compute_failed task=%s command=%s reporter=%s error=%v "+
					"(falling back to summary payload)",
				params.TaskID, params.CommandID, params.Reporter, err)
		} else if diff != "" {
			return diff
		}
	}

	// Synthetic fallback: include task / command / reporter identification
	// AND any worker-supplied summary / files_changed. The IDs guarantee
	// the payload is never empty even when the worker reported nothing,
	// so the dispatcher does not silently short-circuit to skipped. The
	// reviewer can still legitimately return 0 findings if there is
	// nothing concrete to review — that path is observable in the audit
	// trail as Status=Completed with empty Findings, which is materially
	// different from Status=Skipped with SkipReason=empty_diff_content.
	var sb strings.Builder
	sb.WriteString("[review-context-only fallback — no diff available]\n")
	sb.WriteString("task_id: ")
	sb.WriteString(params.TaskID)
	sb.WriteString("\ncommand_id: ")
	sb.WriteString(params.CommandID)
	sb.WriteString("\nreporter: ")
	sb.WriteString(params.Reporter)
	sb.WriteString("\nworker_status: ")
	sb.WriteString(params.Status)
	if params.Summary != "" {
		sb.WriteString("\n\nSummary:\n")
		sb.WriteString(params.Summary)
	}
	if len(params.FilesChanged) > 0 {
		sb.WriteString("\n\nFiles changed: ")
		sb.WriteString(strings.Join(params.FilesChanged, ", "))
	}
	return sb.String()
}

// popRequest atomically removes and returns the request info for taskID.
func (rc *ReviewCoordinator) popRequest(taskID string) (reviewTaskInfo, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	info, ok := rc.requests[taskID]
	if ok {
		delete(rc.requests, taskID)
	}
	return info, ok
}

// registerRequest atomically stores a request entry for taskID.
func (rc *ReviewCoordinator) registerRequest(taskID string, info reviewTaskInfo) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.requests[taskID] = info
}

// unregisterRequest atomically removes the request entry for taskID.
func (rc *ReviewCoordinator) unregisterRequest(taskID string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.requests, taskID)
}

// Close shuts down the review dispatcher, waiting for in-flight reviews
// to complete and closing the results channel.
func (rc *ReviewCoordinator) Close() {
	if rc != nil && rc.dispatcher != nil {
		rc.dispatcher.Close()
	}
}
