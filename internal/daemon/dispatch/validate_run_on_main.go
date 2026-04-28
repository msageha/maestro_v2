package dispatch

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/msageha/maestro_v2/internal/model"
)

// ErrDestructiveContentRejected is returned when a RunOnMain or RunOnIntegration
// task contains shell patterns that could destroy work in the main branch or
// integration worktree (e.g. `git push origin main`, `rm -rf`, `git reset
// --hard`). The error is non-retryable so dispatch is aborted and the caller
// can route the task to operator review.
var ErrDestructiveContentRejected = errors.New("dispatch: task content rejected by run_on_main pre-flight")

// ErrRunOnMainBeforePublish is returned when a RunOnMain task is dispatched
// while the integration branch for its command has not yet been published
// into base. Verification tasks scheduled by the planner *before* publish
// would inspect stale main state and fail spuriously, so dispatch is
// refused.
//
// Unlike ErrDestructiveContentRejected this is a *timing gate*, not a
// terminal failure: the publish step is expected to complete naturally
// during the merge phase, after which the same task can be dispatched
// successfully. Callers must release the lease so other work can proceed
// AND must not consume the task's retry budget — otherwise a planner that
// queues the verification immediately after the merge phase would dead-
// letter the verification before publish even gets a chance to run. See
// queue_scan_apply.go's run-on-main publish-pending branch for the
// retry-preserving handler.
var ErrRunOnMainBeforePublish = errors.New("dispatch: run_on_main task dispatched before integration published")

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

// negationMarkers indicate the enclosing clause is a *prohibition* rather
// than an instruction to execute the matched command. When a destructive
// pattern hit lands in a clause that also carries one of these markers, the
// validator treats the mention as a Planner-authored "do not run X"
// directive and suppresses the rejection. Without this gate, prose like
// "git push 等の変更系 git 操作を行わないこと" — which is the correct way for
// the Planner to *forbid* destructive ops on a read-only verification task —
// would itself be rejected by the pre-flight, blocking dispatch entirely.
//
// The markers are intentionally narrow: only phrases that unambiguously negate
// or forbid an action are listed. Hedges like "maybe" or "consider" are not
// included because they do not flip the imperative meaning.
var negationMarkers = []*regexp.Regexp{
	// English negation/prohibition.
	regexp.MustCompile(`(?i)\b(do\s*not|don't|doesn't|didn't|never|must\s+not|mustn't|should\s+not|shouldn't|cannot|can't|won't|forbidden|prohibited|disallow(ed)?|avoid|refrain\s+from)\b`),
	// Japanese negation/prohibition. Japanese has no word boundaries, so we
	// match these as substrings.
	regexp.MustCompile(`しない|させない|禁止|行わない|行わず|使わない|使用しない|実行しない|してはいけない|してはならない|しないでください|避けて|避ける|不可|してはダメ|しちゃダメ|するな|せず`),
}

// clauseBoundaryRunes mark the end of a "negation scope" — a destructive
// pattern that lands AFTER a boundary is no longer protected by a negation
// that appears BEFORE the boundary, and vice-versa.
//
// Without this scope-narrowing the validator was vulnerable to mixed prose
// like "Avoid breaking tests; run git push origin main": the leading
// "Avoid" suppressed the trailing `git push` even though they are clearly
// distinct clauses. Codex / Gemini workers do not run the Bash policy hook
// so this pre-flight is the only line of defence; widening the negation
// scope is dangerous.
//
// Boundary set:
//   - newline (`\n`): unambiguous list/step separator in both prose and
//     YAML-quoted strings.
//   - shell command separator (`;`, full-width `；`): in shell, `;` always
//     ends one command and starts another; treating it as a clause boundary
//     prevents "skip foo; do bar" from inheriting "skip"'s negation.
//   - Japanese full stop (`。`): canonical sentence end.
//
// ASCII `.` is intentionally NOT a boundary because file paths and shell
// tokens (`./...`, `main.go`, `go test ./...`) use it heavily; sacrificing
// the rare prose "Avoid X. Run git push" case is preferable to false-
// splitting paths and accidentally letting `git push` slip through with no
// negation context.
var clauseBoundaryRunes = map[rune]bool{
	'\n': true,
	';':  true,
	'；':  true,
	'。':  true,
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
			if !destructiveMatchInExecContext(pat, value) {
				continue
			}
			return fmt.Errorf("%w: field=%s matched=%s run_on_main=%t run_on_integration=%t",
				ErrDestructiveContentRejected, name, pat.String(), task.RunOnMain, task.RunOnIntegration)
		}
	}
	return nil
}

