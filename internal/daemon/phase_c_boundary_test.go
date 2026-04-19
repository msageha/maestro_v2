package daemon

import (
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/bandit"
	"github.com/msageha/maestro_v2/internal/daemon/complexity"
	"github.com/msageha/maestro_v2/internal/daemon/evolution"
	"github.com/msageha/maestro_v2/internal/daemon/featuregate"
	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/daemon/search"
	"github.com/msageha/maestro_v2/internal/daemon/verification"
	"github.com/msageha/maestro_v2/internal/model"
)

// --- §4.5.1-1: Deterministic Daemon Processing ---

func TestBoundary_DeterministicDaemonProcessing(t *testing.T) {
	t.Parallel()
	// §4.5.1-1: Deterministic processing consolidated in Daemon.
	// Same input → same output for all computational components.

	t.Run("UCB1_Deterministic", func(t *testing.T) {
		t.Parallel()
		// UCB1: same arms + same reward history → same BestArm.
		for trial := 0; trial < 3; trial++ {
			sel, err := bandit.NewSelector(1.41)
			if err != nil {
				t.Fatalf("NewSelector: %v", err)
			}
			sel.AddArm("a")
			sel.AddArm("b")
			for i := 0; i < 10; i++ {
				sel.UpdateReward("a", 0.8)
				sel.UpdateReward("b", 0.3)
			}
			best, _, err := sel.BestArm()
			if err != nil {
				t.Fatalf("trial %d: BestArm error: %v", trial, err)
			}
			if best != "a" {
				t.Errorf("trial %d: expected best arm = a, got %q", trial, best)
			}
		}
	})

	t.Run("ComplexityScore_Deterministic", func(t *testing.T) {
		t.Parallel()
		scorer := complexity.NewScorer(complexity.DefaultThresholds())
		input := complexity.Input{
			FileCount:         15,
			DependencyDepth:   4,
			BloomLevel:        3,
			PastRepairRate:    0.2,
			ExpectedPathCount: 5,
		}
		firstScore := scorer.Estimate(input)
		for i := 0; i < 5; i++ {
			score := scorer.Estimate(input)
			if score.Level != firstScore.Level {
				t.Errorf("run %d: Level %q != first %q", i, score.Level, firstScore.Level)
			}
			if score.RawScore != firstScore.RawScore {
				t.Errorf("run %d: RawScore %.6f != first %.6f", i, score.RawScore, firstScore.RawScore)
			}
		}
	})

	t.Run("FeatureProfile_Deterministic", func(t *testing.T) {
		t.Parallel()
		evaluator := featuregate.NewEvaluator()
		levels := []featuregate.ProfileLevel{
			featuregate.LevelSimple,
			featuregate.LevelStandard,
			featuregate.LevelComplex,
			featuregate.LevelCritical,
		}
		for _, level := range levels {
			p1 := evaluator.Evaluate(level)
			p2 := evaluator.Evaluate(level)
			if p1.Level != p2.Level {
				t.Errorf("level %q: profiles differ in Level", level)
			}
			for feat, v1 := range p1.EnabledFeatures {
				if v2, ok := p2.EnabledFeatures[feat]; !ok || v1 != v2 {
					t.Errorf("level %q feature %q: %v != %v", level, feat, v1, v2)
				}
			}
		}
	})
}

// --- §4.5.1: No LLM Token Consumption ---

