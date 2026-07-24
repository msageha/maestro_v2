package model

// --- C-1 Evolution Config ---

// EvolutionConfig controls evolutionary quality improvement.
type EvolutionConfig struct {
	Enabled              *bool          `yaml:"enabled,omitempty"`
	MaxMutationsPerRound *int           `yaml:"max_mutations_per_round,omitempty"`
	NoveltyThreshold     *float64       `yaml:"novelty_threshold,omitempty"`
	Strategies           []string       `yaml:"strategies,omitempty"`
	StrategyWeights      map[string]int `yaml:"strategy_weights,omitempty"`
}

// EffectiveEnabled returns Enabled, defaulting to false when unset.
func (e EvolutionConfig) EffectiveEnabled() bool { return effectiveValue(e.Enabled, false) }

// EffectiveMaxMutationsPerRound returns MaxMutationsPerRound, or DefaultMaxMutationsPerRound when unset.
func (e EvolutionConfig) EffectiveMaxMutationsPerRound() int {
	return effectiveValue(e.MaxMutationsPerRound, DefaultMaxMutationsPerRound)
}

// EffectiveNoveltyThreshold returns NoveltyThreshold, or DefaultNoveltyThreshold when unset.
func (e EvolutionConfig) EffectiveNoveltyThreshold() float64 {
	return effectiveValue(e.NoveltyThreshold, DefaultNoveltyThreshold)
}

// EffectiveStrategyWeights returns the configured strategy weights or defaults (diff:2, full:1, cross:1).
func (e EvolutionConfig) EffectiveStrategyWeights() map[string]int {
	if len(e.StrategyWeights) > 0 {
		return e.StrategyWeights
	}
	return map[string]int{"diff": 2, "full": 1, "cross": 1}
}

// EffectiveStrategies returns the configured strategies or ["diff","full","cross"] as default.
func (e EvolutionConfig) EffectiveStrategies() []string {
	if len(e.Strategies) > 0 {
		return e.Strategies
	}
	return []string{"diff", "full", "cross"}
}

// --- C-2 Bandit Config ---

// BanditConfig controls adaptive model selection (UCB1-based).
type BanditConfig struct {
	Enabled              *bool    `yaml:"enabled,omitempty"`
	ExplorationCoeff     *float64 `yaml:"exploration_coefficient,omitempty"`
	MinSamplesBeforeUse  *int     `yaml:"min_samples_before_use,omitempty"`
	TraceDataRequirement *int     `yaml:"trace_data_requirement,omitempty"`
}

// EffectiveEnabled returns Enabled, defaulting to false when unset.
func (b BanditConfig) EffectiveEnabled() bool { return effectiveValue(b.Enabled, false) }

// EffectiveExplorationCoeff returns ExplorationCoeff, or DefaultExplorationCoeff when unset.
func (b BanditConfig) EffectiveExplorationCoeff() float64 {
	return effectiveValue(b.ExplorationCoeff, DefaultExplorationCoeff)
}

// EffectiveMinSamplesBeforeUse returns MinSamplesBeforeUse, or DefaultMinSamplesBeforeUse when unset.
func (b BanditConfig) EffectiveMinSamplesBeforeUse() int {
	return effectiveValue(b.MinSamplesBeforeUse, DefaultMinSamplesBeforeUse)
}

// EffectiveTraceDataRequirement returns TraceDataRequirement, or DefaultTraceDataRequirement when unset.
func (b BanditConfig) EffectiveTraceDataRequirement() int {
	return effectiveValue(b.TraceDataRequirement, DefaultTraceDataRequirement)
}

// --- A/B Candidate Selection Config ---

