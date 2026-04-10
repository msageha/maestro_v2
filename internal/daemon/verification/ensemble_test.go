package verification

import (
	"testing"
	"time"
)

func TestAggregate_AllPass(t *testing.T) {
	v := NewVerifier()
	results := []PerspectiveResult{
		{Name: "build", Passed: true, Duration: time.Second},
		{Name: "lint", Passed: true, Duration: time.Second},
		{Name: "test", Passed: true, Duration: time.Second},
		{Name: "typecheck", Passed: true, Duration: time.Second},
	}
	agg := v.Aggregate(results)
	if agg.TotalScore != 1.0 {
		t.Errorf("expected TotalScore 1.0, got %f", agg.TotalScore)
	}
	if !agg.Passed {
		t.Error("expected Passed=true when all perspectives pass")
	}
}

func TestAggregate_CriticalWeightFailed(t *testing.T) {
	v := NewVerifier()
	// test has weight 1.0 — critical
	results := []PerspectiveResult{
		{Name: "build", Passed: true},
		{Name: "lint", Passed: true},
		{Name: "test", Passed: false},
		{Name: "typecheck", Passed: true},
	}
	agg := v.Aggregate(results)
	if agg.Passed {
		t.Error("expected Passed=false when critical perspective (test, weight>=1.0) fails")
	}
	if agg.TotalScore >= 1.0 {
		t.Errorf("expected TotalScore < 1.0, got %f", agg.TotalScore)
	}
}

func TestAggregate_LintOnlyFailed(t *testing.T) {
	v := NewVerifier()
	// lint has weight 0.8 — not critical
	results := []PerspectiveResult{
		{Name: "build", Passed: true},
		{Name: "lint", Passed: false},
		{Name: "test", Passed: true},
		{Name: "typecheck", Passed: true},
	}
	agg := v.Aggregate(results)
	if agg.Passed {
		t.Error("expected Passed=false when any perspective fails")
	}
	// TotalScore = (1.0 + 0.0 + 1.0 + 0.9) / (1.0 + 0.8 + 1.0 + 0.9) = 2.9 / 3.7
	expected := 2.9 / 3.7
	if diff := agg.TotalScore - expected; diff > 0.001 || diff < -0.001 {
		t.Errorf("expected TotalScore ~%f, got %f", expected, agg.TotalScore)
	}
}

func TestAggregate_EmptyResults(t *testing.T) {
	v := NewVerifier()
	agg := v.Aggregate(nil)
	if !agg.Passed {
		t.Error("expected Passed=true for empty results")
	}
	if agg.TotalScore != 0 {
		t.Errorf("expected TotalScore=0 for empty results, got %f", agg.TotalScore)
	}
}

func TestShouldRetry_NewFingerprint(t *testing.T) {
	v := NewVerifier()
	result := AggregatedResult{Passed: false}
	if !v.ShouldRetry(result, "abc123", []string{"def456", "ghi789"}) {
		t.Error("expected ShouldRetry=true for unseen fingerprint")
	}
}

func TestShouldRetry_KnownFingerprint(t *testing.T) {
	v := NewVerifier()
	result := AggregatedResult{Passed: false}
	if v.ShouldRetry(result, "abc123", []string{"abc123", "def456"}) {
		t.Error("expected ShouldRetry=false for already-seen fingerprint")
	}
}

func TestShouldRetry_Passed(t *testing.T) {
	v := NewVerifier()
	result := AggregatedResult{Passed: true}
	if v.ShouldRetry(result, "abc123", nil) {
		t.Error("expected ShouldRetry=false when result is passed")
	}
}

func TestDefaultPerspectives(t *testing.T) {
	v := NewVerifier()
	perspectives := v.DefaultPerspectives()
	if len(perspectives) != 4 {
		t.Fatalf("expected 4 default perspectives, got %d", len(perspectives))
	}

	expected := map[string]float64{
		"build":     1.0,
		"lint":      0.8,
		"test":      1.0,
		"typecheck": 0.9,
	}
	for _, p := range perspectives {
		w, ok := expected[p.Name]
		if !ok {
			t.Errorf("unexpected perspective: %s", p.Name)
			continue
		}
		if p.Weight != w {
			t.Errorf("perspective %s: expected weight %f, got %f", p.Name, w, p.Weight)
		}
	}
}

func TestAddPerspective(t *testing.T) {
	v := NewVerifier()
	initial := len(v.perspectives)
	v.AddPerspective(Perspective{Name: "security", Commands: []string{"gosec ./..."}, Weight: 0.7})
	if len(v.perspectives) != initial+1 {
		t.Errorf("expected %d perspectives after add, got %d", initial+1, len(v.perspectives))
	}
}
