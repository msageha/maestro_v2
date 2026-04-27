package main

import (
	"fmt"
	"os"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// dabUnset is the sentinel value used by the definition_of_abort CLI flags to
// distinguish "user did not pass the flag" (→ inherit model defaults) from an
// explicit value the user typed. Without a sentinel, a typo such as
// `--max-repair-count 0` is indistinguishable from the unset state and would
// silently fall back to the default — a footgun for an abort threshold that
// REQUIREMENTS.md §S2-2 mandates as a hard stop. -1 was chosen because it is
// outside the valid range for both max_repair_count (≥1) and
// max_wall_clock_sec (≥1) and is unlikely to be typed deliberately.
const dabUnset = -1

// runPlanAddRetryTask replaces a failed task with a new retry task.
func (a *cliApp) runPlanAddRetryTask(args []string) error {
	cmd := NewCommand("maestro plan add-retry-task", "maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> (--content <text>|--content-file <path>) --acceptance-criteria <text> --bloom-level <n> --expected-paths <path> [--expected-paths <path>...] [--max-repair-count <n>] [--max-wall-clock-sec <n>] [--explicit-failure-condition <text>...] [--blocked-by <task_id>]...")
	var commandID, retryOf, purpose, content, contentFile, acceptanceCriteria string
	var bloomLevel, maxRepairCount, maxWallClockSec int
	var blockedBy, expectedPaths, definitionOfDone, explicitFailureConditions stringSliceFlag

	cmd.StringVar(&commandID, "command-id", "", "Parent command ID")
	cmd.StringVar(&retryOf, "retry-of", "", "Task ID of the failed task to retry")
	cmd.StringVar(&purpose, "purpose", "", "Purpose description for the retry task")
	cmd.StringVar(&content, "content", "", "Task content for the retry task")
	cmd.StringVar(&contentFile, "content-file", "", "Read task content for the retry task from a file")
	cmd.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "Acceptance criteria for the retry task")
	cmd.IntVar(&bloomLevel, "bloom-level", 0, "Bloom taxonomy level (1-6)")
	cmd.Var(&blockedBy, "blocked-by", "Task ID dependency (repeatable)")
	cmd.Var(&expectedPaths, "expected-paths", "Expected file path(s) the task is allowed to modify (repeatable, required by REQUIREMENTS.md §S3-1)")
	cmd.Var(&definitionOfDone, "definition-of-done", "definition_of_done entry (repeatable; overrides acceptance_criteria as done conditions)")
	cmd.IntVar(&maxRepairCount, "max-repair-count", dabUnset, "definition_of_abort.max_repair_count (positive integer; omit to inherit model.DefaultDefinitionOfAbort)")
	cmd.IntVar(&maxWallClockSec, "max-wall-clock-sec", dabUnset, "definition_of_abort.max_wall_clock_sec (positive integer; omit to inherit model.DefaultDefinitionOfAbort)")
	cmd.Var(&explicitFailureConditions, "explicit-failure-condition", "definition_of_abort.explicit_failure_conditions entry (repeatable)")

	cmd.AddCheck("all required flags must be set", func() bool {
		return commandID != "" && retryOf != "" && purpose != "" && (content != "" || contentFile != "") && acceptanceCriteria != "" && bloomLevel != 0 && len(expectedPaths) > 0
	})

	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := resolveContentFile(cmd, &content, contentFile); err != nil {
		return err
	}

	if err := validateTaskParams(cmd, commandID, blockedBy, bloomLevel, purpose, content, acceptanceCriteria); err != nil {
		return err
	}
	if err := validate.ID(retryOf); err != nil {
		return cmd.Errorf("invalid --retry-of: %v", err)
	}
	doa, err := buildDefinitionOfAbort(maxRepairCount, maxWallClockSec, explicitFailureConditions)
	if err != nil {
		return cmd.Errorf("%v", err)
	}

	maestroDir, err := requireMaestroDir("plan add-retry-task")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "add_retry_task",
		"data": map[string]any{
			"command_id":          commandID,
			"retry_of":            retryOf,
			"purpose":             purpose,
			"content":             content,
			"acceptance_criteria": acceptanceCriteria,
			"definition_of_done":  []string(definitionOfDone),
			"blocked_by":          blockedBy,
			"bloom_level":         bloomLevel,
			"expected_paths":      []string(expectedPaths),
			"definition_of_abort": doa,
		},
	}

	return a.sendPlanCommand("plan add-retry-task", maestroDir, params, planCommandTimeout)
}

