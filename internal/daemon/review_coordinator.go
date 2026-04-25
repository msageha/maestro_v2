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

// MonitorResults drains the dispatcher's results channel and records each
// result in the usefulness tracker. Runs until the channel is closed (by
// Close during shutdown).
func (rc *ReviewCoordinator) MonitorResults() {
	for result := range rc.dispatcher.Results() {
		rc.log(LogLevelInfo, "review_result_received request=%s model=%s status=%s findings=%d",
			result.RequestID, result.ReviewerModel, result.Status, len(result.Findings))

		if rc.tracker == nil {
			continue
		}

		taskID := extractTaskIDFromRequestID(result.RequestID)

		info, ok := rc.popRequest(taskID)

		if !ok {
			rc.log(LogLevelWarn, "review_result_orphaned request=%s task=%s (no matching dispatch record)",
				result.RequestID, taskID)
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

// DispatchIfEligible checks whether the completed task qualifies for an
// advisory review and dispatches it asynchronously. Failures are logged but
// never block the caller.
func (rc *ReviewCoordinator) DispatchIfEligible(ctx context.Context, params ResultWriteParams) {
	if rc.dispatcher == nil {
		return
	}
	queuePath := filepath.Join(rc.maestroDir, "queue", params.Reporter+".yaml")
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

// buildDiffContent produces the diff payload sent to the advisory reviewer.
// Prefers a real `git diff <merge-base>` against the integration branch
// (computed via WorktreeManager.ComputeWorkerDiff) so that the reviewer's
// system prompt — which expects a unified diff — receives input matching its
// contract.
//
// Falls back to a synthetic "summary + files changed" payload when:
//   - no WorktreeManager is wired (legacy tests / worktree-disabled configs),
//   - the command has no worktree state yet,
//   - or git diff computation fails. In each case, the dispatcher receives
//     degraded but non-empty input rather than blocking the review pipeline.
func (rc *ReviewCoordinator) buildDiffContent(params ResultWriteParams) string {
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

	var sb strings.Builder
	sb.WriteString(params.Summary)
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