// ABTestConfig controls cross-runtime A/B candidate selection (best-of-2).
// Disabled by default. Only knobs the implementation actually reads are
// defined here. See docs/design/ab_candidate_selection.md.
type ABTestConfig struct {
	Enabled *bool `yaml:"enabled,omitempty"`
	// MinBloomLevel is the minimum task bloom_level that triggers an A/B
	// race (default 4 — opus-tier tasks).
	MinBloomLevel *int `yaml:"min_bloom_level,omitempty"`
	// TimeoutSec bounds the TOTAL racing window, measured from group
	// creation. Past it, a candidate that never dispatched (still pending /
	// row lost) is cancelled so a completed opponent can walk over; running
	// candidates are left to their own DOA/lease bounds. 0 = follow the
	// task's definition_of_abort.max_wall_clock_sec.
	TimeoutSec *int `yaml:"timeout_sec,omitempty"`
	// SelectionTimeoutSec bounds how long a group may sit in `selecting`
	// without converging (integration busy, failing candidate commit)
	// before the walkover / repair-degrade escape fires.
	SelectionTimeoutSec *int `yaml:"selection_timeout_sec,omitempty"`
	// CrossTestPatterns extends the built-in test-file basename patterns
	// (DefaultCrossTestPatterns) used by the Stage 1 cross-test matrix to
	// machine-extract a candidate's added/modified test files. Basename
	// globs only — patterns containing '/' are rejected by validation.
	CrossTestPatterns []string `yaml:"cross_test_patterns,omitempty"`
	// JudgeModels are the Stage 3 cross-LLM judges (full-tie decider).
	// Unset (nil) = DefaultABJudgeModels (a fixed cross-runtime pair —
	// intentionally NOT derived from the worker roster so selection
	// semantics stay environment-independent). Explicit empty list
	// disables Stage 3. Exactly one entry is rejected by validation:
	// the agree/disagree protocol needs two votes.
	JudgeModels []string `yaml:"judge_models,omitempty"`
}

// DefaultABJudgeModels is the fixed cross-runtime judge pair for Stage 3.
var DefaultABJudgeModels = []string{"claude-sonnet-4-6", "codex"}

// DefaultCrossTestPatterns are the built-in basename globs identifying test
// files for the cross-test matrix (design §5 Stage 1).
var DefaultCrossTestPatterns = []string{
	"*_test.go", "test_*.py", "*_test.py", "*.test.*", "*.spec.*", "*_spec.rb",
}

// EffectiveEnabled returns Enabled, defaulting to false when unset.
func (a ABTestConfig) EffectiveEnabled() bool { return effectiveValue(a.Enabled, false) }

// EffectiveMinBloomLevel returns MinBloomLevel, or DefaultABMinBloomLevel when unset.
func (a ABTestConfig) EffectiveMinBloomLevel() int {
	return effectiveValue(a.MinBloomLevel, DefaultABMinBloomLevel)
}

// EffectiveTimeoutSec returns TimeoutSec, or 0 (follow the task budget) when unset.
func (a ABTestConfig) EffectiveTimeoutSec() int { return effectiveValue(a.TimeoutSec, 0) }

// EffectiveSelectionTimeoutSec returns SelectionTimeoutSec, or
// DefaultABSelectionTimeoutSec when unset.
func (a ABTestConfig) EffectiveSelectionTimeoutSec() int {
	return effectiveValue(a.SelectionTimeoutSec, DefaultABSelectionTimeoutSec)
}

// EffectiveJudgeModels returns the Stage 3 judges: DefaultABJudgeModels
// when unset, the configured list otherwise (empty = Stage 3 disabled).
func (a ABTestConfig) EffectiveJudgeModels() []string {
	if a.JudgeModels == nil {
		return append([]string{}, DefaultABJudgeModels...)
	}
	return a.JudgeModels
}

