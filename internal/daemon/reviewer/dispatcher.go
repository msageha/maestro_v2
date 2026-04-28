// Package reviewer provides asynchronous dispatch of heterogeneous-model
// code reviews. Reviews are advisory (non-blocking) and run in background
// goroutines so they never stall the task pipeline.
package reviewer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// ErrNotImplemented is a sentinel returned by ClaudeInvoker implementations
// that intentionally do not support the requested review model (for example,
// test stubs or partially-wired alternative invokers). When the dispatcher
// observes this error it logs at info level and skips the review without
// failing the pipeline — reviews are advisory.
var ErrNotImplemented = errors.New("reviewer: not implemented")

// ReviewDispatcher manages asynchronous dispatch of code reviews to
// heterogeneous models. All reviews are advisory and non-blocking.
type ReviewDispatcher struct {
	config         model.ReviewConfig
	mu             sync.RWMutex
	activeReviews  int
	results        chan model.ReviewResult
	wg             sync.WaitGroup
	droppedResults atomic.Int64
	invoker        ClaudeInvoker
	nextModel      atomic.Uint64
}

// NewReviewDispatcher creates a new ReviewDispatcher with the given config.
func NewReviewDispatcher(config model.ReviewConfig) *ReviewDispatcher {
	return &ReviewDispatcher{
		config:  config,
		results: make(chan model.ReviewResult, config.EffectiveMaxConcurrentReviews()),
		invoker: CLIInvoker{},
	}
}

// SetInvoker overrides the default CLI invoker. Intended for tests that
// need to stub the model call without launching a real subprocess.
func (d *ReviewDispatcher) SetInvoker(i ClaudeInvoker) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.invoker = i
}

func (d *ReviewDispatcher) getInvoker() ClaudeInvoker {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.invoker == nil {
		return CLIInvoker{}
	}
	return d.invoker
}

// ShouldReview determines whether a task qualifies for review dispatch.
// Returns true only when reviews are enabled, the task's BloomLevel meets
// the minimum threshold, and the concurrent review limit has not been reached.
func (d *ReviewDispatcher) ShouldReview(task model.Task) bool {
	if !d.config.Enabled {
		return false
	}
	if len(d.config.Models) == 0 {
		return false
	}
	if task.BloomLevel < d.config.EffectiveMinBloomLevel() {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.activeReviews < d.config.EffectiveMaxConcurrentReviews()
}

// Dispatch starts an asynchronous review for the given task and diff content.
// It returns immediately after launching a background goroutine. The result
// will be sent to the Results channel upon completion or timeout.
func (d *ReviewDispatcher) Dispatch(ctx context.Context, task model.Task, diffContent string) error {
	if !d.config.Enabled {
		return fmt.Errorf("review dispatch: reviews are disabled")
	}

	if len(d.config.Models) == 0 {
		return fmt.Errorf("review dispatch: no reviewer models configured")
	}
	reviewerModel := d.nextReviewerModel()

	req := model.ReviewRequest{
		ID:            fmt.Sprintf("review-%s-%d", task.ID, time.Now().UnixNano()),
		TaskID:        task.ID,
		CommandID:     task.CommandID,
		ReviewerModel: reviewerModel,
		DiffContent:   diffContent,
		CreatedAt:     time.Now(),
	}

	d.mu.Lock()
	d.activeReviews++
	defer d.mu.Unlock()

	timeout := time.Duration(d.config.EffectiveTimeoutSec()) * time.Second
	reviewCtx, cancel := context.WithTimeout(ctx, timeout)

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer cancel()
		if err := d.reviewTask(reviewCtx, req); err != nil {
			if errors.Is(err, ErrNotImplemented) {
				slog.Info("reviewer: review not yet implemented, skipping", "review_id", req.ID, "task_id", req.TaskID)
			} else {
				slog.Error("reviewer: review failed", "error", err, "review_id", req.ID, "task_id", req.TaskID)
			}
		}
	}()

	return nil
}

func (d *ReviewDispatcher) nextReviewerModel() string {
	models := d.config.Models
	if len(models) == 0 {
		return ""
	}
	idx := d.nextModel.Add(1) - 1
	// Convert by reducing modulo len(models) FIRST (still uint64) and then
	// casting; the result is provably in [0, len(models)) so the cast can
	// never overflow int even on 32-bit platforms. The previous form
	// (int(idx%uint64(len(models)))) was the same logic but tripped gosec
	// G115 because it could not statically see the bounds.
	n := uint64(len(models)) //nolint:gosec // len() of a slice is non-negative; cast is safe.
	return models[idx%n]
}

// reviewTask executes the review by invoking the configured Claude model
// with the diff from req.DiffContent and parsing findings from the
// model response. Findings are placed on result.Findings; on invocation
// or parse failure the result is marked Skipped (reviews are advisory,
// so failures never block the task pipeline).
func (d *ReviewDispatcher) reviewTask(ctx context.Context, req model.ReviewRequest) error {
	start := time.Now()
	result := model.NewReviewResult(req.ID, req.ReviewerModel, true)

	defer func() {
		result.Duration = time.Since(start)
		d.mu.Lock()
		d.activeReviews--
		d.mu.Unlock()
		select {
		case d.results <- *result:
		default:
			d.droppedResults.Add(1)
			slog.Warn("reviewer: results channel full, dropping result", "review_id", req.ID)
		}
	}()

	select {
	case <-ctx.Done():
		result.Status = model.ReviewStatusSkipped
		return nil
	default:
	}

	if strings.TrimSpace(req.DiffContent) == "" {
		// Empty diff means nothing to review; record as skipped with no findings.
		result.Status = model.ReviewStatusSkipped
		return nil
	}

	userPrompt := renderUserPrompt(req)
	raw, err := d.getInvoker().Invoke(ctx, req.ReviewerModel, reviewSystemPrompt, userPrompt)
	if err != nil {
		result.Status = model.ReviewStatusSkipped
		return fmt.Errorf("reviewer: model invocation failed: %w", err)
	}

	findings, parseErr := parseFindings(raw)
	if parseErr != nil {
		result.Status = model.ReviewStatusSkipped
		return fmt.Errorf("reviewer: response parse failed: %w", parseErr)
	}

	result.Findings = findings
	result.Status = model.ReviewStatusCompleted
	return nil
}

// Results returns a read-only channel from which review results can be received.
func (d *ReviewDispatcher) Results() <-chan model.ReviewResult {
	return d.results
}

// DroppedResults returns the number of review results dropped due to a full channel.
func (d *ReviewDispatcher) DroppedResults() int64 {
	return d.droppedResults.Load()
}

// Close waits for all in-flight reviews to complete, then closes the results channel.
func (d *ReviewDispatcher) Close() {
	d.wg.Wait()
	close(d.results)
}