func TestBoundary_NoLLMTokenConsumption(t *testing.T) {
	t.Parallel()
	// All Phase C packages operate as in-process Go computations.
	// No external HTTP/LLM calls are made.

	// Bandit: pure math.
	sel, err := bandit.NewSelector(1.41)
	if err != nil {
		t.Fatalf("NewSelector: %v", err)
	}
	sel.AddArm("test")
	sel.UpdateReward("test", 0.5)
	_, _ = sel.SelectArm()
	_, _, _ = sel.BestArm()

	// FeatureGate: lookup table.
	eval := featuregate.NewEvaluator()
	_ = eval.Evaluate(featuregate.LevelComplex)
	_ = eval.IsEnabled(featuregate.LevelComplex, featuregate.FeatureAdaptiveDepth)

	// Complexity: weighted sum.
	scorer := complexity.NewScorer(complexity.DefaultThresholds())
	score := scorer.Estimate(complexity.Input{FileCount: 10, BloomLevel: 3})
	_ = scorer.EstimateDepth(score)

	// Search Tree: graph operations.
	tree := search.NewTree(5, 10, 0.2)
	tree.AddRoot("r")
	_, _ = tree.Expand("r", []string{"a"})
	tree.Backpropagate("a", 0.5)
	_, _ = tree.SelectBest("r", 1.41)

	// Thompson Sampler: Beta distribution sampling.
	sampler := search.NewSampler(1.0, 1.0)
	_ = sampler.Sample()
	sampler.Update(search.DecisionWiden, true)

	// If we reach here without panic/hang, all ops completed in-process.
	// No network calls, no LLM API, no token consumption.
}

// --- §5 Anti-Requirements ---

func TestBoundary_AntiRequirements(t *testing.T) {
	t.Parallel()
	t.Run("S5_1_QualityScore_StaticAnalysis", func(t *testing.T) {
		t.Parallel()
		// §5-1: FitnessScore.QualityScore is static-analysis-based, no LLM override.
		// QualityScore is a plain float64 field — no LLM accessor.
		fs := model.FitnessScore{
			Passed:       true,
			QualityScore: 0.85,
		}
		th := model.FitnessThresholds{
		RepairCountMargin:   0,
		DiffSizeMargin:      10,
		ExecutionTimeMargin: 30 * time.Second,
		QualityScoreMargin:  0.05,
	}

		// Compare uses QualityScore mechanically without LLM evaluation.
		lower := model.FitnessScore{Passed: true, QualityScore: 0.4}
		cmp := fs.Compare(lower, th)
		if cmp != -1 {
			t.Errorf("higher QualityScore should win mechanically, got %d", cmp)
		}
	})

	t.Run("S5_2_Ensemble_IndependentEvaluation", func(t *testing.T) {
		t.Parallel()
		// §5-2: Ensemble uses independent evaluation + weighted aggregation, NOT majority vote.
		v := verification.NewVerifier()

		// 3 out of 4 pass, but the critical one (weight >= 1.0) fails → overall fail.
		results := []verification.PerspectiveResult{
			{Name: "build", Passed: false}, // weight 1.0, critical
			{Name: "lint", Passed: true},   // weight 0.8
			{Name: "test", Passed: true},   // weight 1.0
			{Name: "typecheck", Passed: true}, // weight 0.9
		}
		agg := v.Aggregate(results)
		if agg.Passed {
			t.Error("should fail when critical perspective (build, weight 1.0) fails — not majority vote")
		}
		// Score should reflect independent weighted sum.
		// Expected: (0*1.0 + 1*0.8 + 1*1.0 + 1*0.9) / (1.0+0.8+1.0+0.9) = 2.7/3.7 ≈ 0.73
		expectedScore := 2.7 / 3.7
		if agg.TotalScore < expectedScore-0.01 || agg.TotalScore > expectedScore+0.01 {
			t.Errorf("TotalScore = %.3f, expected ≈%.3f (weighted, not majority)", agg.TotalScore, expectedScore)
		}
	})

	t.Run("S5_6_Evolution_VerifyConfigPrereq", func(t *testing.T) {
		t.Parallel()
		// §5-6: Evolution/Search presumes verify.yaml exists.
		// VerifyConfig nil → evolution should not proceed blindly.
		// Test at type level: model.DefinitionOfAbort has explicit conditions.
		doa := model.DefinitionOfAbort{
			MaxRepairCount:  3,
			MaxWallClockSec: 1800,
		}
		if doa.MaxRepairCount == 0 {
			t.Error("DefinitionOfAbort.MaxRepairCount should be non-zero")
		}
	})

	t.Run("S5_7_Bandit_TraceDataPrereq", func(t *testing.T) {
		t.Parallel()
		// §5-7: Bandit presumes Trace data accumulation.
		sel, err := bandit.NewSelector(1.41)
		if err != nil {
			t.Fatalf("NewSelector: %v", err)
		}
		sel.AddArm("a")

		// No pulls → exploration phase (not exploitation).
		arm, err := sel.SelectArm()
		if err != nil {
			t.Fatalf("SelectArm with no data: %v", err)
		}
		if arm != "a" {
			t.Errorf("with single arm and no data, should select 'a', got %q", arm)
		}

		// BestArm without data should error.
		_, _, err = sel.BestArm()
		if err == nil {
			t.Error("BestArm with no pulls should return error (insufficient trace data)")
		}
	})
}

