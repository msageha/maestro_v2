// Package model holds the configuration types and state-machine status
// values shared across maestro_v2. Detailed package-level documentation
// lives in doc.go (including the naming convention for
// Status / State / Phase); this file only re-asserts the package
// directive in the form revive's package-comments check expects.
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
	Verify             VerifyDaemonConfig   `yaml:"verify,omitempty"`
	Review             ReviewConfig         `yaml:"review"`
	ABTest             ABTestConfig         `yaml:"ab_test,omitempty"`

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
	// C-7 (Runtime selection is inferred from model name — no separate config field.)
	// C-8 Feature Profiles
	FeatureProfiles map[string]FeatureProfile `yaml:"feature_profiles,omitempty"`
}

// --- SkillsConfig ---

// SkillsConfig controls the skill reference feature for tasks.
type SkillsConfig struct {
	Enabled          bool   `yaml:"enabled"`
	MaxRefsPerTask   *int   `yaml:"max_refs_per_task"`
	MaxBodyChars     *int   `yaml:"max_body_chars"`
	MissingRefPolicy string `yaml:"missing_ref_policy"`
	// ExtraDirs lists additional skill source directories searched before
	// the bundled <maestro_dir>/skills catalog. Each entry is an absolute
	// path or a path relative to the project root and uses the same
	// <role>/<name>/SKILL.md layout as the bundled catalog. Unlike
	// .maestro/skills (gitignored, overwritten by setup/repair), extra dirs
	// are ordinary repo files, so they are the supported home for
	// version-controlled, team-supplied skills. Earlier entries take
	// precedence, and every entry shadows the bundled catalog on same-name
	// conflicts; conflicts are reported with a WARN, never silently.
	// Missing directories are skipped with a WARN at use time so a stale
	// config never stops the daemon.
	ExtraDirs   []string          `yaml:"extra_dirs"`
	AutoCollect autoCollectConfig `yaml:"auto_collect"`
}

// EffectiveMaxRefsPerTask returns MaxRefsPerTask, or DefaultMaxRefsPerTask when unset.
func (s SkillsConfig) EffectiveMaxRefsPerTask() int {
	return effectiveValue(s.MaxRefsPerTask, DefaultMaxRefsPerTask)
}

// EffectiveMaxBodyChars returns MaxBodyChars, or 0 (unlimited) when unset.
func (s SkillsConfig) EffectiveMaxBodyChars() int { return effectiveValue(s.MaxBodyChars, 0) }

// EffectiveMissingRefPolicy returns MissingRefPolicy, or DefaultMissingRefPolicy when empty.
func (s SkillsConfig) EffectiveMissingRefPolicy() string {
	return effectiveNonZero(s.MissingRefPolicy, DefaultMissingRefPolicy)
}

// autoCollectConfig controls automatic skill collection from learnings.
// (Removed) MinOccurrences / MinCommands — the thresholds were parsed and
// validated but never read by any collection logic. Old YAML configs with
// the keys continue to load (yaml.v3 ignores unknown fields).
type autoCollectConfig struct {
	Enabled bool `yaml:"enabled"`
}

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
	// AwaitingFillStallNotifyMinutes is the elapsed time after which the
	// daemon's awaiting-fill watchdog re-emits an `awaiting_fill_stall`
	// signal to the Planner. The original `awaiting_fill` signal is fired
	// on phase entry; if the Planner pane stalls afterwards (long
	// "Thinking" with no `plan submit`), the signal queue empties and
	// there is no further re-prompt until R6 fires the hard
	// fill_deadline_at timeout (defaults to 3 hours). The watchdog closes
	// that gap. 0 disables the watchdog (no re-prompt; R6 still applies).
	// Default 5 minutes.
	AwaitingFillStallNotifyMinutes *int `yaml:"awaiting_fill_stall_notify_minutes,omitempty"`
}

