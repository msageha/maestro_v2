package templates

import (
	"strings"
	"testing"
)

// TestInstructions_PublishConflict_WorkerPlannerConsistency cross-checks the
// worker.md / planner.md instruction files for the publish_conflict recovery
// contract. The two documents have historically drifted (Worker forbids all
// `git add` / `git commit`, while Planner instructed the worker to run them
// during publish_conflict resolution); this test pins down the corrected
// shape so a future edit cannot silently re-introduce the contradiction.
//
// What the contract says:
//   1. worker.md MUST document an explicit exception for the _integration
//      worktree (otherwise the document tells the agent to refuse the
//      operation it is being asked to perform).
//   2. planner.md MUST instruct publish_conflict tasks to use the
//      `--run-on-integration` flag so the dispatcher places the worker on the
//      _integration worktree where the policy hook permits git mutations.
//   3. planner.md MUST instruct the worker to run `git add` and
//      `git commit` so the half-applied forward-merge can be finalized.
//
// The runtime side of this contract is exercised by
// internal/agent/policy_checker_test.go::
// TestHookScript_WTGIT_AllowsGitChangeInIntegrationWorktree (and its
// subdirectory / non-_integration counterparts) — together they ensure the
// instruction text matches the hook script's exemption.
func TestInstructions_PublishConflict_WorkerPlannerConsistency(t *testing.T) {
	worker := readInstruction(t, "instructions/worker.md")
	planner := readInstruction(t, "instructions/planner.md")

	// (1) worker.md must mention the _integration exception. We anchor on
	// "_integration" because that is the literal token used by the policy
	// hook script (mirrors `_integration` suffix detection in
	// internal/agent/policy_checker.go).
	if !strings.Contains(worker, "_integration") {
		t.Error("worker.md must document the _integration worktree exception for publish_conflict; missing the literal token \"_integration\"")
	}
	// Worker.md must also explicitly enumerate `git add` and `git commit`
	// as allowed inside the exception, otherwise an agent reading only the
	// blanket prohibition would refuse to perform the operation.
	if !strings.Contains(worker, "git add") || !strings.Contains(worker, "git commit") {
		t.Error("worker.md _integration exception must explicitly mention `git add` and `git commit` as allowed (currently missing one or both)")
	}

	// (2) planner.md publish_conflict section must use --run-on-integration.
	if !strings.Contains(planner, "--run-on-integration") {
		t.Error("planner.md publish_conflict instructions must use `--run-on-integration` so the dispatcher places the worker on the _integration worktree")
	}

	// (3) planner.md must instruct the resolver to stage and commit. The
	// reverse pattern (instructing only the worker to "edit" while the
	// daemon commits) is also valid in principle but is NOT what the
	// current Manager.reuseInFlightForwardMerge expects — the worker must
	// drive the merge commit (or stage and let daemon finalize on
	// re-entry). Either path requires `git add` to appear in planner.md.
	if !strings.Contains(planner, "git add") {
		t.Error("planner.md publish_conflict instructions must direct the worker to `git add` resolved files (forward-merge re-entry depends on a populated index)")
	}

	// Sanity: worker.md must still impose the blanket git prohibition for
	// non-_integration worktrees — without that line, the exception
	// section would be redundant and an agent could conclude "git is
	// always allowed".
	if !strings.Contains(worker, "git push") {
		t.Error("worker.md must retain the blanket git mutation prohibition (look for `git push` in the禁止 table) so the _integration exception remains scoped")
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
