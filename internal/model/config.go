// Package model defines the data structures for Maestro's configuration, state, and queue entries.
package model

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

// DefaultMaxYAMLFileBytes is the default maximum size for YAML file reads (5MB).
const DefaultMaxYAMLFileBytes = 5 * 1024 * 1024

// DefaultMaxEntryContentBytes is the default maximum size for queue entry
// content fields such as content, summary, purpose, and acceptance_criteria (64KB).
const DefaultMaxEntryContentBytes = 64 * 1024

// MinWorkers is the minimum allowed worker count.
const MinWorkers = 1

// MaxWorkers is the maximum allowed worker count.
const MaxWorkers = 8

// Upper-bound constants for numeric config fields to prevent resource exhaustion.
const (
	MaxBusyCheckMaxRetries       = 1000
	MaxWaitReadyMaxRetries       = 1000
	MaxDispatchLeaseSec          = 3600
	MaxMaxInProgressMin          = 1440
	MaxShutdownTimeoutSec        = 600
	MaxMaxPendingCommands        = 1000
	MaxMaxPendingTasksPerWorker  = 100
	MaxMaxDeadLetterArchiveFiles = 10000
	MaxMaxQuarantineFiles        = 10000
	MaxMaxWorktrees              = 256
	MaxMaxYAMLFileBytes          = 50 * 1024 * 1024 // 50MB
)

// Default values for Effective*() methods.
const (
	// SkillsConfig
	DefaultMaxRefsPerTask   = 3
	DefaultMissingRefPolicy = "warn"

	// autoCollectConfig
	DefaultAutoCollectMinOccurrences = 3
	DefaultAutoCollectMinCommands    = 2

	// WatcherConfig
	DefaultMaxInProgressMin = 60

	// LimitsConfig
	DefaultMaxDeadLetterArchiveFiles = 100
	DefaultMaxQuarantineFiles        = 100

	// CircuitBreakerConfig
	DefaultCBMaxConsecutiveFailures = 3
	DefaultProgressTimeoutMinutes   = 30

	// LearningsConfig
	DefaultLearningsMaxEntries       = 100
	DefaultLearningsMaxContentLength = 500
	DefaultLearningsInjectCount      = 5

	// AdmissionControl
	DefaultMaxConcurrentVerify  = 2
	DefaultMaxConcurrentRepair  = 1
	DefaultMaxConcurrentRollout = 1

	// Fallback
	DefaultConsecutiveFailureThreshold = 5
	DefaultRecoveryCheckIntervalSec    = 60
	DefaultMinHealthyDurationSec       = 120

	// WorktreeConfig
	DefaultBaseBranch                  = "main"
	DefaultPathPrefix                  = ".maestro/worktrees"
	DefaultMergeStrategy               = "ort"
	DefaultGitTimeoutSec               = 120
	DefaultStallTimeoutMinutes         = 30
	DefaultFallbackMergeTimeoutMinutes = 60
	DefaultStallCleanupAfter           = 10 * time.Minute

	// CommitPolicyConfig
	DefaultCommitMaxFiles = 60

	// WorktreeGCConfig
	DefaultGCTTLHours     = 24
	DefaultGCMaxWorktrees = 32

	// ReviewConfig
	DefaultReviewMinBloomLevel        = 2
	DefaultReviewMaxConcurrentReviews = 2
	DefaultReviewTimeoutSec           = 300

	// EvolutionConfig
	DefaultMaxMutationsPerRound = 3
	DefaultNoveltyThreshold     = 0.99

	// BanditConfig
	DefaultExplorationCoeff     = 1.41
	DefaultMinSamplesBeforeUse  = 10
	DefaultDecayFactor          = 0.95
	DefaultTraceDataRequirement = 50

	// ExtendedVerificationConfig
	DefaultMaxAutoRetries = 2

	// SearchConfig
	DefaultSearchMaxDepth = 3
	DefaultMaxBranching   = 4
	DefaultPruneThreshold = 0.3
	DefaultThompsonAlpha  = 1.0
	DefaultThompsonBeta   = 1.0

	// SelfImprovementConfig
	DefaultArchiveMaxSize = 100

	// ComplexityThresholds
	DefaultSimpleMaxFiles   = 3
	DefaultStandardMaxFiles = 10
	DefaultComplexMaxFiles  = 30

	// FeatureProfile
	DefaultCrossAgentReview = "false"
)

// ValidAgentModels is the whitelist of recognized agent model name identifiers.
// An empty string is always valid (uses the runtime default).
var ValidAgentModels = map[string]struct{}{
	// Claude short aliases
	"sonnet": {},
	"opus":   {},
	"haiku":  {},
	// Claude full model IDs
	"claude-sonnet-4-6":         {},
	"claude-opus-4-6":           {},
	"claude-haiku-4-5-20251001": {},
}

// validModelNameRe validates the format of model name identifiers.
// Names must start with a letter or digit and contain only letters, digits,
// hyphens, dots, and underscores.
var validModelNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// isValidModelName returns true if name is empty (use default) or is in the
// whitelist or matches the valid model name format pattern.
func isValidModelName(name string) bool {
	if name == "" {
		return true
	}
	if _, ok := ValidAgentModels[name]; ok {
		return true
	}
	return validModelNameRe.MatchString(name)
}

// isFiniteFloat64Ptr reports whether a *float64 is nil or points to a finite value.
func isFiniteFloat64Ptr(p *float64) bool {
	return p == nil || (!math.IsNaN(*p) && !math.IsInf(*p, 0))
}

// effectiveValue returns *ptr if ptr is non-nil, or defaultVal otherwise.
func effectiveValue[T any](ptr *T, defaultVal T) T {
	if ptr != nil {
		return *ptr
	}
	return defaultVal
}

// effectiveNonZero returns val if it is not the zero value for its type, or defaultVal otherwise.
func effectiveNonZero[T comparable](val, defaultVal T) T {
	var zero T
	if val != zero {
		return val
	}
	return defaultVal
}

