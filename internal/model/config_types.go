package model

import "time"

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
	HalfOpenDelaySec       *int `yaml:"half_open_delay_sec"`      // default: 60, delay before open→half-open transition
}

func (c CircuitBreakerConfig) EffectiveMaxConsecutiveFailures() int { return effectiveValue(c.MaxConsecutiveFailures, DefaultCBMaxConsecutiveFailures) }
func (c CircuitBreakerConfig) EffectiveProgressTimeoutMinutes() int { return effectiveValue(c.ProgressTimeoutMinutes, DefaultProgressTimeoutMinutes) }

// EffectiveHalfOpenDelaySec returns the configured half-open delay or the default (60s).
func (c CircuitBreakerConfig) EffectiveHalfOpenDelaySec() int { return effectiveValue(c.HalfOpenDelaySec, DefaultCBHalfOpenDelaySec) }

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
