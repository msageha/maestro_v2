package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runQueue dispatches queue subcommands (currently: write).
func (a *cliApp) runQueue(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro queue: missing subcommand\nusage: maestro queue <write> [options]"}
	}
	switch args[0] {
	case "write":
		return a.runQueueWrite(args[1:], os.Stderr)
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue: unknown subcommand: %s\nusage: maestro queue write <target> [options]", args[0])}
	}
}

// buildCommandWriteParams validates and builds params for a command queue write.
func buildCommandWriteParams(params map[string]any, content string, priority int, skillRefs []string) error {
	if content == "" {
		return &CLIError{Code: 1, Msg: "maestro queue write: --content is required for type=command"}
	}
	if err := validate.ContentLength("--content", content, model.DefaultMaxEntryContentBytes); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: %v", err)}
	}
	params["content"] = content
	if priority > 0 {
		params["priority"] = priority
	}
	if len(skillRefs) > 0 {
		params["skill_refs"] = skillRefs
	}
	return nil
}

// buildNotificationWriteParams validates and builds params for a notification queue write.
func buildNotificationWriteParams(params map[string]any, commandID, content, sourceResultID, notificationType string, priority int) error {
	if commandID == "" || content == "" || sourceResultID == "" {
		return &CLIError{Code: 1, Msg: "maestro queue write: required for type=notification: --command-id, --content, --source-result-id"}
	}
	if err := validate.ID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: invalid --command-id: %v", err)}
	}
	if err := validate.ID(sourceResultID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: invalid --source-result-id: %v", err)}
	}
	if err := validate.ContentLength("--content", content, model.DefaultMaxEntryContentBytes); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: %v", err)}
	}
	params["command_id"] = commandID
	params["content"] = content
	params["source_result_id"] = sourceResultID
	if notificationType != "" {
		params["notification_type"] = notificationType
	}
	if priority > 0 {
		params["priority"] = priority
	}
	return nil
}

// buildCancelRequestWriteParams validates and builds params for a cancel-request queue write.
// It also emits a deprecation warning to warnOut (H7).
func buildCancelRequestWriteParams(params map[string]any, commandID, reason string, warnOut io.Writer) error {
	if commandID == "" {
		return &CLIError{Code: 1, Msg: "maestro queue write: required for type=cancel-request: --command-id"}
	}
	if err := validate.ID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: invalid --command-id: %v", err)}
	}
	params["command_id"] = commandID
	if reason != "" {
		params["reason"] = reason
	}
	// H7: deprecated CLI surface. The canonical entrypoint is
	// `maestro plan request-cancel`. Both routes converge at the
	// daemon's queue_write(type=cancel-request) handler, but the
	// queue-write surface is retained only for backward compatibility
	// and emits a warning so operators migrate.
	_, _ = fmt.Fprintln(warnOut,
		"maestro queue write: WARNING: --type cancel-request is deprecated; use `maestro plan request-cancel --command-id <id>` instead")
	return nil
}

