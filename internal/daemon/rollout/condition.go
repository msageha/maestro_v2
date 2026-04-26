// Package rollout implements multi-rollout eligibility checking and lifecycle management.
package rollout

// ConditionInput holds the inputs for eligibility evaluation.
type ConditionInput struct {
	HasVerifyConfig   bool
	FailureCount      int
	BloomLevel        int
	ExpectedPathCount int
}

// ConditionThresholds holds configurable thresholds for eligibility checks.
type ConditionThresholds struct {
	MinBloomLevel    int
	MaxExpectedPaths int
	MinFailureCount  int
}

// DefaultConditionThresholds returns the default threshold values.
func DefaultConditionThresholds() ConditionThresholds {
	return ConditionThresholds{
		MinBloomLevel:    4,
		MaxExpectedPaths: 10,
		MinFailureCount:  1,
	}
}

// EligibilityResult holds the result of an eligibility check.
type EligibilityResult struct {
	Eligible bool
	Reasons  []string
}

// CheckEligibility evaluates whether a task is eligible for multi-rollout
// based on three conditions from REQUIREMENTS §4 B-1:
//
//  1. command-scoped verify config must be defined and executable
//  2. Either FailureCount >= MinFailureCount OR BloomLevel >= MinBloomLevel
//  3. ExpectedPathCount must be > 0 and <= MaxExpectedPaths
//
// All three conditions must be satisfied for Eligible to be true.
func CheckEligibility(input ConditionInput, thresholds ConditionThresholds) EligibilityResult {
	result := EligibilityResult{
		Eligible: true,
	}

	// Condition 1: verify config must exist
	if !input.HasVerifyConfig {
		result.Eligible = false
		result.Reasons = append(result.Reasons, "verify config is not defined")
	}

	// Condition 2: sufficient failures OR high bloom level
	if input.FailureCount < thresholds.MinFailureCount && input.BloomLevel < thresholds.MinBloomLevel {
		result.Eligible = false
		result.Reasons = append(result.Reasons,
			"neither failure count nor bloom level meets threshold")
	}

	// Condition 3: expected paths defined and not excessive
	if input.ExpectedPathCount <= 0 {
		result.Eligible = false
		result.Reasons = append(result.Reasons, "expected paths not defined")
	} else if input.ExpectedPathCount > thresholds.MaxExpectedPaths {
		result.Eligible = false
		result.Reasons = append(result.Reasons, "expected paths count exceeds maximum")
	}

	return result
}