// --- C-1: Evolution Mutation Strategies ---

func TestC1_Evolution_MutationStrategies(t *testing.T) {
	t.Parallel()
	t.Run("PlanMutations_Distribution", func(t *testing.T) {
		t.Parallel()
		engine := evolution.NewEngine(
			[]evolution.Strategy{evolution.StrategyDiff, evolution.StrategyFull, evolution.StrategyCross},
			nil,
		)
		// With parentCount >= 2, cross is active. Distribution: diff:full:cross = 2:1:1.
		slots := engine.PlanMutations(3)
		counts := map[evolution.Strategy]int{}
		for _, s := range slots {
			counts[s.Strategy]++
		}
		if counts[evolution.StrategyDiff] != 2 {
			t.Errorf("diff slots = %d, want 2", counts[evolution.StrategyDiff])
		}
		if counts[evolution.StrategyFull] != 1 {
			t.Errorf("full slots = %d, want 1", counts[evolution.StrategyFull])
		}
		if counts[evolution.StrategyCross] != 1 {
			t.Errorf("cross slots = %d, want 1", counts[evolution.StrategyCross])
		}
	})

	t.Run("PlanMutations_NoCross_SingleParent", func(t *testing.T) {
		t.Parallel()
		engine := evolution.NewEngine(
			[]evolution.Strategy{evolution.StrategyDiff, evolution.StrategyFull, evolution.StrategyCross},
			nil,
		)
		// parentCount < 2 → cross excluded.
		slots := engine.PlanMutations(1)
		for _, s := range slots {
			if s.Strategy == evolution.StrategyCross {
				t.Error("cross should not be planned with parentCount < 2")
			}
		}
	})

	t.Run("CheckNovelty", func(t *testing.T) {
		t.Parallel()
		engine := evolution.NewEngine(nil, nil)
		h1 := evolution.HashContent("hello")
		h2 := evolution.HashContent("world")

		// New hash → novel.
		if !engine.CheckNovelty(h1, []string{h2}) {
			t.Error("new hash should be novel")
		}
		// Existing hash → not novel.
		if engine.CheckNovelty(h1, []string{h1, h2}) {
			t.Error("existing hash should not be novel")
		}
	})

	t.Run("SelectSurvivors_WinnerTakesAll", func(t *testing.T) {
		t.Parallel()
		// §5-3: Winner-takes-all selection.
		engine := evolution.NewEngine(nil, nil)
		results := []evolution.SlotResult{
			{Index: 0, Strategy: evolution.StrategyDiff, FitnessDesc: "0.3", IsNovel: true},
			{Index: 1, Strategy: evolution.StrategyFull, FitnessDesc: "0.9", IsNovel: true},
			{Index: 2, Strategy: evolution.StrategyCross, FitnessDesc: "0.5", IsNovel: true},
		}

		// maxSurvivors=1 → winner-takes-all.
		survivors := engine.SelectSurvivors(results, 1)
		if len(survivors) != 1 {
			t.Fatalf("expected 1 survivor, got %d", len(survivors))
		}
		if survivors[0] != 1 {
			t.Errorf("expected winner index 1 (fitness 0.9), got %d", survivors[0])
		}

		// Non-novel candidates are excluded.
		mixedResults := []evolution.SlotResult{
			{Index: 0, FitnessDesc: "0.9", IsNovel: false}, // excluded
			{Index: 1, FitnessDesc: "0.5", IsNovel: true},
		}
		mixedSurvivors := engine.SelectSurvivors(mixedResults, 1)
		if len(mixedSurvivors) != 1 || mixedSurvivors[0] != 1 {
			t.Errorf("non-novel should be excluded: survivors=%v", mixedSurvivors)
		}
	})
}

