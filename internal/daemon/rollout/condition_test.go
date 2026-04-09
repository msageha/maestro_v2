package rollout

import (
	"testing"
)

func TestCheckEligibility_AllConditionsMet(t *testing.T) {
	input := ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      2,
		BloomLevel:        5,
		ExpectedPathCount: 3,
	}
	result := CheckEligibility(input, DefaultConditionThresholds())
	if !result.Eligible {
		t.Errorf("expected eligible, got ineligible: %v", result.Reasons)
	}
	if len(result.Reasons) != 0 {
		t.Errorf("expected no reasons, got %v", result.Reasons)
	}
}

func TestCheckEligibility_NoVerifyConfig(t *testing.T) {
	input := ConditionInput{
		HasVerifyConfig:   false,
		FailureCount:      2,
		BloomLevel:        5,
		ExpectedPathCount: 3,
	}
	result := CheckEligibility(input, DefaultConditionThresholds())
	if result.Eligible {
		t.Error("expected ineligible when verify config missing")
	}
	assertReasonContains(t, result.Reasons, "verify config")
}

func TestCheckEligibility_NoFailuresLowBloom(t *testing.T) {
	input := ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      0,
		BloomLevel:        3,
		ExpectedPathCount: 3,
	}
	result := CheckEligibility(input, DefaultConditionThresholds())
	if result.Eligible {
		t.Error("expected ineligible with 0 failures and bloom level 3")
	}
	assertReasonContains(t, result.Reasons, "failure count")
}

func TestCheckEligibility_NoFailuresHighBloom(t *testing.T) {
	input := ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      0,
		BloomLevel:        4,
		ExpectedPathCount: 3,
	}
	result := CheckEligibility(input, DefaultConditionThresholds())
	if !result.Eligible {
		t.Errorf("expected eligible via bloom level, got ineligible: %v", result.Reasons)
	}
}

func TestCheckEligibility_FailuresMetLowBloom(t *testing.T) {
	input := ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      2,
		BloomLevel:        2,
		ExpectedPathCount: 3,
	}
	result := CheckEligibility(input, DefaultConditionThresholds())
	if !result.Eligible {
		t.Errorf("expected eligible via failure count, got ineligible: %v", result.Reasons)
	}
}

func TestCheckEligibility_TooManyExpectedPaths(t *testing.T) {
	input := ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      2,
		BloomLevel:        5,
		ExpectedPathCount: 11,
	}
	result := CheckEligibility(input, DefaultConditionThresholds())
	if result.Eligible {
		t.Error("expected ineligible with 11 expected paths")
	}
	assertReasonContains(t, result.Reasons, "exceeds maximum")
}

func TestCheckEligibility_ZeroExpectedPaths(t *testing.T) {
	input := ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      2,
		BloomLevel:        5,
		ExpectedPathCount: 0,
	}
	result := CheckEligibility(input, DefaultConditionThresholds())
	if result.Eligible {
		t.Error("expected ineligible with 0 expected paths")
	}
	assertReasonContains(t, result.Reasons, "not defined")
}

func TestCheckEligibility_CustomThresholds(t *testing.T) {
	thresholds := ConditionThresholds{
		MinBloomLevel:    2,
		MaxExpectedPaths: 5,
		MinFailureCount:  3,
	}

	// Bloom level 2 satisfies custom threshold
	input := ConditionInput{
		HasVerifyConfig:   true,
		FailureCount:      0,
		BloomLevel:        2,
		ExpectedPathCount: 4,
	}
	result := CheckEligibility(input, thresholds)
	if !result.Eligible {
		t.Errorf("expected eligible with custom thresholds: %v", result.Reasons)
	}

	// Expected paths 6 exceeds custom max of 5
	input.ExpectedPathCount = 6
	result = CheckEligibility(input, thresholds)
	if result.Eligible {
		t.Error("expected ineligible when exceeding custom MaxExpectedPaths")
	}
}

func TestCheckEligibility_MultipleFailures(t *testing.T) {
	input := ConditionInput{
		HasVerifyConfig:   false,
		FailureCount:      0,
		BloomLevel:        2,
		ExpectedPathCount: 0,
	}
	result := CheckEligibility(input, DefaultConditionThresholds())
	if result.Eligible {
		t.Error("expected ineligible")
	}
	if len(result.Reasons) != 3 {
		t.Errorf("expected 3 reasons, got %d: %v", len(result.Reasons), result.Reasons)
	}
}

func TestDefaultConditionThresholds(t *testing.T) {
	th := DefaultConditionThresholds()
	if th.MinBloomLevel != 4 {
		t.Errorf("MinBloomLevel = %d, want 4", th.MinBloomLevel)
	}
	if th.MaxExpectedPaths != 10 {
		t.Errorf("MaxExpectedPaths = %d, want 10", th.MaxExpectedPaths)
	}
	if th.MinFailureCount != 1 {
		t.Errorf("MinFailureCount = %d, want 1", th.MinFailureCount)
	}
}

func assertReasonContains(t *testing.T, reasons []string, substr string) {
	t.Helper()
	for _, r := range reasons {
		if contains(r, substr) {
			return
		}
	}
	t.Errorf("expected reason containing %q, got %v", substr, reasons)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
