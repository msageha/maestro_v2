package model

import "github.com/msageha/maestro_v2/internal/ptr"

// Profile level constants — aliases for the canonical ComplexityLevel* constants.
const (
	ProfileLevelSimple   = ComplexityLevelSimple
	ProfileLevelStandard = ComplexityLevelStandard
	ProfileLevelComplex  = ComplexityLevelComplex
	ProfileLevelCritical = ComplexityLevelCritical
)

// ValidateProfileLevel reports whether s is a known profile level.
// Delegates to ValidateComplexityLevel as both share the same valid values.
func ValidateProfileLevel(s string) bool {
	return ValidateComplexityLevel(s)
}

// DefaultFeatureProfiles returns the default feature profiles for all complexity levels.
func DefaultFeatureProfiles() map[string]FeatureProfile {
	f := "false"
	opt := "optional"
	tr := "true"
	return map[string]FeatureProfile{
		ProfileLevelSimple: {
			CrossAgentReview:        &f,
			ExploratoryOptimization: ptr.Bool(false),
			EvolutionaryQuality:     ptr.Bool(false),
			AdaptiveModelSelection:  ptr.Bool(false),
			SelfImprovement:         ptr.Bool(false),
			AdaptiveDepth:           ptr.Bool(false),
		},
		ProfileLevelStandard: {
			CrossAgentReview:        &opt,
			ExploratoryOptimization: ptr.Bool(false),
			EvolutionaryQuality:     ptr.Bool(false),
			AdaptiveModelSelection:  ptr.Bool(true),
			SelfImprovement:         ptr.Bool(false),
			AdaptiveDepth:           ptr.Bool(true),
		},
		ProfileLevelComplex: {
			CrossAgentReview:        &tr,
			ExploratoryOptimization: ptr.Bool(true),
			EvolutionaryQuality:     ptr.Bool(true),
			AdaptiveModelSelection:  ptr.Bool(true),
			SelfImprovement:         ptr.Bool(true),
			AdaptiveDepth:           ptr.Bool(true),
		},
		ProfileLevelCritical: {
			CrossAgentReview:        &tr,
			ExploratoryOptimization: ptr.Bool(true),
			EvolutionaryQuality:     ptr.Bool(true),
			AdaptiveModelSelection:  ptr.Bool(true),
			SelfImprovement:         ptr.Bool(true),
			AdaptiveDepth:           ptr.Bool(true),
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