// Config is the root configuration structure loaded from config.yaml.
type Config struct {
	Project            ProjectConfig        `yaml:"project"`
	Maestro            MaestroConfig        `yaml:"maestro"`
	Agents             AgentsConfig         `yaml:"agents"`
	Continuous         ContinuousConfig     `yaml:"continuous"`
	Watcher            WatcherConfig        `yaml:"watcher"`
	Retry              RetryConfig          `yaml:"retry"`
	Queue              QueueConfig          `yaml:"queue"`
	Limits             LimitsConfig         `yaml:"limits"`
	ShutdownTimeoutSec int                  `yaml:"shutdown_timeout_sec"`
	Logging            LoggingConfig        `yaml:"logging"`
	QualityGates       qualityGatesConfig   `yaml:"quality_gates"`
	CircuitBreaker     CircuitBreakerConfig `yaml:"circuit_breaker"`
	Learnings          LearningsConfig      `yaml:"learnings"`
	Worktree           WorktreeConfig       `yaml:"worktree"`
	Skills             SkillsConfig         `yaml:"skills"`
	AdmissionControl   AdmissionControl     `yaml:"admission_control"`
	Fallback           Fallback             `yaml:"fallback"`
	Review             ReviewConfig         `yaml:"review"`
	Rollout            RolloutConfig        `yaml:"rollout"`
	Judge              JudgeConfig          `yaml:"judge"`

	// C-1 Evolution
	Evolution EvolutionConfig `yaml:"evolution,omitempty"`
	// C-2 Adaptive Model Selection
	Bandit BanditConfig `yaml:"bandit,omitempty"`
	// C-3 Extended Verification
	ExtendedVerification ExtendedVerificationConfig `yaml:"extended_verification,omitempty"`
	// C-4 Search
	Search SearchConfig `yaml:"search,omitempty"`
	// C-5 Self-Improvement
	SelfImprovement SelfImprovementConfig `yaml:"self_improvement,omitempty"`
	// C-6 Complexity
	Complexity ComplexityConfig `yaml:"complexity,omitempty"`
	// C-7 Runtimes
	Runtimes map[string]RuntimeConfig `yaml:"runtimes,omitempty"`
	// C-8 Feature Profiles
	FeatureProfiles map[string]FeatureProfile `yaml:"feature_profiles,omitempty"`
}

// --- SkillsConfig ---

// SkillsConfig controls the skill reference feature for tasks.
type SkillsConfig struct {
	Enabled          bool              `yaml:"enabled"`
	MaxRefsPerTask   *int              `yaml:"max_refs_per_task"`
	MaxBodyChars     *int              `yaml:"max_body_chars"`
	MissingRefPolicy string            `yaml:"missing_ref_policy"`
	AutoCollect      autoCollectConfig `yaml:"auto_collect"`
}

func (s SkillsConfig) EffectiveMaxRefsPerTask() int      { return effectiveValue(s.MaxRefsPerTask, DefaultMaxRefsPerTask) }
func (s SkillsConfig) EffectiveMaxBodyChars() int         { return effectiveValue(s.MaxBodyChars, 0) }
func (s SkillsConfig) EffectiveMissingRefPolicy() string  { return effectiveNonZero(s.MissingRefPolicy, DefaultMissingRefPolicy) }

// autoCollectConfig controls automatic skill collection from learnings.
type autoCollectConfig struct {
	Enabled        bool `yaml:"enabled"`
	MinOccurrences *int `yaml:"min_occurrences"`
	MinCommands    *int `yaml:"min_commands"`
}

func (a autoCollectConfig) EffectiveMinOccurrences() int { return effectiveValue(a.MinOccurrences, DefaultAutoCollectMinOccurrences) }
func (a autoCollectConfig) EffectiveMinCommands() int    { return effectiveValue(a.MinCommands, DefaultAutoCollectMinCommands) }

// --- ProjectConfig / MaestroConfig ---

// ProjectConfig holds project identity information.
type ProjectConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// MaestroConfig holds daemon version and workspace metadata.
type MaestroConfig struct {
	Version     string `yaml:"version"`
	Created     string `yaml:"created"`
	ProjectRoot string `yaml:"project_root"`
}

// --- AgentsConfig ---

// AgentsConfig holds per-role agent configuration.
type AgentsConfig struct {
	Orchestrator AgentConfig  `yaml:"orchestrator"`
	Planner      AgentConfig  `yaml:"planner"`
	Workers      WorkerConfig `yaml:"workers"`
}

// AgentConfig holds configuration for a single agent role (orchestrator or planner).
type AgentConfig struct {
	ID             string `yaml:"id"`
	Model          string `yaml:"model"`
	BasePromptMode string `yaml:"base_prompt_mode"`
}

// EffectiveBasePromptMode returns the configured base prompt mode or "append" as default.
// Valid values: "replace" (--system-prompt), "append" (--append-system-prompt).
func (a AgentConfig) EffectiveBasePromptMode() string {
	if a.BasePromptMode == "replace" {
		return "replace"
	}
	return "append"
}

// WorkerConfig holds configuration for worker agents.
type WorkerConfig struct {
	Count          int               `yaml:"count"`
	DefaultModel   string            `yaml:"default_model"`
	Models         map[string]string `yaml:"models,omitempty"`
	Boost          bool              `yaml:"boost"`
	BasePromptMode string            `yaml:"base_prompt_mode"`
}

// EffectiveBasePromptMode returns the configured base prompt mode or "append" as default.
// Valid values: "replace" (--system-prompt), "append" (--append-system-prompt).
func (w WorkerConfig) EffectiveBasePromptMode() string {
	if w.BasePromptMode == "replace" {
		return "replace"
	}
	return "append"
}

// --- ContinuousConfig ---

// ContinuousConfig holds configuration for continuous mode operation.
type ContinuousConfig struct {
	Enabled        bool `yaml:"enabled"`
	MaxIterations  int  `yaml:"max_iterations"` // 0 means unlimited (no iteration cap); positive value sets the cap
	PauseOnFailure bool `yaml:"pause_on_failure"`
	// MaxConsecutiveFailures sets the pre-generation gate threshold: when the number of
	// consecutive failed commands reaches this value, continuous mode stops automatically.
	// 0 means disabled (no consecutive-failure gate). Independent of pause_on_failure:
	// this gate fires even when pause_on_failure=false.
	//
	// Backward compatibility: config files written before this field existed
	// deserialize with the Go zero value (0 = disabled). New templates ship
	// with a recommended value of 3, but operators upgrading an existing
	// installation must opt in by adding the key explicitly. No non-zero
	// default is synthesized at load time so that "0" always means "disabled"
	// with no surprising hidden defaults.
	MaxConsecutiveFailures int `yaml:"max_consecutive_failures"`
}

