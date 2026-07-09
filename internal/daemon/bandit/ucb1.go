// Package bandit provides UCB1 multi-armed bandit algorithm for adaptive model selection.
package bandit

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
)

// ArmStats holds statistics for a single arm. The JSON tags define the
// persisted snapshot schema (see State) and must stay stable.
type ArmStats struct {
	Name        string  `json:"name"`
	TotalReward float64 `json:"total_reward"`
	PullCount   int64   `json:"pull_count"`
	AvgReward   float64 `json:"avg_reward"`
}

// Selector implements the UCB1 algorithm for arm selection.
type Selector struct {
	arms             map[string]*ArmStats
	explorationCoeff float64
	totalPulls       int64
	mu               sync.RWMutex
}

// NewSelector creates a new UCB1 Selector with the given exploration coefficient.
// Returns an error if explorationCoeff is NaN, Inf, or negative.
func NewSelector(explorationCoeff float64) (*Selector, error) {
	if math.IsNaN(explorationCoeff) {
		return nil, fmt.Errorf("explorationCoeff must not be NaN")
	}
	if math.IsInf(explorationCoeff, 0) {
		return nil, fmt.Errorf("explorationCoeff must not be Inf")
	}
	if explorationCoeff < 0 {
		return nil, fmt.Errorf("explorationCoeff must not be negative, got %v", explorationCoeff)
	}
	return &Selector{
		arms:             make(map[string]*ArmStats),
		explorationCoeff: explorationCoeff,
	}, nil
}

// AddArm registers a new arm with the given name.
func (s *Selector) AddArm(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.arms[name]; exists {
		return
	}
	s.arms[name] = &ArmStats{Name: name}
}

// SelectArm chooses the next arm using the UCB1 strategy.
// Arms with PullCount == 0 are selected first (exploration phase).
// Returns an error if no arms are registered.
func (s *Selector) SelectArm() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.arms) == 0 {
		return "", errors.New("no arms available")
	}

	// Exploration phase: select any arm with zero pulls.
	// Iterate in sorted order for deterministic tie-breaking.
	names := make([]string, 0, len(s.arms))
	for name := range s.arms {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if s.arms[name].PullCount == 0 {
			return name, nil
		}
	}

	// All arms pulled at least once: select by UCB1 score.
	var bestName string
	bestScore := math.Inf(-1)
	for _, name := range names {
		arm := s.arms[name]
		score := s.ucb1Score(arm)
		if score > bestScore {
			bestScore = score
			bestName = name
		}
	}
	return bestName, nil
}

// UpdateReward records a reward observation for the named arm.
// Uses incremental mean: avg = avg + (reward - avg) / count.
func (s *Selector) UpdateReward(armName string, reward float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	arm, ok := s.arms[armName]
	if !ok {
		return
	}
	arm.PullCount++
	arm.TotalReward += reward
	s.totalPulls++
	arm.AvgReward += (reward - arm.AvgReward) / float64(arm.PullCount) //nolint:gosec // PullCount is bounded by usage
}

// State is a serializable snapshot of a Selector's arm statistics, used to
// persist learning across daemon restarts. The exploration coefficient and
// arm registration stay config-owned — only the observed statistics travel.
type State struct {
	Arms []ArmStats `json:"arms"`
}

// ExportState returns a snapshot of the selector's arm statistics in
// deterministic (name-sorted) order.
func (s *Selector) ExportState() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.arms))
	for name := range s.arms {
		names = append(names, name)
	}
	sort.Strings(names)
	st := State{Arms: make([]ArmStats, 0, len(names))}
	for _, name := range names {
		st.Arms = append(st.Arms, *s.arms[name])
	}
	return st
}

// RestoreState overwrites the statistics of arms that are currently
// registered with the snapshot's values. Snapshot entries for unregistered
// arms are dropped (the operator changed the configured model set), and
// registered arms missing from the snapshot keep their zero stats — the
// warm-up gate then requires fresh samples for them before selection, which
// is the safe behaviour for a newly introduced model. totalPulls is
// recomputed from the restored pull counts so the UCB1 exploration term
// stays consistent with the arms that actually exist.
func (s *Selector) RestoreState(st State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, saved := range st.Arms {
		arm, ok := s.arms[saved.Name]
		if !ok {
			continue
		}
		arm.TotalReward = saved.TotalReward
		arm.PullCount = saved.PullCount
		arm.AvgReward = saved.AvgReward
	}
	for _, arm := range s.arms {
		total += arm.PullCount
	}
	s.totalPulls = total
}

// GetStats returns a snapshot of all arm statistics.
func (s *Selector) GetStats() map[string]ArmStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]ArmStats, len(s.arms))
	for name, arm := range s.arms {
		result[name] = *arm
	}
	return result
}

// UCB1Score computes the UCB1 score for the given arm.
// Returns 0 if the arm has never been pulled or totalPulls is 0.
func (s *Selector) UCB1Score(arm *ArmStats) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ucb1Score(arm)
}

// ucb1Score is the internal lock-free implementation.
// Design note: Returns 0 for unpulled arms (PullCount==0) rather than +Inf
// because SelectArm handles exploration-phase selection separately. Callers
// using UCB1Score (exported) on unpulled arms should be aware of this.
func (s *Selector) ucb1Score(arm *ArmStats) float64 {
	if arm.PullCount == 0 || s.totalPulls == 0 {
		return 0
	}
	exploration := s.explorationCoeff * math.Sqrt(math.Log(float64(s.totalPulls))/float64(arm.PullCount)) //nolint:gosec // totalPulls (int64) fits float64 for practical usage
	return arm.AvgReward + exploration
}

// PullCounts returns a map of arm names to their pull counts.
func (s *Selector) PullCounts() map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]int64, len(s.arms))
	for name, arm := range s.arms {
		result[name] = arm.PullCount
	}
	return result
}

// Reset clears all arm statistics.
func (s *Selector) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.arms = make(map[string]*ArmStats)
	s.totalPulls = 0
}

// BestArm returns the arm with the highest average reward and its AvgReward.
// Returns an error if no arms are registered or none have been pulled.
func (s *Selector) BestArm() (string, float64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.arms) == 0 {
		return "", 0, errors.New("no arms available")
	}

	bestNames := make([]string, 0, len(s.arms))
	for name := range s.arms {
		bestNames = append(bestNames, name)
	}
	sort.Strings(bestNames)

	var bestName string
	bestAvg := math.Inf(-1)
	found := false
	for _, name := range bestNames {
		arm := s.arms[name]
		if arm.PullCount > 0 && arm.AvgReward > bestAvg {
			bestAvg = arm.AvgReward
			bestName = name
			found = true
		}
	}
	if !found {
		return "", 0, errors.New("no arms have been pulled")
	}
	return bestName, bestAvg, nil
}
