package bandit

import (
	"sync"
	"testing"
)

func TestSelectArm_NoArms_ReturnsError(t *testing.T) {
	s := NewSelector(1.0)
	_, err := s.SelectArm()
	if err == nil {
		t.Fatal("expected error when no arms registered")
	}
}

func TestSelectArm_ExplorationPhase(t *testing.T) {
	s := NewSelector(1.0)
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
	s := NewSelector(1.0)
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
	s := NewSelector(1.0)
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
	s := NewSelector(0)
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
	s := NewSelector(100.0) // Very high exploration coefficient.
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
	s := NewSelector(1.0)
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
	total := 0
	for _, stat := range stats {
		total += stat.PullCount
	}
	if total != 100 {
		t.Errorf("expected 100 total pulls, got %d", total)
	}
}

func TestBestArm_NoPulls_ReturnsError(t *testing.T) {
	s := NewSelector(1.0)
	s.AddArm("a")
	_, _, err := s.BestArm()
	if err == nil {
		t.Fatal("expected error when no arms have been pulled")
	}
}

func TestBestArm_ReturnsHighestAvg(t *testing.T) {
	s := NewSelector(1.0)
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
	s := NewSelector(1.0)
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
	s := NewSelector(1.0)
	s.AddArm("a")
	s.UpdateReward("a", 1.0)
	s.AddArm("a") // duplicate add should not reset stats

	stats := s.GetStats()
	if stats["a"].PullCount != 1 {
		t.Errorf("expected pull count 1, got %d", stats["a"].PullCount)
	}
}

func TestUCB1Score_ZeroPulls_ReturnsZero(t *testing.T) {
	s := NewSelector(1.0)
	arm := &ArmStats{Name: "test", PullCount: 0}
	score := s.UCB1Score(arm)
	if score != 0 {
		t.Errorf("expected 0 for unpulled arm, got %f", score)
	}
}