// EffectiveAwaitingFillStallNotifyMinutes returns AwaitingFillStallNotifyMinutes
// or DefaultAwaitingFillStallNotifyMinutes when unset.
func (m MaestroConfig) EffectiveAwaitingFillStallNotifyMinutes() int {
	return effectiveValue(m.AwaitingFillStallNotifyMinutes, DefaultAwaitingFillStallNotifyMinutes)
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

// ModelFor returns the model configured for the given worker, applying the
// same precedence as task assignment: boost forces opus, then the per-worker
// Models override, then DefaultModel, then "sonnet".
func (w WorkerConfig) ModelFor(workerID string) string {
	if w.Boost {
		return "opus"
	}
	if m, ok := w.Models[workerID]; ok {
		return m
	}
	if w.DefaultModel != "" {
		return w.DefaultModel
	}
	return "sonnet"
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
	// StallNotificationSec sets the continuous stall watchdog threshold: when
	// continuous mode is running, the last iteration's result has already
	// been processed, no non-terminal command remains in the planner queue,
	// and this many seconds elapse without a next command, the daemon emits a
	// continuous_stalled notification to the Orchestrator. The Orchestrator
	// owns next-iteration generation, so a missed command_completed
	// notification (compaction, lost paste) otherwise stalls the loop with
	// every component nominally healthy.
	//
	// 0 (key absent) applies the 600s default — unlike
	// max_consecutive_failures this watchdog is advisory-only (a
	// notification, no state change), so a hidden default is safe. Negative
	// values disable the watchdog.
	StallNotificationSec int `yaml:"stall_notification_sec"`
}

// EffectiveStallNotificationSec returns the continuous stall watchdog
// threshold in seconds: 600 when unset (0), 0 when explicitly disabled
// (negative), otherwise the configured value.
func (c ContinuousConfig) EffectiveStallNotificationSec() int {
	switch {
	case c.StallNotificationSec < 0:
		return 0
	case c.StallNotificationSec == 0:
		return 600
	default:
		return c.StallNotificationSec
	}
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
	ClearConfirmTimeoutSec int `yaml:"clear_confirm_timeout_sec"` // Per-attempt confirmation window (default 5s)
	ClearConfirmPollMs     int `yaml:"clear_confirm_poll_ms"`     // Polling interval within confirmation window (default 250ms)
	ClearMaxAttempts       int `yaml:"clear_max_attempts"`        // Total send attempts including initial (default 3)
	ClearRetryBackoffMs    int `yaml:"clear_retry_backoff_ms"`    // Base backoff between attempts; doubles each retry (default 500ms)

	// Shell readiness timeout for formation startup (default 10s)
	ShellReadyTimeoutSec int `yaml:"shell_ready_timeout_sec"`
}

// EffectiveMaxInProgressMin returns MaxInProgressMin, or DefaultMaxInProgressMin when unset.
func (w WatcherConfig) EffectiveMaxInProgressMin() int {
	return effectiveValue(w.MaxInProgressMin, DefaultMaxInProgressMin)
}

// --- RetryConfig ---

// RetryConfig holds retry limits for the various dispatch and execution operations.
type RetryConfig struct {
	CommandDispatch                    int             `yaml:"command_dispatch"`
	TaskDispatch                       int             `yaml:"task_dispatch"`
	OrchestratorNotificationDispatch   int             `yaml:"orchestrator_notification_dispatch"`
	SignalDispatch                     int             `yaml:"signal_dispatch"`
	SignalInlineRetries                *int            `yaml:"signal_inline_retries"`
	SignalInlineRetryDelaySec          *int            `yaml:"signal_inline_retry_delay_sec"`
	SignalDeliveryTimeoutSec           *int            `yaml:"signal_delivery_timeout_sec"`
	ResultNotifyInlineRetries          *int            `yaml:"result_notify_inline_retries"`
	ResultNotifyInlineRetryDelaySec    *int            `yaml:"result_notify_inline_retry_delay_sec"`
	ResultNotifyDeliveryTimeoutSec     *int            `yaml:"result_notify_delivery_timeout_sec"`
	CommandDispatchInlineRetries       *int            `yaml:"command_dispatch_inline_retries"`
	CommandDispatchInlineRetryDelaySec *int            `yaml:"command_dispatch_inline_retry_delay_sec"`
	TaskDispatchInlineRetries          *int            `yaml:"task_dispatch_inline_retries"`
	TaskDispatchInlineRetryDelaySec    *int            `yaml:"task_dispatch_inline_retry_delay_sec"`
	TaskExecution                      TaskRetryConfig `yaml:"task_execution"`
}

// EffectiveSignalInlineRetries returns SignalInlineRetries, or DefaultSignalInlineRetries when unset.
func (r RetryConfig) EffectiveSignalInlineRetries() int {
	return effectiveValue(r.SignalInlineRetries, DefaultSignalInlineRetries)
}

// EffectiveSignalInlineRetryDelaySec returns SignalInlineRetryDelaySec, or DefaultSignalInlineRetryDelaySec when unset.
func (r RetryConfig) EffectiveSignalInlineRetryDelaySec() int {
	return effectiveValue(r.SignalInlineRetryDelaySec, DefaultSignalInlineRetryDelaySec)
}

// EffectiveSignalDeliveryTimeoutSec returns SignalDeliveryTimeoutSec, or DefaultSignalDeliveryTimeoutSec when unset.
func (r RetryConfig) EffectiveSignalDeliveryTimeoutSec() int {
	return effectiveValue(r.SignalDeliveryTimeoutSec, DefaultSignalDeliveryTimeoutSec)
}

// EffectiveResultNotifyInlineRetries returns ResultNotifyInlineRetries, or DefaultResultNotifyInlineRetries when unset.
func (r RetryConfig) EffectiveResultNotifyInlineRetries() int {
	return effectiveValue(r.ResultNotifyInlineRetries, DefaultResultNotifyInlineRetries)
}

// EffectiveResultNotifyInlineRetryDelaySec returns ResultNotifyInlineRetryDelaySec, or DefaultResultNotifyInlineRetryDelaySec when unset.
func (r RetryConfig) EffectiveResultNotifyInlineRetryDelaySec() int {
	return effectiveValue(r.ResultNotifyInlineRetryDelaySec, DefaultResultNotifyInlineRetryDelaySec)
}

// EffectiveCommandDispatchInlineRetries returns CommandDispatchInlineRetries, or DefaultCommandDispatchInlineRetries when unset.
func (r RetryConfig) EffectiveCommandDispatchInlineRetries() int {
	return effectiveValue(r.CommandDispatchInlineRetries, DefaultCommandDispatchInlineRetries)
}

// EffectiveCommandDispatchInlineRetryDelaySec returns CommandDispatchInlineRetryDelaySec, or DefaultCommandDispatchInlineRetryDelaySec when unset.
func (r RetryConfig) EffectiveCommandDispatchInlineRetryDelaySec() int {
	return effectiveValue(r.CommandDispatchInlineRetryDelaySec, DefaultCommandDispatchInlineRetryDelaySec)
}

// EffectiveTaskDispatchInlineRetries returns TaskDispatchInlineRetries, or DefaultTaskDispatchInlineRetries when unset.
func (r RetryConfig) EffectiveTaskDispatchInlineRetries() int {
	return effectiveValue(r.TaskDispatchInlineRetries, DefaultTaskDispatchInlineRetries)
}

// EffectiveTaskDispatchInlineRetryDelaySec returns TaskDispatchInlineRetryDelaySec, or DefaultTaskDispatchInlineRetryDelaySec when unset.
func (r RetryConfig) EffectiveTaskDispatchInlineRetryDelaySec() int {
	return effectiveValue(r.TaskDispatchInlineRetryDelaySec, DefaultTaskDispatchInlineRetryDelaySec)
}

// EffectiveResultNotifyDeliveryTimeoutSec returns ResultNotifyDeliveryTimeoutSec, or DefaultResultNotifyDeliveryTimeoutSec when unset.
func (r RetryConfig) EffectiveResultNotifyDeliveryTimeoutSec() int {
	return effectiveValue(r.ResultNotifyDeliveryTimeoutSec, DefaultResultNotifyDeliveryTimeoutSec)
}

// NormalizeRetryConfig fills nil pointer fields in RetryConfig with their default values.
// Call once after unmarshalling config.yaml so that EffectiveXxx() methods
// are guaranteed to find non-nil values when explicitly set (including 0).
func NormalizeRetryConfig(cfg *Config) {
	resolvePtr(&cfg.Retry.SignalInlineRetries, DefaultSignalInlineRetries)
	resolvePtr(&cfg.Retry.SignalInlineRetryDelaySec, DefaultSignalInlineRetryDelaySec)
	resolvePtr(&cfg.Retry.SignalDeliveryTimeoutSec, DefaultSignalDeliveryTimeoutSec)
	resolvePtr(&cfg.Retry.ResultNotifyInlineRetries, DefaultResultNotifyInlineRetries)
	resolvePtr(&cfg.Retry.ResultNotifyInlineRetryDelaySec, DefaultResultNotifyInlineRetryDelaySec)
	resolvePtr(&cfg.Retry.ResultNotifyDeliveryTimeoutSec, DefaultResultNotifyDeliveryTimeoutSec)
	resolvePtr(&cfg.Retry.CommandDispatchInlineRetries, DefaultCommandDispatchInlineRetries)
	resolvePtr(&cfg.Retry.CommandDispatchInlineRetryDelaySec, DefaultCommandDispatchInlineRetryDelaySec)
	resolvePtr(&cfg.Retry.TaskDispatchInlineRetries, DefaultTaskDispatchInlineRetries)
	resolvePtr(&cfg.Retry.TaskDispatchInlineRetryDelaySec, DefaultTaskDispatchInlineRetryDelaySec)
}

// VerifyDaemonConfig holds daemon-side controls for the §S1-1 verification
// runner and the R9 verify-pending stall reconciler. The verify config schema
// itself lives in model.VerifyConfig (see internal/model/verify.go); this
// struct only carries operational parameters that affect daemon behaviour.
type VerifyDaemonConfig struct {
	// Enabled toggles the real verification runner. False is the supported
	// "no machine-checkable verify step" mode for projects that are not
	// running software-development workflows (research, documentation,
	// note-taking, …); the daemon wires NewSkipVerifyRunner and continues
	// normally. Operators opt in to verification by writing
	// `.maestro/verify.yaml` and setting Enabled=true.
	Enabled *bool `yaml:"enabled,omitempty"`
	// StallThresholdSec is the wall-clock window after a task enters
	// verify_pending before R9 transitions it to repair_pending. 0 ≤ value;
	// 0 disables stall recovery. Default DefaultVerifyStallThresholdSec.
	StallThresholdSec *int `yaml:"stall_threshold_sec,omitempty"`
	// PausedForReplanDeadletterSec is the wall-clock window after a task
	// enters paused_for_replan before R10 escalates it to a terminal failed
	// state. Once escalated, the phase containing the task transitions to
	// PhaseStatusFailed (via the dependency resolver applied on the next
	// scan), allowing the publish gate to make a decision instead of
	// spinning on Planner inaction. 0 ≤ value; 0 disables R10.
	// Default DefaultPausedForReplanDeadletterSec (see config_defaults.go).
	PausedForReplanDeadletterSec *int `yaml:"paused_for_replan_deadletter_sec,omitempty"`
}

// EffectiveEnabled returns the configured verify.enabled value or true (the
// default — verification is on unless explicitly disabled).
func (v VerifyDaemonConfig) EffectiveEnabled() bool {
	if v.Enabled == nil {
		return true
	}
	return *v.Enabled
}

// EffectiveStallThresholdSec returns the configured stall threshold in seconds
// or DefaultVerifyStallThresholdSec.
func (v VerifyDaemonConfig) EffectiveStallThresholdSec() int {
	return effectiveValue(v.StallThresholdSec, DefaultVerifyStallThresholdSec)
}

// EffectivePausedForReplanDeadletterSec returns the configured paused_for_replan
// deadletter window or DefaultPausedForReplanDeadletterSec.
func (v VerifyDaemonConfig) EffectivePausedForReplanDeadletterSec() int {
	return effectiveValue(v.PausedForReplanDeadletterSec, DefaultPausedForReplanDeadletterSec)
}

// TaskRetryConfig holds configuration for automatic task execution retries.
type TaskRetryConfig struct {
	Enabled            bool  `yaml:"enabled"`
	RetryableExitCodes []int `yaml:"retryable_exit_codes"`
	MaxRetries         int   `yaml:"max_retries"`
	CooldownSec        int   `yaml:"cooldown_sec"`
}
