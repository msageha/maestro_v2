package model

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/ptr"
)

func TestValidateComplexityLevel(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"simple", true},
		{"standard", true},
		{"complex", true},
		{"critical", true},
		{"unknown", false},
		{"", false},
		{"Simple", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := ValidateComplexityLevel(tt.input); got != tt.want {
				t.Errorf("ValidateComplexityLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestComplexityScore_Fields(t *testing.T) {
	cs := ComplexityScore{
		Level:           ComplexityLevelComplex,
		FileCount:       15,
		DependencyDepth: 4,
		PastRepairRate:  0.3,
		Confidence:      0.85,
	}
	if cs.Level != "complex" {
		t.Errorf("Level = %q, want complex", cs.Level)
	}
	if cs.FileCount != 15 {
		t.Errorf("FileCount = %d, want 15", cs.FileCount)
	}
	if cs.DependencyDepth != 4 {
		t.Errorf("DependencyDepth = %d, want 4", cs.DependencyDepth)
	}
	if cs.Confidence != 0.85 {
		t.Errorf("Confidence = %v, want 0.85", cs.Confidence)
	}
}

func TestComplexityThresholds_Defaults(t *testing.T) {
	ct := ComplexityThresholds{}
	if v := ct.EffectiveSimpleMaxFiles(); v != 3 {
		t.Errorf("EffectiveSimpleMaxFiles() = %d, want 3", v)
	}
	if v := ct.EffectiveStandardMaxFiles(); v != 10 {
		t.Errorf("EffectiveStandardMaxFiles() = %d, want 10", v)
	}
	if v := ct.EffectiveComplexMaxFiles(); v != 30 {
		t.Errorf("EffectiveComplexMaxFiles() = %d, want 30", v)
	}
}

func TestComplexityThresholds_Configured(t *testing.T) {
	ct := ComplexityThresholds{
		SimpleMaxFiles:   ptr.Int(5),
		StandardMaxFiles: ptr.Int(15),
		ComplexMaxFiles:  ptr.Int(50),
	}
	if v := ct.EffectiveSimpleMaxFiles(); v != 5 {
		t.Errorf("EffectiveSimpleMaxFiles() = %d, want 5", v)
	}
	if v := ct.EffectiveStandardMaxFiles(); v != 15 {
		t.Errorf("EffectiveStandardMaxFiles() = %d, want 15", v)
	}
	if v := ct.EffectiveComplexMaxFiles(); v != 50 {
		t.Errorf("EffectiveComplexMaxFiles() = %d, want 50", v)
	}
}
