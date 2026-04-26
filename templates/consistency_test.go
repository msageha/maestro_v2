package templates

import (
	"strings"
	"testing"
)

// TestInstructions_PublishConflict_WorkerPlannerConsistency cross-checks the
// worker.md / planner.md instruction files for the publish_conflict recovery
// contract. The two documents have historically drifted by making publish
// conflict recovery a Worker-owned git commit. This test pins the daemon-owned
// contract so a future edit cannot silently re-introduce that responsibility
// inversion.
//
// What the contract says:
//  1. worker.md MUST document that _integration worktrees still do not allow
//     Worker-owned `git add` / `git commit`.
//  2. planner.md MUST instruct publish_conflict tasks to use the
//     `--run-on-integration` flag so the dispatcher places the worker on the
//     _integration worktree where source edits are permitted.
//  3. planner.md MUST instruct the worker not to run `git add` or
//     `git commit`; the daemon stages and finalizes the forward-merge.
//
// The runtime side of this contract is exercised by
// internal/agent/policy_checker_test.go's WT-GIT coverage ensures the
// instruction text matches the hook script's no-exemption behaviour.
func TestInstructions_PublishConflict_WorkerPlannerConsistency(t *testing.T) {
	worker := readInstruction(t, "instructions/worker.md")
	planner := readInstruction(t, "instructions/planner.md")

	if !strings.Contains(worker, "_integration") {
		t.Error("worker.md must document the _integration worktree publish_conflict path")
	}
	if !strings.Contains(worker, "git add` / `git commit` を実行しない") {
		t.Error("worker.md must explicitly forbid git add / git commit in _integration publish_conflict tasks")
	}

	if !strings.Contains(planner, "--run-on-integration") {
		t.Error("planner.md publish_conflict instructions must use `--run-on-integration` so the dispatcher places the worker on the _integration worktree")
	}
	if !strings.Contains(planner, "stage/commit/retry-publish は Daemon が行う") {
		t.Error("planner.md publish_conflict instructions must assign staging, commit, and retry-publish to the daemon")
	}

	// Sanity: worker.md must still impose the blanket git prohibition so an
	// agent cannot conclude "git is allowed in some worktree mode".
	if !strings.Contains(worker, "git push") {
		t.Error("worker.md must retain the blanket git mutation prohibition (look for `git push` in the禁止 table)")
	}
}

// readInstruction reads a file from the embedded templates FS and fails the
// test on read error. Centralizes the embed access so every consistency
// assertion uses the same literal-bytes-shipped-with-the-binary path.
func readInstruction(t *testing.T, path string) string {
	t.Helper()
	data, err := FS.ReadFile(path)
	if err != nil {
		t.Fatalf("read embedded %s: %v", path, err)
	}
	return string(data)
}

// TestInstructions_WorkerCommitProhibition_HasNoIntegrationCarveOut ensures
// worker.md keeps commit ownership with the daemon in every worktree mode.
func TestInstructions_WorkerCommitProhibition_HasNoIntegrationCarveOut(t *testing.T) {
	worker := readInstruction(t, "instructions/worker.md")

	normalized := strings.ReplaceAll(worker, "\r\n", "\n")
	paragraphs := strings.Split(normalized, "\n\n")

	blanketPatterns := []string{
		"commit / merge / push を一切実行しない",
		"commit/merge/push を一切実行しない",
		"git add / git commit を実行してはならない",
	}
	forbiddenCarveOuts := []string{
		"例外として",
		"許可される",
		"実行する義務",
	}

	for _, para := range paragraphs {
		matched := ""
		for _, p := range blanketPatterns {
			if strings.Contains(para, p) {
				matched = p
				break
			}
		}
		if matched == "" {
			continue
		}
		for _, tok := range forbiddenCarveOuts {
			if strings.Contains(para, tok) {
				t.Errorf("worker.md paragraph contains git mutation prohibition %q but also looks like an integration carve-out %q; git mutation ownership must stay with daemon.\n--- offending paragraph ---\n%s\n--- end ---", matched, tok, para)
			}
		}
	}
}
