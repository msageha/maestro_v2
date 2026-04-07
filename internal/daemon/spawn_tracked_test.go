package daemon

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// newSpawnTrackedTestDaemon builds the minimal Daemon scaffold required to
// exercise spawnTracked + the egMu/shuttingDown synchronization, without going
// through newDaemon (which wires far more dependencies than needed here).
func newSpawnTrackedTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := &Daemon{
		logger:   log.New(&discardWriter{}, "", 0),
		logLevel: LogLevelError,
		clock:    RealClock{},
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.eg, d.egCtx = errgroup.WithContext(d.ctx)
	return d
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// TestSpawnTracked_ShutdownRace stresses the admission path against a
// concurrent shutdown-style flag flip. With the pre-fix code, the unguarded
// `if !shuttingDown { eg.Go(...) }` pattern would race with eg.Wait() and
// occasionally panic with "sync: WaitGroup is reused before previous Wait has
// returned" or leak a goroutine past Wait. With egMu in place, every admitted
// goroutine is fully tracked and Wait drains cleanly.
func TestSpawnTracked_ShutdownRace(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		d := newSpawnTrackedTestDaemon(t)

		var started atomic.Int64
		var finished atomic.Int64
		release := make(chan struct{})

		// Many concurrent untracked callers (simulating UDS handler goroutines).
		var wg sync.WaitGroup
		spawners := 64
		wg.Add(spawners)
		for i := 0; i < spawners; i++ {
			go func() {
				defer wg.Done()
				ok := d.spawnTracked("testSpawn", func(ctx context.Context) {
					started.Add(1)
					select {
					case <-release:
					case <-ctx.Done():
					}
					finished.Add(1)
				})
				_ = ok
			}()
		}

		// Concurrently flip shuttingDown under egMu, mimicking Shutdown step 1.
		// Vary the delay slightly across trials to interleave the flag flip
		// with the spawner admission window.
		time.Sleep(time.Duration(trial%5) * 50 * time.Microsecond)
		d.egMu.Lock()
		d.shuttingDown.Store(true)
		d.egMu.Unlock()

		// Cancel context so already-running goroutines exit, then drain.
		d.cancel()
		close(release)
		wg.Wait()

		if err := d.eg.Wait(); err != nil {
			t.Fatalf("trial %d: eg.Wait returned error: %v", trial, err)
		}

		// Every goroutine that started must have finished — i.e. it was tracked.
		if started.Load() != finished.Load() {
			t.Fatalf("trial %d: started=%d finished=%d (leaked goroutine past eg.Wait)",
				trial, started.Load(), finished.Load())
		}
	}
}

// TestSpawnTracked_AfterShutdown verifies that once shuttingDown is set, no
// new goroutines are admitted to the errgroup.
func TestSpawnTracked_AfterShutdown(t *testing.T) {
	d := newSpawnTrackedTestDaemon(t)

	d.egMu.Lock()
	d.shuttingDown.Store(true)
	d.egMu.Unlock()

	var ran atomic.Bool
	ok := d.spawnTracked("postShutdown", func(ctx context.Context) {
		ran.Store(true)
	})
	if ok {
		t.Fatalf("spawnTracked admitted goroutine after shuttingDown=true")
	}

	d.cancel()
	if err := d.eg.Wait(); err != nil {
		t.Fatalf("eg.Wait: %v", err)
	}
	if ran.Load() {
		t.Fatalf("post-shutdown goroutine should not have run")
	}
}

// TestSpawnTracked_NilEgFallback exercises the test/fallback path where Run()
// was never called and d.eg is nil. The fallback must still respect
// shuttingDown.
func TestSpawnTracked_NilEgFallback(t *testing.T) {
	d := &Daemon{
		logger:   log.New(&discardWriter{}, "", 0),
		logLevel: LogLevelError,
		clock:    RealClock{},
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	defer d.cancel()

	done := make(chan struct{})
	if !d.spawnTracked("nilEg", func(ctx context.Context) { close(done) }) {
		t.Fatalf("spawnTracked returned false in nil-eg path")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("nil-eg goroutine did not run")
	}

	d.shuttingDown.Store(true)
	if d.spawnTracked("nilEgAfter", func(ctx context.Context) {}) {
		t.Fatalf("spawnTracked admitted nil-eg goroutine after shuttingDown")
	}
}
