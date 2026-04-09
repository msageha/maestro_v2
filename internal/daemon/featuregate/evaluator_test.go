package featuregate

import "testing"

func TestDefaultProfiles_SimpleAllDisabled(t *testing.T) {
	e := NewEvaluator()
	p := e.Evaluate(LevelSimple)

	for _, feat := range allFeatures {
		if p.EnabledFeatures[feat] {
			t.Errorf("simple profile: feature %q should be disabled", feat)
		}
	}
}

func TestDefaultProfiles_CriticalAllEnabled(t *testing.T) {
	e := NewEvaluator()
	p := e.Evaluate(LevelCritical)

	for _, feat := range allFeatures {
		if !p.EnabledFeatures[feat] {
			t.Errorf("critical profile: feature %q should be enabled", feat)
		}
	}
}

func TestEvaluate_UnknownLevel_FallsBackToSimple(t *testing.T) {
	e := NewEvaluator()
	p := e.Evaluate(ProfileLevel("unknown"))

	if p.Level != LevelSimple {
		t.Errorf("expected simple fallback, got %q", p.Level)
	}
	for _, feat := range allFeatures {
		if p.EnabledFeatures[feat] {
			t.Errorf("fallback profile: feature %q should be disabled", feat)
		}
	}
}

func TestIsEnabled_StandardAdaptiveModelSelection(t *testing.T) {
	e := NewEvaluator()
	if !e.IsEnabled(LevelStandard, FeatureAdaptiveModelSelection) {
		t.Error("standard: adaptive_model_selection should be enabled")
	}
}

func TestIsEnabled_SimpleEvolutionaryQualityDisabled(t *testing.T) {
	e := NewEvaluator()
	if e.IsEnabled(LevelSimple, FeatureEvolutionaryQuality) {
		t.Error("simple: evolutionary_quality should be disabled")
	}
}

func TestIsEnabled_ComplexProfile(t *testing.T) {
	e := NewEvaluator()

	// Enabled in complex.
	for _, feat := range []Feature{FeatureAdaptiveModelSelection, FeatureCrossAgentReview, FeatureAdaptiveDepth} {
		if !e.IsEnabled(LevelComplex, feat) {
			t.Errorf("complex: feature %q should be enabled", feat)
		}
	}

	// Disabled in complex.
	for _, feat := range []Feature{FeatureExploratoryOpt, FeatureEvolutionaryQuality, FeatureSelfImprovement} {
		if e.IsEnabled(LevelComplex, feat) {
			t.Errorf("complex: feature %q should be disabled", feat)
		}
	}
}

func TestGetLevel_FileCountBased(t *testing.T) {
	e := NewEvaluator()

	tests := []struct {
		fileCount int
		depCount  int
		want      ProfileLevel
	}{
		{1, 0, LevelSimple},
		{3, 0, LevelSimple},
		{4, 0, LevelStandard},
		{7, 3, LevelStandard},
		{10, 0, LevelStandard},
		{11, 0, LevelComplex},
		{20, 5, LevelComplex},
		{30, 0, LevelComplex},
		{31, 0, LevelCritical},
		{50, 10, LevelCritical},
	}

	for _, tt := range tests {
		got := e.GetLevel(tt.fileCount, tt.depCount)
		if got != tt.want {
			t.Errorf("GetLevel(%d, %d) = %q, want %q", tt.fileCount, tt.depCount, got, tt.want)
		}
	}
}

func TestLoadProfiles_CustomConfig(t *testing.T) {
	e := NewEvaluator()

	custom := map[string]map[string]interface{}{
		"simple": {
			"adaptive_model_selection": true, // Override: enable in simple.
		},
		"standard": {
			"adaptive_model_selection": true,
			"cross_agent_review":       true,
		},
	}
	e.LoadProfiles(custom)

	if !e.IsEnabled(LevelSimple, FeatureAdaptiveModelSelection) {
		t.Error("custom simple: adaptive_model_selection should be enabled")
	}
	if !e.IsEnabled(LevelStandard, FeatureCrossAgentReview) {
		t.Error("custom standard: cross_agent_review should be enabled")
	}

	// Critical no longer exists after LoadProfiles replaces all profiles.
	p := e.Evaluate(LevelCritical)
	// Should fall back to simple (which is now the custom simple).
	if p.Level != LevelSimple {
		t.Errorf("expected fallback to simple, got %q", p.Level)
	}
}

func TestOrchestratorOverrideSimulation(t *testing.T) {
	e := NewEvaluator()

	// Simulate orchestrator override: load custom profiles on top of defaults.
	override := map[string]map[string]interface{}{
		"simple": {
			"self_improvement": true, // Orchestrator enables a feature for simple.
		},
		"standard": {
			"adaptive_model_selection": true,
			"self_improvement":         true,
		},
		"complex": {
			"adaptive_model_selection": true,
			"cross_agent_review":       true,
			"adaptive_depth":           true,
			"self_improvement":         true,
		},
		"critical": {
			"cross_agent_review":       true,
			"exploratory_optimization": true,
			"evolutionary_quality":     true,
			"adaptive_model_selection": true,
			"self_improvement":         true,
			"adaptive_depth":           true,
		},
	}
	e.LoadProfiles(override)

	// Verify orchestrator override took effect.
	if !e.IsEnabled(LevelSimple, FeatureSelfImprovement) {
		t.Error("override simple: self_improvement should be enabled")
	}
	if !e.IsEnabled(LevelCritical, FeatureEvolutionaryQuality) {
		t.Error("override critical: evolutionary_quality should be enabled")
	}
}

func TestGetLevel_ZeroCounts(t *testing.T) {
	e := NewEvaluator()
	if got := e.GetLevel(0, 0); got != LevelSimple {
		t.Errorf("GetLevel(0, 0) = %q, want simple", got)
	}
}
