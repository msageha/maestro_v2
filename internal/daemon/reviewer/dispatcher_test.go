package reviewer

import (
	"context"
	"errors"
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

// stubInvoker is a test double for ClaudeInvoker. It returns the configured
// response (or error) without launching any subprocess.
type stubInvoker struct {
	response string
	err      error
	called   int
}

func (s *stubInvoker) Invoke(ctx context.Context, model, systemPrompt, userPrompt string) (string, error) {
	s.called++
	if s.err != nil {
		return "", s.err
	}
	return s.response, nil
}

// newDispatcherWithStub builds a dispatcher whose model invocation is stubbed.
// The stub returns an empty findings array by default so background dispatch
// tests do not attempt to shell out to the real `claude` binary.
func newDispatcherWithStub(cfg model.ReviewConfig) (*ReviewDispatcher, *stubInvoker) {
	d := NewReviewDispatcher(cfg)
	s := &stubInvoker{response: "[]"}
	d.SetInvoker(s)
	return d, s
}

// --- ShouldReview tests ---

func TestShouldReview_Enabled_AboveMinBloom(t *testing.T) {
	t.Parallel()
	d := NewReviewDispatcher(defaultConfig())
	if !d.ShouldReview(taskWithBloom(3)) {
		t.Error("expected ShouldReview=true for bloom_level=3 with min=2")
	}
}

func TestShouldReview_Enabled_AtMinBloom(t *testing.T) {
	t.Parallel()
	d := NewReviewDispatcher(defaultConfig())
	if !d.ShouldReview(taskWithBloom(2)) {
		t.Error("expected ShouldReview=true for bloom_level=2 with min=2")
	}
}

func TestShouldReview_BelowMinBloom(t *testing.T) {
	t.Parallel()
	d := NewReviewDispatcher(defaultConfig())
	if d.ShouldReview(taskWithBloom(1)) {
		t.Error("expected ShouldReview=false for bloom_level=1 with min=2")
	}
}

func TestShouldReview_Disabled(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.Enabled = false
	d := NewReviewDispatcher(cfg)
	if d.ShouldReview(taskWithBloom(5)) {
		t.Error("expected ShouldReview=false when disabled")
	}
}

func TestShouldReview_EmptyModels(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.Models = nil
	d := NewReviewDispatcher(cfg)
	if d.ShouldReview(taskWithBloom(3)) {
		t.Error("expected ShouldReview=false when no models configured")
	}
}

func TestShouldReview_ConcurrentLimitReached(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	d, _ := newDispatcherWithStub(defaultConfig())
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

func TestDispatch_ResultReceived_Completed(t *testing.T) {
	t.Parallel()
	d, stub := newDispatcherWithStub(defaultConfig())
	stub.response = `[{"severity":"warning","file_path":"main.go","line":10,"message":"possible nil deref"}]`
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
		if len(result.Findings) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(result.Findings))
		}
		if result.Findings[0].Severity != model.ReviewSeverityWarning {
			t.Errorf("expected severity warning, got %s", result.Findings[0].Severity)
		}
		if result.Findings[0].FilePath != "main.go" {
			t.Errorf("expected file_path main.go, got %s", result.Findings[0].FilePath)
		}
	default:
		t.Error("expected a result on the channel")
	}
}

func TestDispatch_EmptyDiff_Skipped(t *testing.T) {
	t.Parallel()
	d, stub := newDispatcherWithStub(defaultConfig())
	ctx := context.Background()

	if err := d.Dispatch(ctx, taskWithBloom(3), "   "); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d.Close()

	if stub.called != 0 {
		t.Errorf("expected stub NOT to be invoked for empty diff, got called=%d", stub.called)
	}
	select {
	case result := <-d.Results():
		if result.Status != model.ReviewStatusSkipped {
			t.Errorf("expected status skipped for empty diff, got %s", result.Status)
		}
	default:
		t.Error("expected a result on the channel")
	}
}

func TestDispatch_InvokerError_Skipped(t *testing.T) {
	t.Parallel()
	d := NewReviewDispatcher(defaultConfig())
	d.SetInvoker(&stubInvoker{err: errors.New("boom")})
	ctx := context.Background()

	if err := d.Dispatch(ctx, taskWithBloom(3), "some diff"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d.Close()

	select {
	case result := <-d.Results():
		if result.Status != model.ReviewStatusSkipped {
			t.Errorf("expected status skipped on invoker error, got %s", result.Status)
		}
	default:
		t.Error("expected a result on the channel")
	}
}

func TestDispatch_Disabled(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.Enabled = false
	d := NewReviewDispatcher(cfg)

	err := d.Dispatch(context.Background(), taskWithBloom(3), "diff")
	if err == nil {
		t.Error("expected error when dispatching with reviews disabled")
	}
}

func TestDispatch_EmptyModels(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.Models = nil
	d := NewReviewDispatcher(cfg)

	err := d.Dispatch(context.Background(), taskWithBloom(3), "diff")
	if err == nil {
		t.Error("expected error when no reviewer models configured")
	}
}

func TestDispatch_Timeout_Skipped(t *testing.T) {
	t.Parallel()
	cfg := defaultConfig()
	cfg.TimeoutSec = ptr.Int(0) // 0-second timeout → immediate expiry

	d, _ := newDispatcherWithStub(cfg)
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
	t.Parallel()
	d, _ := newDispatcherWithStub(defaultConfig())
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
	t.Parallel()
	cfg := defaultConfig()
	cfg.MaxConcurrentReviews = ptr.Int(3)
	d, _ := newDispatcherWithStub(cfg)
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

// --- ErrNotImplemented tests ---

func TestErrNotImplemented_IsSentinel(t *testing.T) {
	t.Parallel()
	if !errors.Is(ErrNotImplemented, ErrNotImplemented) {
		t.Error("ErrNotImplemented should match itself via errors.Is")
	}
}

// TestReviewTask_StubInvoker verifies that reviewTask uses the injected
// invoker and parses its findings response into the ReviewResult.
func TestReviewTask_StubInvoker(t *testing.T) {
	t.Parallel()
	d := NewReviewDispatcher(defaultConfig())
	stub := &stubInvoker{response: `[{"severity":"error","file_path":"x.go","line":1,"message":"bad"}]`}
	d.SetInvoker(stub)
	ctx := context.Background()
	req := model.ReviewRequest{
		ID:            "review-test",
		TaskID:        "task-1",
		CommandID:     "cmd-1",
		ReviewerModel: "gpt-4",
		DiffContent:   "diff --git a/x.go b/x.go\n@@ -1 +1 @@\n-old\n+new\n",
	}

	if err := d.reviewTask(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stub.called != 1 {
		t.Errorf("expected stub called once, got %d", stub.called)
	}

	select {
	case result := <-d.results:
		if result.Status != model.ReviewStatusCompleted {
			t.Errorf("expected status completed, got %s", result.Status)
		}
		if len(result.Findings) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(result.Findings))
		}
		if result.Findings[0].Severity != model.ReviewSeverityError {
			t.Errorf("expected severity error, got %s", result.Findings[0].Severity)
		}
	default:
		t.Error("expected a result on the channel")
	}
}