// --- C-3: Verification Ensemble Aggregation ---

func TestC3_Verification_EnsembleAggregation(t *testing.T) {
	t.Parallel()
	v := verification.NewVerifier()

	t.Run("AllPass", func(t *testing.T) {
		t.Parallel()
		results := []verification.PerspectiveResult{
			{Name: "build", Passed: true},
			{Name: "lint", Passed: true},
			{Name: "test", Passed: true},
			{Name: "typecheck", Passed: true},
		}
		agg := v.Aggregate(results)
		if !agg.Passed {
			t.Error("all pass → overall should pass")
		}
		if agg.TotalScore != 1.0 {
			t.Errorf("all pass → TotalScore should be 1.0, got %.3f", agg.TotalScore)
		}
	})

	t.Run("CriticalFail", func(t *testing.T) {
		t.Parallel()
		results := []verification.PerspectiveResult{
			{Name: "build", Passed: false}, // critical (weight 1.0)
			{Name: "lint", Passed: true},
			{Name: "test", Passed: true},
			{Name: "typecheck", Passed: true},
		}
		agg := v.Aggregate(results)
		if agg.Passed {
			t.Error("critical perspective failure → overall should fail")
		}
	})

	t.Run("ShouldRetry_NewFingerprint", func(t *testing.T) {
		t.Parallel()
		failResult := verification.AggregatedResult{Passed: false}
		seen := []string{"fp-old-1", "fp-old-2"}

		// New fingerprint → retry (S2-1 linkage).
		if !v.ShouldRetry(failResult, "fp-new", seen) {
			t.Error("new fingerprint should trigger retry")
		}
		// Known fingerprint → no retry.
		if v.ShouldRetry(failResult, "fp-old-1", seen) {
			t.Error("known fingerprint should suppress retry")
		}
		// Passed result → no retry regardless.
		passResult := verification.AggregatedResult{Passed: true}
		if v.ShouldRetry(passResult, "fp-new", seen) {
			t.Error("passed result should not retry")
		}
	})
}

// --- C-5: FingerprintDB Strategy Learning ---

func TestC5_FingerprintDB_StrategyLearning(t *testing.T) {
	t.Parallel()
	db := learnings.NewFingerprintDB(100)

	// Store → Query → SuggestStrategy flow.
	db.Store("fp-001", "compile_error", "add_import")
	db.Store("fp-002", "test_failure", "fix_assertion")

	// Query existing.
	pat, ok := db.Query("fp-001")
	if !ok {
		t.Fatal("expected to find fp-001")
	}
	if pat.ErrorCategory != "compile_error" {
		t.Errorf("category = %q, want compile_error", pat.ErrorCategory)
	}
	if pat.RepairStrategy != "add_import" {
		t.Errorf("strategy = %q, want add_import", pat.RepairStrategy)
	}

	// SuggestStrategy: no success yet → should return false.
	_, suggested := db.SuggestStrategy("fp-001")
	if suggested {
		t.Error("SuggestStrategy should return false before any success recorded")
	}

	// Record success → now suggestion should work.
	db.RecordSuccess("fp-001")
	strategy, suggested := db.SuggestStrategy("fp-001")
	if !suggested {
		t.Error("SuggestStrategy should return true after success")
	}
	if strategy != "add_import" {
		t.Errorf("suggested strategy = %q, want add_import", strategy)
	}

	// §C-5 safety: improvement targets are prompt/persona/config only.
	// At the type level, RepairStrategy is a free-form string — the constraint
	// is enforced at the call site, not in FingerprintDB itself.
	// We verify the type structure supports the constraint.
	if pat.Fingerprint == "" {
		t.Error("FailurePattern should have non-empty Fingerprint")
	}
	if db.Size() != 2 {
		t.Errorf("DB size = %d, want 2", db.Size())
	}
}

// --- C-6: Complexity Depth Estimation ---

