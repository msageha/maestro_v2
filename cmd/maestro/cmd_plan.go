package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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

// planPhaseFillTimeout is an extended timeout for phase fill operations
// (plan submit --phase). Phase fills during conflict recovery contend with
// the daemon's PeriodicScan exclusive lock (scanMu), which can delay the
// plan handler's shared lock acquisition beyond the standard 30s window.
const planPhaseFillTimeout = 120 * time.Second

// runPlan dispatches plan subcommands (submit, complete, add-retry-task, add-task, request-cancel, rebuild).
func (a *cliApp) runPlan(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro plan: missing subcommand\nusage: maestro plan <submit|complete|add-retry-task|add-task|request-cancel|rebuild|unquarantine|resume-merge> [options]"}
	}
	switch args[0] {
	case "submit":
		return a.runPlanSubmit(args[1:])
	case "complete":
		return a.runPlanComplete(args[1:])
	case "add-retry-task":
		return a.runPlanAddRetryTask(args[1:])
	case "add-task":
		return a.runPlanAddTask(args[1:])
	case "request-cancel":
		return a.runPlanRequestCancel(args[1:])
	case "rebuild":
		return a.runPlanRebuild(args[1:])
	case "unquarantine":
		return a.runPlanUnquarantine(args[1:])
	case "resume-merge":
		return a.runPlanResumeMerge(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan: unknown subcommand: %s\nusage: maestro plan <submit|complete|add-retry-task|add-task|request-cancel|rebuild|unquarantine|resume-merge> [options]", args[0])}
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

	// Phase fill operations (conflict recovery) contend with the daemon's
	// PeriodicScan exclusive lock, so they use an extended timeout.
	timeout := planCommandTimeout
	if phaseName != "" {
		timeout = planPhaseFillTimeout
	}
	return a.sendPlanCommand("plan submit", maestroDir, params, timeout)
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

	return a.sendPlanCommand("plan complete", maestroDir, params, planCommandTimeout)
}

// sendPlanCommand sends a plan operation to the daemon via UDS.
//
// The request is bounded by the given timeout and is interruptible by
// SIGINT/SIGTERM so an operator can abort a hung CLI invocation with Ctrl-C
// without leaving a stuck connection on the daemon side.
func (a *cliApp) sendPlanCommand(cmd string, maestroDir string, params map[string]any, timeout time.Duration) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()

	client := a.createClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	// Align the UDS connection deadline with the operation timeout so that
	// the socket does not expire before the context.
	client.SetTimeout(timeout)
	resp, err := client.SendCommandContext(ctx, "plan", params)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("maestro %s: timed out after %v waiting for daemon response (the daemon may be busy with a scan cycle — consider retrying): %w", cmd, timeout, err)
		}
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
