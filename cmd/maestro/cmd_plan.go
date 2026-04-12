package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// planCommandTimeout bounds how long sendPlanCommand will wait for the daemon
// to respond before aborting the request. The CLI's UDS client also enforces
// a per-connection deadline, but a request-level timeout makes SIGINT-driven
// cancellation deterministic for operators.
const planCommandTimeout = 30 * time.Second

// runPlan dispatches plan subcommands (submit, complete, add-retry-task, request-cancel, rebuild).
func (a *cliApp) runPlan(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro plan: missing subcommand\nusage: maestro plan <submit|complete|add-retry-task|request-cancel|rebuild|unquarantine|resume-merge> [options]"}
	}
	switch args[0] {
	case "submit":
		return a.runPlanSubmit(args[1:])
	case "complete":
		return a.runPlanComplete(args[1:])
	case "add-retry-task":
		return a.runPlanAddRetryTask(args[1:])
	case "request-cancel":
		return a.runPlanRequestCancel(args[1:])
	case "rebuild":
		return a.runPlanRebuild(args[1:])
	case "unquarantine":
		return a.runPlanUnquarantine(args[1:])
	case "resume-merge":
		return a.runPlanResumeMerge(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan: unknown subcommand: %s\nusage: maestro plan <submit|complete|add-retry-task|request-cancel|rebuild|unquarantine|resume-merge> [options]", args[0])}
	}
}

// runPlanSubmit submits a task plan for a command.
func (a *cliApp) runPlanSubmit(args []string) error {
	cmd := NewCommand("maestro plan submit", "maestro plan submit --command-id <id> [--tasks-file <path>] [--phase <name>] [--dry-run]")
	var commandID, tasksFile, phaseName string
	var dryRun bool
	cmd.RequiredString(&commandID, "command-id", "Parent command ID")
	cmd.StringVar(&tasksFile, "tasks-file", "", "Path to tasks YAML file (default: stdin)")
	cmd.StringVar(&phaseName, "phase", "", "Phase name for grouping tasks")
	cmd.BoolVar(&dryRun, "dry-run", false, "Validate plan without submitting")

	if err := cmd.Parse(args); err != nil {
		return err
	}

	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	if tasksFile == "" {
		tasksFile = "-" // default to stdin
	}

	// CRIT-05: Validate non-stdin file path before passing to daemon
	if tasksFile != "-" && tasksFile != "/dev/stdin" {
		cleaned, err := validate.FilePath(tasksFile)
		if err != nil {
			return fmt.Errorf("maestro plan submit: invalid tasks file: %w", err)
		}
		tasksFile = cleaned
	}

	maestroDir, err := requireMaestroDir("plan submit")
	if err != nil {
		return err
	}

	// Build submit data. When reading from stdin, pass YAML inline via
	// tasks_data to avoid a temp-file race where the CLI could remove the
	// file before the daemon finishes reading it.
	dataMap := map[string]any{
		"command_id": commandID,
		"phase_name": phaseName,
		"dry_run":    dryRun,
	}
	if tasksFile == "-" || tasksFile == "/dev/stdin" {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, int64(model.DefaultMaxYAMLFileBytes)+1))
		if err != nil {
			return fmt.Errorf("maestro plan submit: read stdin: %w", err)
		}
		if len(data) > model.DefaultMaxYAMLFileBytes {
			return fmt.Errorf("maestro plan submit: stdin input exceeds maximum size of %d bytes", model.DefaultMaxYAMLFileBytes)
		}
		dataMap["tasks_data"] = string(data)
	} else {
		dataMap["tasks_file"] = tasksFile
	}

	params := map[string]any{
		"operation": "submit",
		"data":      dataMap,
	}

	return a.sendPlanCommand("plan submit", maestroDir, params)
}

