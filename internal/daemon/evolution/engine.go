// Package evolution implements the evolutionary improvement engine for agent plans.
package evolution

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"sync"
)

// Strategy represents a mutation strategy for evolutionary code optimization.
type Strategy string

const (
	// StrategyDiff selects the diff-based mutation strategy.
	StrategyDiff Strategy = "diff"
	// StrategyFull selects the full-replacement mutation strategy.
	StrategyFull Strategy = "full"
	// StrategyCross selects the crossover mutation strategy.
	StrategyCross Strategy = "cross"
)

// MutationSlot represents a planned mutation slot with strategy and parent reference.
type MutationSlot struct {
	Index     int
	Strategy  Strategy
	ParentRef string
}

// SlotResult captures the outcome of a single mutation slot evaluation.
type SlotResult struct {
	Index       int
	Strategy    Strategy
	FitnessDesc string
	IsNovel     bool
}

// CycleResult captures the outcome of one evolutionary cycle.
type CycleResult struct {
	Round        int
	Slots        []SlotResult
	BestSlotIndex int
}

// Engine manages evolutionary mutation planning, novelty checking, and survivor selection.
type Engine struct {
	strategies       []Strategy
	noveltyThreshold float64
	mu               sync.Mutex
}

// NewEngine creates a new evolution engine with the given strategies and novelty threshold.
func NewEngine(strategies []Strategy, noveltyThreshold float64) *Engine {
	return &Engine{
		strategies:       strategies,
		noveltyThreshold: noveltyThreshold,
	}
}

// PlanMutations distributes mutation slots across strategies.
// Distribution ratio: diff:full:cross = 2:1:1 (cross only if parentCount >= 2).
func (e *Engine) PlanMutations(parentCount int) []MutationSlot {
	e.mu.Lock()
	defer e.mu.Unlock()

	hasDiff := e.hasStrategy(StrategyDiff)
	hasFull := e.hasStrategy(StrategyFull)
	hasCross := e.hasStrategy(StrategyCross) && parentCount >= 2

	type stratWeight struct {
		strategy Strategy
		weight   int
	}

	var active []stratWeight
	if hasDiff {
		active = append(active, stratWeight{StrategyDiff, 2})
	}
	if hasFull {
		active = append(active, stratWeight{StrategyFull, 1})
	}
	if hasCross {
		active = append(active, stratWeight{StrategyCross, 1})
	}

	if len(active) == 0 {
		return nil
	}

	totalWeight := 0
	for _, s := range active {
		totalWeight += s.weight
	}

	var slots []MutationSlot
	idx := 0
	for _, s := range active {
		for range s.weight {
			slots = append(slots, MutationSlot{
				Index:    idx,
				Strategy: s.strategy,
			})
			idx++
		}
	}

	return slots
}

// CheckNovelty determines if a candidate is novel compared to existing candidates.
// Currently uses SHA-256 hash exact match. Future: embedding-based similarity.
func (e *Engine) CheckNovelty(candidateHash string, existingHashes []string) bool {
	for _, h := range existingHashes {
		if h == candidateHash {
			return false
		}
	}
	return true
}

// HashContent computes a SHA-256 hash of the given content for novelty comparison.
func HashContent(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
}

// SelectSurvivors selects the top survivors from slot results based on FitnessDesc.
// Only novel candidates are eligible. Uses winner-takes-all selection (§5-3).
func (e *Engine) SelectSurvivors(results []SlotResult, maxSurvivors int) []int {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Filter to novel results only
	type ranked struct {
		originalIndex int
		fitnessDesc   string
	}

	var candidates []ranked
	for _, r := range results {
		if r.IsNovel {
			candidates = append(candidates, ranked{
				originalIndex: r.Index,
				fitnessDesc:   r.FitnessDesc,
			})
		}
	}

	// Sort by FitnessDesc descending (lexicographic — higher string = higher fitness)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].fitnessDesc > candidates[j].fitnessDesc
	})

	limit := maxSurvivors
	if limit > len(candidates) {
		limit = len(candidates)
	}

	survivors := make([]int, limit)
	for i := 0; i < limit; i++ {
		survivors[i] = candidates[i].originalIndex
	}
	return survivors
}

func (e *Engine) hasStrategy(s Strategy) bool {
	for _, st := range e.strategies {
		if st == s {
			return true
		}
	}
	return false
}
