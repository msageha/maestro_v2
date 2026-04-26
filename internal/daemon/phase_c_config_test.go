package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/complexity"
	"github.com/msageha/maestro_v2/internal/daemon/featuregate"
	"github.com/msageha/maestro_v2/internal/model"
)

func TestNewPhaseCManager_WiresExtendedVerificationConfig(t *testing.T) {
	t.Parallel()
	enabled := true
	maxRetries := 5
	cfg := model.Config{
		ExtendedVerification: model.ExtendedVerificationConfig{
			Enabled:          &enabled,
			SecurityCheck:    &enabled,
			PerformanceBench: &enabled,
			PerspectiveWeights: map[string]float64{
				"build":       0.75,
				"security":    0.6,
				"performance": 0.4,
			},
			MaxAutoRetries: &maxRetries,
		},
	}

	m := newPhaseCManager(cfg, t.TempDir(), []string{"sonnet"}, discardDaemonLog)
	if m.EnsembleVerifier == nil {
		t.Fatalf("expected EnsembleVerifier to be initialized")
	}
	if got := m.EnsembleVerifier.MaxAutoRetries(); got != maxRetries {
		t.Fatalf("MaxAutoRetries=%d, want %d", got, maxRetries)
	}

	seen := map[string]float64{}
	for _, p := range m.EnsembleVerifier.Perspectives() {
		if _, exists := seen[p.Name]; exists {
			t.Fatalf("duplicate perspective %q", p.Name)
		}
		seen[p.Name] = p.Weight
	}
	for name, want := range map[string]float64{"build": 0.75, "security": 0.6, "performance": 0.4} {
		if got, ok := seen[name]; !ok || got != want {
			t.Fatalf("perspective %q weight=%v present=%t, want %v", name, got, ok, want)
		}
	}
}

func TestNewPhaseCManager_WiresEvolutionConfig(t *testing.T) {
	t.Parallel()
	enabled := true
	maxMutations := 2
	noveltyThreshold := 0.99
	cfg := model.Config{
		Evolution: model.EvolutionConfig{
			Enabled:              &enabled,
			MaxMutationsPerRound: &maxMutations,
			NoveltyThreshold:     &noveltyThreshold,
			Strategies:           []string{"diff", "full", "cross"},
		},
	}

	m := newPhaseCManager(cfg, t.TempDir(), nil, discardDaemonLog)
	if m.EvolutionEngine == nil {
		t.Fatalf("expected EvolutionEngine to be initialized")
	}
	m.PlanRetryMutations("cmd-1", 2)
	slots, planned := m.PlanRetryMutations("cmd-1", 2)
	if !planned {
		t.Fatalf("expected retry mutation plan at threshold")
	}
	if len(slots) != maxMutations {
		t.Fatalf("mutation slots=%d, want cap %d", len(slots), maxMutations)
	}

	if novel, evaluated := m.RecordTaskCompletionNovelty("cmd-1", "same summary"); !evaluated || !novel {
		t.Fatalf("first summary evaluated=%t novel=%t, want true/true", evaluated, novel)
	}
	if novel, evaluated := m.RecordTaskCompletionNovelty("cmd-1", "same summary"); !evaluated || novel {
		t.Fatalf("duplicate summary evaluated=%t novel=%t, want true/false", evaluated, novel)
	}
}

func TestNewPhaseCManager_WiresComplexityFileThresholds(t *testing.T) {
	t.Parallel()
	enabled := true
	simple, standard, complexMax := 3, 10, 30
	cfg := model.Config{
		Complexity: model.ComplexityConfig{
			Enabled: &enabled,
			Thresholds: model.ComplexityThresholds{
				SimpleMaxFiles:   &simple,
				StandardMaxFiles: &standard,
				ComplexMaxFiles:  &complexMax,
			},
		},
	}

	m := newPhaseCManager(cfg, t.TempDir(), nil, discardDaemonLog)
	if got := m.EvaluateLevel(complexity.Input{FileCount: 4}); got != featuregate.LevelStandard {
		t.Fatalf("FileCount=4 level=%s, want %s", got, featuregate.LevelStandard)
	}
}

func discardDaemonLog(LogLevel, string, ...any) {}
