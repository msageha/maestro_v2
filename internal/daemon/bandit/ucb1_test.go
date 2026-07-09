package bandit

import (
	"math"
	"sync"
	"testing"
)

func mustNewSelector(t *testing.T, coeff float64) *Selector {
	t.Helper()
	s, err := NewSelector(coeff)
	if err != nil {
		t.Fatalf("NewSelector(%v) unexpected error: %v", coeff, err)
	}
	return s
}

func TestNewSelector_InvalidCoeff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		coeff float64
	}{
		{"NaN", math.NaN()},
		{"PositiveInf", math.Inf(1)},
		{"NegativeInf", math.Inf(-1)},
		{"Negative", -1.0},
		{"NegativeSmall", -0.001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := NewSelector(tt.coeff)
			if err == nil {
				t.Errorf("NewSelector(%v) expected error, got nil", tt.coeff)
			}
			if s != nil {
				t.Errorf("NewSelector(%v) expected nil selector on error", tt.coeff)
			}
		})
	}
}

func TestNewSelector_ValidCoeff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		coeff float64
	}{
		{"Zero", 0},
		{"Positive", 1.41},
		{"Large", 100.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, err := NewSelector(tt.coeff)
			if err != nil {
				t.Errorf("NewSelector(%v) unexpected error: %v", tt.coeff, err)
			}
			if s == nil {
				t.Errorf("NewSelector(%v) expected non-nil selector", tt.coeff)
			}
		})
	}
}

func TestSelectArm_NoArms_ReturnsError(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 1.0)
	_, err := s.SelectArm()
	if err == nil {
		t.Fatal("expected error when no arms registered")
	}
}

func TestSelectArm_ExplorationPhase(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 1.0)
	s.AddArm("a")
	s.AddArm("b")
	s.AddArm("c")

	selected := make(map[string]bool)
	for range 3 {
		name, err := s.SelectArm()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		selected[name] = true
		s.UpdateReward(name, 0.5)
	}

	// All arms should have been explored at least once.
	for _, arm := range []string{"a", "b", "c"} {
		if !selected[arm] {
			t.Errorf("arm %q was not selected during exploration phase", arm)
		}
	}
}

func TestSelectArm_UCB1AfterExploration(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 1.0)
	s.AddArm("low")
	s.AddArm("high")

	// Pull each arm once (exploration).
	s.UpdateReward("low", 0.1)
	s.UpdateReward("high", 0.9)

	// After exploration, UCB1 should favor "high" due to higher avg reward.
	name, err := s.SelectArm()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "high" {
		t.Errorf("expected 'high', got %q", name)
	}
}

func TestHighRewardArm_Convergence(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 1.0)
	s.AddArm("bad")
	s.AddArm("good")

	// Initial exploration.
	s.UpdateReward("bad", 0.1)
	s.UpdateReward("good", 0.9)

	// Feed 100 more rounds, always rewarding the selected arm proportionally.
	for range 100 {
		name, err := s.SelectArm()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name == "good" {
			s.UpdateReward(name, 0.9)
		} else {
			s.UpdateReward(name, 0.1)
		}
	}

	bestName, bestAvg, err := s.BestArm()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bestName != "good" {
		t.Errorf("expected best arm 'good', got %q", bestName)
	}
	if bestAvg < 0.8 {
		t.Errorf("expected avg reward >= 0.8, got %f", bestAvg)
	}
}

func TestExplorationCoeff_Zero_PureGreedy(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 0)
	s.AddArm("low")
	s.AddArm("high")

	s.UpdateReward("low", 0.2)
	s.UpdateReward("high", 0.8)

	// With explorationCoeff=0, selection is purely greedy (highest avg reward).
	for range 10 {
		name, err := s.SelectArm()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != "high" {
			t.Errorf("greedy selection expected 'high', got %q", name)
		}
		s.UpdateReward(name, 0.8)
	}
}

func TestExplorationCoeff_High_ExploresMore(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 100.0) // Very high exploration coefficient.
	s.AddArm("low")
	s.AddArm("high")

	s.UpdateReward("low", 0.1)
	s.UpdateReward("high", 0.9)

	// With very high exploration, even the low-reward arm should be selected sometimes.
	lowSelected := 0
	for range 50 {
		name, err := s.SelectArm()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name == "low" {
			lowSelected++
		}
		s.UpdateReward(name, 0.5)
	}

	if lowSelected == 0 {
		t.Error("with high exploration coefficient, 'low' arm was never selected")
	}
}

