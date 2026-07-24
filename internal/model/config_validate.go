package model

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strings"
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
	c.validateQualityGates(&errs)
	c.validateWorktree(&errs)
	c.validateExperimental(&errs)
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
	// Capability tags are free-form (custom tags beyond the documented
	// vocabulary are allowed — matching is exact-string), but empty /
	// whitespace-only entries can never match anything and always indicate a
	// config mistake, so reject them at load time.
	for workerID, caps := range c.Agents.Workers.Capabilities {
		for i, capTag := range caps {
			if strings.TrimSpace(capTag) == "" {
				*errs = append(*errs, fmt.Errorf("agents.workers.capabilities.%s[%d]: capability tag must not be empty", workerID, i))
			}
		}
	}

	// Agent role constraints are enforced via Claude Code's
	// --allowedTools / --disallowedTools CLI flags. codex and gemini have no
	// equivalent enforcement layer, so the
	// "delegation-only" rule for Orchestrator, the "planning-only" rule for
	// Planner, and the Worker control-plane restrictions cannot be technically
	// enforced when those runtimes are used.
	// Past incidents confirmed this is not a prompt-quality issue: codex
	// running as Orchestrator has bypassed delegation entirely and edited
	// files on main directly. Fail-closed at config load to make the role
	// contract honest.
	if r, _ := ParseRuntimeFromModel(c.Agents.Orchestrator.Model); r != RuntimeClaudeCode {
		*errs = append(*errs, fmt.Errorf(
			"agents.orchestrator.model: runtime %q is not supported for the orchestrator role "+
				"(only claude-code enforces tool restrictions; codex/gemini have no equivalent guardrail). "+
				"Set agents.orchestrator.model to a Claude model such as opus, sonnet, or haiku",
			r))
	}
	if r, _ := ParseRuntimeFromModel(c.Agents.Planner.Model); r != RuntimeClaudeCode {
		*errs = append(*errs, fmt.Errorf(
			"agents.planner.model: runtime %q is not supported for the planner role "+
				"(only claude-code enforces tool restrictions; codex/gemini have no equivalent guardrail). "+
				"Set agents.planner.model to a Claude model such as opus, sonnet, or haiku",
			r))
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
	if c.Retry.TaskProgressInterrupts != nil && *c.Retry.TaskProgressInterrupts < 0 {
		*errs = append(*errs, fmt.Errorf("retry.task_progress_interrupts: must be >= 0"))
	}
	if c.Retry.TaskResume != nil && *c.Retry.TaskResume < 0 {
		*errs = append(*errs, fmt.Errorf("retry.task_resume: must be >= 0"))
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
	// Missing directories are deliberately NOT a validation error (they are
	// skipped with a WARN at use time), but an empty entry is always a config
	// mistake: it would resolve to the project root itself.
	for i, dir := range c.Skills.ExtraDirs {
		if strings.TrimSpace(dir) == "" {
			*errs = append(*errs, fmt.Errorf("skills.extra_dirs[%d]: must not be empty", i))
		}
	}
}

func (c Config) validateAdmissionControl(errs *[]error) {
	if c.AdmissionControl.MaxConcurrentVerify < 0 {
		*errs = append(*errs, fmt.Errorf("admission_control.max_concurrent_verify: must be >= 0"))
	}
	if c.AdmissionControl.MaxConcurrentRepair < 0 {
		*errs = append(*errs, fmt.Errorf("admission_control.max_concurrent_repair: must be >= 0"))
	}
}

// validateFallback is retained as a no-op for compatibility with the
// validateAll wiring; the Fallback field has been removed from Config
// (degraded-mode worker blacklisting was retired) so there is nothing to
// validate. Callers may still invoke this without error.
func (c Config) validateFallback(_ *[]error) {}

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

func (c Config) validateQualityGates(errs *[]error) {
	if fa := c.QualityGates.Enforcement.FailureAction; fa != "" && fa != "warn" && fa != "block" {
		*errs = append(*errs, fmt.Errorf("quality_gates.enforcement.failure_action: must be \"warn\" or \"block\""))
	}
}

// validBranchNameRe matches git branch names: letters, digits, hyphens, dots,
// underscores, and slashes are allowed. Must start with a letter or digit.
// Rejects control characters, spaces, tildes, carets, colons, question marks,
// asterisks, open brackets, and backslashes.
var validBranchNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*$`)

// validPathPrefixRe matches relative directory paths: letters, digits, hyphens,
// dots, underscores, and slashes are allowed. Must start with a letter, digit,
// or dot. Rejects control characters, spaces, and shell-special characters.
var validPathPrefixRe = regexp.MustCompile(`^[a-zA-Z0-9.][a-zA-Z0-9._/-]*$`)

func (c Config) validateWorktree(errs *[]error) {
	validateBaseBranch(errs, c.Worktree.BaseBranch)
	validatePathPrefix(errs, c.Worktree.PathPrefix)
	if ms := c.Worktree.MergeStrategy; ms != "" && ms != "ort" && ms != "ours" && ms != "theirs" && ms != "recursive" {
		*errs = append(*errs, fmt.Errorf("worktree.merge_strategy: must be one of \"ort\", \"ours\", \"theirs\", \"recursive\""))
	}
	if c.Worktree.GitTimeoutSec != nil && *c.Worktree.GitTimeoutSec <= 0 {
		*errs = append(*errs, fmt.Errorf("worktree.git_timeout_sec: must be > 0"))
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

func (c Config) validateExperimental(errs *[]error) {
	// C-1 Evolution
	if c.Evolution.MaxMutationsPerRound != nil && *c.Evolution.MaxMutationsPerRound <= 0 {
		*errs = append(*errs, fmt.Errorf("evolution.max_mutations_per_round: must be > 0"))
	}
	if c.Evolution.NoveltyThreshold != nil && (*c.Evolution.NoveltyThreshold < 0 || *c.Evolution.NoveltyThreshold > 1) {
		*errs = append(*errs, fmt.Errorf("evolution.novelty_threshold: must be between 0 and 1"))
	}

	// C-2 Bandit
	if c.Bandit.ExplorationCoeff != nil && *c.Bandit.ExplorationCoeff <= 0 {
		*errs = append(*errs, fmt.Errorf("bandit.exploration_coefficient: must be > 0"))
	}
	if c.Bandit.MinSamplesBeforeUse != nil && *c.Bandit.MinSamplesBeforeUse < 0 {
		*errs = append(*errs, fmt.Errorf("bandit.min_samples_before_use: must be >= 0"))
	}
	if c.Bandit.TraceDataRequirement != nil && *c.Bandit.TraceDataRequirement < 0 {
		*errs = append(*errs, fmt.Errorf("bandit.trace_data_requirement: must be >= 0"))
	}

	// A/B candidate selection
	if c.ABTest.MinBloomLevel != nil && (*c.ABTest.MinBloomLevel < 1 || *c.ABTest.MinBloomLevel > 6) {
		*errs = append(*errs, fmt.Errorf("ab_test.min_bloom_level: must be in [1, 6]"))
	}
	if c.ABTest.TimeoutSec != nil && *c.ABTest.TimeoutSec < 0 {
		*errs = append(*errs, fmt.Errorf("ab_test.timeout_sec: must be >= 0"))
	}
	if c.ABTest.SelectionTimeoutSec != nil && *c.ABTest.SelectionTimeoutSec < 0 {
		*errs = append(*errs, fmt.Errorf("ab_test.selection_timeout_sec: must be >= 0"))
	}
	if len(c.ABTest.JudgeModels) == 1 {
		*errs = append(*errs, fmt.Errorf("ab_test.judge_models: needs 0 (disable) or >= 2 entries (the agree/disagree protocol takes two votes)"))
	}
	seenJudges := map[string]bool{}
	for _, j := range c.ABTest.JudgeModels {
		if strings.TrimSpace(j) == "" {
			*errs = append(*errs, fmt.Errorf("ab_test.judge_models: empty entry"))
			continue
		}
		if seenJudges[j] {
			*errs = append(*errs, fmt.Errorf("ab_test.judge_models: duplicate %q (cross-runtime judging needs distinct judges)", j))
		}
		seenJudges[j] = true
	}
	for _, p := range c.ABTest.CrossTestPatterns {
		if strings.Contains(p, "/") {
			*errs = append(*errs, fmt.Errorf("ab_test.cross_test_patterns: %q must be a basename glob (no '/')", p))
			continue
		}
		if _, err := filepath.Match(p, "probe"); err != nil {
			*errs = append(*errs, fmt.Errorf("ab_test.cross_test_patterns: %q is not a valid glob: %w", p, err))
		}
	}

	// C-3 Extended Verification
	if c.ExtendedVerification.MaxAutoRetries != nil && *c.ExtendedVerification.MaxAutoRetries < 0 {
		*errs = append(*errs, fmt.Errorf("extended_verification.max_auto_retries: must be >= 0"))
	}

	// C-4 Search
	if c.Search.MaxDepth != nil && *c.Search.MaxDepth <= 0 {
		*errs = append(*errs, fmt.Errorf("search.max_depth: must be > 0"))
	}
	if c.Search.MaxBranching != nil && *c.Search.MaxBranching <= 0 {
		*errs = append(*errs, fmt.Errorf("search.max_branching: must be > 0"))
	}
	if c.Search.PruneThreshold != nil && (*c.Search.PruneThreshold < 0 || *c.Search.PruneThreshold > 1) {
		*errs = append(*errs, fmt.Errorf("search.prune_threshold: must be between 0 and 1"))
	}
	if c.Search.ThompsonAlpha != nil && *c.Search.ThompsonAlpha <= 0 {
		*errs = append(*errs, fmt.Errorf("search.thompson_alpha: must be > 0"))
	}
	if c.Search.ThompsonBeta != nil && *c.Search.ThompsonBeta <= 0 {
		*errs = append(*errs, fmt.Errorf("search.thompson_beta: must be > 0"))
	}

	// C-5 Self-Improvement
	if c.SelfImprovement.ArchiveMaxSize != nil && *c.SelfImprovement.ArchiveMaxSize < 0 {
		*errs = append(*errs, fmt.Errorf("self_improvement.archive_max_size: must be >= 0"))
	}
	if c.SelfImprovement.Friction.MinOccurrences != nil && *c.SelfImprovement.Friction.MinOccurrences <= 0 {
		*errs = append(*errs, fmt.Errorf("self_improvement.friction.min_occurrences: must be > 0"))
	}
	if c.SelfImprovement.Friction.VerifyMinSuccesses != nil && *c.SelfImprovement.Friction.VerifyMinSuccesses <= 0 {
		*errs = append(*errs, fmt.Errorf("self_improvement.friction.verify_min_successes: must be > 0"))
	}
	if c.SelfImprovement.Friction.MaxEntries != nil && *c.SelfImprovement.Friction.MaxEntries <= 0 {
		*errs = append(*errs, fmt.Errorf("self_improvement.friction.max_entries: must be > 0"))
	}
	if c.SelfImprovement.Friction.InjectCount != nil && *c.SelfImprovement.Friction.InjectCount < 0 {
		*errs = append(*errs, fmt.Errorf("self_improvement.friction.inject_count: must be >= 0"))
	}

	// C-6 Complexity Thresholds
	if c.Complexity.Thresholds.SimpleMaxFiles != nil && *c.Complexity.Thresholds.SimpleMaxFiles <= 0 {
		*errs = append(*errs, fmt.Errorf("complexity.thresholds.simple_max_files: must be > 0"))
	}
	if c.Complexity.Thresholds.StandardMaxFiles != nil && *c.Complexity.Thresholds.StandardMaxFiles <= 0 {
		*errs = append(*errs, fmt.Errorf("complexity.thresholds.standard_max_files: must be > 0"))
	}
	if c.Complexity.Thresholds.ComplexMaxFiles != nil && *c.Complexity.Thresholds.ComplexMaxFiles <= 0 {
		*errs = append(*errs, fmt.Errorf("complexity.thresholds.complex_max_files: must be > 0"))
	}
	simpleMax := c.Complexity.Thresholds.EffectiveSimpleMaxFiles()
	standardMax := c.Complexity.Thresholds.EffectiveStandardMaxFiles()
	complexMax := c.Complexity.Thresholds.EffectiveComplexMaxFiles()
	if simpleMax > standardMax {
		*errs = append(*errs, fmt.Errorf("complexity.thresholds: simple_max_files (%d) must be <= standard_max_files (%d)", simpleMax, standardMax))
	}
	if standardMax > complexMax {
		*errs = append(*errs, fmt.Errorf("complexity.thresholds: standard_max_files (%d) must be <= complex_max_files (%d)", standardMax, complexMax))
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
	if !isFiniteFloat64Ptr(c.Search.PruneThreshold) {
		*errs = append(*errs, fmt.Errorf("search.prune_threshold: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Search.ThompsonAlpha) {
		*errs = append(*errs, fmt.Errorf("search.thompson_alpha: must be a finite value"))
	}
	if !isFiniteFloat64Ptr(c.Search.ThompsonBeta) {
		*errs = append(*errs, fmt.Errorf("search.thompson_beta: must be a finite value"))
	}
}

// validateNonNegInt appends an error if val is negative.
func validateNonNegInt(errs *[]error, field string, val int) {
	if val < 0 {
		*errs = append(*errs, fmt.Errorf("%s: must be >= 0", field))
	}
}

// validateIntRange appends an error if val is outside [0, upper].
func validateIntRange(errs *[]error, field string, val, upper int) {
	if val < 0 || val > upper {
		*errs = append(*errs, fmt.Errorf("%s: must be between 0 and %d", field, upper))
	}
}

// validateBaseBranch validates the worktree.base_branch field.
// Empty is allowed (defaults apply). When set, it must be a valid git branch name:
// no ".." sequences, no control characters, no spaces, no trailing dots/slashes,
// no trailing ".lock" suffix.
func validateBaseBranch(errs *[]error, branch string) {
	if branch == "" {
		return
	}
	if strings.Contains(branch, "..") {
		*errs = append(*errs, fmt.Errorf("worktree.base_branch: must not contain \"..\""))
		return
	}
	if !validBranchNameRe.MatchString(branch) {
		*errs = append(*errs, fmt.Errorf("worktree.base_branch: contains invalid characters (allowed: letters, digits, hyphens, dots, underscores, slashes)"))
		return
	}
	if strings.HasSuffix(branch, ".") || strings.HasSuffix(branch, "/") {
		*errs = append(*errs, fmt.Errorf("worktree.base_branch: must not end with \".\" or \"/\""))
		return
	}
	if strings.HasSuffix(branch, ".lock") {
		*errs = append(*errs, fmt.Errorf("worktree.base_branch: must not end with \".lock\""))
		return
	}
	if strings.Contains(branch, "//") {
		*errs = append(*errs, fmt.Errorf("worktree.base_branch: must not contain consecutive slashes"))
		return
	}
}

// validatePathPrefix validates the worktree.path_prefix field.
// Empty is allowed (defaults apply). When set, it must be a relative path
// without path traversal (".."), no absolute paths, and no invalid characters.
func validatePathPrefix(errs *[]error, prefix string) {
	if prefix == "" {
		return
	}
	if filepath.IsAbs(prefix) {
		*errs = append(*errs, fmt.Errorf("worktree.path_prefix: must be a relative path, not absolute"))
		return
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == ".." {
			*errs = append(*errs, fmt.Errorf("worktree.path_prefix: must not contain path traversal \"..\""))
			return
		}
	}
	if !validPathPrefixRe.MatchString(prefix) {
		*errs = append(*errs, fmt.Errorf("worktree.path_prefix: contains invalid characters (allowed: letters, digits, hyphens, dots, underscores, slashes)"))
		return
	}
	if strings.HasSuffix(prefix, "/") {
		*errs = append(*errs, fmt.Errorf("worktree.path_prefix: must not end with \"/\""))
		return
	}
}
