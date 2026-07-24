package hud

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "hud_history.jsonl")
}

func mustAppend(t *testing.T, h *History, rec Record) bool {
	t.Helper()
	ok, err := h.Append(rec)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return ok
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

func TestHistory_AppendDedupsUnchangedGauges(t *testing.T) {
	path := historyPath(t)
	h, err := LoadHistory(path, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}

	g := Gauges{TasksCompleted: 5}
	if !mustAppend(t, h, Record{TS: "2026-07-24T12:00:00Z", Gauges: g}) {
		t.Fatal("first append must persist")
	}
	if mustAppend(t, h, Record{TS: "2026-07-24T12:00:02Z", Gauges: g}) {
		t.Fatal("unchanged gauges must be deduplicated")
	}
	g.TasksCompleted = 6
	if !mustAppend(t, h, Record{TS: "2026-07-24T12:00:04Z", Gauges: g}) {
		t.Fatal("changed gauges must persist")
	}
	if n := countLines(t, path); n != 2 {
		t.Errorf("file lines = %d, want 2", n)
	}
	if h.Len() != 2 {
		t.Errorf("in-memory records = %d, want 2", h.Len())
	}
}

func TestHistory_RoundTripAndBaseline(t *testing.T) {
	path := historyPath(t)
	now := fixtureTime

	h, err := LoadHistory(path, now)
	if err != nil {
		t.Fatal(err)
	}
	old := Record{TS: now.Add(-25 * time.Hour).Format(time.RFC3339), Gauges: Gauges{TasksCompleted: 10}}
	mid := Record{TS: now.Add(-2 * time.Hour).Format(time.RFC3339), Gauges: Gauges{TasksCompleted: 20}}
	fresh := Record{TS: now.Add(-time.Minute).Format(time.RFC3339), Gauges: Gauges{TasksCompleted: 30}}
	for _, r := range []Record{old, mid, fresh} {
		mustAppend(t, h, r)
	}

	// Reload from disk and verify diff anchors.
	h2, err := LoadHistory(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if h2.Len() != 3 {
		t.Fatalf("reloaded records = %d, want 3", h2.Len())
	}
	if got := h2.Latest(); got == nil || got.TasksCompleted != 30 {
		t.Errorf("latest = %+v", got)
	}
	if got := h2.Previous(); got == nil || got.TasksCompleted != 20 {
		t.Errorf("previous = %+v", got)
	}
	if got := h2.Baseline(now, DailyBaselineAge); got == nil || got.TasksCompleted != 10 {
		t.Errorf("24h baseline = %+v, want the 25h-old record", got)
	}
}

func TestHistory_BaselineFallsBackToOldest(t *testing.T) {
	h := &History{}
	mustAppend(t, h, Record{TS: fixtureTime.Add(-time.Hour).Format(time.RFC3339), Gauges: Gauges{TasksCompleted: 1}})
	mustAppend(t, h, Record{TS: fixtureTime.Add(-time.Minute).Format(time.RFC3339), Gauges: Gauges{TasksCompleted: 2}})

	got := h.Baseline(fixtureTime, DailyBaselineAge)
	if got == nil || got.TasksCompleted != 1 {
		t.Errorf("baseline = %+v, want oldest record fallback", got)
	}
}

func TestHistory_LoadPrunesOldAndCorruptLines(t *testing.T) {
	path := historyPath(t)
	now := fixtureTime

	stale, _ := json.Marshal(Record{TS: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)})
	valid, _ := json.Marshal(Record{TS: now.Add(-time.Hour).Format(time.RFC3339), Gauges: Gauges{TasksCompleted: 7}})
	content := string(stale) + "\n{corrupt json\n" + string(valid) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := LoadHistory(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if h.Len() != 1 {
		t.Fatalf("records = %d, want 1 (stale + corrupt pruned)", h.Len())
	}
	// Loading alone must not rewrite the file (--once read-only guarantee).
	if n := countLines(t, path); n != 3 {
		t.Errorf("file lines after load = %d, want 3 (untouched)", n)
	}

	// First append compacts the file: stale + corrupt lines disappear.
	mustAppend(t, h, Record{TS: now.Format(time.RFC3339), Gauges: Gauges{TasksCompleted: 8}})
	if n := countLines(t, path); n != 2 {
		t.Errorf("file lines after compacting append = %d, want 2", n)
	}
	h3, err := LoadHistory(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if h3.Len() != 2 {
		t.Errorf("reloaded records = %d, want 2", h3.Len())
	}
}

func TestHistory_InMemoryOnlyWhenPathEmpty(t *testing.T) {
	h, err := LoadHistory("", fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, h, Record{TS: fixtureTime.Format(time.RFC3339), Gauges: Gauges{TasksCompleted: 1}})
	if h.Len() != 1 {
		t.Errorf("in-memory records = %d, want 1", h.Len())
	}
}

func TestSnapshotGauges_ProjectsUsageAndQueues(t *testing.T) {
	dir := newFixtureMaestroDir(t)
	s := Collect(dir, fixtureTime)
	g := SnapshotGauges(s)

	if g.TasksCompleted != 9 || g.TasksFailed != 1 {
		t.Errorf("counters = %+v", g)
	}
	// worker1 pending=1 in_progress=1, planner in_progress=1.
	if g.QueuePending != 1 || g.QueueInProgress != 2 {
		t.Errorf("queue gauges = pending %d / in_progress %d, want 1/2", g.QueuePending, g.QueueInProgress)
	}
	// Only tokens_known agents contribute (worker2/codex is excluded).
	if g.InputTokens != 1200000 || g.OutputTokens != 34000 {
		t.Errorf("token gauges = %d/%d", g.InputTokens, g.OutputTokens)
	}
	if g.EstimatedCostUSD != 4.21 {
		t.Errorf("cost = %v, want 4.21", g.EstimatedCostUSD)
	}
	if g.LearningsTotal != 2 || g.SkillCandidates != 2 {
		t.Errorf("self-improvement gauges = %d/%d", g.LearningsTotal, g.SkillCandidates)
	}
}
