package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runResult dispatches result subcommands (currently: write).
func runResult(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro result: missing subcommand\nusage: maestro result <write> [options]"}
	}
	switch args[0] {
	case "write":
		return runResultWrite(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro result: unknown subcommand: %s\nusage: maestro result write <reporter> [options]", args[0])}
	}
}

// runResultWrite reports task completion or failure via UDS.
func runResultWrite(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro result write: missing reporter\nusage: maestro result write <reporter> [options]"}
	}

	reporter := args[0]

	fs := newFlagSet("maestro result write")
	var taskID, commandID, resultStatus, summary string
	var leaseEpoch int
	var filesChanged, learnings, skillCandidates stringSliceFlag
	var partialChangesPossible, noRetrySafe bool

	fs.StringVar(&taskID, "task-id", "", "")
	fs.StringVar(&commandID, "command-id", "", "")
	fs.IntVar(&leaseEpoch, "lease-epoch", 0, "")
	fs.StringVar(&resultStatus, "status", "", "")
	fs.StringVar(&summary, "summary", "", "")
	fs.Var(&filesChanged, "files-changed", "")
	fs.Var(&learnings, "learnings", "")
	fs.Var(&skillCandidates, "skill-candidates", "")
	fs.BoolVar(&partialChangesPossible, "partial-changes", false, "")
	fs.BoolVar(&noRetrySafe, "no-retry-safe", false, "")

	if err := fs.Parse(args[1:]); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro result write: %v\nusage: maestro result write <reporter> --task-id <id> --command-id <id> --lease-epoch <n> --status <status> [--summary <text>] [--files-changed <file>]... [--learnings <text>]... [--partial-changes] [--no-retry-safe]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro result write: unexpected argument: %s\nusage: maestro result write <reporter> --task-id <id> --command-id <id> --lease-epoch <n> --status <status> [--summary <text>] [--files-changed <file>]... [--learnings <text>]... [--partial-changes] [--no-retry-safe]", fs.Arg(0))}
	}

	if taskID == "" || commandID == "" || resultStatus == "" {
		return &CLIError{Code: 1, Msg: "maestro result write: --task-id, --command-id, and --status are required\nusage: maestro result write <reporter> --task-id <id> --command-id <id> --lease-epoch <n> --status <status> [--summary <text>]"}
	}

	// Validate IDs
	if err := validate.ValidateID(reporter); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro result write: invalid reporter: %v", err)}
	}
	if err := validate.ValidateID(taskID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro result write: invalid --task-id: %v", err)}
	}
	if err := validate.ValidateID(commandID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro result write: invalid --command-id: %v", err)}
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

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("result_write", params)
	if err != nil {
		return fmt.Errorf("maestro result write: %w", err)
	}

	if !resp.Success {
		code := ""
		msg := "unknown error"
		if resp.Error != nil {
			code = resp.Error.Code
			msg = resp.Error.Message
		}
		if code == "FENCING_REJECT" {
			return &CLIError{Code: 2, Msg: fmt.Sprintf("maestro result write: [%s] %s", code, msg)}
		}
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro result write: [%s] %s", code, msg)}
	}

	out, err := json.MarshalIndent(resp.Data, "", "  ")
	if err != nil {
		return fmt.Errorf("maestro result write: format response json: %w", err)
	}
	fmt.Println(string(out))
	return nil
}
