package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runPlan dispatches plan subcommands (submit, complete, add-retry-task, request-cancel, rebuild).
func runPlan(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro plan: missing subcommand\nusage: maestro plan <submit|complete|add-retry-task|request-cancel|rebuild> [options]"}
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
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan: unknown subcommand: %s\nusage: maestro plan <submit|complete|add-retry-task|request-cancel|rebuild> [options]", args[0])}
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

	if tasksFile == "" {
		tasksFile = "-" // default to stdin
	}

	// CRIT-05: Validate non-stdin file path before passing to daemon
	if tasksFile != "-" && tasksFile != "/dev/stdin" {
		cleaned, err := validate.ValidateFilePath(tasksFile)
		if err != nil {
			return fmt.Errorf("maestro plan submit: invalid tasks file: %w", err)
		}
		tasksFile = cleaned
	}

	maestroDir, err := requireMaestroDir("plan submit")
	if err != nil {
		return err
	}

	// If reading from stdin, materialize to a temp file so the daemon can read it
	// (daemon's stdin is not the CLI's stdin when using UDS)
	actualFile := tasksFile
	if tasksFile == "-" || tasksFile == "/dev/stdin" {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, int64(model.DefaultMaxYAMLFileBytes)+1))
		if err != nil {
			return fmt.Errorf("maestro plan submit: read stdin: %w", err)
		}
		if len(data) > model.DefaultMaxYAMLFileBytes {
			return fmt.Errorf("maestro plan submit: stdin input exceeds maximum size of %d bytes", model.DefaultMaxYAMLFileBytes)
		}
		tmpFile, err := os.CreateTemp("", "maestro-plan-submit-*.yaml")
		if err != nil {
			return fmt.Errorf("maestro plan submit: create temp file: %w", err)
		}
		defer func() { _ = os.Remove(tmpFile.Name()) }()
		if _, err := tmpFile.Write(data); err != nil {
			_ = tmpFile.Close()
			return fmt.Errorf("maestro plan submit: write temp file: %w", err)
		}
		_ = tmpFile.Close()
		actualFile = tmpFile.Name()
	}

	params := map[string]any{
		"operation": "submit",
		"data": map[string]any{
			"command_id": commandID,
			"tasks_file": actualFile,
			"phase_name": phaseName,
			"dry_run":    dryRun,
		},
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

	if commandID == "" || retryOf == "" || purpose == "" || content == "" || acceptanceCriteria == "" {
		return &CLIError{Code: 1, Msg: "maestro plan add-retry-task: all required flags must be set\nusage: maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]..."}
	}
	if bloomLevel <= 0 {
		return &CLIError{Code: 1, Msg: "maestro plan add-retry-task: --bloom-level must be a positive integer\nusage: maestro plan add-retry-task --command-id <id> --retry-of <task_id> --purpose <text> --content <text> --acceptance-criteria <text> --bloom-level <n> [--blocked-by <task_id>]..."}
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

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
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

// sendPlanCommand sends a plan operation to the daemon via UDS.
func sendPlanCommand(cmd string, maestroDir string, params map[string]any) error {
	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("plan", params)
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
