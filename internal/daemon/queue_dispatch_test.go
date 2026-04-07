package daemon

import (
	"bytes"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// newDispatchTestQH creates a minimal QueueHandler for unit testing.
func newDispatchTestQH(scanIntervalSec int) *QueueHandler {
	cfg := model.Config{
		Watcher: model.WatcherConfig{
			ScanIntervalSec: scanIntervalSec,
		},
	}
	return NewQueueHandler("", cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
}

// --- computeSignalBackoff ---

func TestComputeSignalBackoff_FirstAttempt(t *testing.T) {
	qh := newDispatchTestQH(60)
	d := qh.computeSignalBackoff(1)
	// base=5, 1<<0=1, backoff=5s, jitter [0.75, 1.25] → [3.75s, 6.25s]
	if d < 3750*time.Millisecond || d > 6250*time.Millisecond {
		t.Errorf("attempt=1: got %v, want [3.75s, 6.25s]", d)
	}
}

func TestComputeSignalBackoff_SecondAttempt(t *testing.T) {
	qh := newDispatchTestQH(60)
	d := qh.computeSignalBackoff(2)
	// base=5, 1<<1=2, backoff=10s, jitter → [7.5s, 12.5s]
	if d < 7500*time.Millisecond || d > 12500*time.Millisecond {
		t.Errorf("attempt=2: got %v, want [7.5s, 12.5s]", d)
	}
}

func TestComputeSignalBackoff_ClampsToMax(t *testing.T) {
	qh := newDispatchTestQH(10) // maxSec=10
	d := qh.computeSignalBackoff(10)
	// backoff = 5*(1<<9) = 2560 → clamped to 10s, jitter → [7.5s, 12.5s]
	if d < 7500*time.Millisecond || d > 12500*time.Millisecond {
		t.Errorf("attempt=10: got %v, want clamped [7.5s, 12.5s]", d)
	}
}

func TestComputeSignalBackoff_ZeroAttempt(t *testing.T) {
	qh := newDispatchTestQH(60)
	d := qh.computeSignalBackoff(0)
	// attempts < 1 → treated as 1 → backoff=5s
	if d < 3750*time.Millisecond || d > 6250*time.Millisecond {
		t.Errorf("attempt=0: got %v, want [3.75s, 6.25s]", d)
	}
}

// TestUpsertPlannerSignal_CommitFailedDedupPerWorker verifies that commit_failed
// signals from different workers in the same phase are NOT collapsed by the
// dedup index — each worker must retain its own entry. Other kinds remain
// phase-level deduped (backwards compatible).
func TestUpsertPlannerSignal_CommitFailedDedupPerWorker(t *testing.T) {
	qh := newDispatchTestQH(60)
	sq := &model.PlannerSignalQueue{}
	dirty := false
	idx := buildSignalIndex(sq.Signals)

	mk := func(worker string) model.PlannerSignal {
		return model.PlannerSignal{
			Kind:      "commit_failed",
			CommandID: "cmd1",
			PhaseID:   "phase1",
			WorkerID:  worker,
			Message:   "err for " + worker,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		}
	}

	qh.upsertPlannerSignal(sq, &dirty, mk("worker1"), idx)
	qh.upsertPlannerSignal(sq, &dirty, mk("worker2"), idx)
	qh.upsertPlannerSignal(sq, &dirty, mk("worker3"), idx)
	// Duplicate of worker2 → should be skipped.
	qh.upsertPlannerSignal(sq, &dirty, mk("worker2"), idx)

	if len(sq.Signals) != 3 {
		t.Fatalf("expected 3 distinct commit_failed signals, got %d: %+v", len(sq.Signals), sq.Signals)
	}
	seen := map[string]bool{}
	for _, s := range sq.Signals {
		if s.Kind != "commit_failed" || s.CommandID != "cmd1" || s.PhaseID != "phase1" {
			t.Errorf("unexpected signal fields: %+v", s)
		}
		seen[s.WorkerID] = true
	}
	for _, w := range []string{"worker1", "worker2", "worker3"} {
		if !seen[w] {
			t.Errorf("missing signal for %s", w)
		}
	}

	// All worker-scoped kinds (e.g. merge_conflict) now dedup by worker_id too:
	// distinct workers in the same phase each get a separate entry, but a
	// repeat for the same worker is still deduped.
	mc := func(worker string) model.PlannerSignal {
		return model.PlannerSignal{
			Kind:      "merge_conflict",
			CommandID: "cmd1",
			PhaseID:   "phase1",
			WorkerID:  worker,
			CreatedAt: "2026-01-01T00:00:00Z",
			UpdatedAt: "2026-01-01T00:00:00Z",
		}
	}
	before := len(sq.Signals)
	qh.upsertPlannerSignal(sq, &dirty, mc("worker1"), idx)
	qh.upsertPlannerSignal(sq, &dirty, mc("worker2"), idx) // distinct worker → distinct entry
	qh.upsertPlannerSignal(sq, &dirty, mc("worker2"), idx) // same key → deduped
	if got := len(sq.Signals) - before; got != 2 {
		t.Errorf("merge_conflict added %d entries, want 2 (worker-scoped dedup)", got)
	}

	// A phase-level signal (no worker_id) is still deduped exactly once across
	// repeated upserts within the same (cmd, phase, kind).
	pl := model.PlannerSignal{
		Kind:      "awaiting_fill",
		CommandID: "cmd1",
		PhaseID:   "phase1",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	before = len(sq.Signals)
	qh.upsertPlannerSignal(sq, &dirty, pl, idx)
	qh.upsertPlannerSignal(sq, &dirty, pl, idx) // dedup
	if got := len(sq.Signals) - before; got != 1 {
		t.Errorf("phase-level signal added %d entries, want 1", got)
	}
}

func TestComputeSignalBackoff_NegativeAttempt(t *testing.T) {
	qh := newDispatchTestQH(60)
	d := qh.computeSignalBackoff(-5)
	// attempts < 1 → treated as 1 → backoff=5s
	if d < 3750*time.Millisecond || d > 6250*time.Millisecond {
		t.Errorf("attempt=-5: got %v, want [3.75s, 6.25s]", d)
	}
}

func TestComputeSignalBackoff_DefaultMaxSec(t *testing.T) {
	qh := newDispatchTestQH(0) // scanIntervalSec=0 → default maxSec=10
	d := qh.computeSignalBackoff(5)
	// backoff = 5*(1<<4)=80 → clamped to 10s, jitter → [7.5s, 12.5s]
	if d < 7500*time.Millisecond || d > 12500*time.Millisecond {
		t.Errorf("default max: got %v, want [7.5s, 12.5s]", d)
	}
}

func TestComputeSignalBackoff_ExponentialGrowth(t *testing.T) {
	qh := newDispatchTestQH(600) // high max to allow growth
	// Verify backoff grows: attempt 1 (5s) < attempt 2 (10s) < attempt 3 (20s)
	// Use many samples to reduce jitter variance
	samples := 100
	var sum1, sum2, sum3 time.Duration
	for range samples {
		sum1 += qh.computeSignalBackoff(1)
		sum2 += qh.computeSignalBackoff(2)
		sum3 += qh.computeSignalBackoff(3)
	}
	avg1 := sum1 / time.Duration(samples)
	avg2 := sum2 / time.Duration(samples)
	avg3 := sum3 / time.Duration(samples)

	if avg1 >= avg2 {
		t.Errorf("backoff should grow: avg1=%v >= avg2=%v", avg1, avg2)
	}
	if avg2 >= avg3 {
		t.Errorf("backoff should grow: avg2=%v >= avg3=%v", avg2, avg3)
	}
}

// --- upsertPlannerSignal ---

func TestUpsertPlannerSignal_Insert(t *testing.T) {
	qh := newDispatchTestQH(10)
	sq := &model.PlannerSignalQueue{}
	dirty := false

	sig := model.PlannerSignal{
		Kind:      "awaiting_fill",
		CommandID: "cmd1",
		PhaseID:   "phase1",
		Message:   "test message",
	}
	qh.upsertPlannerSignal(sq, &dirty, sig, buildSignalIndex(sq.Signals))

	if !dirty {
		t.Error("dirty should be true after insert")
	}
	if len(sq.Signals) != 1 {
		t.Fatalf("expected 1 signal, got %d", len(sq.Signals))
	}
	if sq.Signals[0].Kind != "awaiting_fill" {
		t.Errorf("kind: got %s, want awaiting_fill", sq.Signals[0].Kind)
	}
	if sq.SchemaVersion != 1 {
		t.Errorf("schema_version: got %d, want 1", sq.SchemaVersion)
	}
	if sq.FileType != "planner_signal_queue" {
		t.Errorf("file_type: got %s, want planner_signal_queue", sq.FileType)
	}
}

func TestUpsertPlannerSignal_Dedup(t *testing.T) {
	qh := newDispatchTestQH(10)
	sq := &model.PlannerSignalQueue{}
	dirty := false

	sig := model.PlannerSignal{
		Kind:      "awaiting_fill",
		CommandID: "cmd1",
		PhaseID:   "phase1",
		Message:   "first",
	}
	qh.upsertPlannerSignal(sq, &dirty, sig, buildSignalIndex(sq.Signals))

	// Reset dirty to verify dedup doesn't set it
	dirty = false
	sig2 := model.PlannerSignal{
		Kind:      "awaiting_fill",
		CommandID: "cmd1",
		PhaseID:   "phase1",
		Message:   "second (should be deduped)",
	}
	qh.upsertPlannerSignal(sq, &dirty, sig2, buildSignalIndex(sq.Signals))

	if dirty {
		t.Error("dirty should remain false on dedup")
	}
	if len(sq.Signals) != 1 {
		t.Fatalf("expected 1 signal (deduped), got %d", len(sq.Signals))
	}
	if sq.Signals[0].Message != "first" {
		t.Errorf("message should be unchanged: got %s, want first", sq.Signals[0].Message)
	}
}

func TestUpsertPlannerSignal_DifferentKind(t *testing.T) {
	qh := newDispatchTestQH(10)
	sq := &model.PlannerSignalQueue{}
	dirty := false

	qh.upsertPlannerSignal(sq, &dirty, model.PlannerSignal{
		Kind: "awaiting_fill", CommandID: "cmd1", PhaseID: "phase1",
	}, buildSignalIndex(sq.Signals))
	dirty = false
	qh.upsertPlannerSignal(sq, &dirty, model.PlannerSignal{
		Kind: "fill_timeout", CommandID: "cmd1", PhaseID: "phase1",
	}, buildSignalIndex(sq.Signals))

	if !dirty {
		t.Error("dirty should be true for different kind")
	}
	if len(sq.Signals) != 2 {
		t.Fatalf("expected 2 signals (different kind), got %d", len(sq.Signals))
	}
}

func TestUpsertPlannerSignal_DifferentCommand(t *testing.T) {
	qh := newDispatchTestQH(10)
	sq := &model.PlannerSignalQueue{}
	dirty := false

	qh.upsertPlannerSignal(sq, &dirty, model.PlannerSignal{
		Kind: "awaiting_fill", CommandID: "cmd1", PhaseID: "phase1",
	}, buildSignalIndex(sq.Signals))
	dirty = false
	qh.upsertPlannerSignal(sq, &dirty, model.PlannerSignal{
		Kind: "awaiting_fill", CommandID: "cmd2", PhaseID: "phase1",
	}, buildSignalIndex(sq.Signals))

	if !dirty {
		t.Error("dirty should be true for different command")
	}
	if len(sq.Signals) != 2 {
		t.Fatalf("expected 2 signals (different command), got %d", len(sq.Signals))
	}
}

func TestUpsertPlannerSignal_DifferentPhase(t *testing.T) {
	qh := newDispatchTestQH(10)
	sq := &model.PlannerSignalQueue{}
	dirty := false

	qh.upsertPlannerSignal(sq, &dirty, model.PlannerSignal{
		Kind: "awaiting_fill", CommandID: "cmd1", PhaseID: "phase1",
	}, buildSignalIndex(sq.Signals))
	dirty = false
	qh.upsertPlannerSignal(sq, &dirty, model.PlannerSignal{
		Kind: "awaiting_fill", CommandID: "cmd1", PhaseID: "phase2",
	}, buildSignalIndex(sq.Signals))

	if !dirty {
		t.Error("dirty should be true for different phase")
	}
	if len(sq.Signals) != 2 {
		t.Fatalf("expected 2 signals (different phase), got %d", len(sq.Signals))
	}
}

func TestUpsertPlannerSignal_SchemaVersionPreserved(t *testing.T) {
	qh := newDispatchTestQH(10)
	sq := &model.PlannerSignalQueue{
		SchemaVersion: 2,
		FileType:      "custom_type",
	}
	dirty := false

	qh.upsertPlannerSignal(sq, &dirty, model.PlannerSignal{
		Kind: "test", CommandID: "cmd1",
	}, buildSignalIndex(sq.Signals))

	// Existing schema_version > 0 should not be overwritten
	if sq.SchemaVersion != 2 {
		t.Errorf("schema_version: got %d, want 2 (preserved)", sq.SchemaVersion)
	}
	if sq.FileType != "custom_type" {
		t.Errorf("file_type: got %s, want custom_type (preserved)", sq.FileType)
	}
}

// --- isAgentBusy with busyChecker ---

func TestIsAgentBusy_WithChecker(t *testing.T) {
	qh := newDispatchTestQH(10)

	// Set a custom busy checker that returns busy for "worker1"
	qh.busyChecker = BusyCheckerFunc(func(agentID string) bool {
		return agentID == "worker1"
	})

	busy1, _ := qh.isAgentBusy(nil, "worker1")
	if !busy1 {
		t.Error("worker1 should be busy")
	}
	busy2, _ := qh.isAgentBusy(nil, "worker2")
	if busy2 {
		t.Error("worker2 should not be busy")
	}
}
