package model

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/ptr"
)

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
