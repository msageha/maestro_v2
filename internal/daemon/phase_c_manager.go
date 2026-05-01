package daemon

import (
	"path/filepath"
	"sync"

	"github.com/msageha/maestro_v2/internal/daemon/bandit"
	"github.com/msageha/maestro_v2/internal/daemon/complexity"
	"github.com/msageha/maestro_v2/internal/daemon/evolution"
	"github.com/msageha/maestro_v2/internal/daemon/featuregate"
	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/daemon/search"
	"github.com/msageha/maestro_v2/internal/daemon/verification"
	"github.com/msageha/maestro_v2/internal/model"
)

// PhaseCManager owns all Phase C (continuous improvement) components.
// Extracting these from Daemon reduces its field count and groups related
// functionality behind a single composition boundary.
type PhaseCManager struct {
	EvolutionEngine  *evolution.Engine
	BanditSelector   *bandit.Selector
	EnsembleVerifier *verification.Verifier
	SearchTree       *search.Tree
	SearchSampler    *search.Sampler
	FingerprintDB    *learnings.FingerprintDB
	ComplexityScorer *complexity.Scorer
	FeatureEvaluator *featuregate.Evaluator
	fingerprintPath  string

	// searchMu protects the per-command search-tree bookkeeping below.
	// Required because classifyAndLog* runs on the dispatch goroutine while
	// observeTaskOutcome runs on the result-notification goroutine.
	searchMu sync.Mutex
	// commandRoots tracks commandIDs already added to SearchTree as root nodes.
	// Protects against duplicate AddRoot when a command re-enters dispatch
	// (e.g., after a retry or a partial scan).
	commandRoots map[string]bool
	// taskDecisions maps taskID → Thompson Sampler decision taken at dispatch.
	// Consumed when the task terminates so sampler.Update can reward the
	// original widen/deepen choice. Cleared after consumption to bound memory.
	taskDecisions map[string]search.Decision

	// evolutionMu protects the per-command evolutionary bookkeeping below.
	// Separate from searchMu so the two code paths don't contend.
	evolutionMu sync.Mutex
	// commandNovelty tracks the set of successful-task summary hashes per
	// command, used by the EvolutionEngine.CheckNovelty call at task
	// completion. Bounded by evolutionNoveltyMaxPerCmd entries per command.
	commandNovelty map[string]map[string]struct{}
	// commandFailures tracks per-command failure counts used to decide when
	// to trigger PlanMutations for a retry strategy plan.
	commandFailures map[string]int
}

// evolutionNoveltyMaxPerCmd caps the per-command novelty hash set to keep
// memory bounded for long-running commands. When the cap is reached, new
// hashes are still checked but not stored. 256 entries per command ≈ 8 KB.
const evolutionNoveltyMaxPerCmd = 256

// evolutionFailureRetryThreshold is the failure count at which PlanMutations
// is invoked to plan a retry strategy. Below the threshold, failures are
// handled by the existing retry path without evolutionary mutation planning.
const evolutionFailureRetryThreshold = 2

// logFunc is a logging callback matching Daemon.log signature.
type logFunc func(level LogLevel, format string, args ...any)

