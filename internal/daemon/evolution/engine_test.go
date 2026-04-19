package evolution

import (
	"testing"
)

func TestPlanMutations_AllStrategies(t *testing.T) {
	t.Parallel()
	e := NewEngine([]Strategy{StrategyDiff, StrategyFull, StrategyCross}, nil)
	slots := e.PlanMutations(2)

	// diff:full:cross = 2:1:1 = 4 slots total
	if len(slots) != 4 {
		t.Fatalf("expected 4 slots, got %d", len(slots))
	}

	counts := make(map[Strategy]int)
	for _, s := range slots {
		counts[s.Strategy]++
	}
	if counts[StrategyDiff] != 2 {
		t.Fatalf("expected 2 diff slots, got %d", counts[StrategyDiff])
	}
	if counts[StrategyFull] != 1 {
		t.Fatalf("expected 1 full slot, got %d", counts[StrategyFull])
	}
	if counts[StrategyCross] != 1 {
		t.Fatalf("expected 1 cross slot, got %d", counts[StrategyCross])
	}
}

func TestPlanMutations_SingleParent_NoCross(t *testing.T) {
	t.Parallel()
	e := NewEngine([]Strategy{StrategyDiff, StrategyFull, StrategyCross}, nil)
	slots := e.PlanMutations(1)

	// parentCount=1 → cross excluded → diff:full = 2:1 = 3 slots
	if len(slots) != 3 {
		t.Fatalf("expected 3 slots (no cross), got %d", len(slots))
	}

	for _, s := range slots {
		if s.Strategy == StrategyCross {
			t.Fatal("cross strategy should not appear with parentCount=1")
		}
	}
}

func TestPlanMutations_OnlyDiff(t *testing.T) {
	t.Parallel()
	e := NewEngine([]Strategy{StrategyDiff}, nil)
	slots := e.PlanMutations(3)

	if len(slots) != 2 {
		t.Fatalf("expected 2 diff slots, got %d", len(slots))
	}
	for _, s := range slots {
		if s.Strategy != StrategyDiff {
			t.Fatalf("expected only diff, got %q", s.Strategy)
		}
	}
}

func TestPlanMutations_IndicesSequential(t *testing.T) {
	t.Parallel()
	e := NewEngine([]Strategy{StrategyDiff, StrategyFull, StrategyCross}, nil)
	slots := e.PlanMutations(2)

	for i, s := range slots {
		if s.Index != i {
			t.Fatalf("slot %d has index %d, expected %d", i, s.Index, i)
		}
	}
}

func TestCheckNovelty_NewHash(t *testing.T) {
	t.Parallel()
	e := NewEngine(nil, nil)
	existing := []string{
		HashContent("hello"),
		HashContent("world"),
	}

	if !e.CheckNovelty(HashContent("unique"), existing) {
		t.Fatal("expected novel for new hash")
	}
}

func TestCheckNovelty_ExistingHash(t *testing.T) {
	t.Parallel()
	e := NewEngine(nil, nil)
	h := HashContent("hello")
	existing := []string{h, HashContent("world")}

	if e.CheckNovelty(h, existing) {
		t.Fatal("expected not novel for existing hash")
	}
}

func TestCheckNovelty_EmptyExisting(t *testing.T) {
	t.Parallel()
	e := NewEngine(nil, nil)
	if !e.CheckNovelty(HashContent("anything"), nil) {
		t.Fatal("expected novel when no existing hashes")
	}
}

func TestHashContent_Deterministic(t *testing.T) {
	t.Parallel()
	h1 := HashContent("test content")
	h2 := HashContent("test content")
	if h1 != h2 {
		t.Fatal("expected same hash for same content")
	}

	h3 := HashContent("different content")
	if h1 == h3 {
		t.Fatal("expected different hash for different content")
	}
}

func TestSelectSurvivors_TopN(t *testing.T) {
	t.Parallel()
	e := NewEngine(nil, nil)

	results := []SlotResult{
		{Index: 0, Strategy: StrategyDiff, FitnessDesc: "0.3", IsNovel: true},
		{Index: 1, Strategy: StrategyFull, FitnessDesc: "0.9", IsNovel: true},
		{Index: 2, Strategy: StrategyCross, FitnessDesc: "0.5", IsNovel: true},
		{Index: 3, Strategy: StrategyDiff, FitnessDesc: "0.7", IsNovel: true},
	}

	survivors := e.SelectSurvivors(results, 2)
	if len(survivors) != 2 {
		t.Fatalf("expected 2 survivors, got %d", len(survivors))
	}

	// Best by FitnessDesc (lexicographic desc): "0.9" (idx=1), "0.7" (idx=3)
	if survivors[0] != 1 {
		t.Fatalf("expected first survivor index=1, got %d", survivors[0])
	}
	if survivors[1] != 3 {
		t.Fatalf("expected second survivor index=3, got %d", survivors[1])
	}
}

func TestSelectSurvivors_OnlyNovel(t *testing.T) {
	t.Parallel()
	e := NewEngine(nil, nil)

	results := []SlotResult{
		{Index: 0, FitnessDesc: "0.9", IsNovel: false},
		{Index: 1, FitnessDesc: "0.5", IsNovel: true},
		{Index: 2, FitnessDesc: "0.3", IsNovel: false},
	}

	survivors := e.SelectSurvivors(results, 5)
	if len(survivors) != 1 {
		t.Fatalf("expected 1 survivor (only novel), got %d", len(survivors))
	}
	if survivors[0] != 1 {
		t.Fatalf("expected survivor index=1, got %d", survivors[0])
	}
}

func TestSelectSurvivors_NoNovel(t *testing.T) {
	t.Parallel()
	e := NewEngine(nil, nil)

	results := []SlotResult{
		{Index: 0, FitnessDesc: "0.9", IsNovel: false},
	}

	survivors := e.SelectSurvivors(results, 5)
	if len(survivors) != 0 {
		t.Fatalf("expected 0 survivors with no novel candidates, got %d", len(survivors))
	}
}

func TestSelectSurvivors_MaxLessThanCandidates(t *testing.T) {
	t.Parallel()
	e := NewEngine(nil, nil)

	results := []SlotResult{
		{Index: 0, FitnessDesc: "0.8", IsNovel: true},
		{Index: 1, FitnessDesc: "0.6", IsNovel: true},
		{Index: 2, FitnessDesc: "0.9", IsNovel: true},
	}

	survivors := e.SelectSurvivors(results, 1)
	if len(survivors) != 1 {
		t.Fatalf("expected 1 survivor, got %d", len(survivors))
	}
	if survivors[0] != 2 {
		t.Fatalf("expected survivor index=2 (highest fitness), got %d", survivors[0])
	}
}