// --- WatcherConfig ---

// WatcherConfig holds timing and polling configuration for the task dispatch watcher.
type WatcherConfig struct {
	DebounceSec          float64 `yaml:"debounce_sec"`
	ScanIntervalSec      int     `yaml:"scan_interval_sec"`
	DispatchLeaseSec     int     `yaml:"dispatch_lease_sec"`
	MaxInProgressMin     *int    `yaml:"max_in_progress_min"`
	BusyCheckInterval    int     `yaml:"busy_check_interval"`
	BusyCheckMaxRetries  int     `yaml:"busy_check_max_retries"`
	BusyPatterns         string  `yaml:"busy_patterns"`
	IdleStableSec        int     `yaml:"idle_stable_sec"`
	CooldownAfterClear   int     `yaml:"cooldown_after_clear"`
	NotifyLeaseSec       int     `yaml:"notify_lease_sec"`
	WaitReadyIntervalSec int     `yaml:"wait_ready_interval_sec"`
	WaitReadyMaxRetries  int     `yaml:"wait_ready_max_retries"`

	// Clear confirmation settings (used by clearAndConfirm)
	ClearConfirmTimeoutSec  int `yaml:"clear_confirm_timeout_sec"`   // Per-attempt confirmation window (default 5s)
	ClearConfirmPollMs      int `yaml:"clear_confirm_poll_ms"`       // Polling interval within confirmation window (default 250ms)
	ClearMaxAttempts        int `yaml:"clear_max_attempts"`          // Total send attempts including initial (default 3)
	ClearRetryBackoffMs     int `yaml:"clear_retry_backoff_ms"`      // Base backoff between attempts; doubles each retry (default 500ms)
	ClearSecondEnterDelayMs int `yaml:"clear_second_enter_delay_ms"` // Delay before sending second Enter after /clear (default 500ms)
}

func (w WatcherConfig) EffectiveMaxInProgressMin() int { return effectiveValue(w.MaxInProgressMin, DefaultMaxInProgressMin) }

// --- RetryConfig ---

// RetryConfig holds retry limits for the various dispatch and execution operations.
type RetryConfig struct {
	CommandDispatch                  int             `yaml:"command_dispatch"`
	TaskDispatch                     int             `yaml:"task_dispatch"`
	OrchestratorNotificationDispatch int             `yaml:"orchestrator_notification_dispatch"`
	TaskExecution                    TaskRetryConfig `yaml:"task_execution"`
}

// TaskRetryConfig holds configuration for automatic task execution retries.
type TaskRetryConfig struct {
	Enabled            bool  `yaml:"enabled"`
	RetryableExitCodes []int `yaml:"retryable_exit_codes"`
	MaxRetries         int   `yaml:"max_retries"`
	CooldownSec        int   `yaml:"cooldown_sec"`
}

// --- QueueConfig ---

// QueueConfig holds configuration for queue priority aging.
type QueueConfig struct {
	PriorityAgingSec int `yaml:"priority_aging_sec"`
}

// --- LimitsConfig ---

// LimitsConfig holds resource limits enforced by the daemon.
type LimitsConfig struct {
	MaxPendingCommands        int  `yaml:"max_pending_commands"`
	MaxPendingTasksPerWorker  int  `yaml:"max_pending_tasks_per_worker"`
	MaxEntryContentBytes      int  `yaml:"max_entry_content_bytes"`
	MaxYAMLFileBytes          int  `yaml:"max_yaml_file_bytes"`
	MaxDeadLetterArchiveFiles *int `yaml:"max_dead_letter_archive_files"`
	MaxQuarantineFiles        *int `yaml:"max_quarantine_files"`
}

func (l LimitsConfig) EffectiveMaxDeadLetterArchiveFiles() int { return effectiveValue(l.MaxDeadLetterArchiveFiles, DefaultMaxDeadLetterArchiveFiles) }
func (l LimitsConfig) EffectiveMaxQuarantineFiles() int        { return effectiveValue(l.MaxQuarantineFiles, DefaultMaxQuarantineFiles) }

// --- LoggingConfig ---

// LoggingConfig holds logging verbosity settings.
type LoggingConfig struct {
	Level string `yaml:"level"`
}

// --- qualityGatesConfig ---

type qualityGatesConfig struct {
	Enabled     bool                   `yaml:"enabled"`
	SkipGates   bool                   `yaml:"skip_gates"` // 緊急モードフラグ
	Thresholds  qualityGateThresholds  `yaml:"thresholds"`
	Enforcement qualityGateEnforcement `yaml:"enforcement"`
}

type qualityGateThresholds struct {
}

type qualityGateEnforcement struct {
	PreTaskCheck  bool   `yaml:"pre_task_check"` // タスク実行前チェック
	FailureAction string `yaml:"failure_action"` // 失敗時の動作: "warn", "block"
}

// --- CircuitBreakerConfig ---

// CircuitBreakerConfig controls the command-level circuit breaker that auto-stops
// commands after consecutive task failures.
type CircuitBreakerConfig struct {
	Enabled                bool `yaml:"enabled"`                  // opt-in, default: false
	MaxConsecutiveFailures *int `yaml:"max_consecutive_failures"` // default: 3
	ProgressTimeoutMinutes *int `yaml:"progress_timeout_minutes"` // default: 30, 0=disabled
}

func (c CircuitBreakerConfig) EffectiveMaxConsecutiveFailures() int { return effectiveValue(c.MaxConsecutiveFailures, DefaultCBMaxConsecutiveFailures) }
func (c CircuitBreakerConfig) EffectiveProgressTimeoutMinutes() int { return effectiveValue(c.ProgressTimeoutMinutes, DefaultProgressTimeoutMinutes) }

// --- LearningsConfig ---

// LearningsConfig controls the learning accumulation feature.
type LearningsConfig struct {
	Enabled          bool `yaml:"enabled"`            // opt-in, default: false
	MaxEntries       *int `yaml:"max_entries"`        // default: 100
	MaxContentLength *int `yaml:"max_content_length"` // default: 500
	InjectCount      *int `yaml:"inject_count"`       // top-K learnings injected per task dispatch, default: 5
	TTLHours         int  `yaml:"ttl_hours"`          // learning expiry in hours, default: 72, 0=unlimited
}