// runQueueWrite enqueues a command, task, notification, or cancel-request via UDS.
// warnOut is the destination for deprecation warnings.
func (a *cliApp) runQueueWrite(args []string, warnOut io.Writer) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro queue write: missing target\nusage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]"}
	}

	target := args[0]

	cmd := NewCommand("maestro queue write", "maestro queue write <target> --type <command|task|notification|cancel-request> [options]")
	var writeType, content, commandID, sourceResultID, notificationType, reason string
	var priority int

	cmd.RequiredString(&writeType, "type", "Entry type: command, task, notification, or cancel-request")
	cmd.StringVar(&content, "content", "", "Entry content text")
	cmd.StringVar(&commandID, "command-id", "", "Parent command ID")
	cmd.IntVar(&priority, "priority", 0, "Entry priority (higher = more urgent)")
	cmd.StringVar(&sourceResultID, "source-result-id", "", "Source result ID for notifications")
	cmd.StringVar(&notificationType, "notification-type", "", "Notification type classifier")
	cmd.StringVar(&reason, "reason", "", "Reason text for cancel requests")
	var skillRefs stringSliceFlag
	cmd.Var(&skillRefs, "skill-refs", "Skill reference (repeatable)")

	// Task-specific flags: registered for parse compatibility but tasks cannot
	// be created via CLI (Planner exclusive; audit C3). See "task" case below.
	var purpose, acceptanceCriteria, personaHint string
	var bloomLevel int
	var blockedBy, constraints, toolsHint stringSliceFlag
	cmd.StringVar(&purpose, "purpose", "", "Task purpose description (task only)")
	cmd.StringVar(&acceptanceCriteria, "acceptance-criteria", "", "Task acceptance criteria (task only)")
	cmd.IntVar(&bloomLevel, "bloom-level", 0, "Bloom taxonomy level 1-6 (task only)")
	cmd.Var(&blockedBy, "blocked-by", "Task ID dependency, repeatable (task only)")
	cmd.Var(&constraints, "constraint", "Task constraint, repeatable (task only)")
	cmd.Var(&toolsHint, "tools-hint", "Recommended tool hint, repeatable (task only)")
	cmd.StringVar(&personaHint, "persona-hint", "", "Persona hint for task execution (task only)")

	if err := cmd.Parse(args[1:]); err != nil {
		return err
	}

	params := map[string]any{
		"target": target,
		"type":   writeType,
	}

	switch writeType {
	case "command":
		// Commands have a single canonical consumer (the Planner) — the
		// daemon's queue_write handler always writes to planner.yaml and
		// silently ignores the target argument. Reject any other target
		// at the CLI so operators get an immediate error instead of a
		// confusing "I queued it but the wrong agent reacted" experience
		// (the 2026-04-29 e2e session hit exactly that — `maestro queue
		// write orchestrator --type command ...` looked successful but
		// Planner consumed the entry and Orchestrator stayed idle).
		if target != "planner" {
			return &CLIError{Code: 1, Msg: fmt.Sprintf(
				"maestro queue write: --type command requires target=planner (the only command consumer); got target=%q",
				target,
			)}
		}
		if err := buildCommandWriteParams(params, content, priority, []string(skillRefs)); err != nil {
			return err
		}
	case "task":
		// Task creation is the Planner's exclusive responsibility (audit C3).
		// Task-specific flags are registered for parse compatibility but
		// intentionally unused here.
		return &CLIError{Code: 1, Msg: "maestro queue write: --type task is not supported via CLI; tasks must be created through the Planner (maestro plan submit / maestro plan retry-task)"}
	case "notification":
		if err := buildNotificationWriteParams(params, commandID, content, sourceResultID, notificationType, priority); err != nil {
			return err
		}
	case "cancel-request":
		if err := buildCancelRequestWriteParams(params, commandID, reason, warnOut); err != nil {
			return err
		}
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: unknown type: %s\nusage: maestro queue write <target> --type <command|task|notification|cancel-request> [options]", writeType)}
	}

	return a.sendQueueWrite(params)
}

// sendQueueWrite sends a queue_write request to the daemon via UDS.
func (a *cliApp) sendQueueWrite(params map[string]any) error {
	maestroDir, err := requireMaestroDir("queue write")
	if err != nil {
		return err
	}

	client := a.newDaemonClient(maestroDir)
	resp, err := client.SendCommand("queue_write", params)
	if err != nil {
		return fmt.Errorf("maestro queue write: %w", err)
	}

	if !resp.Success {
		code, msg := udsErrorInfo(resp)
		if code == "BACKPRESSURE" {
			return &CLIError{Code: ExitCodeRetryable, Msg: fmt.Sprintf("maestro queue write: [%s] %s", code, msg)}
		}
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: [%s] %s", code, msg)}
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro queue write: failed to parse response: %v", err)}
	}
	if id, ok := result["id"]; ok {
		fmt.Println(id)
		return nil
	}
	if cid, ok := result["command_id"]; ok {
		fmt.Println(cid)
		return nil
	}
	return &CLIError{Code: 1, Msg: "maestro queue write: response missing both id and command_id"}
}
