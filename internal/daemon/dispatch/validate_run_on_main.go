package dispatch

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// ErrDestructiveContentRejected is returned when a RunOnMain or RunOnIntegration
// task contains shell patterns that could destroy work in the main branch or
// integration worktree (e.g. `git push origin main`, `rm -rf`, `git reset
// --hard`). The error is non-retryable so dispatch is aborted and the caller
// can route the task to operator review.
var ErrDestructiveContentRejected = errors.New("dispatch: task content rejected by run_on_main pre-flight")

// destructivePatterns lists regex matchers for shell snippets that must not be
// dispatched when a task runs on the main branch or integration worktree. The
// patterns are intentionally conservative: they target the most common
// "irrecoverable" git/file operations that the Bash policy hook is supposed to
// catch on the Claude side. This is a defense-in-depth check that protects
// non-Claude workers (Codex, Gemini) which do not honour the hook.
//
// Each pattern is matched case-insensitively against task.Content with word
// boundaries where appropriate so unrelated prose is not flagged.
var destructivePatterns = []*regexp.Regexp{
	// `git push` to a remote — covers `git push origin main`, `git push -f`,
	// `git push --force`, `git push --force-with-lease`. The trailing
	// alternation matches optional flags/refs.
	regexp.MustCompile(`(?i)\bgit\s+push\b`),
	// `git reset --hard` and `git reset --hard <ref>`.
	regexp.MustCompile(`(?i)\bgit\s+reset\s+(--hard|--keep)\b`),
	// `git checkout -- <path>` discards working-tree changes. The trailing
	// `\s` anchors the `--` separator so that ordinary checkouts like
	// `git checkout main` are not flagged.
	regexp.MustCompile(`(?i)\bgit\s+checkout\s+--\s`),
	// `git branch -D` and `git branch --delete --force`.
	regexp.MustCompile(`(?i)\bgit\s+branch\s+(-D|--delete\s+--force)\b`),
	// `git clean -fd` / `git clean -fdx` removes untracked files.
	regexp.MustCompile(`(?i)\bgit\s+clean\s+-[a-z]*f`),
	// `git update-ref -d` deletes a ref directly.
	regexp.MustCompile(`(?i)\bgit\s+update-ref\s+-d\b`),
	// `rm -rf` and `rm -fr` recursive force removal.
	regexp.MustCompile(`(?i)\brm\s+-[a-z]*r[a-z]*f|\brm\s+-[a-z]*f[a-z]*r`),
}

// validateRunOnMainContent inspects worker-visible task instructions for destructive shell
// patterns. It only runs when task.RunOnMain or task.RunOnIntegration is set,
// because those are the only paths where the worker operates against shared
// state. For worktree-only tasks the check is skipped — destructive operations
// inside an isolated worktree are recoverable.
//
// Returns nil when no destructive pattern is found. Returns an error wrapping
// ErrDestructiveContentRejected with the matched pattern for log/audit
// purposes.
func validateRunOnMainContent(task *model.Task) error {
	if task == nil {
		return nil
	}
	if !task.RunOnMain && !task.RunOnIntegration {
		return nil
	}
	fields := runOnMainInstructionFields(task)
	for _, pat := range destructivePatterns {
		for name, value := range fields {
			if pat.MatchString(value) {
				return fmt.Errorf("%w: field=%s matched=%s run_on_main=%t run_on_integration=%t",
					ErrDestructiveContentRejected, name, pat.String(), task.RunOnMain, task.RunOnIntegration)
			}
		}
	}
	return nil
}

func runOnMainInstructionFields(task *model.Task) map[string]string {
	return map[string]string{
		"purpose":             task.Purpose,
		"content":             task.Content,
		"acceptance_criteria": task.AcceptanceCriteria,
		"constraints":         strings.Join(task.Constraints, "\n"),
		"tools_hint":          strings.Join(task.ToolsHint, "\n"),
		"persona_hint":        task.PersonaHint,
	}
}
