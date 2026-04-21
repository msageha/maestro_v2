package daemon

import (
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
func newPhaseCManager(cfg model.Config, availableModels []string, log logFunc) *PhaseCManager {
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
		log(LogLevelInfo, "evolution engine initialized")
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
	if cfg.ExtendedVerification.EffectiveEnabled() {
		m.EnsembleVerifier = verification.NewVerifier()
		for _, p := range m.EnsembleVerifier.DefaultPerspectives() {
			if err := m.EnsembleVerifier.AddPerspective(p); err != nil {
				log(LogLevelWarn, "skipping perspective %s: %v", p.Name, err)
			}
		}
		log(LogLevelInfo, "ensemble verifier initialized")
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
		m.FingerprintDB = learnings.NewFingerprintDB(cfg.SelfImprovement.EffectiveArchiveMaxSize())
		log(LogLevelInfo, "fingerprint DB initialized")
	}

	// C-6 Complexity Scorer
	if cfg.Complexity.EffectiveEnabled() {
		m.ComplexityScorer = complexity.NewScorer(complexity.DefaultThresholds())
		log(LogLevelInfo, "complexity scorer initialized")
	}

	// C-8 Feature Gate
	m.FeatureEvaluator = featuregate.NewEvaluator()
	if len(cfg.FeatureProfiles) > 0 {
		profiles := make(map[string]map[string]interface{}, len(cfg.FeatureProfiles))
		for level, fp := range cfg.FeatureProfiles {
			profiles[level] = map[string]interface{}{
				"cross_agent_review":       fp.EffectiveCrossAgentReview() != "false",
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
