// Package model defines the data structures for Maestro's configuration, state, and queue entries.
package model

import (
	"errors"
	"fmt"
	"strings"

	"github.com/msageha/maestro_v2/internal/validate"
)

// DefaultMaxYAMLFileBytes is the default maximum size for YAML file reads (5MB).
const DefaultMaxYAMLFileBytes = 5 * 1024 * 1024

// MinWorkers is the minimum allowed worker count.
const MinWorkers = 1

// MaxWorkers is the maximum allowed worker count.
const MaxWorkers = 8

type Config struct {
	Project        ProjectConfig        `yaml:"project"`
	Maestro        MaestroConfig        `yaml:"maestro"`
	Agents         AgentsConfig         `yaml:"agents"`
	Continuous     ContinuousConfig     `yaml:"continuous"`
	Watcher        WatcherConfig        `yaml:"watcher"`
	Retry          RetryConfig          `yaml:"retry"`
	Queue          QueueConfig          `yaml:"queue"`
	Limits         LimitsConfig         `yaml:"limits"`
	Daemon         DaemonConfig         `yaml:"daemon"`
	Logging        LoggingConfig        `yaml:"logging"`
	QualityGates   QualityGatesConfig   `yaml:"quality_gates"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
	Learnings      LearningsConfig      `yaml:"learnings"`
	Verification   VerificationConfig   `yaml:"verification"`
	Worktree       WorktreeConfig       `yaml:"worktree"`
	Skills         SkillsConfig             `yaml:"skills"`
	Personas       map[string]PersonaConfig `yaml:"personas,omitempty"`
}

// PersonaConfig defines a persona that can be assigned to agents.
// Either Prompt or File must be set. If File is set, the file content
// is read from the embedded template FS at dispatch time.
// File read failure falls back to the Prompt field.
type PersonaConfig struct {
	Description string `yaml:"description"`
	Prompt      string `yaml:"prompt"`
	File        string `yaml:"file,omitempty"`
}

// SkillsConfig controls the skill reference feature for tasks.
type SkillsConfig struct {
	Enabled          bool              `yaml:"enabled"`
	MaxRefsPerTask   int               `yaml:"max_refs_per_task"`
	MaxBodyChars     int               `yaml:"max_body_chars"`
	MissingRefPolicy string            `yaml:"missing_ref_policy"`
	AutoCollect      AutoCollectConfig `yaml:"auto_collect"`
}

// EffectiveMaxRefsPerTask returns the configured limit or 3 as default.
func (s SkillsConfig) EffectiveMaxRefsPerTask() int {
	if s.MaxRefsPerTask > 0 {
		return s.MaxRefsPerTask
	}
	return 3
}

// EffectiveMaxBodyChars returns the configured limit or 2000 as default.
func (s SkillsConfig) EffectiveMaxBodyChars() int {
	if s.MaxBodyChars > 0 {
		return s.MaxBodyChars
	}
	return 2000
}

// EffectiveMissingRefPolicy returns the configured policy or "warn" as default.
func (s SkillsConfig) EffectiveMissingRefPolicy() string {
	if s.MissingRefPolicy != "" {
		return s.MissingRefPolicy
	}
	return "warn"
}

// AutoCollectConfig controls automatic skill collection from learnings.
type AutoCollectConfig struct {
	Enabled        bool `yaml:"enabled"`
	MinOccurrences int  `yaml:"min_occurrences"`
	MinCommands    int  `yaml:"min_commands"`
}

// EffectiveMinOccurrences returns the configured minimum or 3 as default.
func (a AutoCollectConfig) EffectiveMinOccurrences() int {
	if a.MinOccurrences > 0 {
		return a.MinOccurrences
	}
	return 3
}

// EffectiveMinCommands returns the configured minimum or 2 as default.
func (a AutoCollectConfig) EffectiveMinCommands() int {
	if a.MinCommands > 0 {
		return a.MinCommands
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
	ID    string `yaml:"id"`
	Model string `yaml:"model"`
}

type WorkerConfig struct {
	Count        int               `yaml:"count"`
	DefaultModel string            `yaml:"default_model"`
	Models       map[string]string `yaml:"models,omitempty"`
	Boost        bool              `yaml:"boost"`
}

type ContinuousConfig struct {
	Enabled        bool `yaml:"enabled"`
	MaxIterations  int  `yaml:"max_iterations"`
	PauseOnFailure bool `yaml:"pause_on_failure"`
}

type WatcherConfig struct {
	DebounceSec         float64 `yaml:"debounce_sec"`
	ScanIntervalSec     int     `yaml:"scan_interval_sec"`
	DispatchLeaseSec    int     `yaml:"dispatch_lease_sec"`
	MaxInProgressMin    int     `yaml:"max_in_progress_min"`
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
	ClearRetryBackoffMs    int `yaml:"clear_retry_backoff_ms"`    // Base backoff between attempts; doubles each retry (default 500ms)
}

// EffectiveMaxInProgressMin returns the configured max in-progress timeout or 60 as default.
func (w WatcherConfig) EffectiveMaxInProgressMin() int {
	if w.MaxInProgressMin > 0 {
		return w.MaxInProgressMin
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
	MaxPendingCommands       int `yaml:"max_pending_commands"`
	MaxPendingTasksPerWorker int `yaml:"max_pending_tasks_per_worker"`
	MaxEntryContentBytes     int `yaml:"max_entry_content_bytes"`
	MaxYAMLFileBytes         int `yaml:"max_yaml_file_bytes"`
	MaxDeadLetterArchiveFiles int `yaml:"max_dead_letter_archive_files"`
	MaxQuarantineFiles        int `yaml:"max_quarantine_files"`
}

// EffectiveMaxDeadLetterArchiveFiles returns the configured limit or 100 as default.
func (l LimitsConfig) EffectiveMaxDeadLetterArchiveFiles() int {
	if l.MaxDeadLetterArchiveFiles > 0 {
		return l.MaxDeadLetterArchiveFiles
	}
	return 100
}

// EffectiveMaxQuarantineFiles returns the configured limit or 100 as default.
func (l LimitsConfig) EffectiveMaxQuarantineFiles() int {
	if l.MaxQuarantineFiles > 0 {
		return l.MaxQuarantineFiles
	}
	return 100
}

type DaemonConfig struct {
	ShutdownTimeoutSec int `yaml:"shutdown_timeout_sec"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
}

