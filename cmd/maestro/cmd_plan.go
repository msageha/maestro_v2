package main

import (
	"context"
	"errors"
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
		return &CLIError{Code: 1, Msg: "maestro plan: missing subcommand\nusage: maestro plan <submit|complete|add-retry-task|add-task|request-cancel|rebuild|recover|unquarantine|resume-merge|retry-publish|resolve-conflict> [options]"}
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
	case "recover":
		return a.runPlanRecover(args[1:])
	case "unquarantine":
		return a.runPlanUnquarantine(args[1:])
	case "resume-merge":
		return a.runPlanResumeMerge(args[1:])
	case "retry-publish":
		return a.runPlanRetryPublish(args[1:])
	case "resolve-conflict":
		return a.runResolveConflict(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro plan: unknown subcommand: %s\nusage: maestro plan <submit|complete|add-retry-task|add-task|request-cancel|rebuild|recover|unquarantine|resume-merge|retry-publish|resolve-conflict> [options]", args[0])}
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

	// Validate non-stdin file path before passing to daemon. The path is
	// resolved to an absolute one because the daemon opens it from its own
	// working directory (wherever `maestro up` ran), not from this CLI's —
	// a relative path like ./tasks.yaml from a Planner worktree would
	// otherwise fail with a not-found error the agent cannot self-correct.
	if tasksFile != "-" && tasksFile != "/dev/stdin" {
		cleaned, err := validate.FilePath(tasksFile)
		if err != nil {
			return fmt.Errorf("maestro plan submit: invalid tasks file: %w", err)
		}
		abs, err := filepath.Abs(cleaned)
		if err != nil {
			return fmt.Errorf("maestro plan submit: resolve tasks file path: %w", err)
		}
		tasksFile = abs
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
		if isStdinTerminal() {
			return &CLIError{Code: 1, Msg: "maestro plan submit: stdin is a terminal and no tasks input was piped; pass --tasks-file <path> or pipe the tasks YAML into stdin"}
		}
		data, err := io.ReadAll(io.LimitReader(os.Stdin, int64(maxInlineUDSPayloadBytes)+1))
		if err != nil {
			return fmt.Errorf("maestro plan submit: read stdin: %w", err)
		}
		if len(data) > maxInlineUDSPayloadBytes {
			return fmt.Errorf("maestro plan submit: stdin input exceeds the %d-byte inline limit (UDS frame cap); write the YAML to a file and pass --tasks-file <path> so the daemon reads it directly", maxInlineUDSPayloadBytes)
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
//
// Mirroring the (--content | --content-file) pattern from `plan add-task` /
// `plan add-retry-task`, --summary-file is accepted as an alternative source
// for the summary text. The two flags are mutually exclusive (the daemon
// must receive a single unambiguous value) and the file path is read with
// the same content-size validation as the inline flag.
func (a *cliApp) runPlanComplete(args []string) error {
	cmd := NewCommand("maestro plan complete", "maestro plan complete --command-id <id> (--summary <text> | --summary-file <path>)")
	var commandID, summary, summaryFile string
	cmd.RequiredString(&commandID, "command-id", "Parent command ID")
	cmd.StringVar(&summary, "summary", "", "Completion summary text (mutually exclusive with --summary-file)")
	cmd.StringVar(&summaryFile, "summary-file", "", "Read completion summary from a file or '-' for stdin (mutually exclusive with --summary)")

	if err := cmd.Parse(args); err != nil {
		return err
	}

	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}
	if err := resolveSummaryFile(cmd, &summary, summaryFile); err != nil {
		return err
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

// resolveSummaryFile resolves --summary-file into the summary string. It
// rejects mixed sources so the value sent to the daemon has a single
// obvious origin. Accepts "-" or "/dev/stdin" to read from stdin, matching
// `plan submit --tasks-file -`; Planner agents previously failed with
// `open -: no such file or directory` and had to fall back to a temp
// file. Mirrors resolveContentFile in cmd_plan_tasks.go.
func resolveSummaryFile(cmd *CommandBuilder, summary *string, summaryFile string) error {
	if summaryFile == "" {
		return nil
	}
	if *summary != "" {
		return cmd.Errorf("--summary and --summary-file are mutually exclusive")
	}
	b, err := readFlagInputFile("--summary-file", summaryFile, model.DefaultMaxEntryContentBytes)
	if err != nil {
		return cmd.Errorf("%v", err)
	}
	*summary = string(b)
	return nil
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

	client := a.newDaemonClient(maestroDir)
	// Align the UDS connection deadline with the operation timeout so that
	// the socket does not expire before the context.
	client.SetTimeout(timeout)
	resp, err := client.SendCommandContext(ctx, "plan", params)
	if err != nil {
		return planSendError(cmd, ctx.Err(), err, timeout)
	}

	if !resp.Success {
		code, msg := udsErrorInfo(resp)
		if code == uds.ErrCodeValidation || code == uds.ErrCodeActionRequired {
			// Validation messages may have custom formatting; sanitize to prevent terminal injection
			fmt.Fprintf(os.Stderr, "maestro %s: %s\n", cmd, sanitizeForTerminal(msg))
			return &CLIError{Code: 1, Silent: true}
		}
		return udsCLIError("maestro "+cmd, resp)
	}

	return printJSONResponse(resp.Data, cmd)
}

// planSendError classifies a failed plan RPC by the context state at the
// time of failure: a deadline expiry is reported as a timeout (retry
// guidance), a signal-driven cancellation as an interrupt (the previous
// behaviour reported Ctrl-C as "timed out", pointing operators at the wrong
// diagnosis), and everything else as a plain transport error.
func planSendError(cmd string, ctxErr, sendErr error, timeout time.Duration) error {
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		return fmt.Errorf("maestro %s: timed out after %v waiting for daemon response (the daemon may be busy with a scan cycle — consider retrying): %w", cmd, timeout, sendErr)
	case ctxErr != nil:
		return fmt.Errorf("maestro %s: canceled by interrupt signal while waiting for daemon response: %w", cmd, sendErr)
	default:
		return fmt.Errorf("maestro %s: %w", cmd, sendErr)
	}
}