// newPhaseCManager creates and initializes all Phase C sub-components based on
// the provided configuration. Each sub-component is conditionally initialized
// based on its EffectiveEnabled() flag.
func newPhaseCManager(cfg model.Config, maestroDir string, availableModels []string, log logFunc) *PhaseCManager {
	m := &PhaseCManager{
		commandRoots:    make(map[string]bool),
		taskDecisions:   make(map[string]search.Decision),
		commandNovelty:  make(map[string]map[string]struct{}),
		commandFailures: make(map[string]int),
	}

	// C-1 Evolution Engine
	if cfg.Evolution.EffectiveEnabled() {
		strategies := make([]evolution.Strategy, 0, len(cfg.Evolution.EffectiveStrategies()))
		for _, s := range cfg.Evolution.EffectiveStrategies() {
			strategies = append(strategies, evolution.Strategy(s))
		}
		weights := make(map[evolution.Strategy]int)
		for k, v := range cfg.Evolution.EffectiveStrategyWeights() {
			weights[evolution.Strategy(k)] = v
		}
		m.EvolutionEngine = evolution.NewEngine(strategies, weights)
		m.EvolutionEngine.SetMaxMutationsPerRound(cfg.Evolution.EffectiveMaxMutationsPerRound())
		m.EvolutionEngine.SetNoveltyThreshold(cfg.Evolution.EffectiveNoveltyThreshold())
		log(LogLevelInfo, "evolution engine initialized max_mutations=%d novelty_threshold=%.3f",
			cfg.Evolution.EffectiveMaxMutationsPerRound(),
			cfg.Evolution.EffectiveNoveltyThreshold())
	}

	// C-2 Adaptive Model Selection
	if cfg.Bandit.EffectiveEnabled() {
		sel, err := bandit.NewSelector(cfg.Bandit.EffectiveExplorationCoeff())
		if err != nil {
			log(LogLevelError, "bandit selector initialization failed: %v", err)
		} else {
			m.BanditSelector = sel
			for _, model := range availableModels {
				m.BanditSelector.AddArm(model)
			}
			log(LogLevelInfo, "bandit selector initialized arms=%d", len(m.BanditSelector.GetStats()))
		}
	}

	// C-3 Extended Verification
	//
	// The 2026-04-30 redesign drops every language-specific auto-injection
	// (npm audit / pip-audit / cargo audit / gosec / go test / cargo test /
	// etc.). Auto-detecting a "project language" and running language-bound
	// commands assumes a software-engineering monorepo with a single stack;
	// it breaks for polyglot repositories, research/documentation projects,
	// and any context that doesn't ship the assumed toolchain. Verification
	// is now driven purely by what the operator writes in
	// .maestro/verify.yaml — that file is language-agnostic and lets the
	// project's own concept of "verification" survive without the daemon
	// guessing.
	//
	// EnsembleVerifier is still wired (when extended_verification.enabled
	// is true) so operator-supplied per-perspective weights from
	// PerspectiveWeights continue to apply to verify.yaml-loaded categories
	// via buildVerifyCategories — this preserves the criticality/advisory
	// distinction without re-introducing language detection.
	if cfg.ExtendedVerification.EffectiveEnabled() {
		m.EnsembleVerifier = verification.NewVerifier()
		m.EnsembleVerifier.SetMaxAutoRetries(cfg.ExtendedVerification.EffectiveMaxAutoRetries())
		log(LogLevelInfo,
			"ensemble verifier initialized (verify.yaml-driven; language-specific auto-injection removed) max_auto_retries=%d",
			m.EnsembleVerifier.MaxAutoRetries())
	}

	// C-4 Exploratory Search
	if cfg.Search.EffectiveEnabled() {
		m.SearchTree = search.NewTree(
			cfg.Search.EffectiveMaxDepth(),
			cfg.Search.EffectiveMaxBranching(),
			cfg.Search.EffectivePruneThreshold(),
		)
		m.SearchSampler = search.NewSampler(
			cfg.Search.EffectiveThompsonAlpha(),
			cfg.Search.EffectiveThompsonBeta(),
		)
		log(LogLevelInfo, "search tree initialized")
	}

	// C-5 FingerprintDB (Self-Improvement)
	if cfg.SelfImprovement.EffectiveEnabled() {
		m.fingerprintPath = filepath.Join(maestroDir, "state", "fingerprint_db.json")
		db, err := learnings.LoadFingerprintDB(m.fingerprintPath, cfg.SelfImprovement.EffectiveArchiveMaxSize())
		if err != nil {
			log(LogLevelWarn, "fingerprint DB load failed path=%s error=%v; starting empty", m.fingerprintPath, err)
			db = learnings.NewFingerprintDB(cfg.SelfImprovement.EffectiveArchiveMaxSize())
		}
		m.FingerprintDB = db
		log(LogLevelInfo, "fingerprint DB initialized patterns=%d targets=%v exclude_targets=%v",
			m.FingerprintDB.Size(),
			cfg.SelfImprovement.EffectiveTargets(),
			cfg.SelfImprovement.EffectiveExcludeTargets())
	}

	// C-6 Complexity Scorer
	if cfg.Complexity.EffectiveEnabled() {
		fileThresholds := complexity.FileThresholds{
			SimpleMaxFiles:   cfg.Complexity.Thresholds.EffectiveSimpleMaxFiles(),
			StandardMaxFiles: cfg.Complexity.Thresholds.EffectiveStandardMaxFiles(),
			ComplexMaxFiles:  cfg.Complexity.Thresholds.EffectiveComplexMaxFiles(),
		}
		m.ComplexityScorer = complexity.NewScorerWithFileThresholds(complexity.DefaultThresholds(), fileThresholds)
		log(LogLevelInfo, "complexity scorer initialized simple_max_files=%d standard_max_files=%d complex_max_files=%d",
			fileThresholds.SimpleMaxFiles,
			fileThresholds.StandardMaxFiles,
			fileThresholds.ComplexMaxFiles)
	}

	// C-8 Feature Gate
	m.FeatureEvaluator = featuregate.NewEvaluator()
	if len(cfg.FeatureProfiles) > 0 {
		profiles := make(map[string]map[string]interface{}, len(cfg.FeatureProfiles))
		for level, fp := range cfg.FeatureProfiles {
			profiles[level] = map[string]interface{}{
				"cross_agent_review":       fp.EffectiveCrossAgentReview(),
				"exploratory_optimization": fp.EffectiveExploratoryOptimization(),
				"evolutionary_quality":     fp.EffectiveEvolutionaryQuality(),
				"adaptive_model_selection": fp.EffectiveAdaptiveModelSelection(),
				"self_improvement":         fp.EffectiveSelfImprovement(),
				"adaptive_depth":           fp.EffectiveAdaptiveDepth(),
			}
		}
		m.FeatureEvaluator.LoadProfiles(profiles)
		log(LogLevelInfo, "feature evaluator initialized with %d config profiles", len(cfg.FeatureProfiles))
	} else {
		log(LogLevelInfo, "feature evaluator initialized with default profiles")
	}

	return m
}

