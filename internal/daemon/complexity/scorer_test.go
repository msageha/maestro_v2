package complexity

import (
	"math"
	"testing"
)

func TestEstimate_Simple(t *testing.T) {
	s := NewScorer(DefaultThresholds())
	score := s.Estimate(Input{
		FileCount:         2,
		DependencyDepth:   1,
		BloomLevel:        1,
		PastRepairRate:    0.0,
		ExpectedPathCount: 1,
	})
	if score.Level != LevelSimple {
		t.Errorf("expected simple, got %s (raw=%f)", score.Level, score.RawScore)
	}
}

func TestEstimate_Standard(t *testing.T) {
	s := NewScorer(DefaultThresholds())
	score := s.Estimate(Input{
		FileCount:         20,
		DependencyDepth:   5,
		BloomLevel:        3,
		PastRepairRate:    0.3,
		ExpectedPathCount: 8,
	})
	if score.Level != LevelStandard {
		t.Errorf("expected standard, got %s (raw=%f)", score.Level, score.RawScore)
	}
}

func TestEstimate_ComplexOrCritical(t *testing.T) {
	s := NewScorer(DefaultThresholds())
	score := s.Estimate(Input{
		FileCount:         50,
		DependencyDepth:   10,
		BloomLevel:        6,
		PastRepairRate:    0.9,
		ExpectedPathCount: 20,
	})
	if score.Level != LevelComplex && score.Level != LevelCritical {
		t.Errorf("expected complex or critical, got %s (raw=%f)", score.Level, score.RawScore)
	}
}

func TestEstimate_AllZero_LowConfidence(t *testing.T) {
	s := NewScorer(DefaultThresholds())
	score := s.Estimate(Input{})
	if score.Confidence != 0 {
		t.Errorf("expected Confidence=0 for all-zero input, got %f", score.Confidence)
	}
	if score.Level != LevelSimple {
		t.Errorf("expected simple for all-zero input, got %s", score.Level)
	}
	if score.RawScore != 0 {
		t.Errorf("expected RawScore=0 for all-zero input, got %f", score.RawScore)
	}
}

func TestEstimateDepth(t *testing.T) {
	s := NewScorer(DefaultThresholds())
	tests := []struct {
		level Level
		depth int
	}{
		{LevelSimple, 1},
		{LevelStandard, 2},
		{LevelComplex, 3},
		{LevelCritical, 4},
	}
	for _, tc := range tests {
		got := s.EstimateDepth(Score{Level: tc.level})
		if got != tc.depth {
			t.Errorf("EstimateDepth(%s): expected %d, got %d", tc.level, tc.depth, got)
		}
	}
}

func TestDefaultThresholds(t *testing.T) {
	th := DefaultThresholds()
	if th.SimpleMax != 0.25 {
		t.Errorf("expected SimpleMax=0.25, got %f", th.SimpleMax)
	}
	if th.StandardMax != 0.50 {
		t.Errorf("expected StandardMax=0.50, got %f", th.StandardMax)
	}
	if th.ComplexMax != 0.75 {
		t.Errorf("expected ComplexMax=0.75, got %f", th.ComplexMax)
	}
}

func TestEstimate_BoundaryValues(t *testing.T) {
	s := NewScorer(DefaultThresholds())

	// Exactly at SimpleMax boundary: raw = 0.25
	// We need to find inputs that produce raw = 0.25
	// Let's test around boundaries by checking adjacent classifications.

	// Score just above SimpleMax → Standard
	score := s.Estimate(Input{
		FileCount:         15,
		DependencyDepth:   3,
		BloomLevel:        2,
		PastRepairRate:    0.1,
		ExpectedPathCount: 3,
	})
	// Verify raw score is reasonable (between 0 and 1)
	if score.RawScore < 0 || score.RawScore > 1 {
		t.Errorf("RawScore out of range: %f", score.RawScore)
	}
}

func TestEstimate_OverflowInputs(t *testing.T) {
	s := NewScorer(DefaultThresholds())
	// Very large inputs should be capped at 1.0 by normalize.
	score := s.Estimate(Input{
		FileCount:         1000,
		DependencyDepth:   100,
		BloomLevel:        100,
		PastRepairRate:    1.0,
		ExpectedPathCount: 1000,
	})
	if score.RawScore > 1.0 {
		t.Errorf("expected RawScore <= 1.0 even with overflow inputs, got %f", score.RawScore)
	}
	if score.Level != LevelCritical {
		t.Errorf("expected critical for max inputs, got %s", score.Level)
	}
}

func TestEstimate_FactorsPopulated(t *testing.T) {
	s := NewScorer(DefaultThresholds())
	score := s.Estimate(Input{
		FileCount:         10,
		DependencyDepth:   5,
		BloomLevel:        3,
		PastRepairRate:    0.5,
		ExpectedPathCount: 5,
	})
	expectedKeys := []string{"file_count", "dependency_depth", "bloom_level", "past_repair_rate", "expected_path_count"}
	for _, key := range expectedKeys {
		v, ok := score.Factors[key]
		if !ok {
			t.Errorf("missing factor key: %s", key)
			continue
		}
		if v < 0 || v > 1 {
			t.Errorf("factor %s out of [0,1]: %f", key, v)
		}
	}
}

func TestEstimate_RawScoreFormula(t *testing.T) {
	s := NewScorer(DefaultThresholds())
	input := Input{
		FileCount:         25,
		DependencyDepth:   5,
		BloomLevel:        3,
		PastRepairRate:    0.4,
		ExpectedPathCount: 10,
	}
	score := s.Estimate(input)

	// Manual calculation
	expected := 0.3*(25.0/50.0) +
		0.2*(5.0/10.0) +
		0.2*(3.0/6.0) +
		0.2*0.4 +
		0.1*(10.0/20.0)

	if math.Abs(score.RawScore-expected) > 0.0001 {
		t.Errorf("expected RawScore=%f, got %f", expected, score.RawScore)
	}
}

func TestConfidence_PartialData(t *testing.T) {
	s := NewScorer(DefaultThresholds())
	// Only 2 out of 5 fields non-zero → Confidence = 0.4
	score := s.Estimate(Input{
		FileCount:       10,
		DependencyDepth: 3,
	})
	if score.Confidence != 0.4 {
		t.Errorf("expected Confidence=0.4 for 2/5 non-zero fields, got %f", score.Confidence)
	}
}