// runPlanAddTask injects a new task into an existing sealed plan.
func (a *cliApp) runPlanAddTask(args []string) error {
	cmd := NewCommand("maestro plan add-task", "maestro plan add-task --command-id <id> --purpose <text> (--content <text>|--content-file <path>) --acceptance-criteria <text> --bloom-level <n> --expected-paths <path>... [--max-repair-count <n>] [--max-wall-clock-sec <n>] [--explicit-failure-condition <text>...] [--blocked-by <task_id>]... [--required] [--run-on-main]")
	var commandID, purpose, content, contentFile, acceptanceCriteria, personaHint, workerID, targetPhase, idempotencyKey string
	var bloomLevel, maxRepairCount, maxWallClockSec int
	var required, runOnMain, runOnIntegration bool
	var blockedBy, toolsHint, constraints, skillRefs, expectedPaths, definitionOfDone, explicitFailureConditions stringSliceFlag

	cmd.StringVar(&commandID, "command-id", "", "Parent command ID")
	cmd.StringVar(&purpose, "purpose", "", "Purpose description for the task")
	cmd.StringVar(&content, "content", "", "Task content")
	cmd.StringVar(&contentFile, "content-file", "", "Read task content from a file")
	cmd.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "Acceptance criteria for the task")
	cmd.IntVar(&bloomLevel, "bloom-level", 0, "Bloom taxonomy level (1-6)")
	cmd.BoolVar(&required, "required", true, "Whether the task is required for command completion")
	cmd.BoolVar(&runOnMain, "run-on-main", false, "Run task in main branch directory instead of worker worktree (for read-only verification tasks)")
	cmd.BoolVar(&runOnIntegration, "run-on-integration", false, "Run task in integration worktree instead of worker worktree (for publish_conflict resolution tasks)")
	cmd.Var(&blockedBy, "blocked-by", "Task ID dependency (repeatable)")
	cmd.Var(&constraints, "constraints", "Constraint (repeatable)")
	cmd.Var(&toolsHint, "tools-hint", "Recommended tool (repeatable)")
	cmd.StringVar(&personaHint, "persona-hint", "", "Persona hint")
	cmd.Var(&skillRefs, "skill-refs", "Skill reference (repeatable)")
	cmd.StringVar(&workerID, "worker-id", "", "Target worker for task assignment (optional; defaults to least-loaded)")
	cmd.StringVar(&targetPhase, "target-phase", "", "Phase ID to place the task in (optional; overrides default phase selection)")
	cmd.StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key to prevent duplicate task injection on retry")
	cmd.Var(&expectedPaths, "expected-paths", "Expected file path(s) the task is allowed to modify (repeatable, required by REQUIREMENTS.md §S3-1)")
	cmd.Var(&definitionOfDone, "definition-of-done", "definition_of_done entry (repeatable; overrides acceptance_criteria as done conditions)")
	cmd.IntVar(&maxRepairCount, "max-repair-count", dabUnset, "definition_of_abort.max_repair_count (positive integer; omit to inherit model.DefaultDefinitionOfAbort)")
	cmd.IntVar(&maxWallClockSec, "max-wall-clock-sec", dabUnset, "definition_of_abort.max_wall_clock_sec (positive integer; omit to inherit model.DefaultDefinitionOfAbort)")
	cmd.Var(&explicitFailureConditions, "explicit-failure-condition", "definition_of_abort.explicit_failure_conditions entry (repeatable)")

	cmd.AddCheck("all required flags must be set", func() bool {
		return commandID != "" && purpose != "" && (content != "" || contentFile != "") && acceptanceCriteria != "" && bloomLevel != 0 && len(expectedPaths) > 0
	})

	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := resolveContentFile(cmd, &content, contentFile); err != nil {
		return err
	}

	if err := validateTaskParams(cmd, commandID, blockedBy, bloomLevel, purpose, content, acceptanceCriteria); err != nil {
		return err
	}
	if workerID != "" {
		if err := validate.ID(workerID); err != nil {
			return cmd.Errorf("invalid --worker-id: %v", err)
		}
	}
	if targetPhase != "" {
		if err := validate.PhaseID(targetPhase); err != nil {
			return cmd.Errorf("invalid --target-phase: %v", err)
		}
	}
	doa, err := buildDefinitionOfAbort(maxRepairCount, maxWallClockSec, explicitFailureConditions)
	if err != nil {
		return cmd.Errorf("%v", err)
	}

	maestroDir, err := requireMaestroDir("plan add-task")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "add_task",
		"data": map[string]any{
			"command_id":          commandID,
			"purpose":             purpose,
			"content":             content,
			"acceptance_criteria": acceptanceCriteria,
			"definition_of_done":  []string(definitionOfDone),
			"constraints":         []string(constraints),
			"blocked_by":          []string(blockedBy),
			"bloom_level":         bloomLevel,
			"required":            required,
			"tools_hint":          []string(toolsHint),
			"persona_hint":        personaHint,
			"skill_refs":          []string(skillRefs),
			"expected_paths":      []string(expectedPaths),
			"definition_of_abort": doa,
			"worker_id":           workerID,
			"target_phase":        targetPhase,
			"idempotency_key":     idempotencyKey,
			"run_on_main":         runOnMain,
			"run_on_integration":  runOnIntegration,
		},
	}

	// add-task operates on sealed plans and contends with the daemon's
	// PeriodicScan exclusive lock (scanMu), same as plan submit --phase.
	// Use the extended timeout to avoid spurious timeouts under contention.
	return a.sendPlanCommand("plan add-task", maestroDir, params, planPhaseFillTimeout)
}

