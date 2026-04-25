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

func (e EvolutionConfig) EffectiveEnabled() bool { return effectiveValue(e.Enabled, false) }
func (e EvolutionConfig) EffectiveMaxMutationsPerRound() int {
	return effectiveValue(e.MaxMutationsPerRound, DefaultMaxMutationsPerRound)
}
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
	DecayFactor          *float64 `yaml:"decay_factor,omitempty"`
	TraceDataRequirement *int     `yaml:"trace_data_requirement,omitempty"`
}

func (b BanditConfig) EffectiveEnabled() bool { return effectiveValue(b.Enabled, false) }
func (b BanditConfig) EffectiveExplorationCoeff() float64 {
	return effectiveValue(b.ExplorationCoeff, DefaultExplorationCoeff)
}
func (b BanditConfig) EffectiveMinSamplesBeforeUse() int {
	return effectiveValue(b.MinSamplesBeforeUse, DefaultMinSamplesBeforeUse)
}
func (b BanditConfig) EffectiveDecayFactor() float64 {
	return effectiveValue(b.DecayFactor, DefaultDecayFactor)
}
func (b BanditConfig) EffectiveTraceDataRequirement() int {
	return effectiveValue(b.TraceDataRequirement, DefaultTraceDataRequirement)
}

// --- C-3 Extended Verification Config ---

// ExtendedVerificationConfig controls extended verification perspectives.
type ExtendedVerificationConfig struct {
	Enabled            *bool              `yaml:"enabled,omitempty"`
	SecurityCheck      *bool              `yaml:"security_check,omitempty"`
	PerformanceBench   *bool              `yaml:"performance_bench,omitempty"`
	PerspectiveWeights map[string]float64 `yaml:"perspective_weights,omitempty"`
	MaxAutoRetries     *int               `yaml:"max_auto_retries,omitempty"`
}

func (ev ExtendedVerificationConfig) EffectiveEnabled() bool {
	return effectiveValue(ev.Enabled, false)
}
func (ev ExtendedVerificationConfig) EffectiveSecurityCheck() bool {
	return effectiveValue(ev.SecurityCheck, false)
}
func (ev ExtendedVerificationConfig) EffectivePerformanceBench() bool {
	return effectiveValue(ev.PerformanceBench, false)
}
func (ev ExtendedVerificationConfig) EffectiveMaxAutoRetries() int {
	return effectiveValue(ev.MaxAutoRetries, DefaultMaxAutoRetries)
}

// EffectivePerspectiveWeights returns the configured weights or defaults.
func (ev ExtendedVerificationConfig) EffectivePerspectiveWeights() map[string]float64 {
	if len(ev.PerspectiveWeights) > 0 {
		return ev.PerspectiveWeights
	}
	return map[string]float64{"build": 1.0, "test": 1.0, "security": 0.5}
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

func (s SearchConfig) EffectiveEnabled() bool { return effectiveValue(s.Enabled, false) }
func (s SearchConfig) EffectiveMaxDepth() int {
	return effectiveValue(s.MaxDepth, DefaultSearchMaxDepth)
}
func (s SearchConfig) EffectiveMaxBranching() int {
	return effectiveValue(s.MaxBranching, DefaultMaxBranching)
}
func (s SearchConfig) EffectivePruneThreshold() float64 {
	return effectiveValue(s.PruneThreshold, DefaultPruneThreshold)
}
func (s SearchConfig) EffectiveThompsonAlpha() float64 {
	return effectiveValue(s.ThompsonAlpha, DefaultThompsonAlpha)
}
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
}

func (si SelfImprovementConfig) EffectiveEnabled() bool { return effectiveValue(si.Enabled, false) }
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

func (cc ComplexityConfig) EffectiveEnabled() bool { return effectiveValue(cc.Enabled, false) }

// ComplexityThresholds defines file count thresholds for complexity levels.
type ComplexityThresholds struct {
	SimpleMaxFiles   *int `yaml:"simple_max_files,omitempty"`
	StandardMaxFiles *int `yaml:"standard_max_files,omitempty"`
	ComplexMaxFiles  *int `yaml:"complex_max_files,omitempty"`
}

func (ct ComplexityThresholds) EffectiveSimpleMaxFiles() int {
	return effectiveValue(ct.SimpleMaxFiles, DefaultSimpleMaxFiles)
}
func (ct ComplexityThresholds) EffectiveStandardMaxFiles() int {
	return effectiveValue(ct.StandardMaxFiles, DefaultStandardMaxFiles)
}
func (ct ComplexityThresholds) EffectiveComplexMaxFiles() int {
	return effectiveValue(ct.ComplexMaxFiles, DefaultComplexMaxFiles)
}

// --- C-8 Feature Profiles ---

// FeatureProfile defines feature flags per complexity level.
type FeatureProfile struct {
	CrossAgentReview        *string `yaml:"cross_agent_review,omitempty"`
	ExploratoryOptimization *bool   `yaml:"exploratory_optimization,omitempty"`
	EvolutionaryQuality     *bool   `yaml:"evolutionary_quality,omitempty"`
	AdaptiveModelSelection  *bool   `yaml:"adaptive_model_selection,omitempty"`
	SelfImprovement         *bool   `yaml:"self_improvement,omitempty"`
	AdaptiveDepth           *bool   `yaml:"adaptive_depth,omitempty"`
}

func (fp FeatureProfile) EffectiveCrossAgentReview() string {
	return effectiveValue(fp.CrossAgentReview, DefaultCrossAgentReview)
}
func (fp FeatureProfile) EffectiveExploratoryOptimization() bool {
	return effectiveValue(fp.ExploratoryOptimization, false)
}
func (fp FeatureProfile) EffectiveEvolutionaryQuality() bool {
	return effectiveValue(fp.EvolutionaryQuality, false)
}
func (fp FeatureProfile) EffectiveAdaptiveModelSelection() bool {
	return effectiveValue(fp.AdaptiveModelSelection, false)
}
func (fp FeatureProfile) EffectiveSelfImprovement() bool {
	return effectiveValue(fp.SelfImprovement, false)
}
func (fp FeatureProfile) EffectiveAdaptiveDepth() bool {
	return effectiveValue(fp.AdaptiveDepth, false)
}

// NormalizeExperimentalConfig fills nil pointer fields in experimental config sections (C-1 through C-8)
// with their default values. Call once after unmarshalling config.yaml so that EffectiveXxx() methods
// are guaranteed to find non-nil values. Slice/map fields are also populated with defaults when empty.
func NormalizeExperimentalConfig(cfg *Config) {
	normalizeEvolution(&cfg.Evolution)
	normalizeBandit(&cfg.Bandit)
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

func normalizeBandit(b *BanditConfig) {
	resolvePtr(&b.Enabled, false)
	resolvePtr(&b.ExplorationCoeff, DefaultExplorationCoeff)
	resolvePtr(&b.MinSamplesBeforeUse, DefaultMinSamplesBeforeUse)
	resolvePtr(&b.DecayFactor, DefaultDecayFactor)
	resolvePtr(&b.TraceDataRequirement, DefaultTraceDataRequirement)
}

func normalizeExtendedVerification(ev *ExtendedVerificationConfig) {
	resolvePtr(&ev.Enabled, false)
	resolvePtr(&ev.SecurityCheck, false)
	resolvePtr(&ev.PerformanceBench, false)
	resolvePtr(&ev.MaxAutoRetries, DefaultMaxAutoRetries)
	if len(ev.PerspectiveWeights) == 0 {
		ev.PerspectiveWeights = map[string]float64{"build": 1.0, "test": 1.0, "security": 0.5}
	}
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