type QualityGatesConfig struct {
	Enabled       bool                       `yaml:"enabled"`
	SkipGates     bool                       `yaml:"skip_gates"`      // 緊急モードフラグ
	Thresholds    QualityGateThresholds      `yaml:"thresholds"`
	Enforcement   QualityGateEnforcement     `yaml:"enforcement"`
}

type QualityGateThresholds struct {
	MaxTaskFailureRate      float64 `yaml:"max_task_failure_rate"`       // タスク失敗率閾値（0.0-1.0）
	MinTaskSuccessRate      float64 `yaml:"min_task_success_rate"`       // タスク成功率閾値（0.0-1.0）
	MaxConsecutiveFailures  int     `yaml:"max_consecutive_failures"`    // 連続失敗数の上限
	MaxTaskDurationSec      int     `yaml:"max_task_duration_sec"`       // タスク実行時間の上限（秒）
	MaxPendingTasks         int     `yaml:"max_pending_tasks"`           // ペンディングタスク数の上限
}

type QualityGateEnforcement struct {
	PreTaskCheck   bool   `yaml:"pre_task_check"`    // タスク実行前チェック
	PostTaskCheck  bool   `yaml:"post_task_check"`   // タスク実行後チェック
	FailureAction  string `yaml:"failure_action"`    // 失敗時の動作: "warn", "block"
	LogViolations  bool   `yaml:"log_violations"`    // 違反をログに記録
}

// CircuitBreakerConfig controls the command-level circuit breaker that auto-stops
// commands after consecutive task failures.
type CircuitBreakerConfig struct {
	Enabled                bool `yaml:"enabled"`                   // opt-in, default: false
	MaxConsecutiveFailures int  `yaml:"max_consecutive_failures"`  // default: 3
	ProgressTimeoutMinutes int  `yaml:"progress_timeout_minutes"`  // default: 30, 0=disabled
}

// EffectiveMaxConsecutiveFailures returns the configured threshold or 3 as default.
func (c CircuitBreakerConfig) EffectiveMaxConsecutiveFailures() int {
	if c.MaxConsecutiveFailures > 0 {
		return c.MaxConsecutiveFailures
	}
	return 3
}

// EffectiveProgressTimeoutMinutes returns the configured timeout.
// Returns 0 if set to 0 (disabled). The template default is 30.
func (c CircuitBreakerConfig) EffectiveProgressTimeoutMinutes() int {
	return c.ProgressTimeoutMinutes
}

