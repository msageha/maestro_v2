package model

// Complexity level constants.
const (
	ComplexityLevelSimple   = "simple"
	ComplexityLevelStandard = "standard"
	ComplexityLevelComplex  = "complex"
	ComplexityLevelCritical = "critical"
)

// ValidateComplexityLevel reports whether s is a known complexity level.
func ValidateComplexityLevel(s string) bool {
	switch s {
	case ComplexityLevelSimple, ComplexityLevelStandard, ComplexityLevelComplex, ComplexityLevelCritical:
		return true
	}
	return false
}

// ComplexityScore captures the assessed complexity of a task.
type ComplexityScore struct {
	Level           string  `yaml:"level" json:"level"`
	FileCount       int     `yaml:"file_count" json:"file_count"`
	DependencyDepth int     `yaml:"dependency_depth" json:"dependency_depth"`
	PastRepairRate  float64 `yaml:"past_repair_rate" json:"past_repair_rate"`
	Confidence      float64 `yaml:"confidence" json:"confidence"`
}
