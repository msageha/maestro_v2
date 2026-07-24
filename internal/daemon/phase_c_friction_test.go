package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// newTestFrictionManager builds a PhaseCManager with both the FingerprintDB
// and the friction ImprovementStore wired, mirroring the newPhaseCManager
// C-5 block with friction enabled.
func newTestFrictionManager(maestroDir string) *PhaseCManager {
	return &PhaseCManager{
		FingerprintDB: learnings.NewFingerprintDB(100),
		ImprovementStore: learnings.NewImprovementStore(learnings.ImprovementStoreOptions{
			MaxEntries:         100,
			MinOccurrences:     2,
			VerifyMinSuccesses: 2,
			ExcludeTargets:     []string{"fitness", "daemon_logic", "circuit_breaker"},
		}),
		improvementInjectCount:    5,
		improvementExcludeTargets: []string{"fitness", "daemon_logic", "circuit_breaker"},
		maestroDir:                maestroDir,
	}
}

// failResult builds a failed worker TaskResult with the given summary.
func failResult(taskID, summary string) *model.TaskResult {
	return &model.TaskResult{
		TaskID: taskID, CommandID: "cmd-1",
		Status: model.StatusFailed, Summary: summary,
	}
}

// TestFrictionLoop_FullLifecycle drives the friction-driven improvement
// lifecycle end to end through the same daemon hooks production uses:
// recurring failures propose an idea, injecting the proven strategy applies
// it, measured retry successes verify it, and a post-verification recurrence
// auto-reopens it.
func TestFrictionLoop_FullLifecycle(t *testing.T) {
	dir := t.TempDir()
	rh, _ := newTestResultHandler(dir)
	m := newTestFrictionManager(dir)
	rh.SetPhaseCManager(m)

	failSummary := "build failed: undefined symbol foo in pkg/bar"
	fp, _ := learnings.ComputeErrorFingerprint(failSummary)

	// 1. friction ×2 → proposed (and surfaced in the Planner section).
	rh.recordFingerprintCapture(failResult("task-1", failSummary), "worker1", m)
	rh.recordFrictionSignal(failResult("task-1", failSummary), m)
	rh.recordFrictionSignal(failResult("task-2", failSummary), m)
	imp, ok := m.ImprovementStore.Query(fp)
	if !ok || imp.Status != model.ImprovementStatusProposed {
		t.Fatalf("after 2 frictions: status=%s ok=%v, want proposed", imp.Status, ok)
	}
	if section := m.ImprovementProposalSection(); !strings.Contains(section, "IMPROVEMENT PROPOSALS") {
		t.Fatalf("proposed idea must surface in the Planner section, got %q", section)
	}

	// 2. A retry completes → strategy adopted (FingerprintDB), but the idea
	//    is NOT applied yet: nothing was injected, so nothing is measurable.
	m.RecordRetryFingerprint("task-retry1", "task-1")
	rh.recordRepairOutcome(&model.TaskResult{
		TaskID: "task-retry1", CommandID: "cmd-1",
		Status: model.StatusCompleted, Summary: "added the missing foo symbol",
	}, m)
	if imp, _ := m.ImprovementStore.Query(fp); imp.Status != model.ImprovementStatusProposed {
		t.Fatalf("strategy adoption alone must not apply the idea, got %s", imp.Status)
	}

	// 3. The failure recurs; the NEXT retry gets the hint injected — that
	//    injection is the apply step and opens the measurement window.
	rh.recordFrictionSignal(failResult("task-3", failSummary), m)
	hint := m.RepairHintForRetry(&model.Task{ID: "task-retry2", OriginalTaskID: "task-1"})
	if hint == "" {
		t.Fatal("hint must be injectable once a repair succeeded")
	}
	if imp, _ := m.ImprovementStore.Query(fp); imp.Status != model.ImprovementStatusApplied {
		t.Fatalf("hint injection must apply the idea, got %s", imp.Status)
	}

	// 4. Measurement gate: two consecutive measured successes verify.
	m.RecordRetryFingerprint("task-retry2", "task-1")
	rh.recordRepairOutcome(&model.TaskResult{
		TaskID: "task-retry2", CommandID: "cmd-1",
		Status: model.StatusCompleted, Summary: "fixed",
	}, m)
	if imp, _ := m.ImprovementStore.Query(fp); imp.Status != model.ImprovementStatusApplied {
		t.Fatalf("one measured success must not verify (gate=2), got %s", imp.Status)
	}
	m.RepairHintForRetry(&model.Task{ID: "task-retry3", OriginalTaskID: "task-1"})
	m.RecordRetryFingerprint("task-retry3", "task-1")
	rh.recordRepairOutcome(&model.TaskResult{
		TaskID: "task-retry3", CommandID: "cmd-1",
		Status: model.StatusCompleted, Summary: "fixed again",
	}, m)
	if imp, _ := m.ImprovementStore.Query(fp); imp.Status != model.ImprovementStatusVerified {
		t.Fatalf("two measured successes must verify, got %s", imp.Status)
	}
	// Verified ideas leave the actionable pool.
	if section := m.ImprovementProposalSection(); section != "" {
		t.Fatalf("verified idea must not surface, got %q", section)
	}

	// 5. Regression: the same friction recurs → auto-reopen, back in pool.
	rh.recordFrictionSignal(failResult("task-9", failSummary), m)
	imp, _ = m.ImprovementStore.Query(fp)
	if imp.Status != model.ImprovementStatusReopened || imp.ReopenCount != 1 {
		t.Fatalf("regression must reopen: status=%s reopen_count=%d", imp.Status, imp.ReopenCount)
	}
	if section := m.ImprovementProposalSection(); !strings.Contains(section, "reopen=1回") {
		t.Fatalf("reopened idea must surface with reopen marker, got %q", section)
	}
}

