package main

import (
	"fmt"
	"log/slog"

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

	cmd := NewCommand("maestro result write", "maestro result write <reporter> --task-id <id> --command-id <id> --lease-epoch <n> --status <status> [--summary <text>] [--files-changed <file>]... [--learnings <text>]... [--skill-candidates <text>]... [--partial-changes] [--no-retry-safe] [--exit-code <n>]")
	var taskID, commandID, resultStatus, summary string
	var leaseEpoch, exitCode int
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
	// --exit-code: Worker 子プロセスの終了コード。省略時は -1 (= 未報告)。
	// daemon の retry policy 判定 (ShouldRetryTask) は exit code を必須入力とする
	// ため、--status failed の場合は worker が必ずこの値を渡すこと。
	cmd.IntVar(&exitCode, "exit-code", -1, "Worker process exit code (required for failed status to drive auto-retry)")

	cmd.AddCheck("--task-id, --command-id, --lease-epoch, and --status are required", func() bool {
		return taskID != "" && commandID != "" && resultStatus != "" && leaseEpoch >= 0
	})

	cmd.AddCheck("--status must be 'completed' or 'failed'", func() bool {
		return resultStatus == "" || resultStatus == "completed" || resultStatus == "failed"
	})

	// --status failed では --exit-code が必須。自動リトライの判定は exit code に
	// 依存するので、未指定だと daemon 側 evaluateRetry が即 return し
	// repair pipeline が走らなくなる (silent drop) のを防ぐ。
	cmd.AddCheck("--exit-code is required when --status=failed", func() bool {
		return resultStatus != "failed" || exitCode >= 0
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

	// Validate and truncate individual entries for repeatable flags.
	truncateEntries("--learnings", learnings, model.DefaultMaxEntryContentBytes)
	truncateEntries("--skill-candidates", skillCandidates, model.DefaultMaxEntryContentBytes)
	truncateEntries("--files-changed", filesChanged, model.DefaultMaxEntryContentBytes)

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
	// exit-code が明示された (>= 0) 場合のみ daemon に渡す。-1 は「worker 未報告」
	// の sentinel として扱い、completed の場合は省略する (daemon 側で nil 扱い)。
	if exitCode >= 0 {
		params["exit_code"] = exitCode
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
		// F-019 step 2: structured fencing exit codes when the daemon
		// surfaced FencingDetails. Unknown error codes still fall through
		// to the legacy generic handler.
		if exit := classifyFencingExitCode(resp); exit != 0 {
			return fencingCLIError(resp, false, "maestro result write")
		}
		code, msg := udsErrorInfo(resp)
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro result write: [%s] %s", code, msg)}
	}

	return printJSONResponse(resp.Data, "result write")
}

// truncateEntries checks each entry in entries against maxBytes and truncates
// oversized entries in place with a warning log. This is a graceful approach:
// oversized entries are truncated rather than rejected, allowing the command
// to proceed while alerting operators via logs.
func truncateEntries(flag string, entries stringSliceFlag, maxBytes int) {
	for i, entry := range entries {
		if len(entry) > maxBytes {
			slog.Warn("truncating oversized entry",
				"flag", flag,
				"index", i,
				"original_bytes", len(entry),
				"max_bytes", maxBytes,
			)
			entries[i] = entry[:maxBytes]
		}
	}
}