// LearningsConfig controls the learning accumulation feature.
type LearningsConfig struct {
	Enabled          bool `yaml:"enabled"`            // opt-in, default: false
	MaxEntries       int  `yaml:"max_entries"`        // default: 100
	MaxContentLength int  `yaml:"max_content_length"` // default: 500
	InjectCount      int  `yaml:"inject_count"`       // top-K learnings injected per task dispatch, default: 5
	TTLHours         int  `yaml:"ttl_hours"`          // learning expiry in hours, default: 72, 0=unlimited
}

// EffectiveMaxEntries returns the configured limit or 100 as default.
func (l LearningsConfig) EffectiveMaxEntries() int {
	if l.MaxEntries > 0 {
		return l.MaxEntries
	}
	return 100
}

// EffectiveMaxContentLength returns the configured limit or 500 as default.
func (l LearningsConfig) EffectiveMaxContentLength() int {
	if l.MaxContentLength > 0 {
		return l.MaxContentLength
	}
	return 500
}

// EffectiveInjectCount returns the configured inject count or 5 as default.
func (l LearningsConfig) EffectiveInjectCount() int {
	if l.InjectCount > 0 {
		return l.InjectCount
	}
	return 5
}

// EffectiveTTLHours returns the configured TTL in hours.
// 0 means unlimited (no expiry). The template default is 72.
func (l LearningsConfig) EffectiveTTLHours() int {
	return l.TTLHours
}

// VerificationConfig controls verification commands run by Workers.
// When enabled is true and commands are configured, Workers are guided to run
// basic verification before reporting task completion. Full verification is
// used in dedicated verification phase tasks.
type VerificationConfig struct {
	Enabled        bool   `yaml:"enabled"`         // opt-in, default: false
	BasicCommand   string `yaml:"basic_command"`   // e.g. "go vet ./..."
	FullCommand    string `yaml:"full_command"`     // e.g. "go test ./..."
	TimeoutSeconds int    `yaml:"timeout_seconds"` // default: 300
	MaxRetries     int    `yaml:"max_retries"`      // 0=no retry, template default: 1
}

// EffectiveTimeoutSeconds returns the configured timeout or 300 as default.
func (v VerificationConfig) EffectiveTimeoutSeconds() int {
	if v.TimeoutSeconds > 0 {
		return v.TimeoutSeconds
	}
	return 300
}

// EffectiveMaxRetries returns the configured max retries.
// 0 means no retry. The template default is 1.
func (v VerificationConfig) EffectiveMaxRetries() int {
	return v.MaxRetries
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
	GitTimeoutSec    int                `yaml:"git_timeout_sec"`
	GC               WorktreeGCConfig   `yaml:"gc"`
	CommitPolicy     CommitPolicyConfig `yaml:"commit_policy"`
}

// CommitPolicyConfig enforces safety checks before committing worker changes.
// Zero-valued config means no enforcement. Set fields explicitly via config.yaml
// to enable checks. Recommended template values: MaxFiles=30, RequireGitignore=true,
// MessagePattern="^\\[maestro\\]\\s".
type CommitPolicyConfig struct {
	MaxFiles         int    `yaml:"max_files"`         // max staged files per commit; 0=unlimited
	RequireGitignore bool   `yaml:"require_gitignore"` // require .gitignore existence
	MessagePattern   string `yaml:"message_pattern"`   // regex for commit message validation; empty=no check
}

// EffectiveMaxFiles returns the configured max files or 30 as default.
func (c CommitPolicyConfig) EffectiveMaxFiles() int {
	if c.MaxFiles > 0 {
		return c.MaxFiles
	}
	return 30
}