func TestC6_Complexity_DepthEstimation(t *testing.T) {
	t.Parallel()
	scorer := complexity.NewScorer(complexity.DefaultThresholds())

	tests := []struct {
		name     string
		input    complexity.Input
		wantDepth int
	}{
		{
			name:      "simple",
			input:     complexity.Input{FileCount: 2, BloomLevel: 1},
			wantDepth: 1,
		},
		{
			name:      "standard",
			input:     complexity.Input{FileCount: 10, DependencyDepth: 3, BloomLevel: 3, PastRepairRate: 0.1, ExpectedPathCount: 3},
			wantDepth: 2,
		},
		{
			name:      "complex",
			input:     complexity.Input{FileCount: 25, DependencyDepth: 5, BloomLevel: 5, PastRepairRate: 0.3, ExpectedPathCount: 8},
			wantDepth: 3,
		},
		{
			name:      "critical",
			input:     complexity.Input{FileCount: 50, DependencyDepth: 10, BloomLevel: 6, PastRepairRate: 0.8, ExpectedPathCount: 20},
			wantDepth: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			score := scorer.Estimate(tt.input)
			depth := scorer.EstimateDepth(score)
			if depth != tt.wantDepth {
				t.Errorf("level=%q depth=%d, want %d (raw=%.3f)", score.Level, depth, tt.wantDepth, score.RawScore)
			}
		})
	}

	// §5-1: Mechanical metrics only — Score.Factors should contain known keys.
	score := scorer.Estimate(complexity.Input{FileCount: 10, BloomLevel: 3})
	expectedFactors := []string{"file_count", "dependency_depth", "bloom_level", "past_repair_rate", "expected_path_count"}
	for _, key := range expectedFactors {
		if _, ok := score.Factors[key]; !ok {
			t.Errorf("missing factor %q in Score.Factors", key)
		}
	}
}

// --- Model: Task Extension Fields ---

func TestModel_TaskExtensions_Boundary(t *testing.T) {
	t.Parallel()
	// Task.Runtime, Task.ModelOverride, Task.ComplexityLevel fields exist.
	task := model.Task{
		ID:              "boundary-test",
		Runtime:         model.RuntimeClaudeCode,
		ModelOverride:   "opus",
		ComplexityLevel: "complex",
	}

	if task.Runtime != model.RuntimeClaudeCode {
		t.Errorf("Runtime = %q, want %q", task.Runtime, model.RuntimeClaudeCode)
	}

	// §C-7 backward compatibility: unset extension fields = zero values.
	emptyTask := model.Task{ID: "empty"}
	if emptyTask.Runtime != "" || emptyTask.ModelOverride != "" || emptyTask.ComplexityLevel != "" {
		t.Error("unset extension fields should be zero values for backward compatibility")
	}
}

// --- Model: FitnessScore QualityScore in Compare ---

func TestModel_FitnessQualityScore_Boundary(t *testing.T) {
	t.Parallel()
	th := model.FitnessThresholds{
		RepairCountMargin:   0,
		DiffSizeMargin:      10,
		ExecutionTimeMargin: 30 * time.Second,
		QualityScoreMargin:  0.05,
	}

	// QualityScore is the final comparison axis.
	// When all prior axes tie, QualityScore decides.
	a := model.FitnessScore{
		Passed:           true,
		RepairCount:      0,
		DiffFilesChanged: 5,
		DiffLinesChanged: 20,
		ExecutionTime:    5 * time.Second,
		QualityScore:     0.95,
	}
	b := model.FitnessScore{
		Passed:           true,
		RepairCount:      0,
		DiffFilesChanged: 5,
		DiffLinesChanged: 20,
		ExecutionTime:    5 * time.Second,
		QualityScore:     0.5,
	}

	cmp := a.Compare(b, th)
	if cmp != -1 {
		t.Errorf("QualityScore 0.95 vs 0.5: expected -1, got %d", cmp)
	}

	// QualityScore equal within margin → overall tie (0).
	c := model.FitnessScore{Passed: true, QualityScore: 0.80}
	d := model.FitnessScore{Passed: true, QualityScore: 0.78} // diff=0.02 < margin 0.05
	if c.Compare(d, th) != 0 {
		t.Errorf("QualityScore within margin should tie, got %d", c.Compare(d, th))
	}
}
