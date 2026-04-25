package daemon

import (
	"bytes"
	"context"
	"log"
	"testing"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// TestPhaseBC_CounterPreservation verifies that counters incremented during
// Phase B (e.g., SignalInlineRetrySuccesses) are not lost when Phase C starts.
// This is a regression test for the bug where Phase C's "se.scanCounters =
// pa.counters" overwrote Phase B increments with the Phase A snapshot.
func TestPhaseBC_CounterPreservation(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 1}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	exec := newRecordingExecutor(nil)
	qh.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return exec, nil
	})

	se := qh.scanExecutor

	// Phase A: resets counters and accumulates Phase A values.
	pa := se.periodicScanPhaseA()

	// Verify Phase A snapshot is captured.
	if pa.counters != se.scanCounters {
		t.Fatal("pa.counters should equal se.scanCounters after Phase A")
	}

	// Simulate Phase B incrementing counters (as deliverPlannerSignal does).
	se.scanCounters.SignalInlineRetrySuccesses += 2
	se.scanCounters.LeaseRenewals += 3

	// Phase C: must NOT discard Phase B increments.
	se.periodicScanPhaseC(pa, phaseBResult{})

	// Verify Phase B increments survived Phase C.
	if se.scanCounters.SignalInlineRetrySuccesses != 2 {
		t.Errorf("SignalInlineRetrySuccesses = %d, want 2 (Phase B increment lost)", se.scanCounters.SignalInlineRetrySuccesses)
	}
	if se.scanCounters.LeaseRenewals != 3 {
		t.Errorf("LeaseRenewals = %d, want 3 (Phase B increment lost)", se.scanCounters.LeaseRenewals)
	}
}

// TestPhaseBC_AllCounterFields verifies that every field of ScanCounters
// survives the Phase B→C transition. This guards against future regressions
// if new counter fields are added.
func TestPhaseBC_AllCounterFields(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 1}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	exec := newRecordingExecutor(nil)
	qh.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return exec, nil
	})

	se := qh.scanExecutor

	// Phase A
	pa := se.periodicScanPhaseA()

	// Simulate Phase B incrementing ALL counter fields by 1.
	se.scanCounters.CommandsDispatched++
	se.scanCounters.TasksDispatched++
	se.scanCounters.TasksCompleted++
	se.scanCounters.TasksFailed++
	se.scanCounters.TasksCancelled++
	se.scanCounters.DeadLetters++
	se.scanCounters.ReconciliationRepairs++
	se.scanCounters.NotificationRetries++
	se.scanCounters.SignalDeliveries++
	se.scanCounters.SignalRetries++
	se.scanCounters.SignalDeadLetters++
	se.scanCounters.SignalInlineRetrySuccesses++
	se.scanCounters.LeaseRenewals++
	se.scanCounters.LeaseExtensions++
	se.scanCounters.LeaseReleases++

	// Snapshot before Phase C
	expected := se.scanCounters

	// Phase C
	se.periodicScanPhaseC(pa, phaseBResult{})

	// Phase C may add its own increments (e.g., NotificationRetries,
	// ReconciliationRepairs), so we verify Phase B values are at least
	// preserved (>=), not that they're exactly equal.
	check := func(name string, got, want int) {
		t.Helper()
		if got < want {
			t.Errorf("%s = %d, want >= %d (Phase B increment lost)", name, got, want)
		}
	}
	check("CommandsDispatched", se.scanCounters.CommandsDispatched, expected.CommandsDispatched)
	check("TasksDispatched", se.scanCounters.TasksDispatched, expected.TasksDispatched)
	check("TasksCompleted", se.scanCounters.TasksCompleted, expected.TasksCompleted)
	check("TasksFailed", se.scanCounters.TasksFailed, expected.TasksFailed)
	check("TasksCancelled", se.scanCounters.TasksCancelled, expected.TasksCancelled)
	check("DeadLetters", se.scanCounters.DeadLetters, expected.DeadLetters)
	check("ReconciliationRepairs", se.scanCounters.ReconciliationRepairs, expected.ReconciliationRepairs)
	check("NotificationRetries", se.scanCounters.NotificationRetries, expected.NotificationRetries)
	check("SignalDeliveries", se.scanCounters.SignalDeliveries, expected.SignalDeliveries)
	check("SignalRetries", se.scanCounters.SignalRetries, expected.SignalRetries)
	check("SignalDeadLetters", se.scanCounters.SignalDeadLetters, expected.SignalDeadLetters)
	check("SignalInlineRetrySuccesses", se.scanCounters.SignalInlineRetrySuccesses, expected.SignalInlineRetrySuccesses)
	check("LeaseRenewals", se.scanCounters.LeaseRenewals, expected.LeaseRenewals)
	check("LeaseExtensions", se.scanCounters.LeaseExtensions, expected.LeaseExtensions)
	check("LeaseReleases", se.scanCounters.LeaseReleases, expected.LeaseReleases)
}

// TestPhaseABC_CounterAccumulationAcrossPhases verifies counters accumulate
// correctly across the full Phase A→B→C cycle when Phase B adds increments.
func TestPhaseABC_CounterAccumulationAcrossPhases(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 1}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	exec := newRecordingExecutor(nil)
	qh.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return exec, nil
	})

	se := qh.scanExecutor

	// Set a known value before Phase A to verify reset
	se.scanCounters = metrics.ScanCounters{SignalInlineRetrySuccesses: 99}

	// Phase A resets counters
	pa := se.periodicScanPhaseA()
	if pa.counters.SignalInlineRetrySuccesses != 0 {
		t.Fatalf("Phase A should reset counters, got SignalInlineRetrySuccesses=%d",
			pa.counters.SignalInlineRetrySuccesses)
	}

	// Phase B adds increments
	_ = se.periodicScanPhaseB(context.Background(), pa)
	se.scanCounters.SignalInlineRetrySuccesses = 5

	// Phase C should preserve the Phase B value
	se.periodicScanPhaseC(pa, phaseBResult{})

	if se.scanCounters.SignalInlineRetrySuccesses < 5 {
		t.Errorf("After full A→B→C cycle, SignalInlineRetrySuccesses = %d, want >= 5",
			se.scanCounters.SignalInlineRetrySuccesses)
	}
}
