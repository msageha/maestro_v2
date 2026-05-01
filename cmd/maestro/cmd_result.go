package main

import (
	"fmt"
	"log/slog"
	"strings"

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
//
// 2026-04-30 e2e regression (Worker policy-hook D001 false-positive):
// long human-readable summaries passed inline as `--summary "..."` are
// observed end-to-end as a Bash command string by the Worker PreToolUse
// policy hook (Worker invokes `maestro result write` via Bash). When the
// summary text happened to contain the substring `rm -rf /Users/...` or
// any other D001 trigger as part of describing the work, the hook denied
// the command. The agent then retried with a degenerate `--summary "test
// summary"` placeholder, which Phase A accepted as the canonical result
// and short-circuited the legitimate retry as a duplicate, leaving the
// command stalled with a placeholder result.
//
// The fix is to give Worker an off-argv path for the summary text so the
// policy hook only ever scans short, structured argument values. Mirrors
// `plan complete --summary-file` (cmd_plan.go) and the shared
// readFlagInputFile pattern: accepts a path or "-"/"/dev/stdin" for
// stdin. --summary and --summary-file are mutually exclusive so the
// daemon receives a single unambiguous value.
func (a *cliApp) runResultWrite(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro result write: missing reporter\nusage: maestro result write <reporter> [options]"}
	}

	reporter := args[0]

	cmd := NewCommand("maestro result write", "maestro result write <reporter> --task-id <id> --command-id <id> --lease-epoch <n> --status <status> [--summary <text> | --summary-file <path>] [--files-changed <file>]... [--learnings <text>]... [--skill-candidates <text>]... [--partial-changes] [--no-retry-safe] [--exit-code <n>]")
	var taskID, commandID, resultStatus, summary, summaryFile string
	var leaseEpoch, exitCode int
	var filesChanged, learnings, skillCandidates stringSliceFlag
	var partialChangesPossible, noRetrySafe bool

	cmd.StringVar(&taskID, "task-id", "", "Task ID to report result for")
	cmd.StringVar(&commandID, "command-id", "", "Parent command ID")
	cmd.IntVar(&leaseEpoch, "lease-epoch", -1, "Lease epoch number for fencing")
	cmd.StringVar(&resultStatus, "status", "", "Result status: completed or failed")
	cmd.StringVar(&summary, "summary", "", "Result summary text (mutually exclusive with --summary-file)")
	cmd.StringVar(&summaryFile, "summary-file", "", "Read summary from a file or '-' for stdin (mutually exclusive with --summary; recommended for long summaries to avoid Worker policy-hook substring scanning)")
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
	// Resolve --summary-file before length validation so both inline and
	// file-backed values flow through the same DefaultMaxEntryContentBytes
	// guard. resolveSummaryFile rejects the both-set case with a clear
	// error so the daemon never receives ambiguous input.
	if err := resolveSummaryFile(cmd, &summary, summaryFile); err != nil {
		return err
	}
	if err := validate.ContentLength("--summary", summary, model.DefaultMaxEntryContentBytes); err != nil {
		return cmd.Errorf("%v", err)
	}
	// Reject obvious placeholder summaries so a worker that lost its real
	// result (e.g. policy-hook D001 false-positive followed by a degenerate
	// retry "test summary") cannot silently land its placeholder as the
	// canonical result. The 2026-04-30 e2e regression observed exactly this
	// flow: phase diagnosis reported success because every queue task was
	// `completed`, but the underlying summary held only "test minimal" /
	// "test summary" — the actual implementation report was lost in a
	// duplicate_short_circuited reply on the legitimate retry. Rejecting
	// these patterns at the CLI boundary forces the worker to construct a
	// meaningful summary (or use --summary-file for long content) and
	// surfaces the failure as a clean validation error rather than a
	// silently-completed task.
	if err := validateSummaryNotPlaceholder("--summary", summary, resultStatus); err != nil {
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

// summaryPlaceholderPatterns lists exact-match (case-folded, whitespace-
// normalised) summary values that are recognised as placeholder content and
// rejected by validateSummaryNotPlaceholder. The list is intentionally
// limited to short, content-free strings that real Worker reports never
// produce — adding entries that legitimate summaries might match (e.g.,
// "no changes") would create false positives that block working calls.
var summaryPlaceholderPatterns = map[string]struct{}{
	"":             {},
	"test":         {},
	"test summary": {},
	"test minimal": {},
	"summary":      {},
	"placeholder":  {},
	"todo":         {},
	"tbd":          {},
	"foo":          {},
	"bar":          {},
	"foobar":       {},
	"ok":           {},
	"okay":         {},
	"done":         {},
	"completed":    {},
	"complete":     {},
	"success":      {},
	"successful":   {},
	"finished":     {},
	"n/a":          {},
	"na":           {},
	"none":         {},
	"empty":        {},
	"x":            {},
	"y":            {},
	".":            {},
	"-":            {},
}

// minSummaryLengthForCompleted is the lower bound on a normalised summary's
// rune count for a `--status completed` write. The value is set well below
// the typical worker report length (60–500+ runes) so legitimate short
// reports such as "fixed off-by-one in foo.go; tests pass" still clear the
// gate, but degenerate placeholders from a worker scrambling after a
// policy-hook denial (the 2026-04-30 regression) are rejected. `failed`
// status is not subject to this floor — failure-mode reports are sometimes
// genuinely terse ("worker timeout"), and rejecting them would make
// post-mortem reporting harder.
const minSummaryLengthForCompleted = 16

// validateSummaryNotPlaceholder returns an error when summary matches a
// known placeholder pattern, or — for `--status completed` writes — falls
// below minSummaryLengthForCompleted runes. The check normalises by
// lowercasing and collapsing internal whitespace so trivial formatting
// variants do not bypass the rule.
func validateSummaryNotPlaceholder(flag, summary, resultStatus string) error {
	normalized := strings.ToLower(strings.Join(strings.Fields(summary), " "))
	if _, isPlaceholder := summaryPlaceholderPatterns[normalized]; isPlaceholder {
		return fmt.Errorf("%s value %q is a recognised placeholder; provide a real description of the work performed (or use --summary-file for long-form content)", flag, summary)
	}
	if resultStatus == "completed" {
		runeCount := 0
		for range normalized {
			runeCount++
		}
		if runeCount < minSummaryLengthForCompleted {
			return fmt.Errorf(
				"%s for --status completed must be at least %d non-whitespace characters describing the work performed; got %d. Use --summary-file <path> when the description is long or contains shell-special characters",
				flag, minSummaryLengthForCompleted, runeCount,
			)
		}
	}
	return nil
}
