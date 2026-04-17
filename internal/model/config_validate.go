package model

import (
	"errors"
	"fmt"
	"math"
)

// Validate checks all Config fields for consistency after yaml.Unmarshal.
// Returns a joined error containing all validation failures with field paths.
func (c Config) Validate() error {
	var errs []error

	c.validateProject(&errs)
	c.validateAgents(&errs)
	c.validateWatcher(&errs)
	c.validateQueue(&errs)
	c.validateContinuous(&errs)
	c.validateRetry(&errs)
	c.validateLimits(&errs)
	c.validateShutdown(&errs)
	c.validateCircuitBreaker(&errs)
	c.validateLearnings(&errs)
	c.validateSkills(&errs)
	c.validateAdmissionControl(&errs)
	c.validateFallback(&errs)
	c.validateReview(&errs)
	c.validateRollout(&errs)
	c.validateJudge(&errs)
	c.validateQualityGates(&errs)
	c.validateWorktree(&errs)
	c.validateCrossFieldConstraints(&errs)
	c.validateFloatFields(&errs)

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

func (c Config) validateProject(errs *[]error) {
	if c.Project.Name == "" {
		*errs = append(*errs, fmt.Errorf("project.name: must not be empty"))
	}
	if c.Maestro.Version == "" {
		*errs = append(*errs, fmt.Errorf("maestro.version: must not be empty"))
	}
}

func (c Config) validateAgents(errs *[]error) {
	if c.Agents.Workers.Count < MinWorkers || c.Agents.Workers.Count > MaxWorkers {
		*errs = append(*errs, fmt.Errorf("agents.workers.count: must be between %d and %d", MinWorkers, MaxWorkers))
	}
	if !isValidModelName(c.Agents.Orchestrator.Model) {
		*errs = append(*errs, fmt.Errorf("agents.orchestrator.model: invalid model name %q", c.Agents.Orchestrator.Model))
	}
	if !isValidModelName(c.Agents.Planner.Model) {
		*errs = append(*errs, fmt.Errorf("agents.planner.model: invalid model name %q", c.Agents.Planner.Model))
	}
	if !isValidModelName(c.Agents.Workers.DefaultModel) {
		*errs = append(*errs, fmt.Errorf("agents.workers.default_model: invalid model name %q", c.Agents.Workers.DefaultModel))
	}
	for workerID, m := range c.Agents.Workers.Models {
		if !isValidModelName(m) {
			*errs = append(*errs, fmt.Errorf("agents.workers.models.%s: invalid model name %q", workerID, m))
		}
	}
}

func (c Config) validateWatcher(errs *[]error) {
	validateNonNegInt(errs, "watcher.busy_check_interval", c.Watcher.BusyCheckInterval)
	validateIntRange(errs, "watcher.busy_check_max_retries", c.Watcher.BusyCheckMaxRetries, MaxBusyCheckMaxRetries)
	validateNonNegInt(errs, "watcher.idle_stable_sec", c.Watcher.IdleStableSec)
	validateNonNegInt(errs, "watcher.scan_interval_sec", c.Watcher.ScanIntervalSec)
	validateIntRange(errs, "watcher.dispatch_lease_sec", c.Watcher.DispatchLeaseSec, MaxDispatchLeaseSec)
	if c.Watcher.MaxInProgressMin != nil && (*c.Watcher.MaxInProgressMin < 0 || *c.Watcher.MaxInProgressMin > MaxMaxInProgressMin) {
		*errs = append(*errs, fmt.Errorf("watcher.max_in_progress_min: must be between 0 and %d", MaxMaxInProgressMin))
	}
	if c.Watcher.DebounceSec < 0 || math.IsNaN(c.Watcher.DebounceSec) || math.IsInf(c.Watcher.DebounceSec, 0) {
		*errs = append(*errs, fmt.Errorf("watcher.debounce_sec: must be a finite value >= 0"))
	}
	validateNonNegInt(errs, "watcher.cooldown_after_clear", c.Watcher.CooldownAfterClear)
	validateNonNegInt(errs, "watcher.notify_lease_sec", c.Watcher.NotifyLeaseSec)
	validateNonNegInt(errs, "watcher.wait_ready_interval_sec", c.Watcher.WaitReadyIntervalSec)
	validateIntRange(errs, "watcher.wait_ready_max_retries", c.Watcher.WaitReadyMaxRetries, MaxWaitReadyMaxRetries)
	validateNonNegInt(errs, "watcher.clear_confirm_timeout_sec", c.Watcher.ClearConfirmTimeoutSec)
	validateNonNegInt(errs, "watcher.clear_confirm_poll_ms", c.Watcher.ClearConfirmPollMs)
	validateNonNegInt(errs, "watcher.clear_max_attempts", c.Watcher.ClearMaxAttempts)
	validateNonNegInt(errs, "watcher.clear_retry_backoff_ms", c.Watcher.ClearRetryBackoffMs)
	validateNonNegInt(errs, "watcher.clear_second_enter_delay_ms", c.Watcher.ClearSecondEnterDelayMs)
}

func (c Config) validateQueue(errs *[]error) {
	if c.Queue.PriorityAgingSec < 0 || c.Queue.PriorityAgingSec > MaxPriorityAgingSec {
		*errs = append(*errs, fmt.Errorf("queue.priority_aging_sec: must be between 0 and %d", MaxPriorityAgingSec))
	}
}

func (c Config) validateContinuous(errs *[]error) {
	if c.Continuous.MaxIterations < 0 {
		*errs = append(*errs, fmt.Errorf("continuous.max_iterations: must be >= 0"))
	}
	if c.Continuous.Enabled && c.Continuous.MaxConsecutiveFailures < 0 {
		*errs = append(*errs, fmt.Errorf("continuous.max_consecutive_failures: must be >= 0 when continuous is enabled"))
	}
}

func (c Config) validateRetry(errs *[]error) {
	if c.Retry.CommandDispatch < 0 || c.Retry.CommandDispatch > MaxCommandDispatchRetries {
		*errs = append(*errs, fmt.Errorf("retry.command_dispatch: must be between 0 and %d", MaxCommandDispatchRetries))
	}
	if c.Retry.TaskDispatch < 0 || c.Retry.TaskDispatch > MaxTaskDispatchRetries {
		*errs = append(*errs, fmt.Errorf("retry.task_dispatch: must be between 0 and %d", MaxTaskDispatchRetries))
	}
	if c.Retry.OrchestratorNotificationDispatch < 0 {
		*errs = append(*errs, fmt.Errorf("retry.orchestrator_notification_dispatch: must be >= 0"))
	}
	if c.Retry.TaskExecution.MaxRetries < 0 {
		*errs = append(*errs, fmt.Errorf("retry.task_execution.max_retries: must be >= 0"))
	}
	if c.Retry.TaskExecution.CooldownSec < 0 {
		*errs = append(*errs, fmt.Errorf("retry.task_execution.cooldown_sec: must be >= 0"))
	}
}

func (c Config) validateLimits(errs *[]error) {
	if c.Limits.MaxPendingCommands < 0 || c.Limits.MaxPendingCommands > MaxMaxPendingCommands {
		*errs = append(*errs, fmt.Errorf("limits.max_pending_commands: must be between 0 and %d", MaxMaxPendingCommands))
	}
	if c.Limits.MaxPendingTasksPerWorker < 0 || c.Limits.MaxPendingTasksPerWorker > MaxMaxPendingTasksPerWorker {
		*errs = append(*errs, fmt.Errorf("limits.max_pending_tasks_per_worker: must be between 0 and %d", MaxMaxPendingTasksPerWorker))
	}
	if c.Limits.MaxDeadLetterArchiveFiles != nil && (*c.Limits.MaxDeadLetterArchiveFiles < 0 || *c.Limits.MaxDeadLetterArchiveFiles > MaxMaxDeadLetterArchiveFiles) {
		*errs = append(*errs, fmt.Errorf("limits.max_dead_letter_archive_files: must be between 0 and %d", MaxMaxDeadLetterArchiveFiles))
	}
	if c.Limits.MaxQuarantineFiles != nil && (*c.Limits.MaxQuarantineFiles < 0 || *c.Limits.MaxQuarantineFiles > MaxMaxQuarantineFiles) {
		*errs = append(*errs, fmt.Errorf("limits.max_quarantine_files: must be between 0 and %d", MaxMaxQuarantineFiles))
	}
	if c.Limits.MaxEntryContentBytes < 0 {
		*errs = append(*errs, fmt.Errorf("limits.max_entry_content_bytes: must be >= 0"))
	}
	if c.Limits.MaxYAMLFileBytes < 0 || c.Limits.MaxYAMLFileBytes > MaxMaxYAMLFileBytes {
		*errs = append(*errs, fmt.Errorf("limits.max_yaml_file_bytes: must be between 0 and %d", MaxMaxYAMLFileBytes))
	}
}

func (c Config) validateShutdown(errs *[]error) {
	if c.ShutdownTimeoutSec < 0 || c.ShutdownTimeoutSec > MaxShutdownTimeoutSec {
		*errs = append(*errs, fmt.Errorf("shutdown_timeout_sec: must be between 0 and %d", MaxShutdownTimeoutSec))
	}
}

func (c Config) validateCircuitBreaker(errs *[]error) {
	if c.CircuitBreaker.MaxConsecutiveFailures != nil && *c.CircuitBreaker.MaxConsecutiveFailures < 0 {
		*errs = append(*errs, fmt.Errorf("circuit_breaker.max_consecutive_failures: must be >= 0"))
	}
	if c.CircuitBreaker.ProgressTimeoutMinutes != nil && *c.CircuitBreaker.ProgressTimeoutMinutes < 0 {
		*errs = append(*errs, fmt.Errorf("circuit_breaker.progress_timeout_minutes: must be >= 0"))
	}
}

func (c Config) validateLearnings(errs *[]error) {
	if c.Learnings.MaxEntries != nil && *c.Learnings.MaxEntries < 0 {
		*errs = append(*errs, fmt.Errorf("learnings.max_entries: must be >= 0"))
	}
	if c.Learnings.MaxContentLength != nil && *c.Learnings.MaxContentLength < 0 {
		*errs = append(*errs, fmt.Errorf("learnings.max_content_length: must be >= 0"))
	}
	if c.Learnings.InjectCount != nil && *c.Learnings.InjectCount < 0 {
		*errs = append(*errs, fmt.Errorf("learnings.inject_count: must be >= 0"))
	}
}

func (c Config) validateSkills(errs *[]error) {
	if c.Skills.MaxRefsPerTask != nil && *c.Skills.MaxRefsPerTask < 0 {
		*errs = append(*errs, fmt.Errorf("skills.max_refs_per_task: must be >= 0"))
	}
	if c.Skills.MaxBodyChars != nil && *c.Skills.MaxBodyChars < 0 {
		*errs = append(*errs, fmt.Errorf("skills.max_body_chars: must be >= 0"))
	}
	if p := c.Skills.MissingRefPolicy; p != "" && p != "warn" && p != "error" {
		*errs = append(*errs, fmt.Errorf("skills.missing_ref_policy: must be \"warn\" or \"error\""))
	}
	if c.Skills.AutoCollect.MinOccurrences != nil && *c.Skills.AutoCollect.MinOccurrences < 0 {
		*errs = append(*errs, fmt.Errorf("skills.auto_collect.min_occurrences: must be >= 0"))
	}
	if c.Skills.AutoCollect.MinCommands != nil && *c.Skills.AutoCollect.MinCommands < 0 {
		*errs = append(*errs, fmt.Errorf("skills.auto_collect.min_commands: must be >= 0"))
	}
}

func (c Config) validateAdmissionControl(errs *[]error) {
	if c.AdmissionControl.MaxConcurrentVerify < 0 {
		*errs = append(*errs, fmt.Errorf("admission_control.max_concurrent_verify: must be >= 0"))
	}
	if c.AdmissionControl.MaxConcurrentRepair < 0 {
		*errs = append(*errs, fmt.Errorf("admission_control.max_concurrent_repair: must be >= 0"))
	}
	if c.AdmissionControl.MaxConcurrentRollout < 0 {
		*errs = append(*errs, fmt.Errorf("admission_control.max_concurrent_rollout: must be >= 0"))
	}
}

func (c Config) validateFallback(errs *[]error) {
	if c.Fallback.ConsecutiveFailureThreshold < 0 {
		*errs = append(*errs, fmt.Errorf("fallback.consecutive_failure_threshold: must be >= 0"))
	}
	if c.Fallback.RecoveryCheckIntervalSec < 0 {
		*errs = append(*errs, fmt.Errorf("fallback.recovery_check_interval_sec: must be >= 0"))
	}
	if c.Fallback.MinHealthyDurationSec < 0 {
		*errs = append(*errs, fmt.Errorf("fallback.min_healthy_duration_sec: must be >= 0"))
	}
}

func (c Config) validateReview(errs *[]error) {
	if c.Review.MinBloomLevel != nil && *c.Review.MinBloomLevel < 0 {
		*errs = append(*errs, fmt.Errorf("review.min_bloom_level: must be >= 0"))
	}
	if c.Review.MaxConcurrentReviews != nil && *c.Review.MaxConcurrentReviews < 0 {
		*errs = append(*errs, fmt.Errorf("review.max_concurrent_reviews: must be >= 0"))
	}
	if c.Review.TimeoutSec != nil && *c.Review.TimeoutSec < 0 {
		*errs = append(*errs, fmt.Errorf("review.timeout_sec: must be >= 0"))
	}
}

func (c Config) validateRollout(errs *[]error) {
	if c.Rollout.MaxConcurrent != nil && *c.Rollout.MaxConcurrent < 0 {
		*errs = append(*errs, fmt.Errorf("rollout.max_concurrent: must be >= 0"))
	}
	if c.Rollout.MaxParallelPerTask != nil && *c.Rollout.MaxParallelPerTask < 0 {
		*errs = append(*errs, fmt.Errorf("rollout.max_parallel_per_task: must be >= 0"))
	}
	if c.Rollout.MinBloomLevel != nil && *c.Rollout.MinBloomLevel < 0 {
		*errs = append(*errs, fmt.Errorf("rollout.min_bloom_level: must be >= 0"))
	}
	if c.Rollout.MaxExpectedPaths != nil && *c.Rollout.MaxExpectedPaths < 0 {
		*errs = append(*errs, fmt.Errorf("rollout.max_expected_paths: must be >= 0"))
	}
	if c.Rollout.MinFailureCount != nil && *c.Rollout.MinFailureCount < 0 {
		*errs = append(*errs, fmt.Errorf("rollout.min_failure_count: must be >= 0"))
	}
}

func (c Config) validateJudge(errs *[]error) {
	if c.Judge.TimeoutSec != nil && *c.Judge.TimeoutSec < 0 {
		*errs = append(*errs, fmt.Errorf("judge.timeout_sec: must be >= 0"))
	}
}

func (c Config) validateQualityGates(errs *[]error) {
	if fa := c.QualityGates.Enforcement.FailureAction; fa != "" && fa != "warn" && fa != "block" {
		*errs = append(*errs, fmt.Errorf("quality_gates.enforcement.failure_action: must be \"warn\" or \"block\""))
	}
}

func (c Config) validateWorktree(errs *[]error) {
	if ms := c.Worktree.MergeStrategy; ms != "" && ms != "ort" && ms != "ours" && ms != "theirs" && ms != "recursive" {
		*errs = append(*errs, fmt.Errorf("worktree.merge_strategy: must be one of \"ort\", \"ours\", \"theirs\", \"recursive\""))
	}
	if c.Worktree.GitTimeoutSec != nil && *c.Worktree.GitTimeoutSec <= 0 {
		*errs = append(*errs, fmt.Errorf("worktree.git_timeout_sec: must be > 0"))
	}
	if c.Worktree.CommitPolicy.MaxFiles != nil && *c.Worktree.CommitPolicy.MaxFiles < 0 {
		*errs = append(*errs, fmt.Errorf("worktree.commit_policy.max_files: must be >= 0"))
	}
	if c.Worktree.GC.Enabled {
		if c.Worktree.GC.TTLHours != nil && *c.Worktree.GC.TTLHours <= 0 {
			*errs = append(*errs, fmt.Errorf("worktree.gc.ttl_hours: must be > 0 when gc is enabled"))
		}
		if c.Worktree.GC.MaxWorktrees != nil && (*c.Worktree.GC.MaxWorktrees <= 0 || *c.Worktree.GC.MaxWorktrees > MaxMaxWorktrees) {
			*errs = append(*errs, fmt.Errorf("worktree.gc.max_worktrees: must be between 1 and %d when gc is enabled", MaxMaxWorktrees))
		}
	}
}

func (c Config) validateCrossFieldConstraints(errs *[]error) {
	// circuit_breaker: if enabled, max_consecutive_failures=0 would never trigger the breaker
	if c.CircuitBreaker.Enabled && c.CircuitBreaker.MaxConsecutiveFailures != nil && *c.CircuitBreaker.MaxConsecutiveFailures == 0 {
		*errs = append(*errs, fmt.Errorf("circuit_breaker.max_consecutive_failures: must be > 0 when circuit_breaker is enabled (0 disables failure detection)"))
	}

	// worktree: gc.max_worktrees should be >= workers.count to avoid cleaning active worktrees
	if c.Worktree.Enabled && c.Worktree.GC.Enabled && c.Worktree.GC.MaxWorktrees != nil &&
		*c.Worktree.GC.MaxWorktrees > 0 && *c.Worktree.GC.MaxWorktrees < c.Agents.Workers.Count {
		*errs = append(*errs, fmt.Errorf(
			"worktree.gc.max_worktrees: value %d is less than agents.workers.count (%d); GC may remove active worktrees",
			*c.Worktree.GC.MaxWorktrees, c.Agents.Workers.Count))
	}

	// worktree: if both stall_timeout and fallback_merge_timeout are explicitly disabled (0),
	// stuck worktrees have no safety timeout
	if c.Worktree.Enabled && c.Worktree.StallTimeoutMinutes != nil && *c.Worktree.StallTimeoutMinutes == 0 &&
		c.Worktree.FallbackMergeTimeoutMinutes != nil && *c.Worktree.FallbackMergeTimeoutMinutes == 0 {
		*errs = append(*errs, fmt.Errorf(
			"worktree: both stall_timeout_minutes and fallback_merge_timeout_minutes are explicitly disabled (0); stuck worktrees will have no safety timeout"))
	}

	// watcher: dispatch_lease_sec should be less than max_in_progress_min (converted to seconds)
	// to ensure tasks can be re-dispatched before they are considered stalled.
	if c.Watcher.DispatchLeaseSec > 0 {
		maxInProgressSec := c.Watcher.EffectiveMaxInProgressMin() * 60
		if maxInProgressSec > 0 && c.Watcher.DispatchLeaseSec >= maxInProgressSec {
			*errs = append(*errs, fmt.Errorf(
				"watcher.dispatch_lease_sec (%d) >= watcher.max_in_progress_min (%d) in seconds (%d): lease must be shorter than max runtime",
				c.Watcher.DispatchLeaseSec, c.Watcher.EffectiveMaxInProgressMin(), maxInProgressSec))
		}
	}

	// retry + circuit_breaker: a single task's retries could trip the circuit breaker
	if c.CircuitBreaker.Enabled && c.Retry.TaskExecution.Enabled && c.Retry.TaskExecution.MaxRetries > 0 {
		cbLimit := c.CircuitBreaker.EffectiveMaxConsecutiveFailures()
		if cbLimit > 0 && c.Retry.TaskExecution.MaxRetries >= cbLimit {
			*errs = append(*errs, fmt.Errorf(
				"retry.task_execution.max_retries (%d) >= circuit_breaker.max_consecutive_failures (%d): a single task's retries could trip the circuit breaker",
				c.Retry.TaskExecution.MaxRetries, cbLimit))
		}
	}
}

func (c Config) validateFloatFields(errs *[]error) {
	if !isFiniteFloat64Ptr(c.Evolution.NoveltyThreshold) {
		*errs = append(*errs, fmt.Errorf("evolution.novelty_threshold: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Bandit.ExplorationCoeff) {
		*errs = append(*errs, fmt.Errorf("bandit.exploration_coefficient: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Bandit.DecayFactor) {
		*errs = append(*errs, fmt.Errorf("bandit.decay_factor: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Search.PruneThreshold) {
		*errs = append(*errs, fmt.Errorf("search.prune_threshold: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Search.ThompsonAlpha) {
		*errs = append(*errs, fmt.Errorf("search.thompson_alpha: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Search.ThompsonBeta) {
		*errs = append(*errs, fmt.Errorf("search.thompson_beta: must be a finite value"))
	}
	for k, v := range c.ExtendedVerification.PerspectiveWeights {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			*errs = append(*errs, fmt.Errorf("extended_verification.perspective_weights.%s: must be a finite value", k))
		}
	}
}

// validateNonNegInt appends an error if val is negative.
func validateNonNegInt(errs *[]error, field string, val int) {
	if val < 0 {
		*errs = append(*errs, fmt.Errorf("%s: must be >= 0", field))
	}
}

// validateIntRange appends an error if val is outside [0, max].
func validateIntRange(errs *[]error, field string, val, max int) {
	if val < 0 || val > max {
		*errs = append(*errs, fmt.Errorf("%s: must be between 0 and %d", field, max))
	}
}
