// Package bandit provides UCB1 multi-armed bandit algorithm for adaptive model selection.
package bandit

import (
	"errors"
	"math"
	"sync"
)

// ArmStats holds statistics for a single arm.
type ArmStats struct {
	Name        string
	TotalReward float64
	PullCount   int
	AvgReward   float64
}

// Selector implements the UCB1 algorithm for arm selection.
type Selector struct {
	arms             map[string]*ArmStats
	explorationCoeff float64
	totalPulls       int
	mu               sync.RWMutex
}

// NewSelector creates a new UCB1 Selector with the given exploration coefficient.
func NewSelector(explorationCoeff float64) *Selector {
	return &Selector{
		arms:             make(map[string]*ArmStats),
		explorationCoeff: explorationCoeff,
	}
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
	for _, arm := range s.arms {
		if arm.PullCount == 0 {
			return arm.Name, nil
		}
	}

	// All arms pulled at least once: select by UCB1 score.
	var bestName string
	bestScore := math.Inf(-1)
	for _, arm := range s.arms {
		score := s.ucb1Score(arm)
		if score > bestScore {
			bestScore = score
			bestName = arm.Name
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
	arm.AvgReward += (reward - arm.AvgReward) / float64(arm.PullCount)
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
func (s *Selector) ucb1Score(arm *ArmStats) float64 {
	if arm.PullCount == 0 || s.totalPulls == 0 {
		return 0
	}
	exploration := s.explorationCoeff * math.Sqrt(math.Log(float64(s.totalPulls))/float64(arm.PullCount))
	return arm.AvgReward + exploration
}

// PullCounts returns a map of arm names to their pull counts.
func (s *Selector) PullCounts() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]int, len(s.arms))
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

	var bestName string
	bestAvg := math.Inf(-1)
	found := false
	for _, arm := range s.arms {
		if arm.PullCount > 0 && arm.AvgReward > bestAvg {
			bestAvg = arm.AvgReward
			bestName = arm.Name
			found = true
		}
	}
	if !found {
		return "", 0, errors.New("no arms have been pulled")
	}
	return bestName, bestAvg, nil
}
