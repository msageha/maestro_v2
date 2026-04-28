package model

import "testing"

func TestValidateProfileLevel(t *testing.T) {
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
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := ValidateProfileLevel(tt.input); got != tt.want {
				t.Errorf("ValidateProfileLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDefaultFeatureProfiles(t *testing.T) {
	profiles := DefaultFeatureProfiles()

	// Should have all 4 levels
	for _, level := range []string{"simple", "standard", "complex", "critical"} {
		if _, ok := profiles[level]; !ok {
			t.Errorf("missing profile for level %q", level)
		}
	}

	// Simple level: all features off
	simple := profiles[ProfileLevelSimple]
	if simple.EffectiveCrossAgentReview() {
		t.Errorf("simple.CrossAgentReview = %v, want false", simple.EffectiveCrossAgentReview())
	}
	if simple.EffectiveExploratoryOptimization() {
		t.Error("simple.ExploratoryOptimization should be false")
	}

	// Standard level: adaptive_model_selection only
	std := profiles[ProfileLevelStandard]
	if std.EffectiveCrossAgentReview() {
		t.Errorf("standard.CrossAgentReview = %v, want false", std.EffectiveCrossAgentReview())
	}
	if !std.EffectiveAdaptiveModelSelection() {
		t.Error("standard.AdaptiveModelSelection should be true")
	}
	if std.EffectiveAdaptiveDepth() {
		t.Error("standard.AdaptiveDepth should be false")
	}

	// Complex level: adaptive_model_selection, cross_agent_review, adaptive_depth
	cplx := profiles[ProfileLevelComplex]
	if !cplx.EffectiveCrossAgentReview() {
		t.Errorf("complex.CrossAgentReview = %v, want true", cplx.EffectiveCrossAgentReview())
	}
	if !cplx.EffectiveAdaptiveModelSelection() {
		t.Error("complex.AdaptiveModelSelection should be true")
	}
	if !cplx.EffectiveAdaptiveDepth() {
		t.Error("complex.AdaptiveDepth should be true")
	}
	if cplx.EffectiveExploratoryOptimization() {
		t.Error("complex.ExploratoryOptimization should be false")
	}
	if cplx.EffectiveEvolutionaryQuality() {
		t.Error("complex.EvolutionaryQuality should be false")
	}
	if cplx.EffectiveSelfImprovement() {
		t.Error("complex.SelfImprovement should be false")
	}
}

func TestIsFeatureEnabled(t *testing.T) {
	profiles := DefaultFeatureProfiles()

	tests := []struct {
		level   string
		feature string
		want    bool
	}{
		{"simple", "cross_agent_review", false},
		{"simple", "exploratory_optimization", false},
		{"standard", "adaptive_model_selection", true},
		{"standard", "adaptive_depth", false},
		{"standard", "cross_agent_review", false},
		{"standard", "evolutionary_quality", false},
		{"complex", "cross_agent_review", true},
		{"complex", "adaptive_model_selection", true},
		{"complex", "adaptive_depth", true},
		{"complex", "self_improvement", false},
		{"complex", "exploratory_optimization", false},
		{"complex", "evolutionary_quality", false},
		{"critical", "evolutionary_quality", true},
		{"critical", "self_improvement", true},
		{"critical", "exploratory_optimization", true},
		{"unknown_level", "cross_agent_review", false},
		{"simple", "unknown_feature", false},
	}
	for _, tt := range tests {
		t.Run(tt.level+"_"+tt.feature, func(t *testing.T) {
			if got := IsFeatureEnabled(profiles, tt.level, tt.feature); got != tt.want {
				t.Errorf("IsFeatureEnabled(%q, %q) = %v, want %v", tt.level, tt.feature, got, tt.want)
			}
		})
	}
}

func TestFeatureProfile_Defaults(t *testing.T) {
	fp := FeatureProfile{}
	if fp.EffectiveCrossAgentReview() {
		t.Errorf("default CrossAgentReview = %v, want false", fp.EffectiveCrossAgentReview())
	}
	if fp.EffectiveExploratoryOptimization() {
		t.Error("default ExploratoryOptimization should be false")
	}
	if fp.EffectiveEvolutionaryQuality() {
		t.Error("default EvolutionaryQuality should be false")
	}
	if fp.EffectiveAdaptiveModelSelection() {
		t.Error("default AdaptiveModelSelection should be false")
	}
	if fp.EffectiveSelfImprovement() {
		t.Error("default SelfImprovement should be false")
	}
	if fp.EffectiveAdaptiveDepth() {
		t.Error("default AdaptiveDepth should be false")
	}
}