func TestConcurrentSafety(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 1.0)
	s.AddArm("a")
	s.AddArm("b")
	s.AddArm("c")

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, err := s.SelectArm()
			if err != nil {
				return
			}
			s.UpdateReward(name, 0.5)
		}()
	}
	wg.Wait()

	stats := s.GetStats()
	var total int64
	for _, stat := range stats {
		total += stat.PullCount
	}
	if total != 100 {
		t.Errorf("expected 100 total pulls, got %d", total)
	}
}

func TestBestArm_NoPulls_ReturnsError(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 1.0)
	s.AddArm("a")
	_, _, err := s.BestArm()
	if err == nil {
		t.Fatal("expected error when no arms have been pulled")
	}
}

func TestBestArm_ReturnsHighestAvg(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 1.0)
	s.AddArm("low")
	s.AddArm("mid")
	s.AddArm("high")

	s.UpdateReward("low", 0.2)
	s.UpdateReward("mid", 0.5)
	s.UpdateReward("high", 0.9)

	name, avg, err := s.BestArm()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "high" {
		t.Errorf("expected 'high', got %q", name)
	}
	if avg != 0.9 {
		t.Errorf("expected avg 0.9, got %f", avg)
	}
}

func TestReset_ClearsAllState(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 1.0)
	s.AddArm("a")
	s.UpdateReward("a", 1.0)

	s.Reset()

	stats := s.GetStats()
	if len(stats) != 0 {
		t.Errorf("expected empty stats after reset, got %d arms", len(stats))
	}

	_, err := s.SelectArm()
	if err == nil {
		t.Fatal("expected error after reset with no arms")
	}
}

func TestAddArm_Duplicate_NoOp(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 1.0)
	s.AddArm("a")
	s.UpdateReward("a", 1.0)
	s.AddArm("a") // duplicate add should not reset stats

	stats := s.GetStats()
	if stats["a"].PullCount != 1 {
		t.Errorf("expected pull count 1, got %d", stats["a"].PullCount)
	}
}

func TestUCB1Score_ZeroPulls_ReturnsZero(t *testing.T) {
	t.Parallel()
	s := mustNewSelector(t, 1.0)
	arm := &ArmStats{Name: "test", PullCount: 0}
	score := s.UCB1Score(arm)
	if score != 0 {
		t.Errorf("expected 0 for unpulled arm, got %f", score)
	}
}

// TestSelector_ExportRestoreState_RoundTrip pins the persistence snapshot:
// statistics survive an export/restore cycle onto a freshly registered
// selector, orphaned snapshot arms are dropped, and newly registered arms
// keep zero stats so the warm-up gate demands fresh samples for them.
func TestSelector_ExportRestoreState_RoundTrip(t *testing.T) {
	src, err := NewSelector(1.41)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	for _, arm := range []string{"sonnet", "opus", "legacy"} {
		src.AddArm(arm)
	}
	for i := 0; i < 4; i++ {
		src.UpdateReward("sonnet", 1.0)
	}
	src.UpdateReward("opus", 0.5)
	src.UpdateReward("legacy", 0.2)

	st := src.ExportState()
	if len(st.Arms) != 3 {
		t.Fatalf("exported arms = %d, want 3", len(st.Arms))
	}

	// New process: "legacy" was dropped from config, "haiku" was added.
	dst, err := NewSelector(1.41)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	for _, arm := range []string{"sonnet", "opus", "haiku"} {
		dst.AddArm(arm)
	}
	dst.RestoreState(st)

	pulls := dst.PullCounts()
	if pulls["sonnet"] != 4 || pulls["opus"] != 1 {
		t.Errorf("restored pulls = %v, want sonnet=4 opus=1", pulls)
	}
	if pulls["haiku"] != 0 {
		t.Errorf("new arm haiku pulls = %d, want 0", pulls["haiku"])
	}
	if _, ok := pulls["legacy"]; ok {
		t.Error("orphaned snapshot arm 'legacy' must not be resurrected")
	}
	stats := dst.GetStats()
	if got := stats["sonnet"].AvgReward; got != 1.0 {
		t.Errorf("sonnet AvgReward = %v, want 1.0", got)
	}
	// totalPulls is recomputed from surviving arms (5, not the source's 6):
	// with 5 total pulls the UCB1 exploration term stays consistent.
	best, avg, err := dst.BestArm()
	if err != nil || best != "sonnet" || avg != 1.0 {
		t.Errorf("BestArm = (%s, %v, %v), want (sonnet, 1.0, nil)", best, avg, err)
	}
}
