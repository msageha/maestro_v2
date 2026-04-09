// Package model defines the data structures for Maestro's configuration, state, and queue entries.
package model

import (
	"errors"
	"fmt"
	"time"
)

// DefaultMaxYAMLFileBytes is the default maximum size for YAML file reads (5MB).
const DefaultMaxYAMLFileBytes = 5 * 1024 * 1024

// MinWorkers is the minimum allowed worker count.
const MinWorkers = 1

// MaxWorkers is the maximum allowed worker count.
const MaxWorkers = 8

// IntPtr returns a pointer to the given int value.
// Used for setting *int config fields in tests and struct literals.
func IntPtr(v int) *int { return &v }

type Config struct {
	Project        ProjectConfig        `yaml:"project"`
	Maestro        MaestroConfig        `yaml:"maestro"`
	Agents         AgentsConfig         `yaml:"agents"`
	Continuous     ContinuousConfig     `yaml:"continuous"`
	Watcher        WatcherConfig        `yaml:"watcher"`
	Retry          RetryConfig          `yaml:"retry"`
	Queue          QueueConfig          `yaml:"queue"`
	Limits         LimitsConfig         `yaml:"limits"`
	ShutdownTimeoutSec int              `yaml:"shutdown_timeout_sec"`
	Logging        LoggingConfig        `yaml:"logging"`
	QualityGates   qualityGatesConfig   `yaml:"quality_gates"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
	Learnings      LearningsConfig      `yaml:"learnings"`
	Worktree       WorktreeConfig       `yaml:"worktree"`
	Skills           SkillsConfig     `yaml:"skills"`
	AdmissionControl AdmissionControl `yaml:"admission_control"`
	Fallback         Fallback         `yaml:"fallback"`
}

// SkillsConfig controls the skill reference feature for tasks.
type SkillsConfig struct {
	Enabled          bool              `yaml:"enabled"`
	MaxRefsPerTask   *int              `yaml:"max_refs_per_task"`
	MaxBodyChars     *int              `yaml:"max_body_chars"`
	MissingRefPolicy string            `yaml:"missing_ref_policy"`
	AutoCollect      autoCollectConfig `yaml:"auto_collect"`
}

// EffectiveMaxRefsPerTask returns the configured limit or 3 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (s SkillsConfig) EffectiveMaxRefsPerTask() int {
	if s.MaxRefsPerTask != nil {
		return *s.MaxRefsPerTask
	}
	return 3
}

// EffectiveMaxBodyChars returns the configured limit or 0 (no limit) as default.
// Shared skills are auto-injected and should not be silently dropped by size limits.
// Use max_refs_per_task to control the number of role-specific skills instead.
func (s SkillsConfig) EffectiveMaxBodyChars() int {
	if s.MaxBodyChars != nil {
		return *s.MaxBodyChars
	}
	return 0
}

// EffectiveMissingRefPolicy returns the configured policy or "warn" as default.
func (s SkillsConfig) EffectiveMissingRefPolicy() string {
	if s.MissingRefPolicy != "" {
		return s.MissingRefPolicy
	}
	return "warn"
}

// autoCollectConfig controls automatic skill collection from learnings.
type autoCollectConfig struct {
	Enabled        bool `yaml:"enabled"`
	MinOccurrences *int `yaml:"min_occurrences"`
	MinCommands    *int `yaml:"min_commands"`
}

// EffectiveMinOccurrences returns the configured minimum or 3 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (a autoCollectConfig) EffectiveMinOccurrences() int {
	if a.MinOccurrences != nil {
		return *a.MinOccurrences
	}
	return 3
}

// EffectiveMinCommands returns the configured minimum or 2 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (a autoCollectConfig) EffectiveMinCommands() int {
	if a.MinCommands != nil {
		return *a.MinCommands
	}
	return 2
}

type ProjectConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type MaestroConfig struct {
	Version     string `yaml:"version"`
	Created     string `yaml:"created"`
	ProjectRoot string `yaml:"project_root"`
}

type AgentsConfig struct {
	Orchestrator AgentConfig  `yaml:"orchestrator"`
	Planner      AgentConfig  `yaml:"planner"`
	Workers      WorkerConfig `yaml:"workers"`
}

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

type WatcherConfig struct {
	DebounceSec         float64 `yaml:"debounce_sec"`
	ScanIntervalSec     int     `yaml:"scan_interval_sec"`
	DispatchLeaseSec    int     `yaml:"dispatch_lease_sec"`
	MaxInProgressMin    *int    `yaml:"max_in_progress_min"`
	BusyCheckInterval   int     `yaml:"busy_check_interval"`
	BusyCheckMaxRetries int     `yaml:"busy_check_max_retries"`
	BusyPatterns        string  `yaml:"busy_patterns"`
	IdleStableSec       int     `yaml:"idle_stable_sec"`
	CooldownAfterClear  int     `yaml:"cooldown_after_clear"`
	NotifyLeaseSec      int     `yaml:"notify_lease_sec"`
	WaitReadyIntervalSec int    `yaml:"wait_ready_interval_sec"`
	WaitReadyMaxRetries  int    `yaml:"wait_ready_max_retries"`

	// Clear confirmation settings (used by clearAndConfirm)
	ClearConfirmTimeoutSec int `yaml:"clear_confirm_timeout_sec"` // Per-attempt confirmation window (default 5s)
	ClearConfirmPollMs     int `yaml:"clear_confirm_poll_ms"`     // Polling interval within confirmation window (default 250ms)
	ClearMaxAttempts       int `yaml:"clear_max_attempts"`        // Total send attempts including initial (default 3)
	ClearRetryBackoffMs      int `yaml:"clear_retry_backoff_ms"`      // Base backoff between attempts; doubles each retry (default 500ms)
	ClearSecondEnterDelayMs  int `yaml:"clear_second_enter_delay_ms"` // Delay before sending second Enter after /clear (default 500ms)
}

// EffectiveMaxInProgressMin returns the configured max in-progress timeout or 60 as default.
// nil (unset) returns the default; explicit 0 returns 0 (no timeout).
func (w WatcherConfig) EffectiveMaxInProgressMin() int {
	if w.MaxInProgressMin != nil {
		return *w.MaxInProgressMin
	}
	return 60
}

type RetryConfig struct {
	CommandDispatch                  int   `yaml:"command_dispatch"`
	TaskDispatch                     int   `yaml:"task_dispatch"`
	OrchestratorNotificationDispatch int   `yaml:"orchestrator_notification_dispatch"`
	TaskExecution                    TaskRetryConfig `yaml:"task_execution"`
}

type TaskRetryConfig struct {
	Enabled            bool  `yaml:"enabled"`
	RetryableExitCodes []int `yaml:"retryable_exit_codes"`
	MaxRetries         int   `yaml:"max_retries"`
	CooldownSec        int   `yaml:"cooldown_sec"`
}

type QueueConfig struct {
	PriorityAgingSec int `yaml:"priority_aging_sec"`
}

type LimitsConfig struct {
	MaxPendingCommands       int  `yaml:"max_pending_commands"`
	MaxPendingTasksPerWorker int  `yaml:"max_pending_tasks_per_worker"`
	MaxEntryContentBytes     int  `yaml:"max_entry_content_bytes"`
	MaxYAMLFileBytes         int  `yaml:"max_yaml_file_bytes"`
	MaxDeadLetterArchiveFiles *int `yaml:"max_dead_letter_archive_files"`
	MaxQuarantineFiles        *int `yaml:"max_quarantine_files"`
}

// EffectiveMaxDeadLetterArchiveFiles returns the configured limit or 100 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (l LimitsConfig) EffectiveMaxDeadLetterArchiveFiles() int {
	if l.MaxDeadLetterArchiveFiles != nil {
		return *l.MaxDeadLetterArchiveFiles
	}
	return 100
}

// EffectiveMaxQuarantineFiles returns the configured limit or 100 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (l LimitsConfig) EffectiveMaxQuarantineFiles() int {
	if l.MaxQuarantineFiles != nil {
		return *l.MaxQuarantineFiles
	}
	return 100
}

type LoggingConfig struct {
	Level string `yaml:"level"`
}

type qualityGatesConfig struct {
	Enabled       bool                       `yaml:"enabled"`
	SkipGates     bool                       `yaml:"skip_gates"`      // 緊急モードフラグ
	Thresholds    qualityGateThresholds      `yaml:"thresholds"`
	Enforcement   qualityGateEnforcement     `yaml:"enforcement"`
}

type qualityGateThresholds struct {
}

type qualityGateEnforcement struct {
	PreTaskCheck   bool   `yaml:"pre_task_check"`    // タスク実行前チェック
	FailureAction  string `yaml:"failure_action"`    // 失敗時の動作: "warn", "block"
}

// CircuitBreakerConfig controls the command-level circuit breaker that auto-stops
// commands after consecutive task failures.
type CircuitBreakerConfig struct {
	Enabled                bool `yaml:"enabled"`                   // opt-in, default: false
	MaxConsecutiveFailures *int `yaml:"max_consecutive_failures"`  // default: 3
	ProgressTimeoutMinutes *int `yaml:"progress_timeout_minutes"`  // default: 30, 0=disabled
}

// EffectiveMaxConsecutiveFailures returns the configured threshold or 3 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (c CircuitBreakerConfig) EffectiveMaxConsecutiveFailures() int {
	if c.MaxConsecutiveFailures != nil {
		return *c.MaxConsecutiveFailures
	}
	return 3
}

// EffectiveProgressTimeoutMinutes returns the configured timeout or 30 as default.
// nil (unset) returns the default; explicit 0 means disabled (no timeout).
func (c CircuitBreakerConfig) EffectiveProgressTimeoutMinutes() int {
	if c.ProgressTimeoutMinutes != nil {
		return *c.ProgressTimeoutMinutes
	}
	return 30
}

// LearningsConfig controls the learning accumulation feature.
type LearningsConfig struct {
	Enabled          bool `yaml:"enabled"`            // opt-in, default: false
	MaxEntries       *int `yaml:"max_entries"`        // default: 100
	MaxContentLength *int `yaml:"max_content_length"` // default: 500
	InjectCount      *int `yaml:"inject_count"`       // top-K learnings injected per task dispatch, default: 5
	TTLHours         int  `yaml:"ttl_hours"`          // learning expiry in hours, default: 72, 0=unlimited
}

// EffectiveMaxEntries returns the configured limit or 100 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (l LearningsConfig) EffectiveMaxEntries() int {
	if l.MaxEntries != nil {
		return *l.MaxEntries
	}
	return 100
}

// EffectiveMaxContentLength returns the configured limit or 500 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (l LearningsConfig) EffectiveMaxContentLength() int {
	if l.MaxContentLength != nil {
		return *l.MaxContentLength
	}
	return 500
}

// EffectiveInjectCount returns the configured inject count or 5 as default.
// nil (unset) returns the default; explicit 0 returns 0 (no injection).
func (l LearningsConfig) EffectiveInjectCount() int {
	if l.InjectCount != nil {
		return *l.InjectCount
	}
	return 5
}

// EffectiveTTLHours returns the configured TTL in hours.
// 0 means unlimited (no expiry). The template default is 72.
func (l LearningsConfig) EffectiveTTLHours() int {
	return l.TTLHours
}

// AdmissionControl controls concurrency limits for verify/repair/rollout phases.
type AdmissionControl struct {
	MaxConcurrentVerify  int `yaml:"max_concurrent_verify"`
	MaxConcurrentRepair  int `yaml:"max_concurrent_repair"`
	MaxConcurrentRollout int `yaml:"max_concurrent_rollout"`
}

// EffectiveMaxConcurrentVerify returns the configured limit or 2 as default.
// 0 means use default.
func (a AdmissionControl) EffectiveMaxConcurrentVerify() int {
	if a.MaxConcurrentVerify > 0 {
		return a.MaxConcurrentVerify
	}
	return 2
}

// EffectiveMaxConcurrentRepair returns the configured limit or 1 as default.
// 0 means use default.
func (a AdmissionControl) EffectiveMaxConcurrentRepair() int {
	if a.MaxConcurrentRepair > 0 {
		return a.MaxConcurrentRepair
	}
	return 1
}

// EffectiveMaxConcurrentRollout returns the configured limit or 1 as default.
// 0 means use default.
func (a AdmissionControl) EffectiveMaxConcurrentRollout() int {
	if a.MaxConcurrentRollout > 0 {
		return a.MaxConcurrentRollout
	}
	return 1
}

// Fallback controls degraded-mode behavior when workers experience consecutive failures.
type Fallback struct {
	Enabled                     bool `yaml:"enabled"`
	ConsecutiveFailureThreshold int  `yaml:"consecutive_failure_threshold"`
	RecoveryCheckIntervalSec    int  `yaml:"recovery_check_interval_sec"`
	MinHealthyDurationSec       int  `yaml:"min_healthy_duration_sec"`
}

// EffectiveEnabled returns the configured enabled flag (default false).
func (f Fallback) EffectiveEnabled() bool {
	return f.Enabled
}

// EffectiveConsecutiveFailureThreshold returns the configured threshold or 5 as default.
// 0 means use default.
func (f Fallback) EffectiveConsecutiveFailureThreshold() int {
	if f.ConsecutiveFailureThreshold > 0 {
		return f.ConsecutiveFailureThreshold
	}
	return 5
}

// EffectiveRecoveryCheckIntervalSec returns the configured interval or 60 as default.
// 0 means use default.
func (f Fallback) EffectiveRecoveryCheckIntervalSec() int {
	if f.RecoveryCheckIntervalSec > 0 {
		return f.RecoveryCheckIntervalSec
	}
	return 60
}

// EffectiveMinHealthyDurationSec returns the configured duration or 120 as default.
// 0 means use default.
func (f Fallback) EffectiveMinHealthyDurationSec() int {
	if f.MinHealthyDurationSec > 0 {
		return f.MinHealthyDurationSec
	}
	return 120
}

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
// duration. Empty / unparseable input falls back to 10 minutes; an explicit
// "0" / "0s" returns 0 (disabled).
func (w WorktreeConfig) EffectiveStallCleanupAfter() time.Duration {
	const defaultStallCleanupAfter = 10 * time.Minute
	if w.StallCleanupAfter == "" {
		return defaultStallCleanupAfter
	}
	d, err := time.ParseDuration(w.StallCleanupAfter)
	if err != nil {
		return defaultStallCleanupAfter
	}
	return d
}

// EffectiveFallbackMergeTimeoutMinutes returns the configured fallback merge
// timeout in minutes, or 60 as default. nil (unset) returns the default;
// explicit 0 disables the check.
func (w WorktreeConfig) EffectiveFallbackMergeTimeoutMinutes() int {
	if w.FallbackMergeTimeoutMinutes != nil {
		return *w.FallbackMergeTimeoutMinutes
	}
	return 60
}

// CommitPolicyConfig enforces safety checks before committing worker changes.
// Zero-valued config means no enforcement. Set fields explicitly via config.yaml
// to enable checks. Recommended template values: MaxFiles=30, RequireGitignore=true,
// MessagePattern="^.+" (non-empty message).
type CommitPolicyConfig struct {
	MaxFiles         *int   `yaml:"max_files"`         // max staged files per commit; nil=default(30), 0=unlimited
	RequireGitignore bool   `yaml:"require_gitignore"` // require .gitignore existence
	MessagePattern   string `yaml:"message_pattern"`   // regex for commit message validation; empty=no check
}

// EffectiveMaxFiles returns the configured max files or 30 as default.
// nil (unset) returns the default; explicit 0 returns 0 (unlimited).
func (c CommitPolicyConfig) EffectiveMaxFiles() int {
	if c.MaxFiles != nil {
		return *c.MaxFiles
	}
	return 30
}

// WorktreeGCConfig controls periodic garbage collection of old worktrees.
type WorktreeGCConfig struct {
	Enabled      bool `yaml:"enabled"`
	TTLHours     *int `yaml:"ttl_hours"`
	MaxWorktrees *int `yaml:"max_worktrees"`
}

// EffectiveBaseBranch returns the configured base branch or "main" as default.
func (w WorktreeConfig) EffectiveBaseBranch() string {
	if w.BaseBranch != "" {
		return w.BaseBranch
	}
	return "main"
}

// EffectivePathPrefix returns the configured path prefix or ".maestro/worktrees" as default.
func (w WorktreeConfig) EffectivePathPrefix() string {
	if w.PathPrefix != "" {
		return w.PathPrefix
	}
	return ".maestro/worktrees"
}

// EffectiveMergeStrategy returns the configured merge strategy or "ort" as default.
func (w WorktreeConfig) EffectiveMergeStrategy() string {
	if w.MergeStrategy != "" {
		return w.MergeStrategy
	}
	return "ort"
}

// EffectiveGitTimeout returns the configured git command timeout or 120 seconds as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (w WorktreeConfig) EffectiveGitTimeout() int {
	if w.GitTimeoutSec != nil {
		return *w.GitTimeoutSec
	}
	return 120
}

// EffectiveStallTimeoutMinutes returns the configured worktree stall timeout
// in minutes. nil (unset) returns the default of 30; explicit 0 returns 0
// (disabled).
func (w WorktreeConfig) EffectiveStallTimeoutMinutes() int {
	if w.StallTimeoutMinutes != nil {
		return *w.StallTimeoutMinutes
	}
	return 30
}

// EffectiveTTLHours returns the configured TTL or 24 hours as default.
// nil (unset) returns the default; explicit 0 returns 0 (keep forever).
func (w WorktreeGCConfig) EffectiveTTLHours() int {
	if w.TTLHours != nil {
		return *w.TTLHours
	}
	return 24
}

// EffectiveMaxWorktrees returns the configured max worktrees or 32 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (w WorktreeGCConfig) EffectiveMaxWorktrees() int {
	if w.MaxWorktrees != nil {
		return *w.MaxWorktrees
	}
	return 32
}

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

	// watcher fields: reject negative values; zero means "use runtime default"
	if c.Watcher.BusyCheckInterval < 0 {
		errs = append(errs, fmt.Errorf("watcher.busy_check_interval: must be >= 0"))
	}
	if c.Watcher.BusyCheckMaxRetries < 0 {
		errs = append(errs, fmt.Errorf("watcher.busy_check_max_retries: must be >= 0"))
	}
	if c.Watcher.IdleStableSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.idle_stable_sec: must be >= 0"))
	}
	if c.Watcher.ScanIntervalSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.scan_interval_sec: must be >= 0"))
	}
	if c.Watcher.DispatchLeaseSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.dispatch_lease_sec: must be >= 0"))
	}
	if c.Watcher.MaxInProgressMin != nil && *c.Watcher.MaxInProgressMin < 0 {
		errs = append(errs, fmt.Errorf("watcher.max_in_progress_min: must be >= 0"))
	}
	if c.Watcher.DebounceSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.debounce_sec: must be >= 0"))
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
	if c.Watcher.WaitReadyMaxRetries < 0 {
		errs = append(errs, fmt.Errorf("watcher.wait_ready_max_retries: must be >= 0"))
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
	if c.Limits.MaxPendingCommands < 0 {
		errs = append(errs, fmt.Errorf("limits.max_pending_commands: must be >= 0"))
	}
	if c.Limits.MaxPendingTasksPerWorker < 0 {
		errs = append(errs, fmt.Errorf("limits.max_pending_tasks_per_worker: must be >= 0"))
	}
	if c.Limits.MaxEntryContentBytes < 0 {
		errs = append(errs, fmt.Errorf("limits.max_entry_content_bytes: must be >= 0"))
	}

	// shutdown_timeout_sec
	if c.ShutdownTimeoutSec < 0 {
		errs = append(errs, fmt.Errorf("shutdown_timeout_sec: must be >= 0"))
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
		if c.Worktree.GC.MaxWorktrees != nil && *c.Worktree.GC.MaxWorktrees <= 0 {
			errs = append(errs, fmt.Errorf("worktree.gc.max_worktrees: must be > 0 when gc is enabled"))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
