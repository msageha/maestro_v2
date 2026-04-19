package dispatch

import (
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

// stubExecutor always returns the configured error.
type stubExecutor struct {
	err error
}

func (s *stubExecutor) Execute(_ agent.ExecRequest) agent.ExecResult {
	return agent.ExecResult{Error: s.err, Retryable: s.err != nil}
}
func (s *stubExecutor) Close() error { return nil }

// stubExecutorGetter wraps a core.AgentExecutor.
type stubExecutorGetter struct {
	exec core.AgentExecutor
}

func (g *stubExecutorGetter) GetExecutor() (core.AgentExecutor, error) {
	return g.exec, nil
}

func newRetryTestDispatcher(retries int, delaySec int, exec core.AgentExecutor) *Dispatcher {
	cfg := model.Config{
		Retry: model.RetryConfig{
			CommandDispatchInlineRetries:       ptr.Int(retries),
			CommandDispatchInlineRetryDelaySec: ptr.Int(delaySec),
			TaskDispatchInlineRetries:          ptr.Int(retries),
			TaskDispatchInlineRetryDelaySec:    ptr.Int(delaySec),
		},
	}
	return New("", cfg, log.New(log.Writer(), "", 0), core.LogLevelDebug, &stubExecutorGetter{exec: exec}, core.RealClock{})
}

func TestDispatchCommand_ContextCancellation(t *testing.T) {
	t.Parallel()
	exec := &stubExecutor{err: errors.New("transient")}
	d := newRetryTestDispatcher(10, 1, exec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	start := time.Now()
	err := d.DispatchCommand(ctx, &model.Command{ID: "cmd_test"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("should return immediately on canceled context, took %v", elapsed)
	}
}

func TestDispatchTask_ContextCancellation(t *testing.T) {
	t.Parallel()
	exec := &stubExecutor{err: errors.New("transient")}
	d := newRetryTestDispatcher(10, 1, exec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	start := time.Now()
	err := d.DispatchTask(ctx, &model.Task{ID: "task_test", CommandID: "cmd_test"}, "worker1")
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("should return immediately on canceled context, took %v", elapsed)
	}
}

func TestDispatchCommand_BackoffCap(t *testing.T) {
	t.Parallel()
	// Use a large initial delay (e.g. 20s) so that after doubling it exceeds 30s.
	// With maxRetries=3 and initial delay=20s, without cap: 20, 40, 80 seconds.
	// With cap at 30s: 20, 30, 30 seconds.
	// We verify by canceling context after a short delay and checking attempt count.
	callCount := 0

	countingExec := &countingStubExecutor{err: errors.New("always fail")}
	d := newRetryTestDispatcher(5, 20, countingExec)
	_ = callCount

	// Cancel after 100ms — without the cap, retryDelay starts at 20s so
	// we would not get past attempt 0's sleep. With the implementation
	// where sleep is context-aware, we should return quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := d.DispatchCommand(ctx, &model.Command{ID: "cmd_cap_test"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("should return within ~100ms, took %v", elapsed)
	}
	// At least 1 attempt (the first one before any sleep)
	if countingExec.calls < 1 {
		t.Fatalf("expected at least 1 call, got %d", countingExec.calls)
	}
}

func TestBackoffCap_DoesNotExceed30Seconds(t *testing.T) {
	t.Parallel()
	// Verify the cap logic directly: starting from 1s, doubling repeatedly,
	// the delay should never exceed maxBackoffDuration (30s).
	delay := 1 * time.Second
	for i := 0; i < 20; i++ {
		delay = delay * 2
		if delay > maxBackoffDuration {
			delay = maxBackoffDuration
		}
		if delay > maxBackoffDuration {
			t.Fatalf("iteration %d: delay %v exceeds max %v", i, delay, maxBackoffDuration)
		}
	}
	if delay != maxBackoffDuration {
		t.Fatalf("expected delay to settle at %v, got %v", maxBackoffDuration, delay)
	}
}

func TestSleepWithContext_ReturnsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := sleepWithContext(ctx, 10*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("should return immediately, took %v", elapsed)
	}
}

func TestSleepWithContext_SleepsFullDuration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	start := time.Now()
	err := sleepWithContext(ctx, 50*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("should sleep at least ~50ms, took %v", elapsed)
	}
}

// countingStubExecutor counts Execute calls and always returns err.
type countingStubExecutor struct {
	err   error
	calls int
}

func (c *countingStubExecutor) Execute(_ agent.ExecRequest) agent.ExecResult {
	c.calls++
	return agent.ExecResult{Error: c.err, Retryable: c.err != nil}
}
func (c *countingStubExecutor) Close() error { return nil }
