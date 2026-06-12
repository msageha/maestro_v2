// Package model defines the data structures for Maestro's configuration, state, and queue entries.
package model

import (
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
	MaxPriorityAgingSec          = 86400            // 24 hours
	MaxCommandDispatchRetries    = 100
	MaxTaskDispatchRetries       = 100
)

// Default values for Effective*() methods.
// Checklist when adding a new Effective*() method:
//  1. Add a Default* constant here
//  2. Use effectiveValue(ptr, Default*) or effectiveNonZero(val, Default*) in the method
//  3. Add the corresponding resolvePtr call in NormalizeExperimentalConfig if applicable
//  4. Add test coverage for both nil/zero and configured values
const (
	// SkillsConfig
	DefaultMaxRefsPerTask   = 3
	DefaultMissingRefPolicy = "warn"

	// autoCollectConfig

	// MaestroConfig
	// DefaultAwaitingFillStallNotifyMinutes is the elapsed-since-awaiting_fill
	// threshold after which the awaiting-fill watchdog re-emits an
	// awaiting_fill_stall signal to the Planner.
	//
	// Autonomous LLM orchestration covers research/documentation/refactor
	// flows where a Planner LLM may legitimately deliberate for many minutes
	// before submitting tasks (long context, expensive tool calls, multi-pass
	// reasoning). A 5-minute threshold proved trigger-happy: in e2e logs the
	// stall signal repeatedly fired during the few seconds between dry-run
	// completion and submit, producing a false-positive "Planner busy" prompt
	// that itself disrupted the in-flight submit. 30 minutes is wide enough
	// that genuine Planner inactivity still surfaces well inside the
	// 3-hour fill deadline while leaving comfortable headroom for normal
	// LLM deliberation.
	DefaultAwaitingFillStallNotifyMinutes = 30

	// WatcherConfig
	DefaultMaxInProgressMin = 60

	// LimitsConfig
	DefaultMaxDeadLetterArchiveFiles = 100
	DefaultMaxQuarantineFiles        = 100

	// CircuitBreakerConfig
	DefaultCBMaxConsecutiveFailures = 3
	DefaultProgressTimeoutMinutes   = 30
	DefaultCBHalfOpenDelaySec       = 60

	// LearningsConfig
	DefaultLearningsMaxEntries       = 100
	DefaultLearningsMaxContentLength = 500
	DefaultLearningsInjectCount      = 5

	// AdmissionControl
	DefaultMaxConcurrentVerify = 2
	DefaultMaxConcurrentRepair = 1

	// VerifyDaemonConfig
	// DefaultVerifyStallThresholdSec is the verify_pending stall window after
	// which the R9 reconciler transitions a task to repair_pending so a fresh
	// verification attempt can be planned (default 10 minutes).
	DefaultVerifyStallThresholdSec = 600

	// DefaultPausedForReplanDeadletterSec is how long a task may sit in
	// paused_for_replan before R10 escalates it to a terminal failed state.
	// Once escalated, the phase containing the task transitions to
	// PhaseStatusFailed via the dependency resolver, and the publish gate is
	// no longer blocked by Planner inaction.
	//
	// Default 1 hour (3600 sec). The previous 10-minute default was set when
	// the assumption was "a Planner re-plan response takes a few minutes" —
	// that assumption no longer holds. Autonomous LLM orchestration is the
	// primary use case: research / documentation / cross-domain re-plans
	// can legitimately run beyond 30 minutes before producing a sane
	// add_retry_task, especially when a Planner LLM needs to revisit
	// upstream context. Aggressive deadletter escalation manifests as a
	// "command terminated under the user's nose" failure mode that the user
	// flagged as a 自律 LLM orchestration anti-pattern. The new default
	// trades a slower failure-flow for genuine Planner deliberation windows.
	// 0 disables R10 entirely (acceptable for trusted Planners; the
	// circuit-breaker still bounds total command runtime via
	// progress_timeout_minutes).
	DefaultPausedForReplanDeadletterSec = 3600

	// (Removed) Fallback defaults — degraded-mode worker blacklisting was
	// retired; the daemon no longer reads fallback.* config.

	// WorktreeConfig
	DefaultBaseBranch                  = "main"
	DefaultPathPrefix                  = ".maestro/worktrees"
	DefaultMergeStrategy               = "ort"
	DefaultGitTimeoutSec               = 120
	DefaultStallTimeoutMinutes         = 30
	DefaultFallbackMergeTimeoutMinutes = 60
	DefaultStallCleanupAfter           = 10 * time.Minute

	// ShutdownConfig
	DefaultShutdownTimeoutSec = 30

	// (Removed) CommitPolicyConfig defaults — gates retired (see config_types.go).

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
	DefaultTraceDataRequirement = 50

	// A/B candidate selection (docs/design/ab_candidate_selection.md)
	DefaultABMinBloomLevel       = 4
	DefaultABSelectionTimeoutSec = 1800

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
	DefaultCrossAgentReview = false

	// RetryConfig — signal inline retry
	DefaultSignalInlineRetries       = 2
	DefaultSignalInlineRetryDelaySec = 3
	DefaultSignalDeliveryTimeoutSec  = 15

	// RetryConfig — result notification inline retry
	DefaultResultNotifyInlineRetries       = 2
	DefaultResultNotifyInlineRetryDelaySec = 3
	DefaultResultNotifyDeliveryTimeoutSec  = 15

	// RetryConfig — command dispatch inline retry
	DefaultCommandDispatchInlineRetries       = 2
	DefaultCommandDispatchInlineRetryDelaySec = 2

	// RetryConfig — task dispatch inline retry
	DefaultTaskDispatchInlineRetries       = 5
	DefaultTaskDispatchInlineRetryDelaySec = 1
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
	"claude-opus-4-7":           {},
	"claude-haiku-4-5-20251001": {},
	// Claude full model IDs with the 1M context variant suffix
	"claude-sonnet-4-6[1m]":         {},
	"claude-opus-4-6[1m]":           {},
	"claude-opus-4-7[1m]":           {},
	"claude-haiku-4-5-20251001[1m]": {},
}

// validModelNameRe validates the format of model name identifiers.
// Names must start with a letter or digit and contain only letters, digits,
// hyphens, dots, and underscores. An optional bracketed alphanumeric suffix
// (e.g. "[1m]") is allowed at the end to denote model variants such as the
// 1M-context window flavor.
var validModelNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*(\[[a-zA-Z0-9]+\])?$`)

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

// resolvePtr sets *ptr to &defaultVal when *ptr is nil, ensuring the pointer is always non-nil after normalization.
func resolvePtr[T any](ptr **T, defaultVal T) {
	if *ptr == nil {
		v := defaultVal
		*ptr = &v
	}
}
