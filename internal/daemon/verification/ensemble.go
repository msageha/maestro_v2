// Package verification provides multi-perspective verification ensemble
// for task output validation (C-3).
package verification

import (
	"fmt"
	"sync"
	"time"
)

// defaultPerspectiveWeight is assigned to perspectives not registered via
// AddPerspective. Using 1.0 (critical) ensures unknown perspectives fail-safe:
// an unregistered viewpoint that fails will block the overall result, forcing
// operators to explicitly configure it before it can soft-fail.
const defaultPerspectiveWeight = 1.0

// Perspective defines a single verification viewpoint with associated commands
// and a weight indicating its importance in the overall assessment.
type Perspective struct {
	Name     string
	Commands []string
	Weight   float64
}

// PerspectiveResult holds the outcome of running a single perspective's verification.
type PerspectiveResult struct {
	Name     string
	Passed   bool
	Output   string
	Duration time.Duration
}

// AggregatedResult holds the weighted aggregation of all perspective results.
type AggregatedResult struct {
	TotalScore float64
	MaxScore   float64
	Passed     bool
	Results    []PerspectiveResult
}

// Verifier manages a set of perspectives and aggregates their results.
type Verifier struct {
	perspectives   []Perspective
	maxAutoRetries int
	mu             sync.RWMutex
}

// NewVerifier creates a Verifier pre-loaded with DefaultPerspectives.
func NewVerifier() *Verifier {
	v := &Verifier{}
	v.perspectives = v.DefaultPerspectives()
	return v
}

// SetPerspectives replaces the verifier perspective set. Invalid perspectives
// with non-positive weights are ignored by returning an error before mutation.
func (v *Verifier) SetPerspectives(perspectives []Perspective) error {
	for _, p := range perspectives {
		if p.Weight <= 0 {
			return fmt.Errorf("perspective %q: weight must be positive, got %f", p.Name, p.Weight)
		}
	}
	cp := make([]Perspective, len(perspectives))
	copy(cp, perspectives)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.perspectives = cp
	return nil
}

// Perspectives returns a snapshot of configured verification perspectives.
func (v *Verifier) Perspectives() []Perspective {
	v.mu.RLock()
	defer v.mu.RUnlock()
	cp := make([]Perspective, len(v.perspectives))
	copy(cp, v.perspectives)
	return cp
}

// SetMaxAutoRetries stores the retry budget used by callers that implement
// extended verification retry loops. Negative values are treated as zero.
func (v *Verifier) SetMaxAutoRetries(n int) {
	if n < 0 {
		n = 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.maxAutoRetries = n
}

// MaxAutoRetries returns the configured extended verification retry budget.
func (v *Verifier) MaxAutoRetries() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.maxAutoRetries
}

// AddPerspective appends a custom perspective to the verifier.
// Returns an error if Weight is <= 0.
func (v *Verifier) AddPerspective(p Perspective) error {
	if p.Weight <= 0 {
		return fmt.Errorf("perspective %q: weight must be positive, got %f", p.Name, p.Weight)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.perspectives = append(v.perspectives, p)
	return nil
}

// DefaultPerspectives returns the four baseline verification viewpoints:
// build (1.0), lint (0.8), test (1.0), typecheck (0.9).
func (v *Verifier) DefaultPerspectives() []Perspective {
	return []Perspective{
		{Name: "build", Commands: []string{"go build ./..."}, Weight: 1.0},
		{Name: "lint", Commands: []string{"golangci-lint run ./..."}, Weight: 0.8},
		{Name: "test", Commands: []string{"go test ./..."}, Weight: 1.0},
		{Name: "typecheck", Commands: []string{"go vet ./..."}, Weight: 0.9},
	}
}

// Aggregate computes a weighted score from the provided perspective results.
//
// Scoring rules (mechanical only, no LLM override per S5-1):
//   - TotalScore = sum(weight * passed) / sum(weight), where passed = 1 if true, 0 otherwise
//   - Passed = false when any perspective with Weight >= 1.0 fails (critical perspectives)
//   - Non-critical perspectives (Weight < 1.0) contribute to TotalScore but do not force Passed = false
//   - Each perspective is evaluated independently (not majority-vote, per S5-2)
//
// Empty results yield AggregatedResult{Passed: true, TotalScore: 0}.
func (v *Verifier) Aggregate(results []PerspectiveResult) AggregatedResult {
	if len(results) == 0 {
		return AggregatedResult{Passed: true, TotalScore: 0, Results: results}
	}

	v.mu.RLock()
	weightMap := make(map[string]float64, len(v.perspectives))
	for _, p := range v.perspectives {
		weightMap[p.Name] = p.Weight
	}
	v.mu.RUnlock()

	var totalWeight float64
	var weightedSum float64
	passed := true

	for _, r := range results {
		w, ok := weightMap[r.Name]
		if !ok {
			w = defaultPerspectiveWeight
		}
		totalWeight += w
		if r.Passed {
			weightedSum += w
		} else if w >= 1.0 {
			passed = false
		}
	}

	score := 0.0
	if totalWeight > 0 {
		score = weightedSum / totalWeight
	}

	return AggregatedResult{
		TotalScore: score,
		MaxScore:   totalWeight,
		Passed:     passed,
		Results:    results,
	}
}

// ShouldRetry implements C-3-2 probabilistic retry logic.
// A retry is recommended when the failure fingerprint has not been seen before
// (i.e., it is a novel error). If the fingerprint already appears in
// seenFingerprints, retry is suppressed (S2-1 Failure Fingerprint linkage).
func (v *Verifier) ShouldRetry(result AggregatedResult, fingerprint string, seenFingerprints []string) bool {
	if result.Passed {
		return false
	}
	for _, seen := range seenFingerprints {
		if seen == fingerprint {
			return false
		}
	}
	return true
}
