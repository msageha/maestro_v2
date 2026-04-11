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
func runPlan(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro plan: missing subcommand\nusage: maestro plan <submit|complete|add-retry-task|request-cancel|rebuild|unquarantine|resume-merge> [options]"}
	}
	switch args[0] {
	case "submit":
		return runPlanSubmit(args[1:])
	case "complete":
		return runPlanComplete(args[1:])
	case "add-retry-task":
		return runPlanAddRetryTask(args[1:])
	case "request-cancel":
		return runPlanRequestCancel(args[1:])
	case "rebuild":
		return runPlanRebuild(args[1:])
	case "unquarantine":
		return runPlanUnquarantine(args[1:])
	case "resume-merge":
		return runPlanResumeMerge(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan: unknown subcommand: %s\nusage: maestro plan <submit|complete|add-retry-task|request-cancel|rebuild|unquarantine|resume-merge> [options]", args[0])}
	}
}

// runPlanSubmit submits a task plan for a command.
func runPlanSubmit(args []string) error {
	fs := newFlagSet("maestro plan submit")
	var commandID, tasksFile, phaseName string
	var dryRun bool
	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&tasksFile, "tasks-file", "", "")
	fs.StringVar(&phaseName, "phase", "", "")
	fs.BoolVar(&dryRun, "dry-run", false, "")

	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan submit: %v\nusage: maestro plan submit --command-id <id> [--tasks-file <path>] [--phase <name>] [--dry-run]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan submit: unexpected argument: %s\nusage: maestro plan submit --command-id <id> [--tasks-file <path>] [--phase <name>] [--dry-run]", fs.Arg(0))}
	}

	if commandID == "" {
		return &CLIError{Code: 1, Msg: "maestro plan submit: --command-id is required\nusage: maestro plan submit --command-id <id> [--tasks-file <path>] [--phase <name>] [--dry-run]"}
	}
	if err := validate.ID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan submit: invalid --command-id: %v", err)}
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

	return sendPlanCommand("plan submit", maestroDir, params)
}

// runPlanComplete reports command completion to the daemon.
func runPlanComplete(args []string) error {
	fs := newFlagSet("maestro plan complete")
	var commandID, summary string
	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&summary, "summary", "", "")

	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan complete: %v\nusage: maestro plan complete --command-id <id> --summary <text>", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan complete: unexpected argument: %s\nusage: maestro plan complete --command-id <id> --summary <text>", fs.Arg(0))}
	}

	if commandID == "" {
		return &CLIError{Code: 1, Msg: "maestro plan complete: --command-id is required\nusage: maestro plan complete --command-id <id> --summary <text>"}
	}
	if err := validate.ID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan complete: invalid --command-id: %v", err)}
	}
	if err := validate.ContentLength("--summary", summary, model.DefaultMaxEntryContentBytes); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan complete: %v", err)}
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

	return sendPlanCommand("plan complete", maestroDir, params)
}

// runPlanAddRetryTask replaces a failed task with a new retry task.
func runPlanAddRetryTask(args []string) error {
	fs := newFlagSet("maestro plan add-retry-task")
	var commandID, retryOf, purpose, content, acceptanceCriteria string
	var bloomLevel int
	var blockedBy stringSliceFlag

	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&retryOf, "retry-of", "", "")
	fs.StringVar(&purpose, "purpose", "", "")
	fs.StringVar(&content, "content", "", "")
	fs.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "")
	fs.IntVar(&bloomLevel, "bloom-level", 0, "")
	fs.Var(&blockedBy, "blocked-by", "")

	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan add-retry-task: %v\nusage: maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]...", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan add-retry-task: unexpected argument: %s\nusage: maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]...", fs.Arg(0))}
	}

	if commandID == "" || retryOf == "" || purpose == "" || content == "" || acceptanceCriteria == "" || bloomLevel == 0 {
		return &CLIError{Code: 1, Msg: "maestro plan add-retry-task: all required flags must be set\nusage: maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]..."}
	}

	if err := validate.ID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan add-retry-task: invalid --command-id: %v", err)}
	}
	if err := validate.ID(retryOf); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan add-retry-task: invalid --retry-of: %v", err)}
	}
	for _, dep := range blockedBy {
		if err := validate.ID(dep); err != nil {
			return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan add-retry-task: invalid --blocked-by %q: %v", dep, err)}
		}
	}
	if bloomLevel < 1 || bloomLevel > 6 {
		return &CLIError{Code: 1, Msg: "maestro plan add-retry-task: --bloom-level must be between 1 and 6"}
	}
	for _, pair := range []struct{ name, val string }{
		{"--content", content},
		{"--purpose", purpose},
		{"--acceptance-criteria", acceptanceCriteria},
	} {
		if err := validate.ContentLength(pair.name, pair.val, model.DefaultMaxEntryContentBytes); err != nil {
			return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan add-retry-task: %v", err)}
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

	return sendPlanCommand("plan add-retry-task", maestroDir, params)
}

// runPlanRequestCancel requests cancellation of an active command.
func runPlanRequestCancel(args []string) error {
	fs := newFlagSet("maestro plan request-cancel")
	var commandID, requestedBy, reason string
	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&requestedBy, "requested-by", "", "")
	fs.StringVar(&reason, "reason", "", "")

	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan request-cancel: %v\nusage: maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan request-cancel: unexpected argument: %s\nusage: maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]", fs.Arg(0))}
	}

	if commandID == "" {
		return &CLIError{Code: 1, Msg: "maestro plan request-cancel: --command-id is required\nusage: maestro plan request-cancel --command-id <id> [--requested-by <agent>] [--reason <text>]"}
	}
	if err := validate.ID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan request-cancel: invalid --command-id: %v", err)}
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

	client := newUDSClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("queue_write", params)
	if err != nil {
		return fmt.Errorf("maestro plan request-cancel: %w", err)
	}

	if !resp.Success {
		code := ""
		msg := "unknown error"
		if resp.Error != nil {
			code = resp.Error.Code
			msg = resp.Error.Message
		}
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan request-cancel: [%s] %s", code, msg)}
	}

	fmt.Printf("cancel requested for command %s\n", commandID)
	return nil
}

