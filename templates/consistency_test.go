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

// TestInstructions_WorkerCommitProhibition_HasIntegrationCarveOut guards
// against a specific failure mode: a paragraph of worker.md that flatly
// declares Worker は commit / merge / push を一切実行しない (or any equivalent
// blanket prohibition) without naming the _integration worktree carve-out in
// the same paragraph.
//
// This is an LLM-coherence test, not a syntax test. A single agent reading
// worker.md will frequently anchor on the strongest, most-recent rule it
// sees; if a blanket prohibition appears further down the document than the
// publish_conflict exception, the prohibition wins and publish_conflict
// recovery silently breaks (the worker refuses git add / git commit even
// though planner.md explicitly told it to run them).
//
// The fix is to keep the carve-out *in the same paragraph as* every blanket
// prohibition statement, so the agent cannot pick up one without the other.
// This test enforces that invariant by:
//
//  1. splitting worker.md into blank-line-delimited paragraphs,
//  2. matching paragraphs that look like a blanket commit/merge/push ban,
//  3. requiring each such paragraph to also reference the integration
//     worktree (so the exception travels with the rule).
//
// If the wording is reorganized later (e.g. the exception is moved to a
// dedicated subsection), update the carve-out reference list below rather
// than weakening the match — the goal is to keep the two facts adjacent.
func TestInstructions_WorkerCommitProhibition_HasIntegrationCarveOut(t *testing.T) {
	worker := readInstruction(t, "instructions/worker.md")

	// Paragraph splitter: a paragraph is a run of non-empty lines separated
	// by one or more blank lines. We normalize CRLF first so the test does
	// not depend on the host's line endings.
	normalized := strings.ReplaceAll(worker, "\r\n", "\n")
	paragraphs := strings.Split(normalized, "\n\n")

	// Patterns that constitute a "blanket prohibition" — the agent reading
	// any one of these in isolation would conclude that git mutations are
	// universally banned, including inside the _integration worktree.
	blanketPatterns := []string{
		"commit / merge / push を一切実行しない",
		"commit/merge/push を一切実行しない",
		"git add / git commit を実行してはならない",
	}
	// Tokens that demonstrate the carve-out is co-located with the rule.
	// Any one of these is sufficient: they all unambiguously route the
	// reader to the publish_conflict exception.
	carveOutTokens := []string{
		"_integration",
		"integration worktree",
		"publish_conflict",
		"run-on-integration",
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
		hasCarveOut := false
		for _, tok := range carveOutTokens {
			if strings.Contains(para, tok) {
				hasCarveOut = true
				break
			}
		}
		if !hasCarveOut {
			t.Errorf("worker.md paragraph contains blanket prohibition %q without referencing the integration worktree carve-out in the same paragraph; an agent that reads only this paragraph will refuse publish_conflict recovery operations.\n--- offending paragraph ---\n%s\n--- end ---", matched, para)
		}
	}
}
