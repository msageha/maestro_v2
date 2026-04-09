// Package complexity provides task complexity estimation (C-6).
// It operates as a Daemon pre-processor, computing structural metrics
// to determine subtask decomposition depth.
package complexity

// Level represents a complexity classification.
type Level string

const (
	LevelSimple   Level = "simple"
	LevelStandard Level = "standard"
	LevelComplex  Level = "complex"
	LevelCritical Level = "critical"
)

// Input holds the structural indicators used for complexity estimation.
// All fields are mechanical metrics (S5-1: no LLM subjectivity).
type Input struct {
	FileCount         int
	DependencyDepth   int
	BloomLevel        int
	PastRepairRate    float64
	ExpectedPathCount int
}

// Score is the result of complexity estimation.
type Score struct {
	Level      Level
	RawScore   float64
	Confidence float64
	Factors    map[string]float64
}

// Thresholds defines the boundaries between complexity levels.
type Thresholds struct {
	SimpleMax   float64
	StandardMax float64
	ComplexMax  float64
}

// Scorer estimates task complexity from structural indicators.
type Scorer struct {
	thresholds Thresholds
}

// NewScorer creates a Scorer with the given thresholds.
func NewScorer(thresholds Thresholds) *Scorer {
	return &Scorer{thresholds: thresholds}
}

// DefaultThresholds returns the baseline threshold values:
// SimpleMax=0.25, StandardMax=0.50, ComplexMax=0.75.
func DefaultThresholds() Thresholds {
	return Thresholds{
		SimpleMax:   0.25,
		StandardMax: 0.50,
		ComplexMax:  0.75,
	}
}

// Estimate computes a complexity Score from the given Input.
//
// RawScore is a normalized weighted sum (S5-1: mechanical metrics only):
//
//	0.3 * normalize(FileCount, 50) +
//	0.2 * normalize(DependencyDepth, 10) +
//	0.2 * normalize(BloomLevel, 6) +
//	0.2 * PastRepairRate +
//	0.1 * normalize(ExpectedPathCount, 20)
//
// Confidence reflects data completeness: factors with zero value
// reduce confidence proportionally.
func (s *Scorer) Estimate(input Input) Score {
	factors := map[string]float64{
		"file_count":          normalize(float64(input.FileCount), 50),
		"dependency_depth":    normalize(float64(input.DependencyDepth), 10),
		"bloom_level":         normalize(float64(input.BloomLevel), 6),
		"past_repair_rate":    clamp(input.PastRepairRate, 0, 1),
		"expected_path_count": normalize(float64(input.ExpectedPathCount), 20),
	}

	weights := map[string]float64{
		"file_count":          0.3,
		"dependency_depth":    0.2,
		"bloom_level":         0.2,
		"past_repair_rate":    0.2,
		"expected_path_count": 0.1,
	}

	var raw float64
	for k, w := range weights {
		raw += w * factors[k]
	}

	confidence := computeConfidence(input)
	level := s.classify(raw)

	return Score{
		Level:      level,
		RawScore:   raw,
		Confidence: confidence,
		Factors:    factors,
	}
}

// EstimateDepth returns the recommended subtask decomposition depth
// based on the complexity level (C-6-2):
//
//	simple→1, standard→2, complex→3, critical→4
func (s *Scorer) EstimateDepth(score Score) int {
	switch score.Level {
	case LevelSimple:
		return 1
	case LevelStandard:
		return 2
	case LevelComplex:
		return 3
	case LevelCritical:
		return 4
	default:
		return 2
	}
}

// classify maps a raw score to a Level using the configured thresholds.
func (s *Scorer) classify(raw float64) Level {
	switch {
	case raw <= s.thresholds.SimpleMax:
		return LevelSimple
	case raw <= s.thresholds.StandardMax:
		return LevelStandard
	case raw <= s.thresholds.ComplexMax:
		return LevelComplex
	default:
		return LevelCritical
	}
}

// normalize maps a value to [0, 1] by dividing by max.
// Values exceeding max are capped at 1.0.
func normalize(value, max float64) float64 {
	if max <= 0 {
		return 0
	}
	return clamp(value/max, 0, 1)
}

// clamp restricts v to [lo, hi].
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// computeConfidence returns a value in [0, 1] reflecting how many input
// fields are non-zero. Each zero field reduces confidence by 0.2.
func computeConfidence(input Input) float64 {
	nonZero := 0
	total := 5
	if input.FileCount > 0 {
		nonZero++
	}
	if input.DependencyDepth > 0 {
		nonZero++
	}
	if input.BloomLevel > 0 {
		nonZero++
	}
	if input.PastRepairRate > 0 {
		nonZero++
	}
	if input.ExpectedPathCount > 0 {
		nonZero++
	}
	return float64(nonZero) / float64(total)
}
