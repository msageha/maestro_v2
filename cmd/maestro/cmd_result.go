package main

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runResult dispatches result subcommands (currently: write).
func (a *cliApp) runResult(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro result: missing subcommand\nusage: maestro result <write> [options]"}
	}
	switch args[0] {
	case "write":
		return a.runResultWrite(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro result: unknown subcommand: %s\nusage: maestro result write <reporter> [options]", args[0])}
	}
}

// runResultWrite reports task completion or failure via UDS.
func (a *cliApp) runResultWrite(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro result write: missing reporter\nusage: maestro result write <reporter> [options]"}
	}

	reporter := args[0]

	cmd := NewCommand("maestro result write", "maestro result write <reporter> --task-id <id> --command-id <id> --lease-epoch <n> --status <status> [--summary <text>] [--files-changed <file>]... [--learnings <text>]... [--skill-candidates <text>]... [--partial-changes] [--no-retry-safe]")
	var taskID, commandID, resultStatus, summary string
	var leaseEpoch int
	var filesChanged, learnings, skillCandidates stringSliceFlag
	var partialChangesPossible, noRetrySafe bool

	cmd.StringVar(&taskID, "task-id", "", "Task ID to report result for")
	cmd.StringVar(&commandID, "command-id", "", "Parent command ID")
	cmd.IntVar(&leaseEpoch, "lease-epoch", -1, "Lease epoch number for fencing")
	cmd.StringVar(&resultStatus, "status", "", "Result status: completed or failed")
	cmd.StringVar(&summary, "summary", "", "Result summary text")
	cmd.Var(&filesChanged, "files-changed", "Changed file path (repeatable)")
	cmd.Var(&learnings, "learnings", "Learning insight for other tasks (repeatable)")
	cmd.Var(&skillCandidates, "skill-candidates", "Skill candidate to report (repeatable)")
	cmd.BoolVar(&partialChangesPossible, "partial-changes", false, "Partial changes remain in repo")
	cmd.BoolVar(&noRetrySafe, "no-retry-safe", false, "Mark task as not safe to retry")

	cmd.AddCheck("--task-id, --command-id, --lease-epoch, and --status are required", func() bool {
		return taskID != "" && commandID != "" && resultStatus != "" && leaseEpoch >= 0
	})

	cmd.AddCheck("--status must be 'completed' or 'failed'", func() bool {
		return resultStatus == "" || resultStatus == "completed" || resultStatus == "failed"
	})

	if err := cmd.Parse(args[1:]); err != nil {
		return err
	}

	// Validate IDs
	if err := validate.ID(reporter); err != nil {
		return cmd.Errorf("invalid reporter: %v", err)
	}
	if err := validate.ID(taskID); err != nil {
		return cmd.Errorf("invalid --task-id: %v", err)
	}
	if err := validate.ID(commandID); err != nil {
		return cmd.Errorf("invalid --command-id: %v", err)
	}
	if err := validate.ContentLength("--summary", summary, model.DefaultMaxEntryContentBytes); err != nil {
		return cmd.Errorf("%v", err)
	}

	maestroDir, err := requireMaestroDir("result write")
	if err != nil {
		return err
	}

	params := map[string]any{
		"reporter":    reporter,
		"task_id":     taskID,
		"command_id":  commandID,
		"lease_epoch": leaseEpoch,
		"status":      resultStatus,
		"summary":     summary,
		"retry_safe":  !noRetrySafe,
	}
	if len(filesChanged) > 0 {
		params["files_changed"] = filesChanged
	}
	if partialChangesPossible {
		params["partial_changes_possible"] = true
	}
	if len(learnings) > 0 {
		params["learnings"] = learnings
	}
	if len(skillCandidates) > 0 {
		params["skill_candidates"] = skillCandidates
	}

	client := a.newDaemonClient(maestroDir)
	resp, err := client.SendCommand("result_write", params)
	if err != nil {
		return fmt.Errorf("maestro result write: %w", err)
	}

	if !resp.Success {
		code, msg := udsErrorInfo(resp)
		if code == "FENCING_REJECT" {
			return &CLIError{Code: 2, Msg: fmt.Sprintf("maestro result write: [%s] %s", code, msg)}
		}
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro result write: [%s] %s", code, msg)}
	}

	return printJSONResponse(resp.Data, "result write")
}
