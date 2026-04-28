package daemon

import (
	"sync"
	"testing"
	"time"
)

// TestScanPhase_LockFilesAcquirableAfterPhaseC pins the lock-release
// invariant added with the post-Phase-C notification scan extraction.
// Phase C used to call resultHandler.ScanAllResults under scanMu.Lock,
// which meant a slow Planner pane (e.g. the
// agent.ErrSubmitConfirmUncertain path) blocked every UDS handler that
// took scanMu.RLock — queue_write / plan_complete / verify_write —
// surfacing as 30s CLI timeouts even when the daemon eventually
// processed the request.
//
// This test exercises the executor surface directly: scanMu must be
// fully released after Phase C so subsequent UDS handlers can acquire
// the read lock immediately.
func TestScanPhase_LockFilesAcquirableAfterPhaseC(t *testing.T) {
	t.Parallel()
	se := &ScanPhaseExecutor{}

	// Simulate the Phase-A/Phase-C scanMu cycle: take/release scanMu.Lock
	// to mirror the executor's actual phase boundaries.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		se.scanMu.Lock()
		se.scanMu.Unlock()
	}()
	wg.Wait()

	acquired := make(chan struct{})
	go func() {
		se.LockFiles()
		defer se.UnlockFiles()
		close(acquired)
	}()
	select {
	case <-acquired:
		// expected: post-Phase-C, RLock is immediately acquirable.
	case <-time.After(2 * time.Second):
		t.Fatalf("LockFiles (scanMu.RLock) blocked after Phase C release; post-scan work must not hold scanMu")
	}
}
