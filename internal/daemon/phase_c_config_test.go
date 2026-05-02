package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/complexity"
	"github.com/msageha/maestro_v2/internal/daemon/featuregate"
	"github.com/msageha/maestro_v2/internal/model"
)

// makeMaestroDir builds a fake .maestro directory under a fresh temp
// project root. Language detection is intentionally absent — every test
// exercises the language-agnostic baseline.
func makeMaestroDir(t *testing.T) string {
	t.Helper()
	maestroDir := filepath.Join(t.TempDir(), ".maestro")
	if err := os.MkdirAll(maestroDir, 0o755); err != nil {
		t.Fatalf("mkdir maestro: %v", err)
	}
	return maestroDir
}

// TestNewPhaseCManager_ExtendedVerificationWiresMaxAutoRetriesOnly pins the
// minimum contract of the redesigned extended_verification wiring:
// EnsembleVerifier is initialised with MaxAutoRetries from config, and no
// language-specific perspectives are auto-injected. Operators tailor
// per-project verify commands via .maestro/verify.yaml — that file is the
// single source of truth for which categories and commands run.
func TestNewPhaseCManager_ExtendedVerificationWiresMaxAutoRetriesOnly(t *testing.T) {
	t.Parallel()
	enabled := true
	maxRetries := 5
	cfg := model.Config{
		ExtendedVerification: model.ExtendedVerificationConfig{
			Enabled:        &enabled,
			MaxAutoRetries: &maxRetries,
		},
	}

	m := newPhaseCManager(cfg, makeMaestroDir(t), []string{"sonnet"}, discardDaemonLog)
	if m.EnsembleVerifier == nil {
		t.Fatalf("expected EnsembleVerifier to be initialized")
	}
	if got := m.EnsembleVerifier.MaxAutoRetries(); got != maxRetries {
		t.Fatalf("MaxAutoRetries=%d, want %d", got, maxRetries)
	}
	if got := len(m.EnsembleVerifier.Perspectives()); got != 0 {
		t.Errorf("EnsembleVerifier must not auto-inject perspectives (verify.yaml is the source of truth), got %d: %+v",
			got, m.EnsembleVerifier.Perspectives())
	}
}

// TestNewPhaseCManager_NoLanguageSpecificInjection guards that *every*
// project — regardless of which marker files are present — gets the same
// empty-perspective EnsembleVerifier. The previous implementation
// inspected go.mod / package.json / Cargo.toml / etc. and ran
// gosec / npm audit / cargo audit / etc. accordingly; that coupling is
// incompatible with maestro's autonomous-orchestration goal of being
// useful for polyglot, research, and documentation workloads.
func TestNewPhaseCManager_NoLanguageSpecificInjection(t *testing.T) {
	t.Parallel()
	enabled := true

	cases := []struct {
		name   string
		marker string
		body   []byte
	}{
		{name: "no markers", marker: "", body: nil},
		{name: "go marker", marker: "go.mod", body: []byte("module example.com/test\n")},
		{name: "node marker", marker: "package.json", body: []byte("{}")},
		{name: "rust marker", marker: "Cargo.toml", body: []byte("[package]\nname = \"x\"\nversion = \"0.1.0\"\n")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tc.marker != "" {
				if err := os.WriteFile(filepath.Join(root, tc.marker), tc.body, 0o600); err != nil {
					t.Fatalf("write %s: %v", tc.marker, err)
				}
			}
			maestroDir := filepath.Join(root, ".maestro")
			if err := os.MkdirAll(maestroDir, 0o755); err != nil {
				t.Fatalf("mkdir maestro: %v", err)
			}

			cfg := model.Config{
				ExtendedVerification: model.ExtendedVerificationConfig{Enabled: &enabled},
			}
			m := newPhaseCManager(cfg, maestroDir, []string{"sonnet"}, discardDaemonLog)
			if m.EnsembleVerifier == nil {
				t.Fatalf("expected EnsembleVerifier to be initialized")
			}
			if got := len(m.EnsembleVerifier.Perspectives()); got != 0 {
				t.Errorf("%s: expected zero auto-injected perspectives, got %d: %+v",
					tc.name, got, m.EnsembleVerifier.Perspectives())
			}
		})
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