// configureVerificationPerspectives was removed in the 2026-04-30 redesign.
// The previous implementation auto-injected language-specific perspectives
// (build/lint/test/typecheck/security/performance) by detecting marker
// files like go.mod or package.json and dispatching tool-specific commands.
// That approach baked in the assumption that maestro is always running
// against a single-language software-engineering project, which is
// incompatible with the autonomous-orchestration philosophy: the harness
// must be useful for polyglot monorepos, research, documentation, and any
// other context where the host project defines verification on its own
// terms via .maestro/verify.yaml. EnsembleVerifier remains in place so
// operator-supplied PerspectiveWeights still influence the criticality of
// verify.yaml categories via buildVerifyCategories, but commands no longer
// originate inside the daemon.

// SaveState persists stateful Phase C components that must survive daemon
// restarts.
func (m *PhaseCManager) SaveState(log logFunc) {
	if m == nil {
		return
	}
	if m.FingerprintDB != nil && m.fingerprintPath != "" {
		if err := m.FingerprintDB.SaveJSON(m.fingerprintPath); err != nil {
			log(LogLevelWarn, "fingerprint DB save failed path=%s error=%v", m.fingerprintPath, err)
		} else {
			log(LogLevelInfo, "fingerprint DB saved path=%s patterns=%d", m.fingerprintPath, m.FingerprintDB.Size())
		}
	}
}

// LogShutdownStats logs summary statistics for stateful Phase C components.
// Called during daemon shutdown to capture component state before teardown.
func (m *PhaseCManager) LogShutdownStats(log logFunc) {
	if m == nil {
		return
	}
	if m.SearchTree != nil {
		log(LogLevelInfo, "search tree cleanup nodes=%d", m.SearchTree.NodeCount())
	}
	if m.FingerprintDB != nil {
		log(LogLevelInfo, "fingerprint DB stats patterns=%d", m.FingerprintDB.Size())
	}
}