// runPlanComplete reports command completion to the daemon.
func (a *cliApp) runPlanComplete(args []string) error {
	cmd := NewCommand("maestro plan complete", "maestro plan complete --command-id <id> --summary <text>")
	var commandID, summary string
	cmd.RequiredString(&commandID, "command-id", "Parent command ID")
	cmd.StringVar(&summary, "summary", "", "Completion summary text")

	if err := cmd.Parse(args); err != nil {
		return err
	}

	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}
	if err := validate.ContentLength("--summary", summary, model.DefaultMaxEntryContentBytes); err != nil {
		return cmd.Errorf("%v", err)
	}

	maestroDir, err := requireMaestroDir("plan complete")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "complete",
		"data": map[string]any{
			"command_id": commandID,
			"summary":    summary,
		},
	}

	return a.sendPlanCommand("plan complete", maestroDir, params)
}

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

	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}
	if err := validate.ID(retryOf); err != nil {
		return cmd.Errorf("invalid --retry-of: %v", err)
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

	return a.sendPlanCommand("plan add-retry-task", maestroDir, params)
}

// runPlanRequestCancel requests cancellation of an active command.
func (a *cliApp) runPlanRequestCancel(args []string) error {
	cmd := NewCommand("maestro plan request-cancel", "maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]")
	var commandID, requestedBy, reason string
	cmd.RequiredString(&commandID, "command-id", "Command ID to cancel")
	cmd.StringVar(&requestedBy, "requested-by", "", "Agent or user who requested cancellation")
	cmd.StringVar(&reason, "reason", "", "Reason for cancellation")

	if err := cmd.Parse(args); err != nil {
		return err
	}

	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	if requestedBy == "" {
		requestedBy = "cli"
	}

	maestroDir, err := requireMaestroDir("plan request-cancel")
	if err != nil {
		return err
	}

	// Route through daemon UDS to respect single-writer architecture
	params := map[string]any{
		"target":       "planner",
		"type":         "cancel-request",
		"command_id":   commandID,
		"requested_by": requestedBy,
		"reason":       reason,
	}

	client := a.createClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("queue_write", params)
	if err != nil {
		return fmt.Errorf("maestro plan request-cancel: %w", err)
	}

	if !resp.Success {
		code, msg := udsErrorInfo(resp)
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan request-cancel: [%s] %s", code, msg)}
	}

	fmt.Printf("cancel requested for command %s\n", commandID)
	return nil
}

// runPlanRebuild rebuilds plan state from existing results.
func (a *cliApp) runPlanRebuild(args []string) error {
	cmd := NewCommand("maestro plan rebuild", "maestro plan rebuild --command-id <id>")
	var commandID string
	cmd.RequiredString(&commandID, "command-id", "Command ID to rebuild state for")

	if err := cmd.Parse(args); err != nil {
		return err
	}

	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	maestroDir, err := requireMaestroDir("plan rebuild")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "rebuild",
		"data": map[string]any{
			"command_id": commandID,
		},
	}

	return a.sendPlanCommand("plan rebuild", maestroDir, params)
}

// runPlanUnquarantine clears quarantine state on a command's integration
// branch so the next queue scan can re-enqueue merge attempts.
func (a *cliApp) runPlanUnquarantine(args []string) error {
	cmd := NewCommand("maestro plan unquarantine", "maestro plan unquarantine --command-id <id> [--reason <text>]")
	var commandID, reason string
	cmd.RequiredString(&commandID, "command-id", "Command ID to unquarantine")
	cmd.StringVar(&reason, "reason", "", "Reason for unquarantine")

	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	maestroDir, err := requireMaestroDir("plan unquarantine")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "unquarantine",
		"data": map[string]any{
			"command_id": commandID,
			"reason":     reason,
		},
	}
	return a.sendPlanCommand("plan unquarantine", maestroDir, params)
}

// runPlanResumeMerge resets the merge failure counter and moves a stuck
// integration (conflict / partial_merge / failed) back to a re-mergeable state.
func (a *cliApp) runPlanResumeMerge(args []string) error {
	cmd := NewCommand("maestro plan resume-merge", "maestro plan resume-merge --command-id <id>")
	var commandID string
	cmd.RequiredString(&commandID, "command-id", "Command ID to resume merge for")

	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}

	maestroDir, err := requireMaestroDir("plan resume-merge")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "resume_merge",
		"data": map[string]any{
			"command_id": commandID,
		},
	}
	return a.sendPlanCommand("plan resume-merge", maestroDir, params)
}

