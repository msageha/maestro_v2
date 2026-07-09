package daemon

import (
	"fmt"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/bandit"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

// newTestTaskBloomManager builds a PhaseCManager with the bandit enabled
// (non-nil BanditSelector) and the taskBloom bookkeeping initialized, the
// minimum state RecordTaskBloom / ConsumeTaskBloom require.
func newTestTaskBloomManager(t *testing.T) *PhaseCManager {
	t.Helper()
	sel, err := bandit.NewSelector(1.41)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	return &PhaseCManager{
		BanditSelector: sel,
		taskBloom:      make(map[string]taskBloomEntry),
	}
}

// TestTaskBloom_RecordAndConsume covers the happy path: a recorded level is
// returned exactly once, and the entry is released on consumption.
func TestTaskBloom_RecordAndConsume(t *testing.T) {
	m := newTestTaskBloomManager(t)

	m.RecordTaskBloom("task-1", 5)
	if got := m.ConsumeTaskBloom("task-1"); got != 5 {
		t.Errorf("ConsumeTaskBloom = %d, want 5", got)
	}
	if got := m.ConsumeTaskBloom("task-1"); got != 0 {
		t.Errorf("second ConsumeTaskBloom = %d, want 0 (entry must be released)", got)
	}
}

// TestTaskBloom_NilSafety mirrors the nil-receiver contract of the other
// PhaseCManager learning hooks (ObserveTaskOutcome etc.): both methods must
// tolerate a nil manager because recordBanditReward calls them through
// rh.getPhaseC() without a guard.
func TestTaskBloom_NilSafety(t *testing.T) {
	var m *PhaseCManager
	m.RecordTaskBloom("task-1", 3)
	if got := m.ConsumeTaskBloom("task-1"); got != 0 {
		t.Errorf("nil manager ConsumeTaskBloom = %d, want 0", got)
	}
}

// TestTaskBloom_BanditDisabledNoOp verifies that dispatch-time recording is
// skipped entirely when the bandit is not configured, so a daemon with the
// feature disabled accumulates no bookkeeping.
func TestTaskBloom_BanditDisabledNoOp(t *testing.T) {
	m := &PhaseCManager{taskBloom: make(map[string]taskBloomEntry)} // BanditSelector nil
	m.RecordTaskBloom("task-1", 3)
	if got := m.ConsumeTaskBloom("task-1"); got != 0 {
		t.Errorf("bandit-disabled ConsumeTaskBloom = %d, want 0", got)
	}
}

// TestTaskBloom_InvalidInputsIgnored covers empty task IDs and Bloom levels
// outside the bucketed range — recording them would only waste map entries
// since RecordResult routes such rewards to the global selector anyway.
func TestTaskBloom_InvalidInputsIgnored(t *testing.T) {
	m := newTestTaskBloomManager(t)

	m.RecordTaskBloom("", 3)
	m.RecordTaskBloom("task-0", 0)
	m.RecordTaskBloom("task-7", 7)

	if n := len(m.taskBloom); n != 0 {
		t.Errorf("taskBloom size = %d, want 0 (invalid inputs must not be recorded)", n)
	}
}

// TestTaskBloom_RerecordOverwrites covers re-dispatch of the same task ID:
// the latest Bloom level wins and the entry is still consumed exactly once.
func TestTaskBloom_RerecordOverwrites(t *testing.T) {
	m := newTestTaskBloomManager(t)

	m.RecordTaskBloom("task-1", 2)
	m.RecordTaskBloom("task-1", 4)
	if got := m.ConsumeTaskBloom("task-1"); got != 4 {
		t.Errorf("ConsumeTaskBloom = %d, want 4 (latest record wins)", got)
	}
}

// TestTaskBloom_CapEvictsOldestFIFO fills the map past taskBloomMaxEntries
// and verifies FIFO eviction: the oldest entry is dropped, newer entries
// survive.
func TestTaskBloom_CapEvictsOldestFIFO(t *testing.T) {
	m := newTestTaskBloomManager(t)

	for i := 0; i < taskBloomMaxEntries+1; i++ {
		m.RecordTaskBloom(fmt.Sprintf("task-%d", i), 3)
	}

	if got := m.ConsumeTaskBloom("task-0"); got != 0 {
		t.Errorf("oldest entry: ConsumeTaskBloom = %d, want 0 (must be evicted)", got)
	}
	if got := m.ConsumeTaskBloom("task-1"); got != 3 {
		t.Errorf("second-oldest entry: ConsumeTaskBloom = %d, want 3 (must survive)", got)
	}
	if got := m.ConsumeTaskBloom(fmt.Sprintf("task-%d", taskBloomMaxEntries)); got != 3 {
		t.Errorf("newest entry: ConsumeTaskBloom = %d, want 3", got)
	}
	if n := len(m.taskBloom); n > taskBloomMaxEntries {
		t.Errorf("taskBloom size = %d, want <= %d", n, taskBloomMaxEntries)
	}
}

// TestTaskBloom_StaleOrderEntryDoesNotEvictRerecordedEntry is a regression
// test for the lazy-deletion hazard: after consume → re-record of the same
// taskID, a stale order entry for the old generation sits at the front of
// the FIFO. Eviction must skip it (id+seq mismatch) instead of deleting the
// live re-recorded entry, and evict the oldest *live* entry instead.
func TestTaskBloom_StaleOrderEntryDoesNotEvictRerecordedEntry(t *testing.T) {
	m := newTestTaskBloomManager(t)

	// Generation 1 of task-A: recorded, then consumed → stale order entry
	// remains at the front of the FIFO.
	m.RecordTaskBloom("task-A", 2)
	if got := m.ConsumeTaskBloom("task-A"); got != 2 {
		t.Fatalf("ConsumeTaskBloom = %d, want 2", got)
	}

	// Fill to one below capacity, then re-record task-A (generation 2) as
	// the newest entry — the map is now exactly at capacity.
	for i := 0; i < taskBloomMaxEntries-1; i++ {
		m.RecordTaskBloom(fmt.Sprintf("task-%d", i), 3)
	}
	m.RecordTaskBloom("task-A", 4)

	// The next unique insert triggers eviction: the stale task-A entry is
	// popped first but must not delete generation 2; the oldest live entry
	// (task-0) is the one evicted.
	m.RecordTaskBloom("task-new", 5)

	if got := m.ConsumeTaskBloom("task-A"); got != 4 {
		t.Errorf("re-recorded entry: ConsumeTaskBloom = %d, want 4 (stale order entry must not evict the live generation)", got)
	}
	if got := m.ConsumeTaskBloom("task-0"); got != 0 {
		t.Errorf("oldest live entry: ConsumeTaskBloom = %d, want 0 (must be the entry actually evicted)", got)
	}
}

// TestRecordBanditReward_ConsumesAndRoutesBloom wires a ResultHandler with
// the contextual selector and verifies the C-2 reward path end to end:
// non-terminal statuses leave the dispatch record in place, a terminal
// status consumes it exactly once and routes the reward to the bucket
// matching the recorded Bloom level (plus the global selector).
func TestRecordBanditReward_ConsumesAndRoutesBloom(t *testing.T) {
	rh, _ := newTestResultHandler(t.TempDir())
	m := newTestTaskBloomManager(t)
	for _, arm := range []string{"sonnet", "opus"} {
		m.BanditSelector.AddArm(arm)
	}
	rh.SetPhaseCManager(m)

	cfg := model.BanditConfig{
		Enabled:          ptr.Bool(true),
		ExplorationCoeff: ptr.Float64(1.41),
	}
	sel := newBanditModelSelector(m.BanditSelector, cfg, nil)
	rh.SetModelSelector(sel)

	m.RecordTaskBloom("task-1", 5)

	// Non-terminal status: no reward, dispatch record stays.
	rh.recordBanditReward(&model.TaskResult{TaskID: "task-1", Status: model.StatusInProgress}, "worker1")
	if _, ok := m.taskBloom["task-1"]; !ok {
		t.Fatal("non-terminal status must not consume the dispatch record")
	}

	// Terminal status: record consumed, reward routed to the high bucket
	// and the global selector. workerModelName falls back to "sonnet" for
	// the empty test config.
	rh.recordBanditReward(&model.TaskResult{TaskID: "task-1", Status: model.StatusCompleted}, "worker1")
	if _, ok := m.taskBloom["task-1"]; ok {
		t.Error("terminal status must consume the dispatch record")
	}
	if got := m.BanditSelector.GetStats()["sonnet"].PullCount; got != 1 {
		t.Errorf("global sonnet PullCount = %d, want 1", got)
	}
	if got := sel.buckets[bloomBucketHigh].GetStats()["sonnet"].PullCount; got != 1 {
		t.Errorf("high bucket sonnet PullCount = %d, want 1", got)
	}
}

// TestRecordBanditReward_FamilyNormalization pins the E-4 fix: arms are keyed
// by model family, and a reward whose worker resolves to a full Claude model
// ID (or a boost-mode alias) must land on the family arm instead of being
// silently dropped on the arm-name mismatch.
func TestRecordBanditReward_FamilyNormalization(t *testing.T) {
	rh, _ := newTestResultHandler(t.TempDir())
	m := newTestTaskBloomManager(t)
	// Production registration path: family-normalised arms.
	for _, name := range []string{"claude-opus-4-7", "sonnet"} {
		m.BanditSelector.AddArm(model.ModelFamily(name))
	}
	rh.SetPhaseCManager(m)
	// Worker configured with the full model ID spelling.
	rh.config.Agents.Workers.Models = map[string]string{"worker1": "claude-opus-4-7"}

	cfg := model.BanditConfig{
		Enabled:          ptr.Bool(true),
		ExplorationCoeff: ptr.Float64(1.41),
	}
	sel := newBanditModelSelector(m.BanditSelector, cfg, nil)
	rh.SetModelSelector(sel)

	m.RecordTaskBloom("task-1", 5)
	rh.recordBanditReward(&model.TaskResult{TaskID: "task-1", Status: model.StatusCompleted}, "worker1")
	if got := m.BanditSelector.GetStats()["opus"].PullCount; got != 1 {
		t.Errorf("family arm opus PullCount = %d, want 1 (full-ID reward must normalise to the family arm)", got)
	}
	if got := m.BanditSelector.GetStats()["claude-opus-4-7"].PullCount; got != 0 {
		t.Errorf("raw-ID arm must not exist/receive rewards, got PullCount=%d", got)
	}
}

// TestTaskBloom_OrderCompaction drives steady record→consume traffic (the
// map stays tiny while the order slice accumulates consumed IDs) and checks
// that compaction keeps the order slice bounded instead of growing without
// limit.
func TestTaskBloom_OrderCompaction(t *testing.T) {
	m := newTestTaskBloomManager(t)

	for i := 0; i < taskBloomMaxEntries*3; i++ {
		id := fmt.Sprintf("task-%d", i)
		m.RecordTaskBloom(id, 3)
		if got := m.ConsumeTaskBloom(id); got != 3 {
			t.Fatalf("ConsumeTaskBloom(%s) = %d, want 3", id, got)
		}
	}

	if n := len(m.taskBloomOrder); n > taskBloomMaxEntries*2 {
		t.Errorf("taskBloomOrder length = %d, want <= %d (compaction must bound it)", n, taskBloomMaxEntries*2)
	}
}