// TestFrictionLoop_FailedMeasuredRetryResetsStreak pins the measure-gate
// semantics on the daemon path: a failed measured retry counts a recurrence
// and resets the streak, so verification requires a fresh streak.
func TestFrictionLoop_FailedMeasuredRetryResetsStreak(t *testing.T) {
	dir := t.TempDir()
	rh, _ := newTestResultHandler(dir)
	m := newTestFrictionManager(dir)
	rh.SetPhaseCManager(m)

	failSummary := "test timeout after 300s in integration suite"
	fp, _ := learnings.ComputeErrorFingerprint(failSummary)
	rh.recordFingerprintCapture(failResult("task-1", failSummary), "worker1", m)
	rh.recordFrictionSignal(failResult("task-1", failSummary), m)
	rh.recordFrictionSignal(failResult("task-2", failSummary), m)

	// Adopt a strategy, then inject → applied.
	m.RecordRetryFingerprint("retry-0", "task-1")
	rh.recordRepairOutcome(&model.TaskResult{
		TaskID: "retry-0", CommandID: "cmd-1",
		Status: model.StatusCompleted, Summary: "split the suite",
	}, m)
	m.RepairHintForRetry(&model.Task{ID: "retry-1", OriginalTaskID: "task-1"})

	// success → failure → success: never two consecutive → stays applied.
	m.RecordRetryFingerprint("retry-1", "task-1")
	rh.recordRepairOutcome(&model.TaskResult{TaskID: "retry-1", CommandID: "cmd-1", Status: model.StatusCompleted, Summary: "ok"}, m)
	m.RecordRetryFingerprint("retry-2", "task-1")
	rh.recordRepairOutcome(&model.TaskResult{TaskID: "retry-2", CommandID: "cmd-1", Status: model.StatusFailed, Summary: "still timing out"}, m)
	m.RecordRetryFingerprint("retry-3", "task-1")
	rh.recordRepairOutcome(&model.TaskResult{TaskID: "retry-3", CommandID: "cmd-1", Status: model.StatusCompleted, Summary: "ok"}, m)

	imp, _ := m.ImprovementStore.Query(fp)
	if imp.Status != model.ImprovementStatusApplied {
		t.Fatalf("broken streak must not verify, got %s", imp.Status)
	}
	if imp.Measure.PostApplyRecurrences == 0 {
		t.Fatal("failed measured retry must count a recurrence")
	}
}

