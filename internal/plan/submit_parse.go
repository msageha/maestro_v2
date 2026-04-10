package plan

import (
	"fmt"
	"io"
	"os"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

func readInput(tasksFile string) (*SubmitInput, error) {
	var data []byte
	var err error

	if tasksFile == "-" || tasksFile == "" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, model.DefaultMaxYAMLFileBytes+1))
		if err == nil && len(data) > model.DefaultMaxYAMLFileBytes {
			return nil, fmt.Errorf("stdin input exceeds maximum size of %d bytes", model.DefaultMaxYAMLFileBytes)
		}
	} else {
		// CRIT-05: Validate file path (no null bytes, clean path)
		cleaned, pathErr := validate.FilePath(tasksFile)
		if pathErr != nil {
			return nil, fmt.Errorf("invalid tasks file path: %w", pathErr)
		}
		tasksFile = cleaned

		// HIGH-05: Check file size before reading to prevent memory exhaustion
		info, statErr := os.Stat(tasksFile)
		if statErr != nil {
			return nil, fmt.Errorf("stat tasks file: %w", statErr)
		}
		if info.Size() > int64(model.DefaultMaxYAMLFileBytes) {
			return nil, fmt.Errorf("tasks file exceeds maximum size of %d bytes (got %d)", model.DefaultMaxYAMLFileBytes, info.Size())
		}

		data, err = os.ReadFile(tasksFile)
	}
	if err != nil {
		return nil, fmt.Errorf("read tasks file: %w", err)
	}

	var input SubmitInput
	if err := yamlv3.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("parse tasks YAML: %w", err)
	}
	return &input, nil
}

// shouldInsertSystemCommit centralises the policy that determines whether the
// Planner must inject a __system_commit task into the plan. The task exists to
// have a Worker run `git commit` after all user tasks finish, which is only
// meaningful when:
//   - continuous mode is enabled (otherwise the daemon never re-submits commands), and
//   - worktree isolation is disabled (when worktrees are enabled, the daemon
//     commits worktree changes itself during merge/publish, so a Worker-side
//     commit task is both redundant and harmful).
//
// Keeping this single predicate as the sole authority for system_commit
// insertion prevents the responsibility from drifting between Planner and
// Daemon (see Critical #2 in reports/repo-audit-20260407.md).
func shouldInsertSystemCommit(cfg model.Config) bool {
	return cfg.Continuous.Enabled && !cfg.Worktree.Enabled
}

func buildSystemCommitTask(blockedByNames []string) TaskInput {
	return TaskInput{
		Name:               "__system_commit",
		Purpose:            "コマンド実行結果をリポジトリにコミットする",
		Content:            "変更ファイルを確認し、git add + git commit を実行する。コミットメッセージはタスク実行結果のサマリから生成する。",
		AcceptanceCriteria: "git commit が成功し、コミットハッシュが取得できる",
		BlockedBy:          blockedByNames,
		BloomLevel:         2,
		Required:           true,
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
