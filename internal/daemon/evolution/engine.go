// Package evolution implements the evolutionary improvement engine for agent plans.
package evolution

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
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
//
// FitnessScore is the canonical numeric fitness used by SelectSurvivors for
// ordering. It is computed mechanically by the verifier (docs/requirements/REQUIREMENTS.md
// §C-3-3 — weighted aggregation, no LLM override). FitnessDesc is an optional
// human-readable description (e.g. "0.85 (build:pass test:pass lint:warn)")
// that is only used as a stable tiebreaker — never as the primary key.
type SlotResult struct {
	Index        int
	Strategy     Strategy
	FitnessScore float64
	FitnessDesc  string
	IsNovel      bool
}

// CycleResult captures the outcome of one evolutionary cycle.
type CycleResult struct {
	Round         int
	Slots         []SlotResult
	BestSlotIndex int
}

// Engine manages evolutionary mutation planning, novelty checking, and survivor selection.
type Engine struct {
	strategies       []Strategy
	strategyWeights  map[Strategy]int
	maxMutations     int
	noveltyThreshold float64
	mu               sync.Mutex
}

// NewEngine creates a new evolution engine with the given strategies and optional strategy weights.
func NewEngine(strategies []Strategy, weights map[Strategy]int) *Engine {
	return &Engine{
		strategies:       strategies,
		strategyWeights:  weights,
		noveltyThreshold: 1.0,
	}
}

// SetMaxMutationsPerRound caps the number of mutation slots produced by
// PlanMutations. Non-positive values disable the cap for backwards
// compatibility with callers that construct Engine directly in tests.
func (e *Engine) SetMaxMutationsPerRound(n int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.maxMutations = n
}

// SetNoveltyThreshold configures the similarity threshold used by CheckNovelty.
// With the current exact-hash implementation, identical hashes have similarity
// 1 and different hashes have similarity 0. A candidate is novel when its max
// similarity is below the threshold. Values outside [0,1] are ignored.
func (e *Engine) SetNoveltyThreshold(threshold float64) {
	if threshold < 0 || threshold > 1 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.noveltyThreshold = threshold
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
		active = append(active, stratWeight{StrategyDiff, e.weightFor(StrategyDiff)})
	}
	if hasFull {
		active = append(active, stratWeight{StrategyFull, e.weightFor(StrategyFull)})
	}
	if hasCross {
		active = append(active, stratWeight{StrategyCross, e.weightFor(StrategyCross)})
	}

	if len(active) == 0 {
		return nil
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
	if e.maxMutations > 0 && len(slots) > e.maxMutations {
		slots = slots[:e.maxMutations]
	}

	return slots
}

// CheckNovelty determines if a candidate is novel compared to existing candidates.
// Currently uses SHA-256 hash exact match. Future: embedding-based similarity.
func (e *Engine) CheckNovelty(candidateHash string, existingHashes []string) bool {
	e.mu.Lock()
	threshold := e.noveltyThreshold
	e.mu.Unlock()

	hashSet := make(map[string]struct{}, len(existingHashes))
	for _, h := range existingHashes {
		hashSet[h] = struct{}{}
	}
	_, found := hashSet[candidateHash]
	if found {
		return 1 < threshold
	}
	return 0 < threshold
}

// HashContent computes a SHA-256 hash of the given content for novelty comparison.
func HashContent(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
}

// SelectSurvivors selects the top survivors from slot results.
//
// Ordering: descending by FitnessScore (numeric, mechanically computed by the
// verifier per §C-3-3). FitnessDesc is consulted only as a stable tiebreaker
// when FitnessScores are equal; if FitnessDesc is also tied, the original
// SlotResult.Index is used to make the order deterministic. Only novel
// candidates are eligible (§5-3 winner-takes-all).
//
// Backwards compatibility: if FitnessScore is zero/unset and FitnessDesc
// parses cleanly as a float, the parsed value is treated as the score. This
// preserves behaviour for older callers that only populated FitnessDesc.
func (e *Engine) SelectSurvivors(results []SlotResult, maxSurvivors int) []int {
	e.mu.Lock()
	defer e.mu.Unlock()

	type ranked struct {
		originalIndex int
		score         float64
		desc          string
	}

	scoreOf := func(r SlotResult) float64 {
		if r.FitnessScore != 0 {
			return r.FitnessScore
		}
		if v, err := strconv.ParseFloat(r.FitnessDesc, 64); err == nil {
			return v
		}
		return 0
	}

	var candidates []ranked
	for _, r := range results {
		if r.IsNovel {
			candidates = append(candidates, ranked{
				originalIndex: r.Index,
				score:         scoreOf(r),
				desc:          r.FitnessDesc,
			})
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if candidates[i].desc != candidates[j].desc {
			return candidates[i].desc > candidates[j].desc
		}
		return candidates[i].originalIndex < candidates[j].originalIndex
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

// defaultStrategyWeights are the built-in weights used when no custom weights are configured.
var defaultStrategyWeights = map[Strategy]int{
	StrategyDiff:  2,
	StrategyFull:  1,
	StrategyCross: 1,
}

// weightFor returns the configured weight for a strategy, defaulting to 1.
func (e *Engine) weightFor(s Strategy) int {
	if len(e.strategyWeights) > 0 {
		if w, ok := e.strategyWeights[s]; ok && w > 0 {
			return w
		}
		return 1
	}
	if w, ok := defaultStrategyWeights[s]; ok {
		return w
	}
	return 1
}

func (e *Engine) hasStrategy(s Strategy) bool {
	for _, st := range e.strategies {
		if st == s {
			return true
		}
	}
	return false
}
