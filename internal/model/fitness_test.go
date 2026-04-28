package model

import (
	"testing"
	"time"
)

func TestFitnessScore_IsFailed(t *testing.T) {
	tests := []struct {
		name   string
		passed bool
		want   bool
	}{
		{"passed", true, false},
		{"failed", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := FitnessScore{Passed: tt.passed}
			if got := fs.IsFailed(); got != tt.want {
				t.Errorf("IsFailed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultFitnessThresholds(t *testing.T) {
	th := DefaultFitnessThresholds()
	if th.RepairCountMargin != 0 {
		t.Errorf("RepairCountMargin = %d, want 0", th.RepairCountMargin)
	}
	if th.DiffSizeMargin != 10 {
		t.Errorf("DiffSizeMargin = %d, want 10", th.DiffSizeMargin)
	}
	if th.ExecutionTimeMargin != 30*time.Second {
		t.Errorf("ExecutionTimeMargin = %v, want 30s", th.ExecutionTimeMargin)
	}
}

func TestFitnessScore_Compare_PassFail(t *testing.T) {
	th := FitnessThresholds{}
	// a passed, b failed → a wins
	a := FitnessScore{Passed: true}
	b := FitnessScore{Passed: false}
	if got := a.Compare(b, th); got != -1 {
		t.Errorf("passed vs failed: got %d, want -1", got)
	}
	// b passed, a failed → b wins
	if got := b.Compare(a, th); got != 1 {
		t.Errorf("failed vs passed: got %d, want 1", got)
	}
}

func TestFitnessScore_Compare_RepairCount(t *testing.T) {
	th := FitnessThresholds{RepairCountMargin: 1}
	base := FitnessScore{Passed: true}

	// a=1, b=3, margin=1 → diff=2 > 1 → a wins
	a := base
	a.RepairCount = 1
	b := base
	b.RepairCount = 3
	if got := a.Compare(b, th); got != -1 {
		t.Errorf("repair 1 vs 3 (margin 1): got %d, want -1", got)
	}
	if got := b.Compare(a, th); got != 1 {
		t.Errorf("repair 3 vs 1 (margin 1): got %d, want 1", got)
	}

	// a=2, b=3, margin=1 → diff=1, not > 1 → tie on this axis
	a.RepairCount = 2
	if got := a.Compare(b, th); got != 0 {
		t.Errorf("repair 2 vs 3 (margin 1): got %d, want 0 (within margin)", got)
	}
}

func TestFitnessScore_Compare_PathDeviation(t *testing.T) {
	th := FitnessThresholds{}
	base := FitnessScore{Passed: true}

	// a has no deviation, b has deviation → a wins
	a := base
	a.PathDeviation = false
	b := base
	b.PathDeviation = true
	if got := a.Compare(b, th); got != -1 {
		t.Errorf("no deviation vs deviation: got %d, want -1", got)
	}
	if got := b.Compare(a, th); got != 1 {
		t.Errorf("deviation vs no deviation: got %d, want 1", got)
	}
}

func TestFitnessScore_Compare_DiffLines(t *testing.T) {
	th := FitnessThresholds{DiffSizeMargin: 5}
	base := FitnessScore{Passed: true}

	// a=10 lines, b=20 lines, margin=5 → diff=10 > 5 → a wins
	a := base
	a.DiffLinesChanged = 10
	b := base
	b.DiffLinesChanged = 20
	if got := a.Compare(b, th); got != -1 {
		t.Errorf("diff 10 vs 20 (margin 5): got %d, want -1", got)
	}
	if got := b.Compare(a, th); got != 1 {
		t.Errorf("diff 20 vs 10 (margin 5): got %d, want 1", got)
	}

	// within margin
	a.DiffLinesChanged = 18
	if got := a.Compare(b, th); got != 0 {
		t.Errorf("diff 18 vs 20 (margin 5): got %d, want 0", got)
	}
}

func TestFitnessScore_Compare_ExecutionTime(t *testing.T) {
	th := FitnessThresholds{ExecutionTimeMargin: 10 * time.Second}
	base := FitnessScore{Passed: true}

	// a=30s, b=60s, margin=10s → diff=30s > 10s → a wins
	a := base
	a.ExecutionTime = 30 * time.Second
	b := base
	b.ExecutionTime = 60 * time.Second
	if got := a.Compare(b, th); got != -1 {
		t.Errorf("time 30s vs 60s (margin 10s): got %d, want -1", got)
	}
	if got := b.Compare(a, th); got != 1 {
		t.Errorf("time 60s vs 30s (margin 10s): got %d, want 1", got)
	}

	// within margin
	a.ExecutionTime = 55 * time.Second
	if got := a.Compare(b, th); got != 0 {
		t.Errorf("time 55s vs 60s (margin 10s): got %d, want 0", got)
	}
}

func TestFitnessScore_Compare_Tie(t *testing.T) {
	th := DefaultFitnessThresholds()
	a := FitnessScore{
		Passed:           true,
		RepairCount:      1,
		DiffLinesChanged: 50,
		ExecutionTime:    10 * time.Second,
	}
	b := a // identical
	if got := a.Compare(b, th); got != 0 {
		t.Errorf("identical scores: got %d, want 0", got)
	}
}

func TestFitnessScore_Compare_BothFailed(t *testing.T) {
	th := FitnessThresholds{}
	a := FitnessScore{Passed: false, RepairCount: 1}
	b := FitnessScore{Passed: false, RepairCount: 5}
	// Both failed, but RepairCount still compared
	if got := a.Compare(b, th); got != -1 {
		t.Errorf("both failed, repair 1 vs 5: got %d, want -1", got)
	}
}

func TestFitnessScore_Compare_LexicographicOrder(t *testing.T) {
	// Verify earlier axes take precedence: PassFail > Repair > PathDeviation > Diff > Time
	th := FitnessThresholds{}

	// a: passed=true, repair=100 (worse)
	// b: passed=false, repair=0 (better on repair, but fail)
	a := FitnessScore{Passed: true, RepairCount: 100}
	b := FitnessScore{Passed: false, RepairCount: 0}
	if got := a.Compare(b, th); got != -1 {
		t.Errorf("pass trumps repair: got %d, want -1", got)
	}

	// repair takes precedence over path deviation
	a = FitnessScore{Passed: true, RepairCount: 0, PathDeviation: true}
	b = FitnessScore{Passed: true, RepairCount: 5, PathDeviation: false}
	if got := a.Compare(b, th); got != -1 {
		t.Errorf("repair trumps path deviation: got %d, want -1", got)
	}

	// path deviation takes precedence over diff lines
	a = FitnessScore{Passed: true, PathDeviation: false, DiffLinesChanged: 1000}
	b = FitnessScore{Passed: true, PathDeviation: true, DiffLinesChanged: 1}
	if got := a.Compare(b, th); got != -1 {
		t.Errorf("path deviation trumps diff: got %d, want -1", got)
	}

	// diff lines takes precedence over time
	a = FitnessScore{Passed: true, DiffLinesChanged: 10, ExecutionTime: 999 * time.Second}
	b = FitnessScore{Passed: true, DiffLinesChanged: 100, ExecutionTime: 1 * time.Second}
	if got := a.Compare(b, th); got != -1 {
		t.Errorf("diff trumps time: got %d, want -1", got)
	}
}
