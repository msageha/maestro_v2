package formation

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionRecoverer_SuccessOnFirstAttempt(t *testing.T) {
	t.Parallel()
	r := NewSessionRecoverer(
		WithSessionCreator(func(string) error { return nil }),
		WithSessionChecker(func() bool { return false }),
		WithSleepFn(func(time.Duration) {}),
	)

	if err := r.RecoverSession("orchestrator"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if s := r.State(); s != SessionRecoveryIdle {
		t.Errorf("state = %s, want idle", s)
	}
}

func TestSessionRecoverer_SuccessOnRetry(t *testing.T) {
	t.Parallel()
	var attempt int32
	r := NewSessionRecoverer(
		WithSessionCreator(func(string) error {
			a := atomic.AddInt32(&attempt, 1)
			if a < 3 {
				return fmt.Errorf("transient error")
			}
			return nil
		}),
		WithSessionChecker(func() bool { return false }),
		WithSleepFn(func(time.Duration) {}),
	)

	if err := r.RecoverSession("orchestrator"); err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if got := atomic.LoadInt32(&attempt); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if s := r.State(); s != SessionRecoveryIdle {
		t.Errorf("state = %s, want idle", s)
	}
}

func TestSessionRecoverer_AllRetriesFail(t *testing.T) {
	t.Parallel()
	r := NewSessionRecoverer(
		WithSessionCreator(func(string) error { return fmt.Errorf("permanent error") }),
		WithSessionChecker(func() bool { return false }),
		WithSleepFn(func(time.Duration) {}),
	)

	err := r.RecoverSession("orchestrator")
	if err == nil {
		t.Fatal("expected error when all retries fail")
	}
	if s := r.State(); s != SessionRecoveryFailed {
		t.Errorf("state = %s, want failed", s)
	}
}

func TestSessionRecoverer_ExternalRecovery(t *testing.T) {
	t.Parallel()
	var checkCount int32
	r := NewSessionRecoverer(
		WithSessionCreator(func(string) error {
			return fmt.Errorf("should not be called on 2nd attempt")
		}),
		WithSessionChecker(func() bool {
			c := atomic.AddInt32(&checkCount, 1)
			// First check returns false, second (on retry) returns true
			return c >= 2
		}),
		WithSleepFn(func(time.Duration) {}),
	)

	if err := r.RecoverSession("orchestrator"); err != nil {
		t.Fatalf("expected external recovery success, got %v", err)
	}
	if s := r.State(); s != SessionRecoveryIdle {
		t.Errorf("state = %s, want idle", s)
	}
}

func TestSessionRecoverer_StateTransitions(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	proceed := make(chan struct{})

	r := NewSessionRecoverer(
		WithSessionCreator(func(string) error {
			close(started)
			<-proceed
			return nil
		}),
		WithSessionChecker(func() bool { return false }),
		WithSleepFn(func(time.Duration) {}),
	)

	if s := r.State(); s != SessionRecoveryIdle {
		t.Fatalf("initial state = %s, want idle", s)
	}

	done := make(chan error, 1)
	go func() { done <- r.RecoverSession("orchestrator") }()

	<-started
	if s := r.State(); s != SessionRecoveryRecovering {
		t.Errorf("during recovery state = %s, want recovering", s)
	}

	close(proceed)
	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s := r.State(); s != SessionRecoveryIdle {
		t.Errorf("after recovery state = %s, want idle", s)
	}
}

func TestSessionRecoverer_Reset(t *testing.T) {
	t.Parallel()
	r := NewSessionRecoverer(
		WithSessionCreator(func(string) error { return fmt.Errorf("fail") }),
		WithSessionChecker(func() bool { return false }),
		WithSleepFn(func(time.Duration) {}),
	)

	_ = r.RecoverSession("orchestrator")
	if s := r.State(); s != SessionRecoveryFailed {
		t.Fatalf("state = %s, want failed", s)
	}

	r.Reset()
	if s := r.State(); s != SessionRecoveryIdle {
		t.Errorf("after reset state = %s, want idle", s)
	}
}

func TestSessionRecoverer_ExponentialBackoff(t *testing.T) {
	t.Parallel()
	var sleepDurations []time.Duration
	r := NewSessionRecoverer(
		WithSessionCreator(func(string) error { return fmt.Errorf("fail") }),
		WithSessionChecker(func() bool { return false }),
		WithSleepFn(func(d time.Duration) { sleepDurations = append(sleepDurations, d) }),
	)

	_ = r.RecoverSession("orchestrator")

	// 3 attempts: no sleep before 1st, 1s before 2nd, 2s before 3rd
	if len(sleepDurations) != 2 {
		t.Fatalf("expected 2 sleeps, got %d: %v", len(sleepDurations), sleepDurations)
	}
	if sleepDurations[0] != 1*time.Second {
		t.Errorf("backoff[0] = %v, want 1s", sleepDurations[0])
	}
	if sleepDurations[1] != 2*time.Second {
		t.Errorf("backoff[1] = %v, want 2s", sleepDurations[1])
	}
}

func TestSessionRecoverer_CustomMaxRetries(t *testing.T) {
	t.Parallel()
	var attempts int32
	r := NewSessionRecoverer(
		WithMaxRetries(5),
		WithSessionCreator(func(string) error {
			atomic.AddInt32(&attempts, 1)
			return fmt.Errorf("fail")
		}),
		WithSessionChecker(func() bool { return false }),
		WithSleepFn(func(time.Duration) {}),
	)

	_ = r.RecoverSession("orchestrator")
	if got := atomic.LoadInt32(&attempts); got != 5 {
		t.Errorf("attempts = %d, want 5", got)
	}
}
