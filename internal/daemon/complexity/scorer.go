// Package complexity provides task complexity estimation (C-6).
// It operates as a Daemon pre-processor, computing structural metrics
// to determine subtask decomposition depth.
package complexity

// Level represents a complexity classification
// ("simple" / "standard" / "complex" / "critical").
// This package defines its own type to avoid coupling the daemon scoring
// layer to the model package (Dependency Inversion Principle).
type Level string

const (
	// LevelSimple indicates a task with minimal structural complexity.
	LevelSimple Level = "simple"
	// LevelStandard indicates a task with typical structural complexity.
	LevelStandard Level = "standard"
	// LevelComplex indicates a task requiring deeper decomposition.
	LevelComplex Level = "complex"
	// LevelCritical indicates a task at the maximum complexity threshold.
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

// FileThresholds defines explicit file-count boundaries for complexity levels.
// A configured file-count level is treated as a lower bound: other structural
// signals may still raise the level, but they will not lower a task below the
// operator-provided file threshold classification.
type FileThresholds struct {
	SimpleMaxFiles   int
	StandardMaxFiles int
	ComplexMaxFiles  int
}

// Scorer estimates task complexity from structural indicators.
type Scorer struct {
	thresholds     Thresholds
	fileThresholds *FileThresholds
}

// NewScorer creates a Scorer with the given thresholds.
func NewScorer(thresholds Thresholds) *Scorer {
	return &Scorer{thresholds: thresholds}
}

// NewScorerWithFileThresholds creates a Scorer that also honors configured
// file-count thresholds from complexity.thresholds.
func NewScorerWithFileThresholds(thresholds Thresholds, fileThresholds FileThresholds) *Scorer {
	ft := fileThresholds
	return &Scorer{thresholds: thresholds, fileThresholds: &ft}
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
	// Fixed-order slice ensures deterministic floating-point summation.
	// Map iteration order is random in Go, which causes non-deterministic
	// results due to floating-point addition not being commutative.
	type weightedFactor struct {
		name   string
		value  float64
		weight float64
	}
	wfs := []weightedFactor{
		{"file_count", normalize(float64(input.FileCount), 50), 0.3},
		{"dependency_depth", normalize(float64(input.DependencyDepth), 10), 0.2},
		{"bloom_level", normalize(float64(input.BloomLevel), 6), 0.2},
		{"past_repair_rate", clamp(input.PastRepairRate, 0, 1), 0.2},
		{"expected_path_count", normalize(float64(input.ExpectedPathCount), 20), 0.1},
	}

	factors := make(map[string]float64, len(wfs))
	var raw float64
	for _, wf := range wfs {
		factors[wf.name] = wf.value
		raw += wf.weight * wf.value
	}

	confidence := computeConfidence(input)
	level := s.classifyInput(raw, input)

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

func (s *Scorer) classifyInput(raw float64, input Input) Level {
	rawLevel := s.classify(raw)
	if s.fileThresholds == nil || input.FileCount <= 0 {
		return rawLevel
	}
	return maxLevel(rawLevel, s.classifyFileCount(input.FileCount))
}

func (s *Scorer) classifyFileCount(fileCount int) Level {
	ft := s.fileThresholds
	switch {
	case fileCount <= ft.SimpleMaxFiles:
		return LevelSimple
	case fileCount <= ft.StandardMaxFiles:
		return LevelStandard
	case fileCount <= ft.ComplexMaxFiles:
		return LevelComplex
	default:
		return LevelCritical
	}
}

func maxLevel(a, b Level) Level {
	if levelRank(b) > levelRank(a) {
		return b
	}
	return a
}

func levelRank(l Level) int {
	switch l {
	case LevelSimple:
		return 0
	case LevelStandard:
		return 1
	case LevelComplex:
		return 2
	case LevelCritical:
		return 3
	default:
		return 0
	}
}

// normalize maps a value to [0, 1] by dividing by maxVal.
// Values exceeding maxVal are capped at 1.0.
func normalize(value, maxVal float64) float64 {
	if maxVal <= 0 {
		return 0
	}
	return clamp(value/maxVal, 0, 1)
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
