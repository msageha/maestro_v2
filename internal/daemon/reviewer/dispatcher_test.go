package reviewer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

func defaultConfig() model.ReviewConfig {
	return model.ReviewConfig{
		Enabled:              true,
		Models:               []string{"gpt-4"},
		MinBloomLevel:        ptr.Int(2),
		MaxConcurrentReviews: ptr.Int(2),
		TimeoutSec:           ptr.Int(5),
	}
}

func taskWithBloom(level int) model.Task {
	return model.Task{
		ID:         "task-1",
		CommandID:  "cmd-1",
		BloomLevel: level,
	}
}

// --- ShouldReview tests ---

func TestShouldReview_Enabled_AboveMinBloom(t *testing.T) {
	d := NewReviewDispatcher(defaultConfig())
	if !d.ShouldReview(taskWithBloom(3)) {
		t.Error("expected ShouldReview=true for bloom_level=3 with min=2")
	}
}

func TestShouldReview_Enabled_AtMinBloom(t *testing.T) {
	d := NewReviewDispatcher(defaultConfig())
	if !d.ShouldReview(taskWithBloom(2)) {
		t.Error("expected ShouldReview=true for bloom_level=2 with min=2")
	}
}

func TestShouldReview_BelowMinBloom(t *testing.T) {
	d := NewReviewDispatcher(defaultConfig())
	if d.ShouldReview(taskWithBloom(1)) {
		t.Error("expected ShouldReview=false for bloom_level=1 with min=2")
	}
}

func TestShouldReview_Disabled(t *testing.T) {
	cfg := defaultConfig()
	cfg.Enabled = false
	d := NewReviewDispatcher(cfg)
	if d.ShouldReview(taskWithBloom(5)) {
		t.Error("expected ShouldReview=false when disabled")
	}
}

func TestShouldReview_ConcurrentLimitReached(t *testing.T) {
	cfg := defaultConfig()
	cfg.MaxConcurrentReviews = ptr.Int(1)
	d := NewReviewDispatcher(cfg)

	// Simulate one active review.
	d.mu.Lock()
	d.activeReviews = 1
	d.mu.Unlock()

	if d.ShouldReview(taskWithBloom(3)) {
		t.Error("expected ShouldReview=false when concurrent limit reached")
	}
}

func TestShouldReview_ConcurrentLimitNotReached(t *testing.T) {
	cfg := defaultConfig()
	cfg.MaxConcurrentReviews = ptr.Int(2)
	d := NewReviewDispatcher(cfg)

	d.mu.Lock()
	d.activeReviews = 1
	d.mu.Unlock()

	if !d.ShouldReview(taskWithBloom(3)) {
		t.Error("expected ShouldReview=true when below concurrent limit")
	}
}

// --- Dispatch tests ---

func TestDispatch_Async_NonBlocking(t *testing.T) {
	d := NewReviewDispatcher(defaultConfig())
	ctx := context.Background()

	start := time.Now()
	err := d.Dispatch(ctx, taskWithBloom(3), "diff content")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("Dispatch took %v; expected non-blocking return", elapsed)
	}

	d.Close()
}

func TestDispatch_ResultReceived(t *testing.T) {
	d := NewReviewDispatcher(defaultConfig())
	ctx := context.Background()

	if err := d.Dispatch(ctx, taskWithBloom(3), "some diff"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d.Close()

	select {
	case result := <-d.Results():
		if result.Status != model.ReviewStatusCompleted {
			t.Errorf("expected status completed, got %s", result.Status)
		}
		if !result.IsAdvisory {
			t.Error("expected IsAdvisory=true")
		}
		if result.Duration <= 0 {
			t.Error("expected positive duration")
		}
	default:
		t.Error("expected a result on the channel")
	}
}

func TestDispatch_Disabled(t *testing.T) {
	cfg := defaultConfig()
	cfg.Enabled = false
	d := NewReviewDispatcher(cfg)

	err := d.Dispatch(context.Background(), taskWithBloom(3), "diff")
	if err == nil {
		t.Error("expected error when dispatching with reviews disabled")
	}
}

func TestDispatch_Timeout_Skipped(t *testing.T) {
	cfg := defaultConfig()
	cfg.TimeoutSec = ptr.Int(0) // 0-second timeout → immediate expiry

	d := NewReviewDispatcher(cfg)
	// Use an already-cancelled context to guarantee timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := d.Dispatch(ctx, taskWithBloom(3), "diff"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d.Close()

	select {
	case result := <-d.Results():
		if result.Status != model.ReviewStatusSkipped {
			t.Errorf("expected status skipped on timeout, got %s", result.Status)
		}
	default:
		t.Error("expected a skipped result on the channel")
	}
}

func TestDispatch_ActiveReviews_Decremented(t *testing.T) {
	d := NewReviewDispatcher(defaultConfig())
	ctx := context.Background()

	if err := d.Dispatch(ctx, taskWithBloom(3), "diff"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d.Close()

	d.mu.Lock()
	active := d.activeReviews
	d.mu.Unlock()

	if active != 0 {
		t.Errorf("expected activeReviews=0 after completion, got %d", active)
	}
}

func TestDispatch_MultipleReviews(t *testing.T) {
	cfg := defaultConfig()
	cfg.MaxConcurrentReviews = ptr.Int(3)
	d := NewReviewDispatcher(cfg)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		task := model.Task{
			ID:         fmt.Sprintf("task-%d", i),
			CommandID:  "cmd-1",
			BloomLevel: 3,
		}
		if err := d.Dispatch(ctx, task, "diff"); err != nil {
			t.Fatalf("dispatch %d: unexpected error: %v", i, err)
		}
	}

	d.Close()

	count := 0
	for range d.Results() {
		count++
	}
	if count != 3 {
		t.Errorf("expected 3 results, got %d", count)
	}
}
