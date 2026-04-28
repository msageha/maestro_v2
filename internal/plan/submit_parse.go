package plan

import (
	"fmt"
	"io"
	"os"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// parseInput parses YAML task data from raw bytes without file I/O.
func parseInput(data []byte) (*SubmitInput, error) {
	if len(data) > model.DefaultMaxYAMLFileBytes {
		// Operator-supplied input that exceeds the size limit is a bad
		// request, not an internal failure. Wrap as planValidationError so
		// the daemon's plan handler routes it to ErrCodeValidation instead
		// of ErrCodeInternal — without this, the agent CLI surfaces an
		// "[INTERNAL_ERROR]" prefix that misleads operators into thinking
		// the daemon crashed.
		return nil, &planValidationError{
			Msg: fmt.Sprintf("input exceeds maximum size of %d bytes", model.DefaultMaxYAMLFileBytes),
		}
	}
	// Sanitise invalid backslash escape sequences in double-quoted strings
	// before parsing. Planner agents may generate YAML with sequences like \!
	// or \: inside double-quoted values, which yaml.v3 rejects as invalid.
	data = yamlutil.SanitizeDoubleQuoteEscapes(data)

	var input SubmitInput
	if err := yamlutil.SafeUnmarshalStrict(data, &input); err != nil {
		// YAML strict-decode failures (unknown field, type mismatch, etc.)
		// are caused by operator input shape, not by a daemon bug. Mirror
		// the size-limit branch and surface them as validation errors so
		// the CLI prints `[VALIDATION_ERROR]` and the operator sees the
		// underlying yaml.v3 message verbatim. The 2026-04-28 E2E pass hit
		// this exact path with `field worker_id not found in type
		// plan.TaskInput` and reported INTERNAL_ERROR — that misclassification
		// is the bug we are fixing here.
		return nil, &planValidationError{Msg: fmt.Sprintf("parse tasks YAML: %v", err)}
	}
	return &input, nil
}

func readInput(tasksFile string) (*SubmitInput, error) {
	var data []byte
	var err error

	if tasksFile == "-" || tasksFile == "" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, model.DefaultMaxYAMLFileBytes+1))
		if err == nil && len(data) > model.DefaultMaxYAMLFileBytes {
			return nil, fmt.Errorf("stdin input exceeds maximum size of %d bytes", model.DefaultMaxYAMLFileBytes)
		}
	} else {
		// Validate file path (no null bytes, clean path)
		cleaned, pathErr := validate.FilePath(tasksFile)
		if pathErr != nil {
			return nil, fmt.Errorf("invalid tasks file path: %w", pathErr)
		}
		tasksFile = cleaned

		// Check file size before reading to prevent memory exhaustion
		info, statErr := os.Stat(tasksFile)
		if statErr != nil {
			return nil, fmt.Errorf("stat tasks file: %w", statErr)
		}
		if info.Size() > int64(model.DefaultMaxYAMLFileBytes) {
			return nil, fmt.Errorf("tasks file exceeds maximum size of %d bytes (got %d)", model.DefaultMaxYAMLFileBytes, info.Size())
		}

		data, err = os.ReadFile(tasksFile) //nolint:gosec // tasksFile is a user-specified config file path validated by the CLI
	}
	if err != nil {
		return nil, fmt.Errorf("read tasks file: %w", err)
	}

	return parseInput(data)
}

// shouldInsertSystemCommit centralises the policy that determines whether the
// Planner must inject a __system_commit task into the plan. The task exists
// to have a Worker run `git commit` after all user tasks finish, and the
// invariant that drives it is "the Worker is editing main directly":
//
//   - worktree mode disabled → Worker mutates the main checkout in place;
//     without an explicit commit task the changes sit dirty after the
//     command completes (2026-04-28 E2E confirmed `value.go`/`value_test.go`
//     left uncommitted with `worktree.enabled=false` + `continuous=false`).
//     Inject __system_commit in this mode regardless of continuous, so the
//     "completed = work persisted to git history" contract holds for both
//     single-shot and continuous runs.
//   - worktree mode enabled → the Daemon commits worktree changes itself
//     during merge/publish; a Worker-side commit task would race with
//     that pipeline and is both redundant and harmful.
//
// Keeping this single predicate as the sole authority for system_commit
// insertion prevents the responsibility from drifting between Planner and
// Daemon (see Critical #2 in reports/repo-audit-20260407.md).
func shouldInsertSystemCommit(cfg model.Config) bool {
	return !cfg.Worktree.Enabled
}

func buildSystemCommitTask(blockedByNames []string) TaskInput {
	// REQUIREMENTS.md §S3-1: every task MUST declare expected_paths and
	// definition_of_abort. The system-injected commit task is no exception —
	// without explicit values here, validation will reject the auto-injected
	// task and the whole submit will fail.
	doa := model.DefaultDefinitionOfAbort()
	return TaskInput{
		Name:               "__system_commit",
		Purpose:            "コマンド実行結果をリポジトリにコミットする",
		Content:            "変更ファイルを確認し、git add + git commit を実行する。コミットメッセージはタスク実行結果のサマリから生成する。",
		AcceptanceCriteria: "git commit が成功し、コミットハッシュが取得できる",
		BlockedBy:          blockedByNames,
		BloomLevel:         2,
		Required:           true,
		// __system_commit operates on the entire working tree (it stages and
		// commits whatever the prior tasks produced). Use the broadest single
		// prefix permitted by validateExpectedPaths: an empty path is rejected,
		// "/" is rejected as absolute, and ".." is rejected as traversal, so
		// we settle on "." to declare "anywhere in the repo".
		ExpectedPaths:     []string{"."},
		DefinitionOfAbort: &doa,
	}
}

// validateSystemTask validates a system-generated task's fields without checking
// the reserved __ name prefix. This ensures system tasks meet the same field
// integrity requirements (non-empty fields, length limits, bloom_level range)
// as user-submitted tasks.
func validateSystemTask(task TaskInput) error {
	errs := &ValidationErrors{}
	// Use the core field validator which skips the reserved __ name-prefix check.
	validateTaskFieldsCore(task, "system_task", errs)
	if errs.HasErrors() {
		return errs
	}
	return nil
}

func insertSystemCommitTask(tasks []TaskInput) ([]TaskInput, error) {
	allNames := make([]string, 0, len(tasks))
	for _, t := range tasks {
		allNames = append(allNames, t.Name)
	}

	commitTask := buildSystemCommitTask(allNames)
	if err := validateSystemTask(commitTask); err != nil {
		return nil, fmt.Errorf("validate system commit task: %w", err)
	}

	return append(tasks, commitTask), nil
}

func resolveNames(tasks []TaskInput) (map[string]string, error) {
	nameToID := make(map[string]string, len(tasks))
	for _, t := range tasks {
		id, err := model.NewTaskID(model.TaskIDCallerPlannerSubmit)
		if err != nil {
			return nil, fmt.Errorf("generate task ID for %s: %w", t.Name, err)
		}
		nameToID[t.Name] = id
	}
	return nameToID, nil
}
