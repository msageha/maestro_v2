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

	// ImprovementStore tracks the C-5 friction-driven improvement lifecycle
	// (issue #26): friction events → proposals → effect measurement gate →
	// auto-reopen on regression. nil unless both self_improvement.enabled
	// and self_improvement.friction.enabled are set. Shares fingerprints
	// with FingerprintDB — not a parallel learning loop.
	ImprovementStore *learnings.ImprovementStore
	improvementsPath string
	// improvementInjectCount caps proposals injected into Planner command
	// envelopes; improvementExcludeTargets echoes the config exclusions
	// into the injected section (and filters entries, defense in depth).
	improvementInjectCount    int
	improvementExcludeTargets []string
	// maestroDir supports best-effort state file reads (metrics baseline
	// snapshots for improvement measurement).
	maestroDir string

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

	// banditMu protects the C-2 taskBloom bookkeeping below. Separate from
	// searchMu / evolutionMu so the code paths don't contend.
	banditMu sync.Mutex
	// taskBloom maps taskID → Bloom level recorded at dispatch (tagged with
	// a generation seq). Consumed when the task terminates so
	// recordBanditReward can route the reward to the matching difficulty
	// bucket (contextual bandit). Bounded by taskBloomMaxEntries — see
	// RecordTaskBloom.
	taskBloom map[string]taskBloomEntry
	// taskBloomOrder tracks taskBloom insertion order for FIFO eviction.
	// May contain already-consumed entries (lazy deletion): eviction skips
	// entries whose id+seq no longer match the live record.
	taskBloomOrder []taskBloomOrderEntry
	// taskBloomSeq is the generation counter for taskBloom entries.
	taskBloomSeq uint64

	// repairMu protects the C-5 repair-strategy attribution tables below
	// (see phase_c_repair.go). Separate from the sibling mutexes so the
	// failure/retry paths don't contend with bloom or search bookkeeping.
	repairMu sync.Mutex
	// taskFailureFP maps a FAILED taskID → its error fingerprint, so a
	// retry of that task can resolve the failure pattern at dispatch time.
	taskFailureFP *boundedFPMap
	// retryFP maps a dispatched RETRY taskID → the predecessor's failure
	// fingerprint; consumed at the retry's terminal result to credit the
	// pattern on success.
	retryFP *boundedFPMap

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
		taskBloom:       make(map[string]taskBloomEntry),
		commandNovelty:  make(map[string]map[string]struct{}),
		commandFailures: make(map[string]int),
		maestroDir:      maestroDir,
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
			// Arms are keyed by model FAMILY (E-4): worker eligibility in
			// plan.AssignWorkers matches by family, and the reward path
			// resolves aliases ("opus" under boost, "sonnet" fallback) —
			// keying arms by the raw config spelling ("claude-opus-4-7")
			// silently dropped those rewards on the arm-name mismatch.
			// AddArm dedups, so several spellings of one family collapse
			// into a single arm.
			for _, name := range availableModels {
				m.BanditSelector.AddArm(model.Family(name))
			}
			log(LogLevelInfo, "bandit selector initialized arms=%d", len(m.BanditSelector.GetStats()))
		}
	}

	// C-3 Extended Verification
	//
	// Verification is driven purely by .maestro/verify.yaml (language-
	// agnostic). Auto-detecting a "project language" and running language-
	// bound commands would assume a software-engineering monorepo with a
	// single stack and would break for polyglot repositories, research/
	// documentation projects, and any context that doesn't ship the
	// assumed toolchain.
	//
	// EnsembleVerifier is still wired (when extended_verification.enabled
	// is true) so operator-supplied per-perspective weights from
	// PerspectiveWeights continue to apply to verify.yaml-loaded categories
	// via buildVerifyCategories — preserves the criticality/advisory
	// distinction without language detection.
	if cfg.ExtendedVerification.EffectiveEnabled() {
		m.EnsembleVerifier = verification.NewVerifier()
		m.EnsembleVerifier.SetMaxAutoRetries(cfg.ExtendedVerification.EffectiveMaxAutoRetries())
		// Perspectives are named after verify.yaml categories and carry the
		// operator-supplied weight only — the commands themselves come from
		// verify.yaml at run time (RealVerifyRunner.buildVerifyCategories
		// looks the weight up by category name). Non-positive weights are
		// dropped so a config typo cannot silently zero out a category.
		perspectives := make([]verification.Perspective, 0, len(cfg.ExtendedVerification.PerspectiveWeights))
		for name, weight := range cfg.ExtendedVerification.PerspectiveWeights {
			if weight <= 0 {
				log(LogLevelWarn, "ensemble perspective %q has non-positive weight %v; ignored (category stays critical)", name, weight)
				continue
			}
			perspectives = append(perspectives, verification.Perspective{Name: name, Weight: weight})
		}
		if len(perspectives) > 0 {
			if err := m.EnsembleVerifier.SetPerspectives(perspectives); err != nil {
				log(LogLevelWarn, "ensemble perspectives rejected: %v", err)
			}
		}
		log(LogLevelInfo,
			"ensemble verifier initialized (verify.yaml-driven; weights from extended_verification.perspective_weights) perspectives=%d max_auto_retries=%d",
			len(m.EnsembleVerifier.Perspectives()), m.EnsembleVerifier.MaxAutoRetries())
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

		// C-5 friction-driven improvement loop (issue #26) — nested opt-in
		// under self_improvement so it extends the fingerprint machinery
		// above instead of running beside it.
		if cfg.SelfImprovement.Friction.EffectiveEnabled() {
			opts := learnings.ImprovementStoreOptions{
				MaxEntries:         cfg.SelfImprovement.Friction.EffectiveMaxEntries(),
				MinOccurrences:     cfg.SelfImprovement.Friction.EffectiveMinOccurrences(),
				VerifyMinSuccesses: cfg.SelfImprovement.Friction.EffectiveVerifyMinSuccesses(),
				ExcludeTargets:     cfg.SelfImprovement.EffectiveExcludeTargets(),
			}
			m.improvementsPath = filepath.Join(maestroDir, "state", "improvements.yaml")
			store, err := learnings.LoadImprovementStore(m.improvementsPath, opts)
			if err != nil {
				log(LogLevelWarn, "improvement store load failed path=%s error=%v; starting empty", m.improvementsPath, err)
				store = learnings.NewImprovementStore(opts)
			}
			m.ImprovementStore = store
			m.improvementInjectCount = cfg.SelfImprovement.Friction.EffectiveInjectCount()
			m.improvementExcludeTargets = cfg.SelfImprovement.EffectiveExcludeTargets()
			log(LogLevelInfo, "improvement store initialized entries=%d min_occurrences=%d verify_min_successes=%d inject_count=%d",
				store.Size(), opts.MinOccurrences, opts.VerifyMinSuccesses, m.improvementInjectCount)
		}
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

// Verification commands are not auto-injected by the daemon based on
// language detection (go.mod / package.json / etc.) — verification is
// driven by .maestro/verify.yaml so the harness remains useful for
// polyglot monorepos, research, and documentation projects.
// EnsembleVerifier still applies operator-supplied PerspectiveWeights to
// verify.yaml categories via buildVerifyCategories.

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
	if m.ImprovementStore != nil && m.improvementsPath != "" {
		if err := m.ImprovementStore.SaveYAML(m.improvementsPath); err != nil {
			log(LogLevelWarn, "improvement store save failed path=%s error=%v", m.improvementsPath, err)
		} else {
			log(LogLevelInfo, "improvement store saved path=%s entries=%d", m.improvementsPath, m.ImprovementStore.Size())
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
	if m.ImprovementStore != nil {
		counts := m.ImprovementStore.CountsByStatus()
		log(LogLevelInfo, "improvement store stats entries=%d proposed=%d applied=%d verified=%d reopened=%d",
			m.ImprovementStore.Size(),
			counts[model.ImprovementStatusProposed],
			counts[model.ImprovementStatusApplied],
			counts[model.ImprovementStatusVerified],
			counts[model.ImprovementStatusReopened])
	}
}