func (l LearningsConfig) EffectiveMaxEntries() int       { return effectiveValue(l.MaxEntries, DefaultLearningsMaxEntries) }
func (l LearningsConfig) EffectiveMaxContentLength() int  { return effectiveValue(l.MaxContentLength, DefaultLearningsMaxContentLength) }
func (l LearningsConfig) EffectiveInjectCount() int       { return effectiveValue(l.InjectCount, DefaultLearningsInjectCount) }

// EffectiveTTLHours returns the configured TTL in hours.
// 0 means unlimited (no expiry). The template default is 72.
func (l LearningsConfig) EffectiveTTLHours() int { return l.TTLHours }

// --- AdmissionControl ---

// AdmissionControl controls concurrency limits for verify/repair/rollout phases.
type AdmissionControl struct {
	MaxConcurrentVerify  int `yaml:"max_concurrent_verify"`
	MaxConcurrentRepair  int `yaml:"max_concurrent_repair"`
	MaxConcurrentRollout int `yaml:"max_concurrent_rollout"`
}

func (a AdmissionControl) EffectiveMaxConcurrentVerify() int  { return effectiveNonZero(a.MaxConcurrentVerify, DefaultMaxConcurrentVerify) }
func (a AdmissionControl) EffectiveMaxConcurrentRepair() int  { return effectiveNonZero(a.MaxConcurrentRepair, DefaultMaxConcurrentRepair) }
func (a AdmissionControl) EffectiveMaxConcurrentRollout() int { return effectiveNonZero(a.MaxConcurrentRollout, DefaultMaxConcurrentRollout) }

// --- Fallback ---

// Fallback controls degraded-mode behavior when workers experience consecutive failures.
type Fallback struct {
	Enabled                     bool `yaml:"enabled"`
	ConsecutiveFailureThreshold int  `yaml:"consecutive_failure_threshold"`
	RecoveryCheckIntervalSec    int  `yaml:"recovery_check_interval_sec"`
	MinHealthyDurationSec       int  `yaml:"min_healthy_duration_sec"`
}

func (f Fallback) EffectiveEnabled() bool                     { return f.Enabled }
func (f Fallback) EffectiveConsecutiveFailureThreshold() int  { return effectiveNonZero(f.ConsecutiveFailureThreshold, DefaultConsecutiveFailureThreshold) }
func (f Fallback) EffectiveRecoveryCheckIntervalSec() int     { return effectiveNonZero(f.RecoveryCheckIntervalSec, DefaultRecoveryCheckIntervalSec) }
func (f Fallback) EffectiveMinHealthyDurationSec() int        { return effectiveNonZero(f.MinHealthyDurationSec, DefaultMinHealthyDurationSec) }

// --- WorktreeConfig ---

// WorktreeConfig controls Worker worktree isolation (default enabled).
type WorktreeConfig struct {
	Enabled          bool               `yaml:"enabled"`
	BaseBranch       string             `yaml:"base_branch"`
	PathPrefix       string             `yaml:"path_prefix"`
	AutoCommit       bool               `yaml:"auto_commit"`
	AutoMerge        bool               `yaml:"auto_merge"`
	MergeStrategy    string             `yaml:"merge_strategy"`
	CleanupOnSuccess bool               `yaml:"cleanup_on_success"`
	CleanupOnFailure bool               `yaml:"cleanup_on_failure"`
	GitTimeoutSec    *int               `yaml:"git_timeout_sec"`
	GC               WorktreeGCConfig   `yaml:"gc"`
	CommitPolicy     CommitPolicyConfig `yaml:"commit_policy"`
	// StallTimeoutMinutes is the threshold after which a command whose tasks
	// and phases are all terminal but whose integration branch is still in
	// {created, merged} is treated as stalled and surfaced to the planner.
	// nil (unset) → default of 30 minutes; explicit 0 disables stall detection.
	StallTimeoutMinutes *int `yaml:"stall_timeout_minutes,omitempty"`
	// FallbackMergeTimeoutMinutes controls how long an integration branch may
	// remain unmerged before the daemon emits a worktree_config_violation
	// warning when AutoCommit/AutoMerge are disabled. nil=default(60), 0=disabled.
	FallbackMergeTimeoutMinutes *int `yaml:"fallback_merge_timeout_minutes"`
	// StallCleanupAfter is the duration after which a command whose tasks are
	// all terminal but whose phases remain non-terminal (pending / awaiting_fill
	// / filling / active) is treated as stalled. The daemon force-fails the
	// stuck phases and triggers a worktree cleanup (skipping the merge path) to
	// avoid leaking .maestro/worktrees/cmd_*/ directories when daemon or worker
	// sessions are interrupted mid-phase. Parsed via time.ParseDuration. Empty
	// or unparseable → default 10m. "0" / "0s" disables fast-track cleanup.
	StallCleanupAfter string `yaml:"stall_cleanup_after,omitempty"`
}

// EffectiveStallCleanupAfter returns the configured fast-track stall cleanup
// duration. Empty / unparseable input falls back to DefaultStallCleanupAfter;
// an explicit "0" / "0s" returns 0 (disabled).
func (w WorktreeConfig) EffectiveStallCleanupAfter() time.Duration {
	if w.StallCleanupAfter == "" {
		return DefaultStallCleanupAfter
	}
	d, err := time.ParseDuration(w.StallCleanupAfter)
	if err != nil {
		return DefaultStallCleanupAfter
	}
	return d
}

func (w WorktreeConfig) EffectiveBaseBranch() string                  { return effectiveNonZero(w.BaseBranch, DefaultBaseBranch) }
func (w WorktreeConfig) EffectivePathPrefix() string                  { return effectiveNonZero(w.PathPrefix, DefaultPathPrefix) }
func (w WorktreeConfig) EffectiveMergeStrategy() string               { return effectiveNonZero(w.MergeStrategy, DefaultMergeStrategy) }
func (w WorktreeConfig) EffectiveGitTimeout() int                     { return effectiveValue(w.GitTimeoutSec, DefaultGitTimeoutSec) }
func (w WorktreeConfig) EffectiveStallTimeoutMinutes() int            { return effectiveValue(w.StallTimeoutMinutes, DefaultStallTimeoutMinutes) }
func (w WorktreeConfig) EffectiveFallbackMergeTimeoutMinutes() int    { return effectiveValue(w.FallbackMergeTimeoutMinutes, DefaultFallbackMergeTimeoutMinutes) }

// --- CommitPolicyConfig ---

