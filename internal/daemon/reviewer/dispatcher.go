// Package reviewer provides asynchronous dispatch of heterogeneous-model
// code reviews. Reviews are advisory (non-blocking) and run in background
// goroutines so they never stall the task pipeline.
package reviewer

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// ReviewDispatcher manages asynchronous dispatch of code reviews to
// heterogeneous models. All reviews are advisory and non-blocking.
type ReviewDispatcher struct {
	config        model.ReviewConfig
	mu            sync.Mutex
	activeReviews int
	results       chan model.ReviewResult
	wg            sync.WaitGroup
}

// NewReviewDispatcher creates a new ReviewDispatcher with the given config.
func NewReviewDispatcher(config model.ReviewConfig) *ReviewDispatcher {
	return &ReviewDispatcher{
		config:  config,
		results: make(chan model.ReviewResult, config.EffectiveMaxConcurrentReviews()),
	}
}

// ShouldReview determines whether a task qualifies for review dispatch.
// Returns true only when reviews are enabled, the task's BloomLevel meets
// the minimum threshold, and the concurrent review limit has not been reached.
func (d *ReviewDispatcher) ShouldReview(task model.Task) bool {
	if !d.config.Enabled {
		return false
	}
	if task.BloomLevel < d.config.EffectiveMinBloomLevel() {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.activeReviews < d.config.EffectiveMaxConcurrentReviews()
}

// Dispatch starts an asynchronous review for the given task and diff content.
// It returns immediately after launching a background goroutine. The result
// will be sent to the Results channel upon completion or timeout.
func (d *ReviewDispatcher) Dispatch(ctx context.Context, task model.Task, diffContent string) error {
	if !d.config.Enabled {
		return fmt.Errorf("review dispatch: reviews are disabled")
	}

	reviewerModel := ""
	if len(d.config.Models) > 0 {
		reviewerModel = d.config.Models[0]
	}

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
		d.reviewTask(reviewCtx, req)
	}()

	return nil
}

// reviewTask executes the review. The actual model invocation is deferred to
// a future implementation; for now it produces a dummy advisory result.
func (d *ReviewDispatcher) reviewTask(ctx context.Context, req model.ReviewRequest) {
	start := time.Now()
	result := model.NewReviewResult(req.ID, req.ReviewerModel)

	defer func() {
		result.Duration = time.Since(start)
		d.mu.Lock()
		d.activeReviews--
		d.mu.Unlock()
		select {
		case d.results <- *result:
		default:
			log.Printf("reviewer: results channel full, dropping result for %s", req.ID)
		}
	}()

	select {
	case <-ctx.Done():
		result.Status = model.ReviewStatusSkipped
		return
	default:
	}

	// Placeholder: future implementation will call the reviewer model here.
	log.Printf("reviewer: executing review %s for task %s (model=%s)", req.ID, req.TaskID, req.ReviewerModel)
	result.Status = model.ReviewStatusCompleted
	result.Findings = nil
}

// Results returns a read-only channel from which review results can be received.
func (d *ReviewDispatcher) Results() <-chan model.ReviewResult {
	return d.results
}

// Close waits for all in-flight reviews to complete, then closes the results channel.
func (d *ReviewDispatcher) Close() {
	d.wg.Wait()
	close(d.results)
}
