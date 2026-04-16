// Package model defines the data structures for Maestro's configuration, state, and queue entries.
package model

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

// effectiveBasePromptMode returns "replace" if mode equals "replace", or "append" otherwise.
func effectiveBasePromptMode(mode string) string {
	if mode == "replace" {
		return "replace"
	}
	return "append"
}

// EffectiveBasePromptMode returns the configured base prompt mode or "append" as default.
// Valid values: "replace" (--system-prompt), "append" (--append-system-prompt).
func (a AgentConfig) EffectiveBasePromptMode() string {
	return effectiveBasePromptMode(a.BasePromptMode)
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
	return effectiveBasePromptMode(w.BasePromptMode)
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
	SignalDispatch                   int             `yaml:"signal_dispatch"`
	SignalInlineRetries              int             `yaml:"signal_inline_retries"`
	SignalInlineRetryDelaySec        int             `yaml:"signal_inline_retry_delay_sec"`
	SignalDeliveryTimeoutSec         int             `yaml:"signal_delivery_timeout_sec"`
	ResultNotifyInlineRetries            int             `yaml:"result_notify_inline_retries"`
	ResultNotifyInlineRetryDelaySec      int             `yaml:"result_notify_inline_retry_delay_sec"`
	ResultNotifyDeliveryTimeoutSec       int             `yaml:"result_notify_delivery_timeout_sec"`
	CommandDispatchInlineRetries         int             `yaml:"command_dispatch_inline_retries"`
	CommandDispatchInlineRetryDelaySec   int             `yaml:"command_dispatch_inline_retry_delay_sec"`
	CommandDispatchTimeoutSec            int             `yaml:"command_dispatch_timeout_sec"`
	TaskExecution                        TaskRetryConfig `yaml:"task_execution"`
}

func (r RetryConfig) EffectiveSignalInlineRetries() int {
	return effectiveNonZero(r.SignalInlineRetries, DefaultSignalInlineRetries)
}

func (r RetryConfig) EffectiveSignalInlineRetryDelaySec() int {
	return effectiveNonZero(r.SignalInlineRetryDelaySec, DefaultSignalInlineRetryDelaySec)
}

func (r RetryConfig) EffectiveSignalDeliveryTimeoutSec() int {
	return effectiveNonZero(r.SignalDeliveryTimeoutSec, DefaultSignalDeliveryTimeoutSec)
}

func (r RetryConfig) EffectiveResultNotifyInlineRetries() int {
	return effectiveNonZero(r.ResultNotifyInlineRetries, DefaultResultNotifyInlineRetries)
}

func (r RetryConfig) EffectiveResultNotifyInlineRetryDelaySec() int {
	return effectiveNonZero(r.ResultNotifyInlineRetryDelaySec, DefaultResultNotifyInlineRetryDelaySec)
}

func (r RetryConfig) EffectiveCommandDispatchInlineRetries() int {
	return effectiveNonZero(r.CommandDispatchInlineRetries, DefaultCommandDispatchInlineRetries)
}

func (r RetryConfig) EffectiveCommandDispatchInlineRetryDelaySec() int {
	return effectiveNonZero(r.CommandDispatchInlineRetryDelaySec, DefaultCommandDispatchInlineRetryDelaySec)
}

func (r RetryConfig) EffectiveCommandDispatchTimeoutSec() int {
	return effectiveNonZero(r.CommandDispatchTimeoutSec, DefaultCommandDispatchTimeoutSec)
}

func (r RetryConfig) EffectiveResultNotifyDeliveryTimeoutSec() int {
	return effectiveNonZero(r.ResultNotifyDeliveryTimeoutSec, DefaultResultNotifyDeliveryTimeoutSec)
}

// TaskRetryConfig holds configuration for automatic task execution retries.
type TaskRetryConfig struct {
	Enabled            bool  `yaml:"enabled"`
	RetryableExitCodes []int `yaml:"retryable_exit_codes"`
	MaxRetries         int   `yaml:"max_retries"`
	CooldownSec        int   `yaml:"cooldown_sec"`
}

// --- Config convenience methods for deep access chains ---

// EffectiveSimpleMaxFiles returns Complexity.Thresholds.EffectiveSimpleMaxFiles().
func (c Config) EffectiveSimpleMaxFiles() int { return c.Complexity.Thresholds.EffectiveSimpleMaxFiles() }

// EffectiveStandardMaxFiles returns Complexity.Thresholds.EffectiveStandardMaxFiles().
func (c Config) EffectiveStandardMaxFiles() int { return c.Complexity.Thresholds.EffectiveStandardMaxFiles() }

// EffectiveComplexMaxFiles returns Complexity.Thresholds.EffectiveComplexMaxFiles().
func (c Config) EffectiveComplexMaxFiles() int { return c.Complexity.Thresholds.EffectiveComplexMaxFiles() }

// EffectiveGCTTLHours returns Worktree.GC.EffectiveTTLHours().
func (c Config) EffectiveGCTTLHours() int { return c.Worktree.GC.EffectiveTTLHours() }

// EffectiveGCMaxWorktrees returns Worktree.GC.EffectiveMaxWorktrees().
func (c Config) EffectiveGCMaxWorktrees() int { return c.Worktree.GC.EffectiveMaxWorktrees() }

// EffectiveCommitMaxFiles returns Worktree.CommitPolicy.EffectiveMaxFiles().
func (c Config) EffectiveCommitMaxFiles() int { return c.Worktree.CommitPolicy.EffectiveMaxFiles() }

// EffectiveAutoCollectMinOccurrences returns Skills.AutoCollect.EffectiveMinOccurrences().
func (c Config) EffectiveAutoCollectMinOccurrences() int { return c.Skills.AutoCollect.EffectiveMinOccurrences() }

// EffectiveAutoCollectMinCommands returns Skills.AutoCollect.EffectiveMinCommands().
func (c Config) EffectiveAutoCollectMinCommands() int { return c.Skills.AutoCollect.EffectiveMinCommands() }