// EffectiveCrossTestPatterns returns the built-in patterns plus any
// configured additions (duplicates removed, order preserved).
func (a ABTestConfig) EffectiveCrossTestPatterns() []string {
	out := make([]string, 0, len(DefaultCrossTestPatterns)+len(a.CrossTestPatterns))
	seen := map[string]bool{}
	for _, p := range append(append([]string{}, DefaultCrossTestPatterns...), a.CrossTestPatterns...) {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// --- C-3 Extended Verification Config ---

// ExtendedVerificationConfig controls retry-on-fail behaviour for verify
// runs. The single runtime knob is max_auto_retries, which gates how many
// automatic retries the EnsembleVerifier performs against the
// operator-supplied verify.yaml.
type ExtendedVerificationConfig struct {
	Enabled        *bool `yaml:"enabled,omitempty"`
	MaxAutoRetries *int  `yaml:"max_auto_retries,omitempty"`
	// PerspectiveWeights assigns a weight to a verify.yaml category name
	// (build/lint/typecheck/test/security/performance). Weight >= 1.0 keeps
	// the category critical (its failure fails the run, the default for
	// unlisted categories); 0 < weight < 1.0 demotes it to advisory (its
	// failure is recorded and lowers the ensemble score but does not fail
	// the run). Non-positive weights are ignored at wiring time.
	PerspectiveWeights map[string]float64 `yaml:"perspective_weights,omitempty"`
}

// EffectiveEnabled returns Enabled, defaulting to false when unset.
func (ev ExtendedVerificationConfig) EffectiveEnabled() bool {
	return effectiveValue(ev.Enabled, false)
}

// EffectiveMaxAutoRetries returns MaxAutoRetries, or DefaultMaxAutoRetries when unset.
func (ev ExtendedVerificationConfig) EffectiveMaxAutoRetries() int {
	return effectiveValue(ev.MaxAutoRetries, DefaultMaxAutoRetries)
}

// --- C-4 Search Config ---

// SearchConfig controls search-based optimization (Alpha-Beta, Thompson Sampling).
type SearchConfig struct {
	Enabled        *bool    `yaml:"enabled,omitempty"`
	MaxDepth       *int     `yaml:"max_depth,omitempty"`
	MaxBranching   *int     `yaml:"max_branching,omitempty"`
	PruneThreshold *float64 `yaml:"prune_threshold,omitempty"`
	ThompsonAlpha  *float64 `yaml:"thompson_alpha,omitempty"`
	ThompsonBeta   *float64 `yaml:"thompson_beta,omitempty"`
}

// EffectiveEnabled returns Enabled, defaulting to false when unset.
func (s SearchConfig) EffectiveEnabled() bool { return effectiveValue(s.Enabled, false) }

// EffectiveMaxDepth returns MaxDepth, or DefaultSearchMaxDepth when unset.
func (s SearchConfig) EffectiveMaxDepth() int {
	return effectiveValue(s.MaxDepth, DefaultSearchMaxDepth)
}

// EffectiveMaxBranching returns MaxBranching, or DefaultMaxBranching when unset.
func (s SearchConfig) EffectiveMaxBranching() int {
	return effectiveValue(s.MaxBranching, DefaultMaxBranching)
}

// EffectivePruneThreshold returns PruneThreshold, or DefaultPruneThreshold when unset.
func (s SearchConfig) EffectivePruneThreshold() float64 {
	return effectiveValue(s.PruneThreshold, DefaultPruneThreshold)
}

// EffectiveThompsonAlpha returns ThompsonAlpha, or DefaultThompsonAlpha when unset.
func (s SearchConfig) EffectiveThompsonAlpha() float64 {
	return effectiveValue(s.ThompsonAlpha, DefaultThompsonAlpha)
}

// EffectiveThompsonBeta returns ThompsonBeta, or DefaultThompsonBeta when unset.
func (s SearchConfig) EffectiveThompsonBeta() float64 {
	return effectiveValue(s.ThompsonBeta, DefaultThompsonBeta)
}

// --- C-5 Self-Improvement Config ---

// SelfImprovementConfig controls self-improvement of prompts and personas.
type SelfImprovementConfig struct {
	Enabled        *bool    `yaml:"enabled,omitempty"`
	Targets        []string `yaml:"targets,omitempty"`
	ExcludeTargets []string `yaml:"exclude_targets,omitempty"`
	ArchiveMaxSize *int     `yaml:"archive_max_size,omitempty"`
	// Friction configures the friction-driven improvement loop (issue #26):
	// recurring operational friction → improvement proposal → effect
	// measurement gate → auto-reopen on regression. Nested under
	// self_improvement (no parallel feature gate); active only when BOTH
	// self_improvement.enabled and friction.enabled are true.
	Friction FrictionConfig `yaml:"friction,omitempty"`
}

// FrictionConfig controls the C-5 friction-driven improvement loop. All
// fields are opt-in with safe defaults; the loop records and surfaces
// proposals only — the daemon never rewrites templates or config itself.
type FrictionConfig struct {
	Enabled *bool `yaml:"enabled,omitempty"`
	// MinOccurrences is how many times the same friction fingerprint must
	// recur before it is promoted from observed to proposed.
	MinOccurrences *int `yaml:"min_occurrences,omitempty"`
	// VerifyMinSuccesses is the measurement gate: the number of CONSECUTIVE
	// successful repairs (with the strategy applied, no recurrence in
	// between) required to promote an applied improvement to verified.
	VerifyMinSuccesses *int `yaml:"verify_min_successes,omitempty"`
	// MaxEntries bounds state/improvements.yaml (oldest-seen eviction).
	MaxEntries *int `yaml:"max_entries,omitempty"`
	// InjectCount caps how many actionable (proposed/reopened) improvements
	// are injected into Planner command envelopes. 0 disables injection
	// while keeping recording active.
	InjectCount *int `yaml:"inject_count,omitempty"`
}

// EffectiveEnabled returns Enabled, defaulting to false when unset.
func (f FrictionConfig) EffectiveEnabled() bool { return effectiveValue(f.Enabled, false) }

// EffectiveMinOccurrences returns MinOccurrences, or DefaultFrictionMinOccurrences when unset.
func (f FrictionConfig) EffectiveMinOccurrences() int {
	return effectiveValue(f.MinOccurrences, DefaultFrictionMinOccurrences)
}

// EffectiveVerifyMinSuccesses returns VerifyMinSuccesses, or DefaultFrictionVerifyMinSuccesses when unset.
func (f FrictionConfig) EffectiveVerifyMinSuccesses() int {
	return effectiveValue(f.VerifyMinSuccesses, DefaultFrictionVerifyMinSuccesses)
}

// EffectiveMaxEntries returns MaxEntries, or DefaultFrictionMaxEntries when unset.
func (f FrictionConfig) EffectiveMaxEntries() int {
	return effectiveValue(f.MaxEntries, DefaultFrictionMaxEntries)
}

// EffectiveInjectCount returns InjectCount, or DefaultFrictionInjectCount when unset.
func (f FrictionConfig) EffectiveInjectCount() int {
	return effectiveValue(f.InjectCount, DefaultFrictionInjectCount)
}

// EffectiveEnabled returns Enabled, defaulting to false when unset.
func (si SelfImprovementConfig) EffectiveEnabled() bool { return effectiveValue(si.Enabled, false) }

// EffectiveArchiveMaxSize returns ArchiveMaxSize, or DefaultArchiveMaxSize when unset.
func (si SelfImprovementConfig) EffectiveArchiveMaxSize() int {
	return effectiveValue(si.ArchiveMaxSize, DefaultArchiveMaxSize)
}

// EffectiveTargets returns the configured targets or defaults.
func (si SelfImprovementConfig) EffectiveTargets() []string {
	if len(si.Targets) > 0 {
		return si.Targets
	}
	return []string{"planner_prompt", "persona", "worker_prompt"}
}

// EffectiveExcludeTargets returns the configured exclusions or defaults.
func (si SelfImprovementConfig) EffectiveExcludeTargets() []string {
	if len(si.ExcludeTargets) > 0 {
		return si.ExcludeTargets
	}
	return []string{"fitness", "daemon_logic", "circuit_breaker"}
}

// --- C-6 Complexity Config ---

// ComplexityConfig controls adaptive computation depth.
type ComplexityConfig struct {
	Enabled    *bool                `yaml:"enabled,omitempty"`
	Thresholds ComplexityThresholds `yaml:"thresholds,omitempty"`
}

// EffectiveEnabled returns Enabled, defaulting to false when unset.
func (cc ComplexityConfig) EffectiveEnabled() bool { return effectiveValue(cc.Enabled, false) }

// ComplexityThresholds defines file count thresholds for complexity levels.
type ComplexityThresholds struct {
	SimpleMaxFiles   *int `yaml:"simple_max_files,omitempty"`
	StandardMaxFiles *int `yaml:"standard_max_files,omitempty"`
	ComplexMaxFiles  *int `yaml:"complex_max_files,omitempty"`
}

// EffectiveSimpleMaxFiles returns SimpleMaxFiles, or DefaultSimpleMaxFiles when unset.
func (ct ComplexityThresholds) EffectiveSimpleMaxFiles() int {
	return effectiveValue(ct.SimpleMaxFiles, DefaultSimpleMaxFiles)
}

// EffectiveStandardMaxFiles returns StandardMaxFiles, or DefaultStandardMaxFiles when unset.
func (ct ComplexityThresholds) EffectiveStandardMaxFiles() int {
	return effectiveValue(ct.StandardMaxFiles, DefaultStandardMaxFiles)
}

// EffectiveComplexMaxFiles returns ComplexMaxFiles, or DefaultComplexMaxFiles when unset.
func (ct ComplexityThresholds) EffectiveComplexMaxFiles() int {
	return effectiveValue(ct.ComplexMaxFiles, DefaultComplexMaxFiles)
}

// --- C-8 Feature Profiles ---

// FeatureProfile defines feature flags per complexity level.
type FeatureProfile struct {
	CrossAgentReview        *bool `yaml:"cross_agent_review,omitempty"`
	ExploratoryOptimization *bool `yaml:"exploratory_optimization,omitempty"`
	EvolutionaryQuality     *bool `yaml:"evolutionary_quality,omitempty"`
	AdaptiveModelSelection  *bool `yaml:"adaptive_model_selection,omitempty"`
	SelfImprovement         *bool `yaml:"self_improvement,omitempty"`
	AdaptiveDepth           *bool `yaml:"adaptive_depth,omitempty"`
}

// EffectiveCrossAgentReview returns CrossAgentReview, defaulting to false when unset.
func (fp FeatureProfile) EffectiveCrossAgentReview() bool {
	return effectiveValue(fp.CrossAgentReview, DefaultCrossAgentReview)
}

// EffectiveExploratoryOptimization returns ExploratoryOptimization, defaulting to false when unset.
func (fp FeatureProfile) EffectiveExploratoryOptimization() bool {
	return effectiveValue(fp.ExploratoryOptimization, false)
}

// EffectiveEvolutionaryQuality returns EvolutionaryQuality, defaulting to false when unset.
func (fp FeatureProfile) EffectiveEvolutionaryQuality() bool {
	return effectiveValue(fp.EvolutionaryQuality, false)
}

// EffectiveAdaptiveModelSelection returns AdaptiveModelSelection, defaulting to false when unset.
func (fp FeatureProfile) EffectiveAdaptiveModelSelection() bool {
	return effectiveValue(fp.AdaptiveModelSelection, false)
}

// EffectiveSelfImprovement returns SelfImprovement, defaulting to false when unset.
func (fp FeatureProfile) EffectiveSelfImprovement() bool {
	return effectiveValue(fp.SelfImprovement, false)
}

// EffectiveAdaptiveDepth returns AdaptiveDepth, defaulting to false when unset.
func (fp FeatureProfile) EffectiveAdaptiveDepth() bool {
	return effectiveValue(fp.AdaptiveDepth, false)
}

// NormalizeExperimentalConfig fills nil pointer fields in experimental config sections (C-1 through C-8)
// with their default values. Call once after unmarshalling config.yaml so that EffectiveXxx() methods
// are guaranteed to find non-nil values. Slice/map fields are also populated with defaults when empty.
func NormalizeExperimentalConfig(cfg *Config) {
	normalizeEvolution(&cfg.Evolution)
	normalizeBandit(&cfg.Bandit)
	normalizeABTest(&cfg.ABTest)
	normalizeExtendedVerification(&cfg.ExtendedVerification)
	normalizeSearch(&cfg.Search)
	normalizeSelfImprovement(&cfg.SelfImprovement)
	normalizeComplexity(&cfg.Complexity)
	for name, fp := range cfg.FeatureProfiles {
		normalizeFeatureProfile(&fp)
		cfg.FeatureProfiles[name] = fp
	}
}

func normalizeEvolution(e *EvolutionConfig) {
	resolvePtr(&e.Enabled, false)
	resolvePtr(&e.MaxMutationsPerRound, DefaultMaxMutationsPerRound)
	resolvePtr(&e.NoveltyThreshold, DefaultNoveltyThreshold)
	if len(e.Strategies) == 0 {
		e.Strategies = []string{"diff", "full", "cross"}
	}
	if len(e.StrategyWeights) == 0 {
		e.StrategyWeights = map[string]int{"diff": 2, "full": 1, "cross": 1}
	}
}

func normalizeABTest(a *ABTestConfig) {
	resolvePtr(&a.Enabled, false)
	resolvePtr(&a.MinBloomLevel, DefaultABMinBloomLevel)
	resolvePtr(&a.TimeoutSec, 0)
	resolvePtr(&a.SelectionTimeoutSec, DefaultABSelectionTimeoutSec)
}

func normalizeBandit(b *BanditConfig) {
	resolvePtr(&b.Enabled, false)
	resolvePtr(&b.ExplorationCoeff, DefaultExplorationCoeff)
	resolvePtr(&b.MinSamplesBeforeUse, DefaultMinSamplesBeforeUse)
	resolvePtr(&b.TraceDataRequirement, DefaultTraceDataRequirement)
}

func normalizeExtendedVerification(ev *ExtendedVerificationConfig) {
	resolvePtr(&ev.Enabled, false)
	resolvePtr(&ev.MaxAutoRetries, DefaultMaxAutoRetries)
}

func normalizeSearch(s *SearchConfig) {
	resolvePtr(&s.Enabled, false)
	resolvePtr(&s.MaxDepth, DefaultSearchMaxDepth)
	resolvePtr(&s.MaxBranching, DefaultMaxBranching)
	resolvePtr(&s.PruneThreshold, DefaultPruneThreshold)
	resolvePtr(&s.ThompsonAlpha, DefaultThompsonAlpha)
	resolvePtr(&s.ThompsonBeta, DefaultThompsonBeta)
}

func normalizeSelfImprovement(si *SelfImprovementConfig) {
	resolvePtr(&si.Enabled, false)
	resolvePtr(&si.ArchiveMaxSize, DefaultArchiveMaxSize)
	if len(si.Targets) == 0 {
		si.Targets = []string{"planner_prompt", "persona", "worker_prompt"}
	}
	if len(si.ExcludeTargets) == 0 {
		si.ExcludeTargets = []string{"fitness", "daemon_logic", "circuit_breaker"}
	}
	resolvePtr(&si.Friction.Enabled, false)
	resolvePtr(&si.Friction.MinOccurrences, DefaultFrictionMinOccurrences)
	resolvePtr(&si.Friction.VerifyMinSuccesses, DefaultFrictionVerifyMinSuccesses)
	resolvePtr(&si.Friction.MaxEntries, DefaultFrictionMaxEntries)
	resolvePtr(&si.Friction.InjectCount, DefaultFrictionInjectCount)
}

func normalizeComplexity(cc *ComplexityConfig) {
	resolvePtr(&cc.Enabled, false)
	resolvePtr(&cc.Thresholds.SimpleMaxFiles, DefaultSimpleMaxFiles)
	resolvePtr(&cc.Thresholds.StandardMaxFiles, DefaultStandardMaxFiles)
	resolvePtr(&cc.Thresholds.ComplexMaxFiles, DefaultComplexMaxFiles)
}

func normalizeFeatureProfile(fp *FeatureProfile) {
	resolvePtr(&fp.CrossAgentReview, DefaultCrossAgentReview)
	resolvePtr(&fp.ExploratoryOptimization, false)
	resolvePtr(&fp.EvolutionaryQuality, false)
	resolvePtr(&fp.AdaptiveModelSelection, false)
	resolvePtr(&fp.SelfImprovement, false)
	resolvePtr(&fp.AdaptiveDepth, false)
}