// WorktreeGCConfig controls periodic garbage collection of old worktrees.
type WorktreeGCConfig struct {
	Enabled      bool `yaml:"enabled"`
	TTLHours     int  `yaml:"ttl_hours"`
	MaxWorktrees int  `yaml:"max_worktrees"`
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
func (w WorktreeConfig) EffectiveGitTimeout() int {
	if w.GitTimeoutSec > 0 {
		return w.GitTimeoutSec
	}
	return 120
}

// EffectiveTTLHours returns the configured TTL or 24 hours as default.
func (w WorktreeGCConfig) EffectiveTTLHours() int {
	if w.TTLHours > 0 {
		return w.TTLHours
	}
	return 24
}

// EffectiveMaxWorktrees returns the configured max worktrees or 32 as default.
func (w WorktreeGCConfig) EffectiveMaxWorktrees() int {
	if w.MaxWorktrees > 0 {
		return w.MaxWorktrees
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

	// watcher timeout fields (negative values invalid)
	if c.Watcher.ScanIntervalSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.scan_interval_sec: must be >= 0"))
	}
	if c.Watcher.DispatchLeaseSec < 0 {
		errs = append(errs, fmt.Errorf("watcher.dispatch_lease_sec: must be >= 0"))
	}
	if c.Watcher.MaxInProgressMin < 0 {
		errs = append(errs, fmt.Errorf("watcher.max_in_progress_min: must be >= 0"))
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

	// daemon.shutdown_timeout_sec
	if c.Daemon.ShutdownTimeoutSec < 0 {
		errs = append(errs, fmt.Errorf("daemon.shutdown_timeout_sec: must be >= 0"))
	}

	// circuit_breaker
	if c.CircuitBreaker.MaxConsecutiveFailures < 0 {
		errs = append(errs, fmt.Errorf("circuit_breaker.max_consecutive_failures: must be >= 0"))
	}
	if c.CircuitBreaker.ProgressTimeoutMinutes < 0 {
		errs = append(errs, fmt.Errorf("circuit_breaker.progress_timeout_minutes: must be >= 0"))
	}

	// verification
	if c.Verification.TimeoutSeconds < 0 {
		errs = append(errs, fmt.Errorf("verification.timeout_seconds: must be >= 0"))
	}
	if c.Verification.MaxRetries < 0 {
		errs = append(errs, fmt.Errorf("verification.max_retries: must be >= 0"))
	}

	// learnings
	if c.Learnings.MaxEntries < 0 {
		errs = append(errs, fmt.Errorf("learnings.max_entries: must be >= 0"))
	}
	if c.Learnings.MaxContentLength < 0 {
		errs = append(errs, fmt.Errorf("learnings.max_content_length: must be >= 0"))
	}
	if c.Learnings.InjectCount < 0 {
		errs = append(errs, fmt.Errorf("learnings.inject_count: must be >= 0"))
	}

	// skills
	if c.Skills.MaxRefsPerTask < 0 {
		errs = append(errs, fmt.Errorf("skills.max_refs_per_task: must be >= 0"))
	}
	if c.Skills.MaxBodyChars < 0 {
		errs = append(errs, fmt.Errorf("skills.max_body_chars: must be >= 0"))
	}
	if p := c.Skills.MissingRefPolicy; p != "" && p != "warn" && p != "error" {
		errs = append(errs, fmt.Errorf("skills.missing_ref_policy: must be \"warn\" or \"error\""))
	}
	if c.Skills.AutoCollect.MinOccurrences < 0 {
		errs = append(errs, fmt.Errorf("skills.auto_collect.min_occurrences: must be >= 0"))
	}
	if c.Skills.AutoCollect.MinCommands < 0 {
		errs = append(errs, fmt.Errorf("skills.auto_collect.min_commands: must be >= 0"))
	}

	// personas
	for name, p := range c.Personas {
		if err := validate.ValidateID(name); err != nil {
			errs = append(errs, fmt.Errorf("personas.%s: invalid persona name: %w", name, err))
		}
		if strings.TrimSpace(p.Prompt) == "" && strings.TrimSpace(p.File) == "" {
			errs = append(errs, fmt.Errorf("personas.%s: prompt or file must be set", name))
		}
	}

	// quality_gates
	if c.QualityGates.Thresholds.MaxTaskFailureRate < 0 || c.QualityGates.Thresholds.MaxTaskFailureRate > 1 {
		errs = append(errs, fmt.Errorf("quality_gates.thresholds.max_task_failure_rate: must be between 0.0 and 1.0"))
	}
	if c.QualityGates.Thresholds.MinTaskSuccessRate < 0 || c.QualityGates.Thresholds.MinTaskSuccessRate > 1 {
		errs = append(errs, fmt.Errorf("quality_gates.thresholds.min_task_success_rate: must be between 0.0 and 1.0"))
	}
	if fa := c.QualityGates.Enforcement.FailureAction; fa != "" && fa != "warn" && fa != "block" {
		errs = append(errs, fmt.Errorf("quality_gates.enforcement.failure_action: must be \"warn\" or \"block\""))
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}