// runResolveConflict resolves a worker merge conflict by delegating to the
// daemon's plan handler with the resolve_conflict operation.
//
// Usage:
//
//	maestro resolve-conflict \
//	    --command-id   <id>           # parent command id
//	    --phase-id     <id>           # phase containing the conflicting merge
//	    --worker-id    <id>           # worker whose worktree has the conflict
//	    [--conflicting-files <list>]  # repeat or comma-separated; optional hint
//
// Example:
//
//	maestro resolve-conflict --command-id cmd_42 --phase-id ph_3 \
//	    --worker-id worker2 --conflicting-files internal/a.go,internal/b.go
func (a *cliApp) runResolveConflict(args []string) error {
	cmd := NewCommand("maestro resolve-conflict", "maestro resolve-conflict --command-id <id> --phase-id <id> --worker-id <id> [--conflicting-files <path>[,<path>...]]...")
	var commandID, phaseID, workerID string
	var conflictingFiles stringSliceFlag
	cmd.RequiredString(&commandID, "command-id", "parent command id")
	cmd.RequiredString(&phaseID, "phase-id", "phase id containing the conflict")
	cmd.RequiredString(&workerID, "worker-id", "worker id whose worktree conflicts")
	cmd.Var(&conflictingFiles, "conflicting-files", "conflicting file paths (repeat flag or comma-separated)")

	if err := cmd.Parse(args); err != nil {
		return err
	}
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}
	if err := validate.ID(phaseID); err != nil {
		return cmd.Errorf("invalid --phase-id: %v", err)
	}
	if err := validate.ID(workerID); err != nil {
		return cmd.Errorf("invalid --worker-id: %v", err)
	}

	// Allow comma-separated values inside a single --conflicting-files flag
	// in addition to repeated flag invocations. Empty entries are dropped so
	// "--conflicting-files a.go," does not propagate a blank path.
	files := make([]string, 0, len(conflictingFiles))
	for _, raw := range conflictingFiles {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				files = append(files, p)
			}
		}
	}

	maestroDir, err := requireMaestroDir("resolve-conflict")
	if err != nil {
		return err
	}

	params := map[string]any{
		"operation": "resolve_conflict",
		"data": map[string]any{
			"command_id":        commandID,
			"phase_id":          phaseID,
			"worker_id":         workerID,
			"conflicting_files": files,
		},
	}
	return a.sendPlanCommand("resolve-conflict", maestroDir, params)
}

// sendPlanCommand sends a plan operation to the daemon via UDS.
//
// The request is bounded by [planCommandTimeout] and is interruptible by
// SIGINT/SIGTERM so an operator can abort a hung CLI invocation with Ctrl-C
// without leaving a stuck connection on the daemon side.
func (a *cliApp) sendPlanCommand(cmd string, maestroDir string, params map[string]any) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, planCommandTimeout)
	defer cancelTimeout()

	client := a.createClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommandContext(ctx, "plan", params)
	if err != nil {
		return fmt.Errorf("maestro %s: %w", cmd, err)
	}

	if !resp.Success {
		code, msg := udsErrorInfo(resp)
		if code == uds.ErrCodeValidation || code == uds.ErrCodeActionRequired {
			// Validation messages may have custom formatting; sanitize to prevent terminal injection
			fmt.Fprint(os.Stderr, sanitizeForTerminal(msg))
			return &CLIError{Code: 1, Silent: true}
		}
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro %s: [%s] %s", cmd, code, msg)}
	}

	out, err := json.MarshalIndent(resp.Data, "", "  ")
	if err != nil {
		return fmt.Errorf("maestro %s: format response json: %w", cmd, err)
	}
	fmt.Println(string(out))
	return nil
}
