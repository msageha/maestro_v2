package featuregate

import "testing"

func TestDefaultProfiles_SimpleAllDisabled(t *testing.T) {
	t.Parallel()
	e := NewEvaluator()
	p := e.Evaluate(LevelSimple)

	for _, feat := range allFeatures {
		if p.EnabledFeatures[feat] {
			t.Errorf("simple profile: feature %q should be disabled", feat)
		}
	}
}

func TestDefaultProfiles_CriticalAllEnabled(t *testing.T) {
	t.Parallel()
	e := NewEvaluator()
	p := e.Evaluate(LevelCritical)

	for _, feat := range allFeatures {
		if !p.EnabledFeatures[feat] {
			t.Errorf("critical profile: feature %q should be enabled", feat)
		}
	}
}

func TestEvaluate_UnknownLevel_FallsBackToSimple(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	e := NewEvaluator()
	if !e.IsEnabled(LevelStandard, FeatureAdaptiveModelSelection) {
		t.Error("standard: adaptive_model_selection should be enabled")
	}
}

func TestIsEnabled_SimpleEvolutionaryQualityDisabled(t *testing.T) {
	t.Parallel()
	e := NewEvaluator()
	if e.IsEnabled(LevelSimple, FeatureEvolutionaryQuality) {
		t.Error("simple: evolutionary_quality should be disabled")
	}
}

func TestIsEnabled_ComplexProfile(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

	// Critical should still exist after partial LoadProfiles (merge, not replace).
	p := e.Evaluate(LevelCritical)
	if p.Level != LevelCritical {
		t.Errorf("expected critical to be preserved, got %q", p.Level)
	}
	// Default critical has all features enabled.
	if !p.EnabledFeatures[FeatureSelfImprovement] {
		t.Error("critical: self_improvement should still be enabled after partial load")
	}
}

func TestLoadProfiles_NilAndEmptyPreserveExisting(t *testing.T) {
	t.Parallel()
	e := NewEvaluator()

	// Load nil — existing profiles must be unchanged.
	e.LoadProfiles(nil)
	if !e.IsEnabled(LevelCritical, FeatureSelfImprovement) {
		t.Error("after nil load: critical self_improvement should be preserved")
	}

	// Load empty map — existing profiles must be unchanged.
	e.LoadProfiles(map[string]map[string]interface{}{})
	if !e.IsEnabled(LevelCritical, FeatureSelfImprovement) {
		t.Error("after empty load: critical self_improvement should be preserved")
	}
}

func TestLoadProfiles_PartialMergePreservesOtherLevels(t *testing.T) {
	t.Parallel()
	e := NewEvaluator()

	// Load only simple — complex and critical must survive.
	partial := map[string]map[string]interface{}{
		"simple": {
			"self_improvement": true,
		},
	}
	e.LoadProfiles(partial)

	// simple was overridden.
	if !e.IsEnabled(LevelSimple, FeatureSelfImprovement) {
		t.Error("simple: self_improvement should be enabled after partial load")
	}

	// complex preserved from defaults.
	if !e.IsEnabled(LevelComplex, FeatureAdaptiveModelSelection) {
		t.Error("complex: adaptive_model_selection should be preserved")
	}
	if !e.IsEnabled(LevelComplex, FeatureCrossAgentReview) {
		t.Error("complex: cross_agent_review should be preserved")
	}

	// critical preserved from defaults.
	for _, feat := range allFeatures {
		if !e.IsEnabled(LevelCritical, feat) {
			t.Errorf("critical: feature %q should be preserved after partial load", feat)
		}
	}
}

func TestOrchestratorOverrideSimulation(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	e := NewEvaluator()
	if got := e.GetLevel(0, 0); got != LevelSimple {
		t.Errorf("GetLevel(0, 0) = %q, want simple", got)
	}
}
