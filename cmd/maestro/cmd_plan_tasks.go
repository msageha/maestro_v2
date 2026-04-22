package main

import (
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runPlanAddRetryTask replaces a failed task with a new retry task.
func (a *cliApp) runPlanAddRetryTask(args []string) error {
	cmd := NewCommand("maestro plan add-retry-task", "maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]...")
	var commandID, retryOf, purpose, content, acceptanceCriteria string
	var bloomLevel int
	var blockedBy stringSliceFlag

	cmd.StringVar(&commandID, "command-id", "", "Parent command ID")
	cmd.StringVar(&retryOf, "retry-of", "", "Task ID of the failed task to retry")
	cmd.StringVar(&purpose, "purpose", "", "Purpose description for the retry task")
	cmd.StringVar(&content, "content", "", "Task content for the retry task")
	cmd.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "Acceptance criteria for the retry task")
	cmd.IntVar(&bloomLevel, "bloom-level", 0, "Bloom taxonomy level (1-6)")
	cmd.Var(&blockedBy, "blocked-by", "Task ID dependency (repeatable)")

	cmd.AddCheck("all required flags must be set", func() bool {
		return commandID != "" && retryOf != "" && purpose != "" && content != "" && acceptanceCriteria != "" && bloomLevel != 0
	})

	if err := cmd.Parse(args); err != nil {
		return err
	}

	if err := validateTaskParams(cmd, commandID, blockedBy, bloomLevel, purpose, content, acceptanceCriteria); err != nil {
		return err
	}
	if err := validate.ID(retryOf); err != nil {
		return cmd.Errorf("invalid --retry-of: %v", err)
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
			"blocked_by":          blockedBy,
			"bloom_level":         bloomLevel,
		},
	}

	return a.sendPlanCommand("plan add-retry-task", maestroDir, params, planCommandTimeout)
}

// runPlanAddTask injects a new task into an existing sealed plan.
func (a *cliApp) runPlanAddTask(args []string) error {
	cmd := NewCommand("maestro plan add-task", "maestro plan add-task --command-id <id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]... [--required] [--run-on-main]")
	var commandID, purpose, content, acceptanceCriteria, personaHint, workerID, targetPhase, idempotencyKey string
	var bloomLevel int
	var required, runOnMain bool
	var blockedBy, toolsHint, constraints, skillRefs stringSliceFlag

	cmd.StringVar(&commandID, "command-id", "", "Parent command ID")
	cmd.StringVar(&purpose, "purpose", "", "Purpose description for the task")
	cmd.StringVar(&content, "content", "", "Task content")
	cmd.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "Acceptance criteria for the task")
	cmd.IntVar(&bloomLevel, "bloom-level", 0, "Bloom taxonomy level (1-6)")
	cmd.BoolVar(&required, "required", true, "Whether the task is required for command completion")
	cmd.BoolVar(&runOnMain, "run-on-main", false, "Run task in main branch directory instead of worker worktree (for read-only verification tasks)")
	cmd.Var(&blockedBy, "blocked-by", "Task ID dependency (repeatable)")
	cmd.Var(&constraints, "constraints", "Constraint (repeatable)")
	cmd.Var(&toolsHint, "tools-hint", "Recommended tool (repeatable)")
	cmd.StringVar(&personaHint, "persona-hint", "", "Persona hint")
	cmd.Var(&skillRefs, "skill-refs", "Skill reference (repeatable)")
	cmd.StringVar(&workerID, "worker-id", "", "Target worker for task assignment (optional; defaults to least-loaded)")
	cmd.StringVar(&targetPhase, "target-phase", "", "Phase ID to place the task in (optional; overrides default phase selection)")
	cmd.StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key to prevent duplicate task injection on retry")

	cmd.AddCheck("all required flags must be set", func() bool {
		return commandID != "" && purpose != "" && content != "" && acceptanceCriteria != "" && bloomLevel != 0
	})

	if err := cmd.Parse(args); err != nil {
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
			"constraints":         []string(constraints),
			"blocked_by":          []string(blockedBy),
			"bloom_level":         bloomLevel,
			"required":            required,
			"tools_hint":          []string(toolsHint),
			"persona_hint":        personaHint,
			"skill_refs":          []string(skillRefs),
			"worker_id":           workerID,
			"target_phase":        targetPhase,
			"idempotency_key":     idempotencyKey,
			"run_on_main":         runOnMain,
		},
	}

	// add-task operates on sealed plans and contends with the daemon's
	// PeriodicScan exclusive lock (scanMu), same as plan submit --phase.
	// Use the extended timeout to avoid spurious timeouts under contention.
	return a.sendPlanCommand("plan add-task", maestroDir, params, planPhaseFillTimeout)
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