// CommitPolicyConfig enforces safety checks before committing worker changes.
// Zero-valued config means no enforcement. Set fields explicitly via config.yaml
// to enable checks. Recommended template values: MaxFiles=60, RequireGitignore=true,
// MessagePattern="^.+" (non-empty message).
type CommitPolicyConfig struct {
	MaxFiles         *int   `yaml:"max_files"`         // max staged files per commit; nil=default(60), 0=unlimited
	RequireGitignore bool   `yaml:"require_gitignore"` // require .gitignore existence
	MessagePattern   string `yaml:"message_pattern"`   // regex for commit message validation; empty=no check
}

func (c CommitPolicyConfig) EffectiveMaxFiles() int { return effectiveValue(c.MaxFiles, DefaultCommitMaxFiles) }

// --- WorktreeGCConfig ---

// WorktreeGCConfig controls periodic garbage collection of old worktrees.
type WorktreeGCConfig struct {
	Enabled      bool `yaml:"enabled"`
	TTLHours     *int `yaml:"ttl_hours"`
	MaxWorktrees *int `yaml:"max_worktrees"`
}

func (w WorktreeGCConfig) EffectiveTTLHours() int     { return effectiveValue(w.TTLHours, DefaultGCTTLHours) }
func (w WorktreeGCConfig) EffectiveMaxWorktrees() int  { return effectiveValue(w.MaxWorktrees, DefaultGCMaxWorktrees) }

// --- ReviewConfig ---

// ReviewConfig controls asynchronous heterogeneous-model code review.
type ReviewConfig struct {
	Enabled              bool     `yaml:"enabled"`
	Models               []string `yaml:"models"`
	MinBloomLevel        *int     `yaml:"min_bloom_level"`
	MaxConcurrentReviews *int     `yaml:"max_concurrent_reviews"`
	TimeoutSec           *int     `yaml:"timeout_sec"`
}

func (r ReviewConfig) EffectiveMinBloomLevel() int        { return effectiveValue(r.MinBloomLevel, DefaultReviewMinBloomLevel) }
func (r ReviewConfig) EffectiveMaxConcurrentReviews() int { return effectiveValue(r.MaxConcurrentReviews, DefaultReviewMaxConcurrentReviews) }
func (r ReviewConfig) EffectiveTimeoutSec() int           { return effectiveValue(r.TimeoutSec, DefaultReviewTimeoutSec) }

// --- C-1 Evolution Config ---

// EvolutionConfig controls evolutionary quality improvement.
type EvolutionConfig struct {
	Enabled              *bool    `yaml:"enabled,omitempty"`
	MaxMutationsPerRound *int     `yaml:"max_mutations_per_round,omitempty"`
	NoveltyThreshold     *float64 `yaml:"novelty_threshold,omitempty"`
	Strategies           []string `yaml:"strategies,omitempty"`
}

func (e EvolutionConfig) EffectiveEnabled() bool                { return effectiveValue(e.Enabled, false) }
func (e EvolutionConfig) EffectiveMaxMutationsPerRound() int    { return effectiveValue(e.MaxMutationsPerRound, DefaultMaxMutationsPerRound) }
func (e EvolutionConfig) EffectiveNoveltyThreshold() float64    { return effectiveValue(e.NoveltyThreshold, DefaultNoveltyThreshold) }

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

func (b BanditConfig) EffectiveEnabled() bool                { return effectiveValue(b.Enabled, false) }
func (b BanditConfig) EffectiveExplorationCoeff() float64    { return effectiveValue(b.ExplorationCoeff, DefaultExplorationCoeff) }
func (b BanditConfig) EffectiveMinSamplesBeforeUse() int     { return effectiveValue(b.MinSamplesBeforeUse, DefaultMinSamplesBeforeUse) }
func (b BanditConfig) EffectiveDecayFactor() float64         { return effectiveValue(b.DecayFactor, DefaultDecayFactor) }
func (b BanditConfig) EffectiveTraceDataRequirement() int    { return effectiveValue(b.TraceDataRequirement, DefaultTraceDataRequirement) }

// --- C-3 Extended Verification Config ---

// ExtendedVerificationConfig controls extended verification perspectives.
type ExtendedVerificationConfig struct {
	Enabled            *bool              `yaml:"enabled,omitempty"`
	SecurityCheck      *bool              `yaml:"security_check,omitempty"`
	PerformanceBench   *bool              `yaml:"performance_bench,omitempty"`
	PerspectiveWeights map[string]float64 `yaml:"perspective_weights,omitempty"`
	MaxAutoRetries     *int               `yaml:"max_auto_retries,omitempty"`
}

func (ev ExtendedVerificationConfig) EffectiveEnabled() bool          { return effectiveValue(ev.Enabled, false) }
func (ev ExtendedVerificationConfig) EffectiveSecurityCheck() bool    { return effectiveValue(ev.SecurityCheck, false) }
func (ev ExtendedVerificationConfig) EffectivePerformanceBench() bool { return effectiveValue(ev.PerformanceBench, false) }
func (ev ExtendedVerificationConfig) EffectiveMaxAutoRetries() int    { return effectiveValue(ev.MaxAutoRetries, DefaultMaxAutoRetries) }

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

func (s SearchConfig) EffectiveEnabled() bool            { return effectiveValue(s.Enabled, false) }
func (s SearchConfig) EffectiveMaxDepth() int            { return effectiveValue(s.MaxDepth, DefaultSearchMaxDepth) }
func (s SearchConfig) EffectiveMaxBranching() int        { return effectiveValue(s.MaxBranching, DefaultMaxBranching) }
func (s SearchConfig) EffectivePruneThreshold() float64  { return effectiveValue(s.PruneThreshold, DefaultPruneThreshold) }
func (s SearchConfig) EffectiveThompsonAlpha() float64   { return effectiveValue(s.ThompsonAlpha, DefaultThompsonAlpha) }
func (s SearchConfig) EffectiveThompsonBeta() float64    { return effectiveValue(s.ThompsonBeta, DefaultThompsonBeta) }

// --- C-5 Self-Improvement Config ---

// SelfImprovementConfig controls self-improvement of prompts and personas.
type SelfImprovementConfig struct {
	Enabled        *bool    `yaml:"enabled,omitempty"`
	Targets        []string `yaml:"targets,omitempty"`
	ExcludeTargets []string `yaml:"exclude_targets,omitempty"`
	ArchiveMaxSize *int     `yaml:"archive_max_size,omitempty"`
}

