package model

import "testing"

func TestNewReviewResult_IsAdvisoryAlwaysTrue(t *testing.T) {
	r := NewReviewResult("req-1", "claude-sonnet")
	if !r.IsAdvisory {
		t.Fatal("expected IsAdvisory to be true")
	}
}

func TestNewReviewResult_DefaultStatus(t *testing.T) {
	r := NewReviewResult("req-1", "claude-sonnet")
	if r.Status != ReviewStatusPending {
		t.Fatalf("expected status %q, got %q", ReviewStatusPending, r.Status)
	}
}

func TestNewReviewResult_FieldsSet(t *testing.T) {
	r := NewReviewResult("req-123", "gpt-4o")
	if r.RequestID != "req-123" {
		t.Errorf("expected RequestID %q, got %q", "req-123", r.RequestID)
	}
	if r.ReviewerModel != "gpt-4o" {
		t.Errorf("expected ReviewerModel %q, got %q", "gpt-4o", r.ReviewerModel)
	}
	if r.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
}

func TestReviewStatus_ValidStatuses(t *testing.T) {
	expected := []ReviewStatus{
		ReviewStatusPending,
		ReviewStatusInProgress,
		ReviewStatusCompleted,
		ReviewStatusSkipped,
	}
	for _, s := range expected {
		if !ValidReviewStatuses[s] {
			t.Errorf("expected %q to be a valid review status", s)
		}
	}
	if ValidReviewStatuses["unknown"] {
		t.Error("expected 'unknown' to be invalid")
	}
}

func TestIsTerminalReviewStatus(t *testing.T) {
	tests := []struct {
		status   ReviewStatus
		terminal bool
	}{
		{ReviewStatusPending, false},
		{ReviewStatusInProgress, false},
		{ReviewStatusCompleted, true},
		{ReviewStatusSkipped, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			if got := IsTerminalReviewStatus(tt.status); got != tt.terminal {
				t.Errorf("IsTerminalReviewStatus(%q) = %v, want %v", tt.status, got, tt.terminal)
			}
		})
	}
}

func TestReviewSeverity_ValidSeverities(t *testing.T) {
	expected := []ReviewSeverity{
		ReviewSeverityInfo,
		ReviewSeverityWarning,
		ReviewSeverityError,
	}
	for _, s := range expected {
		if !ValidReviewSeverities[s] {
			t.Errorf("expected %q to be a valid review severity", s)
		}
	}
	if ValidReviewSeverities["critical"] {
		t.Error("expected 'critical' to be invalid")
	}
}

func TestReviewConfig_Defaults(t *testing.T) {
	rc := ReviewConfig{}
	if rc.Enabled {
		t.Error("expected Enabled default to be false")
	}
	if rc.EffectiveMinBloomLevel() != 2 {
		t.Errorf("expected default MinBloomLevel=2, got %d", rc.EffectiveMinBloomLevel())
	}
	if rc.EffectiveMaxConcurrentReviews() != 2 {
		t.Errorf("expected default MaxConcurrentReviews=2, got %d", rc.EffectiveMaxConcurrentReviews())
	}
	if rc.EffectiveTimeoutSec() != 300 {
		t.Errorf("expected default TimeoutSec=300, got %d", rc.EffectiveTimeoutSec())
	}
}

func TestReviewConfig_Configured(t *testing.T) {
	rc := ReviewConfig{
		Enabled:              true,
		Models:               []string{"claude-sonnet", "gpt-4o"},
		MinBloomLevel:        IntPtr(3),
		MaxConcurrentReviews: IntPtr(4),
		TimeoutSec:           IntPtr(600),
	}
	if !rc.Enabled {
		t.Error("expected Enabled to be true")
	}
	if len(rc.Models) != 2 {
		t.Errorf("expected 2 models, got %d", len(rc.Models))
	}
	if rc.EffectiveMinBloomLevel() != 3 {
		t.Errorf("expected MinBloomLevel=3, got %d", rc.EffectiveMinBloomLevel())
	}
	if rc.EffectiveMaxConcurrentReviews() != 4 {
		t.Errorf("expected MaxConcurrentReviews=4, got %d", rc.EffectiveMaxConcurrentReviews())
	}
	if rc.EffectiveTimeoutSec() != 600 {
		t.Errorf("expected TimeoutSec=600, got %d", rc.EffectiveTimeoutSec())
	}
}
