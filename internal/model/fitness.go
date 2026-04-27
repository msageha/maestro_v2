package model

import (
	"context"
	"fmt"
	"time"
)

// FitnessScore is the mechanical evaluation score for a task result.
// Scores are compared lexicographically to select the best candidate.
type FitnessScore struct {
	Passed           bool
	RepairCount      int
	DiffFilesChanged int
	DiffLinesChanged int
	PathDeviation    bool // whether changes deviated from expected_paths
	ExecutionTime    time.Duration
	QualityScore     float64 // C-1: 0.0-1.0, based on static analysis metrics
}

// FitnessThresholds defines per-axis margins for FitnessScore comparisons.
// Axis differences within the margin are treated as equivalent.
type FitnessThresholds struct {
	RepairCountMargin   int
	DiffSizeMargin      int
	ExecutionTimeMargin time.Duration
	QualityScoreMargin  float64 // C-1: margin for QualityScore differences
}

// DefaultFitnessThresholds returns the default FitnessScore comparison margins.
func DefaultFitnessThresholds() FitnessThresholds {
	return FitnessThresholds{
		RepairCountMargin:   0,
		DiffSizeMargin:      10,
		ExecutionTimeMargin: 30 * time.Second,
		QualityScoreMargin:  0.05,
	}
}

// IsFailed reports whether the score failed the primary pass/fail axis.
func (a FitnessScore) IsFailed() bool {
	return !a.Passed
}

// Compare compares a and b lexicographically across the configured axes.
// It returns -1 when a wins, +1 when b wins, and 0 when they are equivalent.
func (a FitnessScore) Compare(b FitnessScore, th FitnessThresholds) int {
	// Axis 1: Pass / Fail
	if a.Passed != b.Passed {
		if a.Passed {
			return -1
		}
		return 1
	}

	// Axis 2: RepairCount (lower wins)
	repairDiff := a.RepairCount - b.RepairCount
	if abs(repairDiff) > th.RepairCountMargin {
		if repairDiff < 0 {
			return -1
		}
		return 1
	}

	// Axis 3: PathDeviation (no deviation wins)
	if a.PathDeviation != b.PathDeviation {
		if !a.PathDeviation {
			return -1
		}
		return 1
	}

	// Axis 4: DiffLinesChanged (lower wins)
	diffLinesDiff := a.DiffLinesChanged - b.DiffLinesChanged
	if abs(diffLinesDiff) > th.DiffSizeMargin {
		if diffLinesDiff < 0 {
			return -1
		}
		return 1
	}

	// Axis 5: ExecutionTime (shorter wins)
	timeDiff := a.ExecutionTime - b.ExecutionTime
	if timeDiff < 0 {
		timeDiff = -timeDiff
	}
	if timeDiff > th.ExecutionTimeMargin {
		if a.ExecutionTime < b.ExecutionTime {
			return -1
		}
		return 1
	}

	// Axis 6: QualityScore (higher wins, C-1 static-analysis based)
	qualDiff := a.QualityScore - b.QualityScore
	if qualDiff < 0 {
		qualDiff = -qualDiff
	}
	if qualDiff > th.QualityScoreMargin {
		if a.QualityScore > b.QualityScore {
			return -1
		}
		return 1
	}

	return 0
}

// SelectWinner returns the winning candidate index.
// If all candidates are equivalent, it returns isTie=true and winnerIndex=0.
// For an empty candidate set, it returns winnerIndex=-1 and isTie=false.
func SelectWinner(candidates []FitnessScore, th FitnessThresholds) (winnerIndex int, isTie bool) {
	if len(candidates) == 0 {
		return -1, false
	}
	if len(candidates) == 1 {
		return 0, false
	}

	best := 0
	allTie := true

	for i := 1; i < len(candidates); i++ {
		cmp := candidates[best].Compare(candidates[i], th)
		if cmp > 0 {
			// candidates[i] wins.
			best = i
			allTie = false
		} else if cmp < 0 {
			// candidates[best] wins.
			allTie = false
		}
	}

	if allTie && len(candidates) > 1 {
		return 0, true
	}

	// Re-check that best wins or ties against every candidate.
	for i := 0; i < len(candidates); i++ {
		if i == best {
			continue
		}
		cmp := candidates[best].Compare(candidates[i], th)
		if cmp > 0 {
			// Per-axis margin comparisons are non-strict, so boundary inputs
			// can break transitivity. Treat the inconsistency as a tie instead
			// of panicking and let the judge resolve it.
			return 0, true
		}
	}

	return best, allTie
}

// ResolveWinner calls SelectWinner and uses judge only when the mechanical
// FitnessScore comparison ties. Without a judge, or when judge returns an
// error, index 0 remains the deterministic fallback.
func ResolveWinner(scores []FitnessScore, thresholds FitnessThresholds, judge JudgeFunc) (winnerIdx int, judgeUsed bool, err error) {
	winner, isTie := SelectWinner(scores, thresholds)

	if !isTie {
		return winner, false, nil
	}

	if judge == nil {
		return winner, false, nil
	}

	decision, judgeErr := judge(context.Background(), scores, nil)
	if judgeErr != nil {
		return winner, false, judgeErr
	}

	if decision.WinnerIndex == nil {
		return winner, false, fmt.Errorf("judge returned nil WinnerIndex")
	}
	return *decision.WinnerIndex, true, nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
