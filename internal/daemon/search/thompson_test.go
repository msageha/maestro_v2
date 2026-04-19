package search

import (
	"math"
	"testing"
)

func TestSample_ReturnsValidDecision(t *testing.T) {
	t.Parallel()
	s := NewSampler(1.0, 1.0)

	// Sample many times to ensure both decisions appear
	widenCount := 0
	deepenCount := 0
	const iterations = 1000

	for range iterations {
		d := s.Sample()
		switch d {
		case DecisionWiden:
			widenCount++
		case DecisionDeepen:
			deepenCount++
		default:
			t.Fatalf("unexpected decision: %q", d)
		}
	}

	// With alpha=beta=1 (uniform), both should appear
	if widenCount == 0 {
		t.Fatal("expected some widen decisions with uniform prior")
	}
	if deepenCount == 0 {
		t.Fatal("expected some deepen decisions with uniform prior")
	}
}

func TestUpdate_WidenSuccess(t *testing.T) {
	t.Parallel()
	s := NewSampler(1.0, 1.0)

	s.Update(DecisionWiden, true)
	if s.Alpha() != 2.0 {
		t.Fatalf("expected alpha=2.0, got %f", s.Alpha())
	}
	if s.Beta() != 1.0 {
		t.Fatalf("expected beta=1.0, got %f", s.Beta())
	}
}

func TestUpdate_DeepenSuccess(t *testing.T) {
	t.Parallel()
	s := NewSampler(1.0, 1.0)

	s.Update(DecisionDeepen, true)
	if s.Alpha() != 1.0 {
		t.Fatalf("expected alpha=1.0, got %f", s.Alpha())
	}
	if s.Beta() != 2.0 {
		t.Fatalf("expected beta=2.0, got %f", s.Beta())
	}
}

func TestUpdate_FailureNoChange(t *testing.T) {
	t.Parallel()
	s := NewSampler(1.0, 1.0)

	s.Update(DecisionWiden, false)
	if s.Alpha() != 1.0 || s.Beta() != 1.0 {
		t.Fatalf("failure should not change parameters, got alpha=%f beta=%f", s.Alpha(), s.Beta())
	}
}

func TestSampler_BiasedAlpha(t *testing.T) {
	t.Parallel()
	// High alpha → should favor widen
	s := NewSampler(100.0, 1.0)

	widenCount := 0
	const iterations = 500

	for range iterations {
		if s.Sample() == DecisionWiden {
			widenCount++
		}
	}

	// With alpha=100, beta=1, the vast majority should be widen
	ratio := float64(widenCount) / float64(iterations)
	if ratio < 0.9 {
		t.Fatalf("expected >90%% widen with alpha=100, got %.1f%%", ratio*100)
	}
}

func TestSampler_BiasedBeta(t *testing.T) {
	t.Parallel()
	// High beta → should favor deepen
	s := NewSampler(1.0, 100.0)

	deepenCount := 0
	const iterations = 500

	for range iterations {
		if s.Sample() == DecisionDeepen {
			deepenCount++
		}
	}

	ratio := float64(deepenCount) / float64(iterations)
	if ratio < 0.9 {
		t.Fatalf("expected >90%% deepen with beta=100, got %.1f%%", ratio*100)
	}
}

func TestSampler_UpdateShiftsBias(t *testing.T) {
	t.Parallel()
	s := NewSampler(1.0, 1.0)

	// Simulate many widen successes
	for range 50 {
		s.Update(DecisionWiden, true)
	}

	// Should now strongly favor widen
	widenCount := 0
	const iterations = 200

	for range iterations {
		if s.Sample() == DecisionWiden {
			widenCount++
		}
	}

	ratio := float64(widenCount) / float64(iterations)
	if ratio < 0.8 {
		t.Fatalf("expected >80%% widen after 50 widen successes, got %.1f%%", ratio*100)
	}
}

func TestGammaSample_InvalidInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		alpha float64
	}{
		{"zero", 0},
		{"negative", -1.0},
		{"large_negative", -100.0},
		{"NaN", math.NaN()},
		{"positive_inf", math.Inf(1)},
		{"negative_inf", math.Inf(-1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := gammaSample(tt.alpha)
			if result != 0 {
				t.Fatalf("gammaSample(%v) = %v, want 0", tt.alpha, result)
			}
		})
	}
}

func TestGammaSample_AlphaLessThanOne(t *testing.T) {
	t.Parallel()

	// alpha < 1 should produce valid positive samples via the boost transform.
	alphas := []float64{0.1, 0.3, 0.5, 0.7, 0.99}
	for _, alpha := range alphas {
		sum := 0.0
		const n = 500
		for range n {
			v := gammaSample(alpha)
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("gammaSample(%v) returned invalid value: %v", alpha, v)
			}
			sum += v
		}
		mean := sum / float64(n)
		// Gamma(alpha, 1) has mean = alpha. Allow generous tolerance for small n.
		if mean < alpha*0.1 || mean > alpha*5 {
			t.Fatalf("gammaSample(%v): mean=%v, expected near %v", alpha, mean, alpha)
		}
	}
}

func TestGammaSample_AlphaGreaterThanOne(t *testing.T) {
	t.Parallel()

	alphas := []float64{1.0, 2.0, 5.0, 10.0}
	for _, alpha := range alphas {
		sum := 0.0
		const n = 500
		for range n {
			v := gammaSample(alpha)
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("gammaSample(%v) returned invalid value: %v", alpha, v)
			}
			sum += v
		}
		mean := sum / float64(n)
		if mean < alpha*0.3 || mean > alpha*3 {
			t.Fatalf("gammaSample(%v): mean=%v, expected near %v", alpha, mean, alpha)
		}
	}
}

func TestGammaSample_VerySmallAlpha(t *testing.T) {
	t.Parallel()

	// Extremely small alpha should not panic or infinite-loop.
	for range 100 {
		v := gammaSample(1e-10)
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("gammaSample(1e-10) returned invalid value: %v", v)
		}
	}
}

func TestBetaSample_InvalidGammaFallback(t *testing.T) {
	t.Parallel()

	// When both gamma samples return 0 (invalid alpha/beta), betaSample returns 0.5.
	result := betaSample(0, 0)
	if result != 0.5 {
		t.Fatalf("betaSample(0, 0) = %v, want 0.5", result)
	}

	result = betaSample(-1, -1)
	if result != 0.5 {
		t.Fatalf("betaSample(-1, -1) = %v, want 0.5", result)
	}
}

func TestMaxGammaIterationsConstant(t *testing.T) {
	t.Parallel()

	if maxGammaIterations < 256 {
		t.Fatalf("maxGammaIterations = %d, should be at least 256 for safety", maxGammaIterations)
	}
}
