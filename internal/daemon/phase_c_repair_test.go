package daemon

import (
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/model"
)

// newTestRepairManager builds a PhaseCManager with a FingerprintDB, the
// minimum state the C-5 repair-loop bookkeeping requires.
func newTestRepairManager() *PhaseCManager {
	return &PhaseCManager{
		FingerprintDB: learnings.NewFingerprintDB(100),
	}
}

// TestRepairLoop_FailureRetrySuccess_ClosesLoop drives the full C-5 cycle:
// a task fails (fingerprint stored + attributed), its retry is dispatched
// (linked via OriginalTaskID), the retry completes (pattern credited and the
// retry summary adopted as strategy), and a SECOND retry of the same failure
// class receives the injectable repair hint.
func TestRepairLoop_FailureRetrySuccess_ClosesLoop(t *testing.T) {
	rh, _ := newTestResultHandler(t.TempDir())
	m := newTestRepairManager()
	rh.SetPhaseCManager(m)

	failSummary := "build failed: undefined symbol foo in pkg/bar"

	// 1. Original task fails → fingerprint stored + failure attributed.
	rh.recordFingerprintCapture(&model.TaskResult{
		TaskID: "task-orig", CommandID: "cmd-1",
		Status: model.StatusFailed, Summary: failSummary,
	}, "worker1", m)
	fp, _ := learnings.ComputeErrorFingerprint(failSummary)
	if fp == "" {
		t.Fatal("fingerprint must be computable from the failure summary")
	}
	if _, ok := m.FingerprintDB.Query(fp); !ok {
		t.Fatal("failure pattern must be stored")
	}

	// No hint yet: the pattern has no successful repair on record.
	retry1 := &model.Task{ID: "task-retry1", OriginalTaskID: "task-orig"}
	if hint := m.RepairHintForRetry(retry1); hint != "" {
		t.Fatalf("hint before any successful repair must be empty, got %q", hint)
	}

	// 2. Retry dispatched → linked to the predecessor's fingerprint.
	m.RecordRetryFingerprint(retry1.ID, retry1.OriginalTaskID)

	// 3. Retry completes → pattern credited, summary adopted as strategy.
	rh.recordRepairOutcome(&model.TaskResult{
		TaskID: retry1.ID, CommandID: "cmd-1",
		Status:  model.StatusCompleted,
		Summary: "added the missing foo symbol to pkg/bar and re-ran the build",
	}, m)

	pattern, ok := m.FingerprintDB.Query(fp)
	if !ok {
		t.Fatal("pattern must survive the repair credit")
	}
	if pattern.SuccessCount != 1 || pattern.SuccessRate <= 0 {
		t.Fatalf("pattern success stats = (%d, %v), want credited", pattern.SuccessCount, pattern.SuccessRate)
	}
	if pattern.RepairStrategy == "" {
		t.Fatal("retry summary must be adopted as the repair strategy")
	}

	// 4. A second retry of the same failure now gets the injectable hint.
	//    (Same original task: the failure attribution is peek-only.)
	retry2 := &model.Task{ID: "task-retry2", OriginalTaskID: "task-orig"}
	hint := m.RepairHintForRetry(retry2)
	if hint == "" {
		t.Fatal("hint after a successful repair must be non-empty")
	}
	if !strings.Contains(hint, "BEGIN REPAIR STRATEGY") || !strings.Contains(hint, "missing foo symbol") {
		t.Fatalf("hint must carry the DATA framing and the strategy text, got %q", hint)
	}
}

// TestRepairLoop_FailedRetryReleasesAttributionWithoutCredit pins the
// terminal-consumption contract: a FAILED retry releases its attribution
// entry but must not credit the pattern.
func TestRepairLoop_FailedRetryReleasesAttributionWithoutCredit(t *testing.T) {
	rh, _ := newTestResultHandler(t.TempDir())
	m := newTestRepairManager()
	rh.SetPhaseCManager(m)

	failSummary := "test timeout after 300s in integration suite"
	rh.recordFingerprintCapture(&model.TaskResult{
		TaskID: "task-orig", CommandID: "cmd-1",
		Status: model.StatusFailed, Summary: failSummary,
	}, "worker1", m)
	fp, _ := learnings.ComputeErrorFingerprint(failSummary)

	m.RecordRetryFingerprint("task-retry1", "task-orig")
	rh.recordRepairOutcome(&model.TaskResult{
		TaskID: "task-retry1", CommandID: "cmd-1",
		Status: model.StatusFailed, Summary: "still timing out",
	}, m)

	pattern, ok := m.FingerprintDB.Query(fp)
	if !ok {
		t.Fatal("pattern must exist")
	}
	if pattern.SuccessCount != 0 {
		t.Fatalf("failed retry must not credit the pattern, SuccessCount=%d", pattern.SuccessCount)
	}
	// Attribution was consumed: a later spurious "completed" for the same
	// task ID must be a no-op.
	rh.recordRepairOutcome(&model.TaskResult{
		TaskID: "task-retry1", CommandID: "cmd-1",
		Status: model.StatusCompleted, Summary: "late duplicate",
	}, m)
	if pattern, _ := m.FingerprintDB.Query(fp); pattern.SuccessCount != 0 {
		t.Fatal("consumed attribution must not credit on a duplicate result")
	}
}

// TestRepairLoop_NilSafety covers nil manager / disabled FingerprintDB paths.
func TestRepairLoop_NilSafety(t *testing.T) {
	var nilM *PhaseCManager
	nilM.RecordTaskFailureFingerprint("t", "fp")
	nilM.RecordRetryFingerprint("t", "o")
	if got := nilM.ConsumeRetryFingerprint("t"); got != "" {
		t.Errorf("nil manager consume = %q, want empty", got)
	}
	if got := nilM.RepairHintForRetry(&model.Task{OriginalTaskID: "o"}); got != "" {
		t.Errorf("nil manager hint = %q, want empty", got)
	}

	noDB := &PhaseCManager{}
	noDB.RecordTaskFailureFingerprint("t", "fp")
	noDB.RecordRetryFingerprint("t", "o")
	if got := noDB.RepairHintForRetry(&model.Task{OriginalTaskID: "o"}); got != "" {
		t.Errorf("disabled FingerprintDB hint = %q, want empty", got)
	}
}

// TestBoundedFPMap_FIFOEviction pins the attribution table's bound.
func TestBoundedFPMap_FIFOEviction(t *testing.T) {
	b := newBoundedFPMap(2)
	b.set("a", "1")
	b.set("b", "2")
	b.set("c", "3") // evicts "a"
	if got := b.get("a"); got != "" {
		t.Errorf("evicted key returned %q, want empty", got)
	}
	if b.get("b") != "2" || b.get("c") != "3" {
		t.Error("surviving keys must retain their values")
	}
	if got := b.consume("b"); got != "2" {
		t.Errorf("consume = %q, want 2", got)
	}
	if got := b.consume("b"); got != "" {
		t.Errorf("second consume = %q, want empty", got)
	}
}