// runPlanRebuild rebuilds plan state from existing results.
func runPlanRebuild(args []string) error {
	fs := newFlagSet("maestro plan rebuild")
	var commandID string
	fs.StringVar(&commandID, "command-id", "", "")

	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan rebuild: %v\nusage: maestro plan rebuild --command-id <id>", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan rebuild: unexpected argument: %s\nusage: maestro plan rebuild --command-id <id>", fs.Arg(0))}
	}

	if commandID == "" {
		return &CLIError{Code: 1, Msg: "maestro plan rebuild: --command-id is required\nusage: maestro plan rebuild --command-id <id>"}
	}
	if err := validate.ID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan rebuild: invalid --command-id: %v", err)}
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

	return sendPlanCommand("plan rebuild", maestroDir, params)
}

// runPlanUnquarantine clears quarantine state on a command's integration
// branch so the next queue scan can re-enqueue merge attempts.
func runPlanUnquarantine(args []string) error {
	fs := newFlagSet("maestro plan unquarantine")
	var commandID, reason string
	fs.StringVar(&commandID, "command-id", "", "")
	fs.StringVar(&reason, "reason", "", "")

	usage := "usage: maestro plan unquarantine --command-id <id> [--reason <text>]"
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan unquarantine: %v\n%s", err, usage)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan unquarantine: unexpected argument: %s\n%s", fs.Arg(0), usage)}
	}
	if commandID == "" {
		return &CLIError{Code: 1, Msg: "maestro plan unquarantine: --command-id is required\n" + usage}
	}
	if err := validate.ID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan unquarantine: invalid --command-id: %v", err)}
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
	return sendPlanCommand("plan unquarantine", maestroDir, params)
}

// runPlanResumeMerge resets the merge failure counter and moves a stuck
// integration (conflict / partial_merge / failed) back to a re-mergeable state.
func runPlanResumeMerge(args []string) error {
	fs := newFlagSet("maestro plan resume-merge")
	var commandID string
	fs.StringVar(&commandID, "command-id", "", "")

	usage := "usage: maestro plan resume-merge --command-id <id>"
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan resume-merge: %v\n%s", err, usage)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan resume-merge: unexpected argument: %s\n%s", fs.Arg(0), usage)}
	}
	if commandID == "" {
		return &CLIError{Code: 1, Msg: "maestro plan resume-merge: --command-id is required\n" + usage}
	}
	if err := validate.ID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan resume-merge: invalid --command-id: %v", err)}
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
	return sendPlanCommand("plan resume-merge", maestroDir, params)
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
func runResolveConflict(args []string) error {
	fs := newFlagSet("maestro resolve-conflict")
	var commandID, phaseID, workerID string
	var conflictingFiles stringSliceFlag
	fs.StringVar(&commandID, "command-id", "", "parent command id")
	fs.StringVar(&phaseID, "phase-id", "", "phase id containing the conflict")
	fs.StringVar(&workerID, "worker-id", "", "worker id whose worktree conflicts")
	fs.Var(&conflictingFiles, "conflicting-files", "conflicting file paths (repeat flag or comma-separated)")

	usage := "usage: maestro resolve-conflict --command-id <id> --phase-id <id> --worker-id <id> [--conflicting-files <path>[,<path>...]]..."
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro resolve-conflict: %v\n%s", err, usage)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro resolve-conflict: unexpected argument: %s\n%s", fs.Arg(0), usage)}
	}
	if commandID == "" {
		return &CLIError{Code: 1, Msg: "maestro resolve-conflict: --command-id is required\n" + usage}
	}
	if err := validate.ID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro resolve-conflict: invalid --command-id: %v", err)}
	}
	if phaseID == "" {
		return &CLIError{Code: 1, Msg: "maestro resolve-conflict: --phase-id is required\n" + usage}
	}
	if err := validate.ID(phaseID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro resolve-conflict: invalid --phase-id: %v", err)}
	}
	if workerID == "" {
		return &CLIError{Code: 1, Msg: "maestro resolve-conflict: --worker-id is required\n" + usage}
	}
	if err := validate.ID(workerID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro resolve-conflict: invalid --worker-id: %v", err)}
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
	return sendPlanCommand("resolve-conflict", maestroDir, params)
}

// sendPlanCommand sends a plan operation to the daemon via UDS.
//
// The request is bounded by [planCommandTimeout] and is interruptible by
// SIGINT/SIGTERM so an operator can abort a hung CLI invocation with Ctrl-C
// without leaving a stuck connection on the daemon side.
func sendPlanCommand(cmd string, maestroDir string, params map[string]any) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, planCommandTimeout)
	defer cancelTimeout()

	client := newUDSClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommandContext(ctx, "plan", params)
	if err != nil {
		return fmt.Errorf("maestro %s: %w", cmd, err)
	}

	if !resp.Success {
		code := ""
		msg := "unknown error"
		if resp.Error != nil {
			code = resp.Error.Code
			msg = resp.Error.Message
		}
		if code == uds.ErrCodeValidation || code == uds.ErrCodeActionRequired {
			// Validation messages may have custom formatting; print directly
			fmt.Fprint(os.Stderr, msg)
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