// resolveContentFile resolves --content-file into content. It rejects mixed
// sources so the value sent to the daemon has a single obvious origin.
func resolveContentFile(cmd *CommandBuilder, content *string, contentFile string) error {
	if contentFile == "" {
		return nil
	}
	if *content != "" {
		return cmd.Errorf("--content and --content-file are mutually exclusive")
	}
	b, err := os.ReadFile(contentFile)
	if err != nil {
		return cmd.Errorf("read --content-file: %v", err)
	}
	*content = string(b)
	return nil
}

// buildDefinitionOfAbort assembles a *model.DefinitionOfAbort from the
// individual CLI flags shared by add-task and add-retry-task. The dabUnset
// sentinel inherits the model defaults; any other non-positive value is
// rejected so a typo cannot silently disable a REQUIREMENTS.md §S2-2 abort
// threshold. The plan layer validates the final values against §S3-1 bounds.
func buildDefinitionOfAbort(maxRepairCount, maxWallClockSec int, explicitFailureConditions []string) (*model.DefinitionOfAbort, error) {
	defaults := model.DefaultDefinitionOfAbort()
	doa := model.DefinitionOfAbort{
		MaxRepairCount:            defaults.MaxRepairCount,
		MaxWallClockSec:           defaults.MaxWallClockSec,
		ExplicitFailureConditions: explicitFailureConditions,
	}
	if maxRepairCount != dabUnset {
		if maxRepairCount <= 0 {
			return nil, fmt.Errorf("--max-repair-count must be a positive integer (got %d)", maxRepairCount)
		}
		doa.MaxRepairCount = maxRepairCount
	}
	if maxWallClockSec != dabUnset {
		if maxWallClockSec <= 0 {
			return nil, fmt.Errorf("--max-wall-clock-sec must be a positive integer (got %d)", maxWallClockSec)
		}
		doa.MaxWallClockSec = maxWallClockSec
	}
	return &doa, nil
}

// validateTaskParams validates fields common to add-retry-task and add-task:
// command ID, blocked-by dependencies, bloom level range, and content lengths.
func validateTaskParams(cmd *CommandBuilder, commandID string, blockedBy []string, bloomLevel int, purpose, content, acceptanceCriteria string) error {
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}
	for _, dep := range blockedBy {
		if err := validate.ID(dep); err != nil {
			return cmd.Errorf("invalid --blocked-by %q: %v", dep, err)
		}
	}
	if bloomLevel < 1 || bloomLevel > 6 {
		return cmd.Errorf("--bloom-level must be between 1 and 6")
	}
	for _, pair := range []struct{ name, val string }{
		{"--content", content},
		{"--purpose", purpose},
		{"--acceptance-criteria", acceptanceCriteria},
	} {
		if err := validate.ContentLength(pair.name, pair.val, model.DefaultMaxEntryContentBytes); err != nil {
			return cmd.Errorf("%v", err)
		}
	}
	return nil
}