func (si SelfImprovementConfig) EffectiveEnabled() bool     { return effectiveValue(si.Enabled, false) }
func (si SelfImprovementConfig) EffectiveArchiveMaxSize() int { return effectiveValue(si.ArchiveMaxSize, DefaultArchiveMaxSize) }

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

func (ct ComplexityThresholds) EffectiveSimpleMaxFiles() int   { return effectiveValue(ct.SimpleMaxFiles, DefaultSimpleMaxFiles) }
func (ct ComplexityThresholds) EffectiveStandardMaxFiles() int { return effectiveValue(ct.StandardMaxFiles, DefaultStandardMaxFiles) }
func (ct ComplexityThresholds) EffectiveComplexMaxFiles() int  { return effectiveValue(ct.ComplexMaxFiles, DefaultComplexMaxFiles) }

// --- C-7 Runtime Config ---

// RuntimeConfig holds per-runtime configuration.
type RuntimeConfig struct {
	Enabled      *bool    `yaml:"enabled,omitempty"`
	Default      *bool    `yaml:"default,omitempty"`
	Models       []string `yaml:"models,omitempty"`
	DefaultModel *string  `yaml:"default_model,omitempty"`
}

func (rc RuntimeConfig) EffectiveEnabled() bool       { return effectiveValue(rc.Enabled, false) }
func (rc RuntimeConfig) EffectiveDefault() bool       { return effectiveValue(rc.Default, false) }
func (rc RuntimeConfig) EffectiveDefaultModel() string { return effectiveValue(rc.DefaultModel, "") }

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

func (fp FeatureProfile) EffectiveCrossAgentReview() string        { return effectiveValue(fp.CrossAgentReview, DefaultCrossAgentReview) }
func (fp FeatureProfile) EffectiveExploratoryOptimization() bool   { return effectiveValue(fp.ExploratoryOptimization, false) }
func (fp FeatureProfile) EffectiveEvolutionaryQuality() bool       { return effectiveValue(fp.EvolutionaryQuality, false) }
func (fp FeatureProfile) EffectiveAdaptiveModelSelection() bool    { return effectiveValue(fp.AdaptiveModelSelection, false) }
func (fp FeatureProfile) EffectiveSelfImprovement() bool           { return effectiveValue(fp.SelfImprovement, false) }
func (fp FeatureProfile) EffectiveAdaptiveDepth() bool             { return effectiveValue(fp.AdaptiveDepth, false) }

