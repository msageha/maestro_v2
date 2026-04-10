package model

// Profile level constants (matching complexity levels).
const (
	ProfileLevelSimple   = "simple"
	ProfileLevelStandard = "standard"
	ProfileLevelComplex  = "complex"
	ProfileLevelCritical = "critical"
)

// ValidateProfileLevel reports whether s is a known profile level.
func ValidateProfileLevel(s string) bool {
	switch s {
	case ProfileLevelSimple, ProfileLevelStandard, ProfileLevelComplex, ProfileLevelCritical:
		return true
	}
	return false
}

// DefaultFeatureProfiles returns the default feature profiles for all complexity levels.
func DefaultFeatureProfiles() map[string]FeatureProfile {
	f := "false"
	opt := "optional"
	tr := "true"
	return map[string]FeatureProfile{
		ProfileLevelSimple: {
			CrossAgentReview:        &f,
			ExploratoryOptimization: BoolPtr(false),
			EvolutionaryQuality:     BoolPtr(false),
			AdaptiveModelSelection:  BoolPtr(false),
			SelfImprovement:         BoolPtr(false),
			AdaptiveDepth:           BoolPtr(false),
		},
		ProfileLevelStandard: {
			CrossAgentReview:        &opt,
			ExploratoryOptimization: BoolPtr(false),
			EvolutionaryQuality:     BoolPtr(false),
			AdaptiveModelSelection:  BoolPtr(true),
			SelfImprovement:         BoolPtr(false),
			AdaptiveDepth:           BoolPtr(true),
		},
		ProfileLevelComplex: {
			CrossAgentReview:        &tr,
			ExploratoryOptimization: BoolPtr(true),
			EvolutionaryQuality:     BoolPtr(true),
			AdaptiveModelSelection:  BoolPtr(true),
			SelfImprovement:         BoolPtr(true),
			AdaptiveDepth:           BoolPtr(true),
		},
		ProfileLevelCritical: {
			CrossAgentReview:        &tr,
			ExploratoryOptimization: BoolPtr(true),
			EvolutionaryQuality:     BoolPtr(true),
			AdaptiveModelSelection:  BoolPtr(true),
			SelfImprovement:         BoolPtr(true),
			AdaptiveDepth:           BoolPtr(true),
		},
	}
}

// IsFeatureEnabled checks if a specific feature is enabled for a given complexity level.
// Returns false if the level or feature is unknown.
func IsFeatureEnabled(profiles map[string]FeatureProfile, level string, feature string) bool {
	p, ok := profiles[level]
	if !ok {
		return false
	}
	switch feature {
	case "cross_agent_review":
		return p.EffectiveCrossAgentReview() == "true"
	case "exploratory_optimization":
		return p.EffectiveExploratoryOptimization()
	case "evolutionary_quality":
		return p.EffectiveEvolutionaryQuality()
	case "adaptive_model_selection":
		return p.EffectiveAdaptiveModelSelection()
	case "self_improvement":
		return p.EffectiveSelfImprovement()
	case "adaptive_depth":
		return p.EffectiveAdaptiveDepth()
	}
	return false
}