// TestRecordFrictionSignal_ClassifiesSyntheticSummaries verifies the daemon
// capture path labels the operational frictions carried by synthetic result
// summaries (blocked prompt / terminal error / dead letter) — friction is a
// first-class trigger, not just plain task failure.
func TestRecordFrictionSignal_ClassifiesSyntheticSummaries(t *testing.T) {
	dir := t.TempDir()
	rh, _ := newTestResultHandler(dir)
	m := newTestFrictionManager(dir)
	rh.SetPhaseCManager(m)

	cases := []struct {
		summary  string
		status   model.Status
		wantKind string
	}{
		{"blocked_pane_timeout: worker worker1 pane has been wedged on a runtime confirmation/approval prompt for 3m0s", model.StatusFailed, learnings.FrictionKindBlockedPrompt},
		{"runtime_terminal_error: worker worker2 pane emitted a non-recoverable error frame", model.StatusFailed, learnings.FrictionKindTerminalError},
		{"dead-lettered: dispatch retry exhausted", model.StatusDeadLetter, learnings.FrictionKindDeadLetter},
	}
	for i, tc := range cases {
		r := &model.TaskResult{TaskID: "t", CommandID: "c", Status: tc.status, Summary: tc.summary}
		rh.recordFrictionSignal(r, m)
		fp, _ := learnings.ComputeErrorFingerprint(tc.summary)
		imp, ok := m.ImprovementStore.Query(fp)
		if !ok || imp.Kind != tc.wantKind {
			t.Fatalf("case %d: kind=%q ok=%v, want %q", i, imp.Kind, ok, tc.wantKind)
		}
	}

	// Non-terminal / successful results record nothing.
	before := m.ImprovementStore.Size()
	rh.recordFrictionSignal(&model.TaskResult{TaskID: "t", CommandID: "c", Status: model.StatusCompleted, Summary: "done"}, m)
	if m.ImprovementStore.Size() != before {
		t.Fatal("completed results must not record friction")
	}
}

// TestMetricsCountersSnapshot_BestEffort verifies the measurement baseline
// reads state/metrics.yaml when present and degrades to zero counters (never
// an error) when missing.
func TestMetricsCountersSnapshot_BestEffort(t *testing.T) {
	dir := t.TempDir()
	m := newTestFrictionManager(dir)

	if got := m.metricsCountersSnapshot(); got != (model.MetricsCounters{}) {
		t.Fatalf("missing metrics.yaml must yield zero counters, got %+v", got)
	}

	metrics := model.Metrics{
		SchemaVersion: 1,
		FileType:      "state_metrics",
		Counters:      model.MetricsCounters{TasksFailed: 4, DeadLetters: 2},
	}
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o750); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	if err := yamlutil.AtomicWrite(filepath.Join(dir, "state", "metrics.yaml"), metrics); err != nil {
		t.Fatalf("seed metrics.yaml: %v", err)
	}
	got := m.metricsCountersSnapshot()
	if got.TasksFailed != 4 || got.DeadLetters != 2 {
		t.Fatalf("counters = %+v, want tasks_failed=4 dead_letters=2", got)
	}
}

// TestNewPhaseCManager_FrictionGating pins the opt-in wiring: the store
// exists only when BOTH self_improvement.enabled and friction.enabled are
// set, and persists across SaveState → newPhaseCManager restarts.
func TestNewPhaseCManager_FrictionGating(t *testing.T) {
	noop := func(level LogLevel, format string, args ...any) {}

	t.Run("disabled by default", func(t *testing.T) {
		m := newPhaseCManager(model.Config{}, t.TempDir(), nil, noop)
		if m.ImprovementStore != nil {
			t.Fatal("friction loop must be opt-in")
		}
	})

	t.Run("self_improvement on, friction off", func(t *testing.T) {
		cfg := model.Config{SelfImprovement: model.SelfImprovementConfig{Enabled: ptr.Bool(true)}}
		m := newPhaseCManager(cfg, t.TempDir(), nil, noop)
		if m.FingerprintDB == nil {
			t.Fatal("fingerprint DB must still initialize")
		}
		if m.ImprovementStore != nil {
			t.Fatal("friction loop must stay off without friction.enabled")
		}
	})

	t.Run("both on, state persists across restart", func(t *testing.T) {
		dir := t.TempDir()
		cfg := model.Config{SelfImprovement: model.SelfImprovementConfig{
			Enabled:  ptr.Bool(true),
			Friction: model.FrictionConfig{Enabled: ptr.Bool(true), MinOccurrences: ptr.Int(1)},
		}}
		m := newPhaseCManager(cfg, dir, nil, noop)
		if m.ImprovementStore == nil {
			t.Fatal("friction loop must initialize when enabled")
		}
		m.ImprovementStore.RecordFriction("fp-persist", learnings.FrictionKindTaskFailure, "build", RealClock{}.Now())
		m.SaveState(noop)

		m2 := newPhaseCManager(cfg, dir, nil, noop)
		if m2.ImprovementStore == nil || m2.ImprovementStore.Size() != 1 {
			t.Fatalf("improvements.yaml must reload on restart, size=%d", m2.ImprovementStore.Size())
		}
		if imp, ok := m2.ImprovementStore.Query("fp-persist"); !ok || imp.Status != model.ImprovementStatusProposed {
			t.Fatalf("persisted entry lost lifecycle state: %+v ok=%v", imp, ok)
		}
	})
}
