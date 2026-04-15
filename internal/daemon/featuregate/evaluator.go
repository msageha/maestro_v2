// Package featuregate provides feature gate profile evaluation for task complexity levels.
package featuregate

import "sync"

// ProfileLevel represents task complexity classification.
// Values are identical to model.ComplexityLevel* / model.ProfileLevel*
// constants by convention. This package defines its own type to avoid
// coupling the feature gate layer to the model package (DIP).
type ProfileLevel string

const (
	// LevelSimple is the lowest complexity profile level.
	LevelSimple ProfileLevel = "simple"
	// LevelStandard is the default complexity profile level.
	LevelStandard ProfileLevel = "standard"
	// LevelComplex is used for multi-step or high-risk tasks.
	LevelComplex ProfileLevel = "complex"
	// LevelCritical is used for the highest-risk tasks.
	LevelCritical ProfileLevel = "critical"
)

// Feature represents a toggleable system capability.
type Feature string

const (
	// FeatureCrossAgentReview enables cross-agent review of task results.
	FeatureCrossAgentReview Feature = "cross_agent_review"
	// FeatureExploratoryOpt enables exploratory optimization passes.
	FeatureExploratoryOpt Feature = "exploratory_optimization"
	// FeatureEvolutionaryQuality enables evolutionary quality improvement.
	FeatureEvolutionaryQuality Feature = "evolutionary_quality"
	// FeatureAdaptiveModelSelection enables adaptive model selection per task.
	FeatureAdaptiveModelSelection Feature = "adaptive_model_selection"
	// FeatureSelfImprovement enables self-improvement learning loops.
	FeatureSelfImprovement Feature = "self_improvement"
	// FeatureAdaptiveDepth enables adaptive search depth adjustment.
	FeatureAdaptiveDepth Feature = "adaptive_depth"
)

// allFeatures lists every known feature for default profile construction.
var allFeatures = []Feature{
	FeatureCrossAgentReview,
	FeatureExploratoryOpt,
	FeatureEvolutionaryQuality,
	FeatureAdaptiveModelSelection,
	FeatureSelfImprovement,
	FeatureAdaptiveDepth,
}

// Profile holds the feature toggle set for a complexity level.
type Profile struct {
	Level           ProfileLevel
	EnabledFeatures map[Feature]bool
}

// Evaluator manages profiles and evaluates feature gates.
type Evaluator struct {
	profiles map[ProfileLevel]Profile
	mu       sync.RWMutex
}

// NewEvaluator creates an Evaluator pre-loaded with default profiles.
func NewEvaluator() *Evaluator {
	e := &Evaluator{}
	e.profiles = e.DefaultProfiles()
	return e
}

// DefaultProfiles returns the §4.5 C-8 default feature gate definitions.
//
//	simple:   all features disabled
//	standard: adaptive_model_selection enabled
//	complex:  adaptive_model_selection, cross_agent_review, adaptive_depth enabled
//	critical: all features enabled
func (e *Evaluator) DefaultProfiles() map[ProfileLevel]Profile {
	return map[ProfileLevel]Profile{
		LevelSimple: {
			Level:           LevelSimple,
			EnabledFeatures: makeFeatureMap(nil),
		},
		LevelStandard: {
			Level: LevelStandard,
			EnabledFeatures: makeFeatureMap([]Feature{
				FeatureAdaptiveModelSelection,
			}),
		},
		LevelComplex: {
			Level: LevelComplex,
			EnabledFeatures: makeFeatureMap([]Feature{
				FeatureAdaptiveModelSelection,
				FeatureCrossAgentReview,
				FeatureAdaptiveDepth,
			}),
		},
		LevelCritical: {
			Level:           LevelCritical,
			EnabledFeatures: makeFeatureMap(allFeatures),
		},
	}
}

// LoadProfiles replaces profiles from an external config map.
// The map key is the profile level string; value is a map of feature name → enabled bool.
// Unknown levels are stored as-is; Evaluate() handles fallback to LevelSimple.
func (e *Evaluator) LoadProfiles(profiles map[string]map[string]interface{}) {
	e.mu.Lock()
	defer e.mu.Unlock()

	loaded := make(map[ProfileLevel]Profile, len(profiles))
	for levelStr, featureMap := range profiles {
		level := ProfileLevel(levelStr)
		enabled := make(map[Feature]bool, len(featureMap))
		for feat, val := range featureMap {
			if b, ok := val.(bool); ok {
				enabled[Feature(feat)] = b
			}
		}
		loaded[level] = Profile{Level: level, EnabledFeatures: enabled}
	}
	e.profiles = loaded
}

// Evaluate returns the profile for the given level.
// Unknown levels fall back to simple (§C-8-6).
func (e *Evaluator) Evaluate(level ProfileLevel) Profile {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if p, ok := e.profiles[level]; ok {
		return p
	}
	if p, ok := e.profiles[LevelSimple]; ok {
		return p
	}
	return Profile{Level: LevelSimple, EnabledFeatures: make(map[Feature]bool)}
}

// IsEnabled checks whether a feature is enabled at the given level.
func (e *Evaluator) IsEnabled(level ProfileLevel, feature Feature) bool {
	p := e.Evaluate(level)
	return p.EnabledFeatures[feature]
}

// GetLevel determines the profile level from file and dependency counts.
//
//	1-3 files  → simple
//	4-10 files → standard
//	11-30 files → complex
//	30+ files  → critical
func (e *Evaluator) GetLevel(fileCount, depCount int) ProfileLevel {
	total := fileCount + depCount
	switch {
	case total <= 3:
		return LevelSimple
	case total <= 10:
		return LevelStandard
	case total <= 30:
		return LevelComplex
	default:
		return LevelCritical
	}
}

// makeFeatureMap builds a feature map with the given features enabled.
func makeFeatureMap(enabled []Feature) map[Feature]bool {
	m := make(map[Feature]bool, len(allFeatures))
	for _, f := range allFeatures {
		m[f] = false
	}
	for _, f := range enabled {
		m[f] = true
	}
	return m
}