// destructiveMatchInExecContext reports whether pat matches value in a way
// that should be treated as an executable instruction. Matches whose enclosing
// clause carries a negation/prohibition marker are filtered out so prose
// directives like "X しないこと" / "do not run X" do not trigger the validator.
//
// Scope is the smallest substring around the match bounded by
// clauseBoundaryRunes; a negation marker outside that clause does NOT
// suppress the match. Japanese negation can attach AFTER the verb
// (e.g. "git push を行わない") so we examine text on both sides of the match
// up to the next boundary, not just the prefix.
func destructiveMatchInExecContext(pat *regexp.Regexp, value string) bool {
	locs := pat.FindAllStringIndex(value, -1)
	if len(locs) == 0 {
		return false
	}
	for _, loc := range locs {
		if !clauseHasNegationMarker(value, loc[0], loc[1]) {
			return true
		}
	}
	return false
}

// clauseHasNegationMarker checks whether the clause containing the byte
// range [start:end) in s carries any negation marker. The clause is bounded
// by clauseBoundaryRunes (or the start/end of s).
func clauseHasNegationMarker(s string, start, end int) bool {
	clauseStart := findClauseBoundaryBefore(s, start)
	clauseEnd := findClauseBoundaryAfter(s, end)
	clause := s[clauseStart:clauseEnd]
	for _, neg := range negationMarkers {
		if neg.MatchString(clause) {
			return true
		}
	}
	return false
}

// findClauseBoundaryBefore walks backward from idx (a byte offset in s) and
// returns the byte offset just past the nearest preceding boundary rune,
// or 0 if no boundary is reached. Walks rune-by-rune so multi-byte
// boundaries (`。`, `；`) are detected correctly.
func findClauseBoundaryBefore(s string, idx int) int {
	pos := idx
	for pos > 0 {
		r, sz := utf8.DecodeLastRuneInString(s[:pos])
		if r == utf8.RuneError && sz <= 1 {
			// Malformed UTF-8: stop scanning to avoid infinite loops; the
			// remaining prefix becomes the clause start.
			return pos
		}
		if clauseBoundaryRunes[r] {
			return pos
		}
		pos -= sz
	}
	return 0
}

// findClauseBoundaryAfter walks forward from idx (a byte offset in s) and
// returns the byte offset of the nearest boundary rune, or len(s) if no
// boundary is reached.
func findClauseBoundaryAfter(s string, idx int) int {
	pos := idx
	for pos < len(s) {
		r, sz := utf8.DecodeRuneInString(s[pos:])
		if r == utf8.RuneError && sz <= 1 {
			return pos
		}
		if clauseBoundaryRunes[r] {
			return pos
		}
		pos += sz
	}
	return len(s)
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

// validateRunOnMainPublishGuard rejects RunOnMain tasks whose command has not
// yet published its integration branch into base.
//
// Background (cmd_1777330979_d3c29242530906ac post-mortem): the planner can
// queue a verification task with run_on_main: true alongside the merge phase
// that produces the build artefact. If the dispatcher honours the dispatch
// before integration → base publish completes, the worker reads the
// pre-publish version of main and the task fails with no diagnostic value.
// Defense-in-depth: also catches operator-authored RunOnMain tasks that
// arrive before the integration branch is published or while it is in the
// failure quarantine path.
//
// resolver may be nil (worktree-disabled daemon); in that case the guard is a
// no-op because there is no integration concept. A resolver error wrapping
// os.ErrNotExist means worktree state was never written for this command,
// which has the same "no integration enforcement" meaning. Any other
// resolver error is propagated so callers can decide whether to retry.
func validateRunOnMainPublishGuard(task *model.Task, resolver WorktreeResolver) error {
	if task == nil || !task.RunOnMain {
		return nil
	}
	if resolver == nil {
		return nil
	}
	status, err := resolver.GetIntegrationStatus(task.CommandID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read integration status for %s: %w", task.CommandID, err)
	}
	if status == model.IntegrationStatusPublished {
		return nil
	}
	return fmt.Errorf("%w: command=%s integration_status=%s",
		ErrRunOnMainBeforePublish, task.CommandID, status)
}
