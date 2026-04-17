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

