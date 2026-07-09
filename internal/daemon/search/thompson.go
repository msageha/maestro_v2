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

// betaArm holds the Beta posterior parameters of a single bandit arm:
// Beta(alpha, beta) with alpha = prior + observed successes and
// beta = prior + observed failures.
type betaArm struct {
	alpha float64
	beta  float64
}

// Sampler implements standard Thompson Sampling for width-vs-depth
// exploration decisions. Each arm (widen, deepen) keeps an independent
// Beta(successes+prior, failures+prior) posterior; successes increment the
// arm's alpha, failures increment the arm's beta.
type Sampler struct {
	widen  betaArm
	deepen betaArm
	mu     sync.Mutex
}

// NewSampler creates a Thompson Sampler with initial per-arm priors.
// alpha is the widen arm's prior success weight, beta is the deepen arm's
// prior success weight (both failure priors start at 1): a larger alpha
// biases toward "widen", a larger beta biases toward "deepen".
func NewSampler(alpha, beta float64) *Sampler {
	return &Sampler{
		widen:  betaArm{alpha: alpha, beta: 1},
		deepen: betaArm{alpha: beta, beta: 1},
	}
}

// Sample draws from each arm's Beta posterior and returns the decision
// whose sampled success probability is higher.
func (s *Sampler) Sample() Decision {
	s.mu.Lock()
	defer s.mu.Unlock()

	widenSample := betaSample(s.widen.alpha, s.widen.beta)
	deepenSample := betaSample(s.deepen.alpha, s.deepen.beta)
	if widenSample > deepenSample {
		return DecisionWiden
	}
	return DecisionDeepen
}

// Update adjusts the chosen arm's posterior based on the observed outcome:
// success increments the arm's alpha, failure increments the arm's beta
// (standard Thompson Sampling posterior update).
func (s *Sampler) Update(decision Decision, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var arm *betaArm
	switch decision {
	case DecisionWiden:
		arm = &s.widen
	case DecisionDeepen:
		arm = &s.deepen
	default:
		return
	}

	if success {
		arm.alpha++
	} else {
		arm.beta++
	}
}

// Alpha returns the widen arm's alpha parameter (prior + observed successes).
func (s *Sampler) Alpha() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.widen.alpha
}

// Beta returns the deepen arm's alpha parameter (prior + observed successes).
func (s *Sampler) Beta() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deepen.alpha
}

// WidenParams returns the widen arm's Beta posterior parameters.
func (s *Sampler) WidenParams() (alpha, beta float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.widen.alpha, s.widen.beta
}

// DeepenParams returns the deepen arm's Beta posterior parameters.
func (s *Sampler) DeepenParams() (alpha, beta float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deepen.alpha, s.deepen.beta
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
