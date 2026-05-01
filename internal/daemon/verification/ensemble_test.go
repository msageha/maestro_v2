package verification

import (
	"testing"
	"time"
)

// baselinePerspectives returns a language-neutral perspective scaffold used by
// aggregation tests below. Commands are intentionally empty: these tests
// exercise the scoring/aggregation contract, not command execution, so the
// scaffold only needs Name and Weight to drive Aggregate.
func baselinePerspectives() []Perspective {
	return []Perspective{
		{Name: "build", Weight: 1.0},
		{Name: "lint", Weight: 0.8},
		{Name: "test", Weight: 1.0},
		{Name: "typecheck", Weight: 0.9},
	}
}

func newVerifierWithBaseline(t *testing.T) *Verifier {
	t.Helper()
	v := NewVerifier()
	if err := v.SetPerspectives(baselinePerspectives()); err != nil {
		t.Fatalf("SetPerspectives: %v", err)
	}
	return v
}

func TestAggregate_AllPass(t *testing.T) {
	t.Parallel()
	v := newVerifierWithBaseline(t)
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
	t.Parallel()
	v := newVerifierWithBaseline(t)
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
	t.Parallel()
	v := newVerifierWithBaseline(t)
	// lint has weight 0.8 — not critical (< 1.0), so Passed remains true
	results := []PerspectiveResult{
		{Name: "build", Passed: true},
		{Name: "lint", Passed: false},
		{Name: "test", Passed: true},
		{Name: "typecheck", Passed: true},
	}
	agg := v.Aggregate(results)
	if !agg.Passed {
		t.Error("expected Passed=true when only non-critical perspective (weight < 1.0) fails")
	}
	// TotalScore = (1.0 + 0.0 + 1.0 + 0.9) / (1.0 + 0.8 + 1.0 + 0.9) = 2.9 / 3.7
	expected := 2.9 / 3.7
	if diff := agg.TotalScore - expected; diff > 0.001 || diff < -0.001 {
		t.Errorf("expected TotalScore ~%f, got %f", expected, agg.TotalScore)
	}
}

func TestAggregate_EmptyResults(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	v := NewVerifier()
	result := AggregatedResult{Passed: false}
	if !v.ShouldRetry(result, "abc123", []string{"def456", "ghi789"}) {
		t.Error("expected ShouldRetry=true for unseen fingerprint")
	}
}

func TestShouldRetry_KnownFingerprint(t *testing.T) {
	t.Parallel()
	v := NewVerifier()
	result := AggregatedResult{Passed: false}
	if v.ShouldRetry(result, "abc123", []string{"abc123", "def456"}) {
		t.Error("expected ShouldRetry=false for already-seen fingerprint")
	}
}

func TestShouldRetry_Passed(t *testing.T) {
	t.Parallel()
	v := NewVerifier()
	result := AggregatedResult{Passed: true}
	if v.ShouldRetry(result, "abc123", nil) {
		t.Error("expected ShouldRetry=false when result is passed")
	}
}

func TestNewVerifier_StartsEmpty(t *testing.T) {
	t.Parallel()
	v := NewVerifier()
	if got := len(v.Perspectives()); got != 0 {
		t.Errorf("NewVerifier must not auto-populate perspectives (production callers inject language-aware perspectives via SetPerspectives); got %d", got)
	}
}

func TestAddPerspective(t *testing.T) {
	t.Parallel()
	v := NewVerifier()
	initial := len(v.perspectives)
	if err := v.AddPerspective(Perspective{Name: "security", Commands: []string{"gosec ./..."}, Weight: 0.7}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(v.perspectives) != initial+1 {
		t.Errorf("expected %d perspectives after add, got %d", initial+1, len(v.perspectives))
	}
}

func TestAddPerspective_InvalidWeight(t *testing.T) {
	t.Parallel()
	v := NewVerifier()
	tests := []struct {
		name   string
		weight float64
	}{
		{"zero weight", 0},
		{"negative weight", -1.0},
	}
	for _, tt := range tests {
		if err := v.AddPerspective(Perspective{Name: tt.name, Weight: tt.weight}); err == nil {
			t.Errorf("%s: expected error for weight %f, got nil", tt.name, tt.weight)
		}
	}
}

func TestAggregate_WeightBasedPassed(t *testing.T) {
	t.Parallel()
	v := &Verifier{}
	v.perspectives = []Perspective{
		{Name: "critical", Weight: 1.0},
		{Name: "important", Weight: 0.9},
		{Name: "optional", Weight: 0.5},
	}

	tests := []struct {
		name       string
		results    []PerspectiveResult
		wantPassed bool
		wantScore  float64
	}{
		{
			name: "all pass",
			results: []PerspectiveResult{
				{Name: "critical", Passed: true},
				{Name: "important", Passed: true},
				{Name: "optional", Passed: true},
			},
			wantPassed: true,
			wantScore:  1.0,
		},
		{
			name: "critical fails - Passed must be false",
			results: []PerspectiveResult{
				{Name: "critical", Passed: false},
				{Name: "important", Passed: true},
				{Name: "optional", Passed: true},
			},
			wantPassed: false,
			wantScore:  1.4 / 2.4, // (0 + 0.9 + 0.5) / (1.0 + 0.9 + 0.5)
		},
		{
			name: "only non-critical fail - Passed stays true",
			results: []PerspectiveResult{
				{Name: "critical", Passed: true},
				{Name: "important", Passed: false},
				{Name: "optional", Passed: false},
			},
			wantPassed: true,
			wantScore:  1.0 / 2.4, // (1.0 + 0 + 0) / (1.0 + 0.9 + 0.5)
		},
		{
			name: "unknown perspective fails - treated as critical (default weight 1.0)",
			results: []PerspectiveResult{
				{Name: "critical", Passed: true},
				{Name: "unknown", Passed: false},
			},
			wantPassed: false,
			wantScore:  1.0 / 2.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := v.Aggregate(tt.results)
			if agg.Passed != tt.wantPassed {
				t.Errorf("Passed = %v, want %v", agg.Passed, tt.wantPassed)
			}
			if diff := agg.TotalScore - tt.wantScore; diff > 0.001 || diff < -0.001 {
				t.Errorf("TotalScore = %f, want %f", agg.TotalScore, tt.wantScore)
			}
		})
	}
}
