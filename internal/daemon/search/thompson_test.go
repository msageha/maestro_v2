package search

import (
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
