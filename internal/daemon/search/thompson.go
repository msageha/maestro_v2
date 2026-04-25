package search

import (
	"math"
	"math/rand/v2"
	"sync"
)

// Decision represents whether to widen (explore new approaches) or deepen (improve existing).
type Decision string

const (
	// DecisionWiden directs the search to explore new branches.
	DecisionWiden Decision = "widen"
	// DecisionDeepen directs the search to improve an existing branch.
	DecisionDeepen Decision = "deepen"
)

// Sampler implements Thompson Sampling for width-vs-depth exploration decisions.
type Sampler struct {
	alpha float64
	beta  float64
	mu    sync.Mutex
}

// NewSampler creates a Thompson Sampler with initial Beta distribution parameters.
// alpha biases toward "widen", beta biases toward "deepen".
func NewSampler(alpha, beta float64) *Sampler {
	return &Sampler{
		alpha: alpha,
		beta:  beta,
	}
}

// Sample draws from the Beta(alpha, beta) distribution and returns a decision.
// If the sampled value exceeds 0.5, returns "widen"; otherwise "deepen".
func (s *Sampler) Sample() Decision {
	s.mu.Lock()
	defer s.mu.Unlock()

	sample := betaSample(s.alpha, s.beta)
	if sample > 0.5 {
		return DecisionWiden
	}
	return DecisionDeepen
}

// Update adjusts the distribution based on observed success/failure.
// For "widen" + success: alpha++. For "deepen" + success: beta++.
// Failures do not update (standard Thompson Sampling reward model).
func (s *Sampler) Update(decision Decision, success bool) {
	if !success {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	switch decision {
	case DecisionWiden:
		s.alpha++
	case DecisionDeepen:
		s.beta++
	}
}

// Alpha returns the current alpha parameter.
func (s *Sampler) Alpha() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alpha
}

// Beta returns the current beta parameter.
func (s *Sampler) Beta() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beta
}

// betaSample draws a sample from Beta(alpha, beta) using the Gamma distribution method.
func betaSample(alpha, beta float64) float64 {
	// Beta(a,b) = Gamma(a,1) / (Gamma(a,1) + Gamma(b,1))
	x := gammaSample(alpha)
	y := gammaSample(beta)
	if x+y == 0 {
		return 0.5
	}
	return x / (x + y)
}

// maxGammaIterations is the safety bound for the Marsaglia-Tsang rejection loop.
// In a correct implementation with valid parameters, hitting this limit is
// effectively impossible; it exists only as a guard against degenerate inputs.
const maxGammaIterations = 1024

// gammaSample draws a sample from Gamma(alpha, 1) using Marsaglia and Tsang's method.
// Returns 0 for invalid inputs (alpha <= 0, NaN, Inf). When combined with
// betaSample, a zero return causes betaSample to fall back to 0.5 (unbiased).
func gammaSample(alpha float64) float64 {
	if alpha <= 0 || math.IsNaN(alpha) || math.IsInf(alpha, 0) {
		return 0
	}

	// For alpha < 1, use the relation: Gamma(alpha) = Gamma(alpha+1) * U^(1/alpha).
	// Computed iteratively instead of recursively to avoid stack overflow in Go
	// (which does not optimize tail recursion).
	boost := 1.0
	if alpha < 1 {
		boost = math.Pow(rand.Float64(), 1.0/alpha) //nolint:gosec // math/rand is appropriate for statistical sampling
		alpha++
	}

	d := alpha - 1.0/3.0
	c := 1.0 / math.Sqrt(9.0*d)

	for range maxGammaIterations {
		x := rand.NormFloat64() //nolint:gosec // math/rand is appropriate for statistical sampling
		v := 1.0 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rand.Float64() //nolint:gosec // math/rand is appropriate for statistical sampling

		if u < 1.0-0.0331*(x*x)*(x*x) {
			return d * v * boost
		}
		if math.Log(u) < 0.5*x*x+d*(1.0-v+math.Log(v)) {
			return d * v * boost
		}
	}

	// Max iterations exceeded — return 0 as a safe fallback.
	return 0
}