// Validate checks all Config fields for consistency after yaml.Unmarshal.
// Returns a joined error containing all validation failures with field paths.
func (c Config) Validate() error {
	var errs []error

	// project.name
	if c.Project.Name == "" {
		errs = append(errs, fmt.Errorf("project.name: must not be empty"))
	}

	// maestro.version
	if c.Maestro.Version == "" {
		errs = append(errs, fmt.Errorf("maestro.version: must not be empty"))
	}

	// agents.workers.count
	if c.Agents.Workers.Count < MinWorkers || c.Agents.Workers.Count > MaxWorkers {
		errs = append(errs, fmt.Errorf("agents.workers.count: must be between %d and %d", MinWorkers, MaxWorkers))
	}

	// agents model name validation
	if !isValidModelName(c.Agents.Orchestrator.Model) {
		errs = append(errs, fmt.Errorf("agents.orchestrator.model: invalid model name %q", c.Agents.Orchestrator.Model))
	}
	if !isValidModelName(c.Agents.Planner.Model) {
		errs = append(errs, fmt.Errorf("agents.planner.model: invalid model name %q", c.Agents.Planner.Model))
	}
	if !isValidModelName(c.Agents.Workers.DefaultModel) {
		errs = append(errs, fmt.Errorf("agents.workers.default_model: invalid model name %q", c.Agents.Workers.DefaultModel))
	}
	for workerID, m := range c.Agents.Workers.Models {
		if !isValidModelName(m) {
			errs = append(errs, fmt.Errorf("agents.workers.models.%s: invalid model name %q", workerID, m))
		}
	}

	// watcher fields: reject negative values; zero means "use runtime default"
	if c.Watcher.BusyCheckInterval < 0 {
		errs = append(errs, fmt.Errorf("watcher.busy_check_interval: must be >= 0"))
	}
	if c.Watcher.BusyCheckMaxRetries < 0 || c.Watcher.BusyCheckMaxRetries > MaxBusyCheckMaxRetries {
		errs = append(errs, fmt.Errorf("watcher.busy_check_max_retries: must be between 0 and %d", MaxBusyCheckMaxRetries))
	}
	if c.Watcher.IdleStableSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.idle_stable_sec: must be >= 0"))
	}
	if c.Watcher.ScanIntervalSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.scan_interval_sec: must be >= 0"))
	}
	if c.Watcher.DispatchLeaseSec < 0 || c.Watcher.DispatchLeaseSec > MaxDispatchLeaseSec {
		errs = append(errs, fmt.Errorf("watcher.dispatch_lease_sec: must be between 0 and %d", MaxDispatchLeaseSec))
	}
	if c.Watcher.MaxInProgressMin != nil && (*c.Watcher.MaxInProgressMin < 0 || *c.Watcher.MaxInProgressMin > MaxMaxInProgressMin) {
		errs = append(errs, fmt.Errorf("watcher.max_in_progress_min: must be between 0 and %d", MaxMaxInProgressMin))
	}
	if c.Watcher.DebounceSec < 0 || math.IsNaN(c.Watcher.DebounceSec) || math.IsInf(c.Watcher.DebounceSec, 0) {
		errs = append(errs, fmt.Errorf("watcher.debounce_sec: must be a finite value >= 0"))
	}
	if c.Watcher.CooldownAfterClear < 0 {
		errs = append(errs, fmt.Errorf("watcher.cooldown_after_clear: must be >= 0"))
	}
	if c.Watcher.NotifyLeaseSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.notify_lease_sec: must be >= 0"))
	}
	if c.Watcher.WaitReadyIntervalSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.wait_ready_interval_sec: must be >= 0"))
	}
	if c.Watcher.WaitReadyMaxRetries < 0 || c.Watcher.WaitReadyMaxRetries > MaxWaitReadyMaxRetries {
		errs = append(errs, fmt.Errorf("watcher.wait_ready_max_retries: must be between 0 and %d", MaxWaitReadyMaxRetries))
	}
	if c.Watcher.ClearConfirmTimeoutSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.clear_confirm_timeout_sec: must be >= 0"))
	}
	if c.Watcher.ClearConfirmPollMs < 0 {
		errs = append(errs, fmt.Errorf("watcher.clear_confirm_poll_ms: must be >= 0"))
	}
	if c.Watcher.ClearMaxAttempts < 0 {
		errs = append(errs, fmt.Errorf("watcher.clear_max_attempts: must be >= 0"))
	}
	if c.Watcher.ClearRetryBackoffMs < 0 {
		errs = append(errs, fmt.Errorf("watcher.clear_retry_backoff_ms: must be >= 0"))
	}
	if c.Watcher.ClearSecondEnterDelayMs < 0 {
		errs = append(errs, fmt.Errorf("watcher.clear_second_enter_delay_ms: must be >= 0"))
	}

	// queue fields
	if c.Queue.PriorityAgingSec < 0 {
		errs = append(errs, fmt.Errorf("queue.priority_aging_sec: must be >= 0"))
	}

	// continuous fields (0 means unlimited — no iteration cap)
	if c.Continuous.Enabled && c.Continuous.MaxIterations < 0 {
		errs = append(errs, fmt.Errorf("continuous.max_iterations: must be >= 0 when continuous is enabled"))
	}
	if c.Continuous.Enabled && c.Continuous.MaxConsecutiveFailures < 0 {
		errs = append(errs, fmt.Errorf("continuous.max_consecutive_failures: must be >= 0 when continuous is enabled"))
	}

	// retry fields
	if c.Retry.CommandDispatch < 0 {
		errs = append(errs, fmt.Errorf("retry.command_dispatch: must be >= 0"))
	}
	if c.Retry.TaskDispatch < 0 {
		errs = append(errs, fmt.Errorf("retry.task_dispatch: must be >= 0"))
	}
	if c.Retry.OrchestratorNotificationDispatch < 0 {
		errs = append(errs, fmt.Errorf("retry.orchestrator_notification_dispatch: must be >= 0"))
	}
	if c.Retry.TaskExecution.MaxRetries < 0 {
		errs = append(errs, fmt.Errorf("retry.task_execution.max_retries: must be >= 0"))
	}
	if c.Retry.TaskExecution.CooldownSec < 0 {
		errs = append(errs, fmt.Errorf("retry.task_execution.cooldown_sec: must be >= 0"))
	}

	// limits
	if c.Limits.MaxPendingCommands < 0 || c.Limits.MaxPendingCommands > MaxMaxPendingCommands {
		errs = append(errs, fmt.Errorf("limits.max_pending_commands: must be between 0 and %d", MaxMaxPendingCommands))
	}
	if c.Limits.MaxPendingTasksPerWorker < 0 || c.Limits.MaxPendingTasksPerWorker > MaxMaxPendingTasksPerWorker {
		errs = append(errs, fmt.Errorf("limits.max_pending_tasks_per_worker: must be between 0 and %d", MaxMaxPendingTasksPerWorker))
	}
	if c.Limits.MaxDeadLetterArchiveFiles != nil && (*c.Limits.MaxDeadLetterArchiveFiles < 0 || *c.Limits.MaxDeadLetterArchiveFiles > MaxMaxDeadLetterArchiveFiles) {
		errs = append(errs, fmt.Errorf("limits.max_dead_letter_archive_files: must be between 0 and %d", MaxMaxDeadLetterArchiveFiles))
	}
	if c.Limits.MaxQuarantineFiles != nil && (*c.Limits.MaxQuarantineFiles < 0 || *c.Limits.MaxQuarantineFiles > MaxMaxQuarantineFiles) {
		errs = append(errs, fmt.Errorf("limits.max_quarantine_files: must be between 0 and %d", MaxMaxQuarantineFiles))
	}
	if c.Limits.MaxEntryContentBytes < 0 {
		errs = append(errs, fmt.Errorf("limits.max_entry_content_bytes: must be >= 0"))
	}
	if c.Limits.MaxYAMLFileBytes < 0 || c.Limits.MaxYAMLFileBytes > MaxMaxYAMLFileBytes {
		errs = append(errs, fmt.Errorf("limits.max_yaml_file_bytes: must be between 0 and %d", MaxMaxYAMLFileBytes))
	}

	// shutdown_timeout_sec
	if c.ShutdownTimeoutSec < 0 || c.ShutdownTimeoutSec > MaxShutdownTimeoutSec {
		errs = append(errs, fmt.Errorf("shutdown_timeout_sec: must be between 0 and %d", MaxShutdownTimeoutSec))
	}

	// circuit_breaker
	if c.CircuitBreaker.MaxConsecutiveFailures != nil && *c.CircuitBreaker.MaxConsecutiveFailures < 0 {
		errs = append(errs, fmt.Errorf("circuit_breaker.max_consecutive_failures: must be >= 0"))
	}
	if c.CircuitBreaker.ProgressTimeoutMinutes != nil && *c.CircuitBreaker.ProgressTimeoutMinutes < 0 {
		errs = append(errs, fmt.Errorf("circuit_breaker.progress_timeout_minutes: must be >= 0"))
	}

	// learnings
	if c.Learnings.MaxEntries != nil && *c.Learnings.MaxEntries < 0 {
		errs = append(errs, fmt.Errorf("learnings.max_entries: must be >= 0"))
	}
	if c.Learnings.MaxContentLength != nil && *c.Learnings.MaxContentLength < 0 {
		errs = append(errs, fmt.Errorf("learnings.max_content_length: must be >= 0"))
	}
	if c.Learnings.InjectCount != nil && *c.Learnings.InjectCount < 0 {
		errs = append(errs, fmt.Errorf("learnings.inject_count: must be >= 0"))
	}

	// skills
	if c.Skills.MaxRefsPerTask != nil && *c.Skills.MaxRefsPerTask < 0 {
		errs = append(errs, fmt.Errorf("skills.max_refs_per_task: must be >= 0"))
	}
	if c.Skills.MaxBodyChars != nil && *c.Skills.MaxBodyChars < 0 {
		errs = append(errs, fmt.Errorf("skills.max_body_chars: must be >= 0"))
	}
	if p := c.Skills.MissingRefPolicy; p != "" && p != "warn" && p != "error" {
		errs = append(errs, fmt.Errorf("skills.missing_ref_policy: must be \"warn\" or \"error\""))
	}
	if c.Skills.AutoCollect.MinOccurrences != nil && *c.Skills.AutoCollect.MinOccurrences < 0 {
		errs = append(errs, fmt.Errorf("skills.auto_collect.min_occurrences: must be >= 0"))
	}
	if c.Skills.AutoCollect.MinCommands != nil && *c.Skills.AutoCollect.MinCommands < 0 {
		errs = append(errs, fmt.Errorf("skills.auto_collect.min_commands: must be >= 0"))
	}

	// admission_control: negative values are invalid
	if c.AdmissionControl.MaxConcurrentVerify < 0 {
		errs = append(errs, fmt.Errorf("admission_control.max_concurrent_verify: must be >= 0"))
	}
	if c.AdmissionControl.MaxConcurrentRepair < 0 {
		errs = append(errs, fmt.Errorf("admission_control.max_concurrent_repair: must be >= 0"))
	}
	if c.AdmissionControl.MaxConcurrentRollout < 0 {
		errs = append(errs, fmt.Errorf("admission_control.max_concurrent_rollout: must be >= 0"))
	}

	// fallback
	if c.Fallback.ConsecutiveFailureThreshold < 0 {
		errs = append(errs, fmt.Errorf("fallback.consecutive_failure_threshold: must be >= 0"))
	}
	if c.Fallback.RecoveryCheckIntervalSec < 0 {
		errs = append(errs, fmt.Errorf("fallback.recovery_check_interval_sec: must be >= 0"))
	}
	if c.Fallback.MinHealthyDurationSec < 0 {
		errs = append(errs, fmt.Errorf("fallback.min_healthy_duration_sec: must be >= 0"))
	}

	// review
	if c.Review.MinBloomLevel != nil && *c.Review.MinBloomLevel < 0 {
		errs = append(errs, fmt.Errorf("review.min_bloom_level: must be >= 0"))
	}
	if c.Review.MaxConcurrentReviews != nil && *c.Review.MaxConcurrentReviews < 0 {
		errs = append(errs, fmt.Errorf("review.max_concurrent_reviews: must be >= 0"))
	}
	if c.Review.TimeoutSec != nil && *c.Review.TimeoutSec < 0 {
		errs = append(errs, fmt.Errorf("review.timeout_sec: must be >= 0"))
	}

	// rollout
	if c.Rollout.MaxConcurrent != nil && *c.Rollout.MaxConcurrent < 0 {
		errs = append(errs, fmt.Errorf("rollout.max_concurrent: must be >= 0"))
	}
	if c.Rollout.MaxParallelPerTask != nil && *c.Rollout.MaxParallelPerTask < 0 {
		errs = append(errs, fmt.Errorf("rollout.max_parallel_per_task: must be >= 0"))
	}
	if c.Rollout.MinBloomLevel != nil && *c.Rollout.MinBloomLevel < 0 {
		errs = append(errs, fmt.Errorf("rollout.min_bloom_level: must be >= 0"))
	}
	if c.Rollout.MaxExpectedPaths != nil && *c.Rollout.MaxExpectedPaths < 0 {
		errs = append(errs, fmt.Errorf("rollout.max_expected_paths: must be >= 0"))
	}
	if c.Rollout.MinFailureCount != nil && *c.Rollout.MinFailureCount < 0 {
		errs = append(errs, fmt.Errorf("rollout.min_failure_count: must be >= 0"))
	}

	// judge
	if c.Judge.TimeoutSec != nil && *c.Judge.TimeoutSec < 0 {
		errs = append(errs, fmt.Errorf("judge.timeout_sec: must be >= 0"))
	}

	// quality_gates
	if fa := c.QualityGates.Enforcement.FailureAction; fa != "" && fa != "warn" && fa != "block" {
		errs = append(errs, fmt.Errorf("quality_gates.enforcement.failure_action: must be \"warn\" or \"block\""))
	}

	// worktree fields
	if ms := c.Worktree.MergeStrategy; ms != "" && ms != "ort" && ms != "ours" && ms != "theirs" && ms != "recursive" {
		errs = append(errs, fmt.Errorf("worktree.merge_strategy: must be one of \"ort\", \"ours\", \"theirs\", \"recursive\""))
	}
	if c.Worktree.GitTimeoutSec != nil && *c.Worktree.GitTimeoutSec <= 0 {
		errs = append(errs, fmt.Errorf("worktree.git_timeout_sec: must be > 0"))
	}
	if c.Worktree.CommitPolicy.MaxFiles != nil && *c.Worktree.CommitPolicy.MaxFiles < 0 {
		errs = append(errs, fmt.Errorf("worktree.commit_policy.max_files: must be >= 0"))
	}
	if c.Worktree.GC.Enabled {
		if c.Worktree.GC.TTLHours != nil && *c.Worktree.GC.TTLHours <= 0 {
			errs = append(errs, fmt.Errorf("worktree.gc.ttl_hours: must be > 0 when gc is enabled"))
		}
		if c.Worktree.GC.MaxWorktrees != nil && (*c.Worktree.GC.MaxWorktrees <= 0 || *c.Worktree.GC.MaxWorktrees > MaxMaxWorktrees) {
			errs = append(errs, fmt.Errorf("worktree.gc.max_worktrees: must be between 1 and %d when gc is enabled", MaxMaxWorktrees))
		}
	}

	// float64 pointer fields: reject NaN/Inf
	if !isFiniteFloat64Ptr(c.Evolution.NoveltyThreshold) {
		errs = append(errs, fmt.Errorf("evolution.novelty_threshold: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Bandit.ExplorationCoeff) {
		errs = append(errs, fmt.Errorf("bandit.exploration_coefficient: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Bandit.DecayFactor) {
		errs = append(errs, fmt.Errorf("bandit.decay_factor: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Search.PruneThreshold) {
		errs = append(errs, fmt.Errorf("search.prune_threshold: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Search.ThompsonAlpha) {
		errs = append(errs, fmt.Errorf("search.thompson_alpha: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Search.ThompsonBeta) {
		errs = append(errs, fmt.Errorf("search.thompson_beta: must be a finite value"))
	}
	for k, v := range c.ExtendedVerification.PerspectiveWeights {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			errs = append(errs, fmt.Errorf("extended_verification.perspective_weights.%s: must be a finite value", k))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
