package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/evolution"
	"github.com/msageha/maestro_v2/internal/daemon/search"
)

func newPhaseCWithEvolution(strategies []evolution.Strategy) *PhaseCManager {
	return &PhaseCManager{
		EvolutionEngine: evolution.NewEngine(strategies, nil),
		commandRoots:    make(map[string]bool),
		taskDecisions:   make(map[string]search.Decision),
		commandNovelty:  make(map[string]map[string]struct{}),
		commandFailures: make(map[string]int),
	}
}

func TestPhaseCManager_RecordTaskCompletionNovelty_FirstIsNovel(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithEvolution([]evolution.Strategy{evolution.StrategyDiff, evolution.StrategyFull})

	novel, evaluated := m.RecordTaskCompletionNovelty("cmd-1", "first summary")
	if !evaluated {
		t.Fatalf("expected evaluated=true with engine configured")
	}
	if !novel {
		t.Fatalf("first summary should be novel")
	}

	// Same summary again → not novel.
	novel2, evaluated2 := m.RecordTaskCompletionNovelty("cmd-1", "first summary")
	if !evaluated2 {
		t.Fatalf("expected evaluated=true on second call")
	}
	if novel2 {
		t.Fatalf("duplicate summary should not be novel")
	}

	// Different summary → novel.
	novel3, _ := m.RecordTaskCompletionNovelty("cmd-1", "different summary")
	if !novel3 {
		t.Fatalf("different summary should be novel")
	}
}

func TestPhaseCManager_RecordTaskCompletionNovelty_PerCommandIsolation(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithEvolution([]evolution.Strategy{evolution.StrategyDiff})

	m.RecordTaskCompletionNovelty("cmd-1", "summary")
	novel, _ := m.RecordTaskCompletionNovelty("cmd-2", "summary")
	if !novel {
		t.Fatalf("novelty tracking must not leak across commands")
	}
}

func TestPhaseCManager_RecordTaskCompletionNovelty_Disabled(t *testing.T) {
	t.Parallel()
	// No EvolutionEngine configured.
	m := &PhaseCManager{
		commandRoots:    make(map[string]bool),
		taskDecisions:   make(map[string]search.Decision),
		commandNovelty:  make(map[string]map[string]struct{}),
		commandFailures: make(map[string]int),
	}
	_, evaluated := m.RecordTaskCompletionNovelty("cmd-1", "x")
	if evaluated {
		t.Fatalf("evaluated must be false when engine is nil")
	}

	var nilMgr *PhaseCManager
	_, evaluated = nilMgr.RecordTaskCompletionNovelty("cmd-1", "x")
	if evaluated {
		t.Fatalf("evaluated must be false on nil receiver")
	}
}

func TestPhaseCManager_RecordTaskCompletionNovelty_EmptyInputsIgnored(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithEvolution([]evolution.Strategy{evolution.StrategyDiff})
	if _, ev := m.RecordTaskCompletionNovelty("", "summary"); ev {
		t.Fatalf("empty commandID must short-circuit")
	}
	if _, ev := m.RecordTaskCompletionNovelty("cmd", ""); ev {
		t.Fatalf("empty summary must short-circuit")
	}
}

func TestPhaseCManager_PlanRetryMutations_BelowThreshold(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithEvolution([]evolution.Strategy{evolution.StrategyDiff, evolution.StrategyFull})

	// First failure: below threshold (threshold=2).
	slots, planned := m.PlanRetryMutations("cmd-1", 2)
	if planned {
		t.Fatalf("first failure must not trigger planning")
	}
	if slots != nil {
		t.Fatalf("expected nil slots below threshold, got %v", slots)
	}
}

func TestPhaseCManager_PlanRetryMutations_AtThreshold(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithEvolution([]evolution.Strategy{evolution.StrategyDiff, evolution.StrategyFull, evolution.StrategyCross})

	// Trip the threshold.
	m.PlanRetryMutations("cmd-1", 2)
	slots, planned := m.PlanRetryMutations("cmd-1", 2)
	if !planned {
		t.Fatalf("second failure should trigger planning")
	}
	if len(slots) == 0 {
		t.Fatalf("expected non-empty mutation plan")
	}
	// Verify at least one strategy type is present.
	seen := map[evolution.Strategy]bool{}
	for _, s := range slots {
		seen[s.Strategy] = true
	}
	if !seen[evolution.StrategyDiff] {
		t.Errorf("diff strategy missing from plan")
	}
}

func TestPhaseCManager_PlanRetryMutations_NoEngine(t *testing.T) {
	t.Parallel()
	m := &PhaseCManager{
		commandFailures: make(map[string]int),
	}
	if _, planned := m.PlanRetryMutations("cmd-1", 2); planned {
		t.Fatalf("must return planned=false without engine")
	}
}

func TestPhaseCManager_ResetEvolutionState_ClearsTracking(t *testing.T) {
	t.Parallel()
	m := newPhaseCWithEvolution([]evolution.Strategy{evolution.StrategyDiff, evolution.StrategyFull})

	m.RecordTaskCompletionNovelty("cmd-1", "summary")
	m.PlanRetryMutations("cmd-1", 2)
	m.ResetEvolutionState("cmd-1")

	// After reset, the same summary must be novel again.
	novel, _ := m.RecordTaskCompletionNovelty("cmd-1", "summary")
	if !novel {
		t.Fatalf("novelty should be reset; duplicate summary treated as novel")
	}

	// Failure count should also be cleared (next failure restarts below threshold).
	_, planned := m.PlanRetryMutations("cmd-1", 2)
	if planned {
		t.Fatalf("failure count should reset; first post-reset failure must not plan")
	}
}
