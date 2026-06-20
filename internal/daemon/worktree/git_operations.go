package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
)

// slowGitOperationThreshold is the duration above which a git operation
// is logged at WARN level to aid hang detection and performance analysis.
const slowGitOperationThreshold = 30 * time.Second

// gitErrorClass classifies git errors for retry decisions.
type gitErrorClass int

const (
	// gitErrorTransient indicates a temporary error that may succeed on retry (e.g. lock contention).
	gitErrorTransient gitErrorClass = iota
	// gitErrorPermanent indicates an error that will not recover on retry.
	gitErrorPermanent
)

// gitErrorPattern maps a substring pattern to its error classification.
type gitErrorPattern struct {
	pattern string
	class   gitErrorClass
}

// gitErrorPatterns is the ordered error classification table.
// The first matching pattern wins. Permanent patterns are listed first to
// prevent false transient matches (e.g. "invalid" must not be overridden).
// "Connection timed out" (transient) must precede generic "timeout" (permanent).
var gitErrorPatterns = []gitErrorPattern{
	// Permanent: repository corruption / invalid state
	{"bad object", gitErrorPermanent},
	{"corrupt", gitErrorPermanent},
	{"fatal: not a git repository", gitErrorPermanent},
	{"invalid", gitErrorPermanent},

	// Transient: git lock contention
	{"lock", gitErrorTransient},
	{"Unable to create", gitErrorTransient},
	{".lock", gitErrorTransient},
	{"Another git process seems to be running", gitErrorTransient},

	// Transient: network errors
	{"Connection timed out", gitErrorTransient},
	{"Connection refused", gitErrorTransient},
	{"Could not resolve host", gitErrorTransient},
	{"Connection reset by peer", gitErrorTransient},

	// Permanent: generic timeout — must follow "Connection timed out" to
	// avoid masking the more specific transient pattern.
	{"timeout", gitErrorPermanent},
}

// classifyGitError determines whether a git error is transient (retryable) or permanent.
// Timeout errors (context.DeadlineExceeded, "timeout") are classified as Permanent
// because a timed-out git command may leave the worktree in a dirty state.
// Unknown errors default to Permanent (safe fallback).
func classifyGitError(err error) gitErrorClass {
	if err == nil {
		return gitErrorPermanent
	}

	// Timeout errors are Permanent — dirty worktree risk.
	if errors.Is(err, context.DeadlineExceeded) {
		return gitErrorPermanent
	}

	msg := err.Error()
	for _, p := range gitErrorPatterns {
		if strings.Contains(msg, p.pattern) {
			return p.class
		}
	}

	// Unknown errors default to Permanent.
	return gitErrorPermanent
}

// isTransientGitError returns true if the error is classified as transient (retryable).
func isTransientGitError(err error) bool {
	return classifyGitError(err) == gitErrorTransient
}

// pathStripRe matches absolute paths and replaces them with "…/<basename>"
// to prevent internal path leakage while preserving error classification
// patterns like ".lock" or ".git".
var pathStripRe = regexp.MustCompile(`/(?:[^\s'"]+/)([^\s'"]+)`)

// sanitizeGitStderr sanitizes git stderr output by stripping absolute paths
// and truncating to a reasonable length. Error classification keywords (like
// "lock", "Unable to create") are preserved.
func sanitizeGitStderr(s string) string {
	const maxLen = 256
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return pathStripRe.ReplaceAllString(s, "…/$1")
}

// wrapGitOutputError wraps a git exec error, including sanitized stderr from
// exec.ExitError so that classifyGitError can match transient patterns in
// stderr content while preventing internal path leakage.
func wrapGitOutputError(err error, args []string) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, sanitizeGitStderr(string(exitErr.Stderr)))
	}
	return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

// sleepCtx waits for the specified duration or until the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// gitOutputWithRetry executes a git command via gitExecOutput with retry for transient errors.
// The ctx parameter allows callers to cancel retry waits. Returns stdout on success.
func (wm *Manager) gitOutputWithRetry(ctx context.Context, dir string, maxRetries int, args ...string) (string, error) {
	output, err := wm.gitExecOutput(dir, args...)
	if err == nil {
		if output == nil {
			return "", nil
		}
		return string(output), nil
	}
	firstErr := wrapGitOutputError(err, args)

	if maxRetries <= 0 || !isTransientGitError(firstErr) {
		return "", firstErr
	}

	backoff := 50 * time.Millisecond
	const maxBackoff = 1 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		wm.Log(core.LogLevelWarn, "git_retry attempt=%d/%d backoff=%s cmd=\"git %s\" error=%v",
			attempt, maxRetries, backoff, strings.Join(args, " "), firstErr)

		if err := sleepCtx(ctx, backoff); err != nil {
			return "", fmt.Errorf("git %s: retry cancelled: %w", strings.Join(args, " "), err)
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}

		output, err = wm.gitExecOutput(dir, args...)
		if err == nil {
			if output == nil {
				return "", nil
			}
			return string(output), nil
		}
		firstErr = wrapGitOutputError(err, args)

		if !isTransientGitError(firstErr) {
			return "", firstErr
		}
	}

	return "", firstErr
}

// gitTimeout returns the configured git command timeout as a time.Duration.
func (wm *Manager) gitTimeout() time.Duration {
	return time.Duration(wm.config.EffectiveGitTimeout()) * time.Second
}

// gitExec is the shared git execution helper. All git operations go through
// this method to ensure consistent timeout and error handling.
// dir specifies the working directory; if empty, projectRoot is used.
// Returns (stdout, combinedOutput, error). Callers that need only the exit
// status use gitRun/gitRunInDir; callers that need stdout use gitOutput/gitOutputInDir.
func (wm *Manager) gitExecCombined(dir string, args ...string) ([]byte, error) {
	start := time.Now()
	defer func() {
		if duration := time.Since(start); duration >= slowGitOperationThreshold {
			wm.Log(core.LogLevelWarn, "slow_git_operation duration=%s cmd=\"git %s\" dir=%s",
				duration, strings.Join(args, " "), dir)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), wm.gitTimeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are constructed internally from validated inputs
	if dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Dir = wm.projectRoot
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return output, fmt.Errorf("git %s: timeout after %s: %w",
				strings.Join(args, " "), wm.gitTimeout(), ctx.Err())
		}
		return output, err
	}
	return output, nil
}

func (wm *Manager) gitExecOutput(dir string, args ...string) ([]byte, error) {
	start := time.Now()
	defer func() {
		if duration := time.Since(start); duration >= slowGitOperationThreshold {
			wm.Log(core.LogLevelWarn, "slow_git_operation duration=%s cmd=\"git %s\" dir=%s",
				duration, strings.Join(args, " "), dir)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), wm.gitTimeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are constructed internally from validated inputs
	if dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Dir = wm.projectRoot
	}
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("git %s: timeout after %s: %w",
				strings.Join(args, " "), wm.gitTimeout(), ctx.Err())
		}
		return nil, err
	}
	return output, nil
}

// gitRun executes a git command in the project root.
func (wm *Manager) gitRun(args ...string) error {
	output, err := wm.gitExecCombined("", args...)
	if err != nil {
		return fmt.Errorf("git %s: %w\noutput: %s", strings.Join(args, " "), err, sanitizeGitStderr(string(output)))
	}
	return nil
}

// gitOutput executes a git command and returns stdout.
func (wm *Manager) gitOutput(args ...string) (string, error) {
	output, err := wm.gitExecOutput("", args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, sanitizeGitStderr(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(output), nil
}

// gitRunInDir executes a git command in a specific directory.
func (wm *Manager) gitRunInDir(dir string, args ...string) error {
	output, err := wm.gitExecCombined(dir, args...)
	if err != nil {
		return fmt.Errorf("git %s: %w\noutput: %s", strings.Join(args, " "), err, sanitizeGitStderr(string(output)))
	}
	return nil
}

// gitOutputInDir executes a git command in a specific directory and returns stdout.
func (wm *Manager) gitOutputInDir(dir string, args ...string) (string, error) {
	output, err := wm.gitExecOutput(dir, args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("git %s: %w\nstderr: %s", strings.Join(args, " "), err, sanitizeGitStderr(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(output), nil
}

// listGitWorktreesUnlocked returns paths of all git worktrees via `git worktree list --porcelain`.
// Caller must hold wm.mu.
func (wm *Manager) listGitWorktreesUnlocked() ([]string, error) {
	output, err := wm.gitOutput("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeListPorcelain(output), nil
}

// parseWorktreeListPorcelain extracts worktree paths from `git worktree list --porcelain` output.
func parseWorktreeListPorcelain(output string) []string {
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			paths = append(paths, strings.TrimPrefix(line, "worktree "))
		}
	}
	return paths
}

// ErrWorkerOwnedByResumeMerge is returned when CommitWorkerChanges is called
// on a worker whose state is owned by the resume-merge pipeline (Conflict or
// Resolving). Those workers must only be committed via
// commitResolvedWorkerChanges, which bypasses the transition machine. This
// sentinel lets the Phase B auto-commit caller distinguish an "out of scope"
// worker from a genuine commit failure and avoid recording a spurious
// commit_failed signal.
var ErrWorkerOwnedByResumeMerge = errors.New("worker is owned by resume-merge pipeline; skipping auto-commit")

// gitAddAllAttemptLimit caps how many times gitAddAllWithUnstattableFallback
// will append paths to .git/info/exclude and retry. Each retry handles one
// or more files surfaced by the previous attempt's "unable to stat" error;
// runaway loops are impossible because each attempt either makes progress
// (adds at least one new exclude entry) or returns the underlying error.
const gitAddAllAttemptLimit = 5

// unstattablePathRe extracts the path from a `git add -A` "unable to stat"
// error. The git error format is stable: `fatal: unable to stat '<path>':
// Operation not permitted` (or "Permission denied" on POSIX systems).
// We capture the single-quoted path. Falls back gracefully when the
// regex doesn't match — in that case the caller surfaces the original
// error rather than silently swallowing it.
var unstattablePathRe = regexp.MustCompile(`unable to stat '([^']+)'`)

// gitAddAllWithUnstattableFallback runs `git add -A` and, when git rejects
// it with "unable to stat ...: Operation not permitted" / "Permission
// denied", appends the offending paths to the worktree's
// .git/info/exclude (worktree-local; never touches the repo's tracked
// .gitignore) and retries. This pattern is triggered by sandboxed
// environments (macOS Claude Code rules denying `/**/.env*` reads, etc.)
// where the file genuinely cannot be added to the index — git can't
// possibly stat it, so excluding it is the only autonomous path forward.
//
// Bounded: gives up after gitAddAllAttemptLimit attempts so a truly
// unrecoverable file-system error (volume offline, etc.) returns the
// original error instead of looping forever. Returns nil on success.
func (wm *Manager) gitAddAllWithUnstattableFallback(dir, commandID, workerID string) error {
	excludedAlready := make(map[string]struct{})
	var lastErr error
	for attempt := 1; attempt <= gitAddAllAttemptLimit; attempt++ {
		err := wm.gitRunInDir(dir, "add", "-A")
		if err == nil {
			return nil
		}
		lastErr = err
		if !isUnstattableError(err) {
			return err
		}
		paths := extractUnstattablePaths(err.Error())
		if len(paths) == 0 {
			return err
		}
		fresh := make([]string, 0, len(paths))
		for _, p := range paths {
			if _, dup := excludedAlready[p]; dup {
				continue
			}
			excludedAlready[p] = struct{}{}
			fresh = append(fresh, p)
		}
		if len(fresh) == 0 {
			// Every offender already in exclude — git is still tripping
			// on something we cannot identify. Surface the error.
			return err
		}
		if appendErr := appendToGitInfoExclude(dir, commandID, fresh); appendErr != nil {
			wm.Log(core.LogLevelWarn,
				"git_add_unstattable_exclude_failed command=%s worker=%s paths=%v error=%v",
				commandID, workerID, fresh, appendErr)
			return err
		}
		wm.Log(core.LogLevelWarn,
			"git_add_unstattable_excluded command=%s worker=%s paths=%v attempt=%d "+
				"(file unreadable to git — typical cause: ~/.claude rules or OS sandbox denying access; "+
				"appended to repo-shared .git/info/exclude as a command-tagged block and retrying)",
			commandID, workerID, fresh, attempt)
	}
	return fmt.Errorf("git add -A: exceeded %d unstattable-fallback attempts: %w",
		gitAddAllAttemptLimit, lastErr)
}

// isUnstattableError reports whether the error text matches the canonical
// git "unable to stat ...: Operation not permitted / Permission denied"
// failure mode. We pattern-match the message because git does not expose
// a stable error code for it.
//
// Two phrasings are recognised:
//   - `git add -A`: "unable to stat '<path>': Operation not permitted"
//   - `git worktree add` checkout: "error: unable to stat just-written file
//     <path>: Operation not permitted"
//
// Both ultimately mean "the kernel refuses to expose this file's metadata to
// the daemon process" (typically a sandbox / SIP / quarantine rule denying
// access to a tracked file like `.env.example`). The recovery is the same
// in both cases: skip the file and continue.
func isUnstattableError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "unable to stat") &&
		!strings.Contains(msg, "just-written file") {
		return false
	}
	return strings.Contains(msg, "Operation not permitted") ||
		strings.Contains(msg, "Permission denied")
}

// worktreeJustWrittenPathRe extracts the offending file path from
// `git worktree add` checkout failures. Format:
//
//	error: unable to stat just-written file <path>: Operation not permitted
//
// Note the path is NOT quoted (unlike `git add -A`), so a separate regex is
// needed to capture it.
var worktreeJustWrittenPathRe = regexp.MustCompile(`unable to stat just-written file ([^:]+):`)

// extractUnstattablePathsForWorktreeAdd returns every offending path from
// `git worktree add` checkout errors. Used by the worktree-add fallback to
// pre-populate `.git/info/sparse-checkout` so the second attempt does not
// try to materialise the same files.
func extractUnstattablePathsForWorktreeAdd(msg string) []string {
	out := make([]string, 0)
	seen := make(map[string]struct{})
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, m := range worktreeJustWrittenPathRe.FindAllStringSubmatch(msg, -1) {
		if len(m) >= 2 {
			add(m[1])
		}
	}
	for _, m := range unstattablePathRe.FindAllStringSubmatch(msg, -1) {
		if len(m) >= 2 {
			add(m[1])
		}
	}
	return out
}

// extractUnstattablePaths returns every path quoted in `unable to stat
// '<path>'` segments of the error message. Multiple paths can appear when
// git emitted them in a single batch.
func extractUnstattablePaths(msg string) []string {
	matches := unstattablePathRe.FindAllStringSubmatch(msg, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		p := strings.TrimSpace(m[1])
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// gitWorktreeAddWithUnstattableFallback runs `git worktree add` and, when
// the checkout step fails because git cannot stat one or more files
// (sandbox / SIP / quarantine denying access to a tracked file like
// `.env.example`), retries with `--no-checkout` and then materialises the
// remaining files via `git checkout-index`, marking the offending paths
// `--skip-worktree` so subsequent reads do not trip the same error.
//
// The args follow the same shape as `git worktree add`'s positional
// arguments (e.g. `["worktree", "add", path, branch]` or `["worktree",
// "add", "-b", branch, path, baseSHA]`); we extract `path` from the
// argv structure to know where the worktree lives so the post-add
// recovery can run inside it.
//
// Behaviour:
//   - First attempt: run the command verbatim. Success → return nil.
//   - Failure with isUnstattableError: clean up partial directory,
//     extract offending paths from the error message, retry with
//     `--no-checkout` so the worktree exists at all. Then mark each
//     offending path with `update-index --skip-worktree` and run
//     `git checkout-index -a -f` for the remainder. Final
//     `appendToGitInfoExclude` keeps the path out of future `git add`.
//   - Any other failure: caller's normal error path.
//
// Returns the path argument so the caller can apply branch / state
// rollback if even the `--no-checkout` retry fails.
func (wm *Manager) gitWorktreeAddWithUnstattableFallback(commandID string, addArgs []string) error {
	worktreePath := worktreePathFromArgs(addArgs)

	addErr := wm.gitRun(addArgs...)
	if addErr == nil {
		return nil
	}
	if !isUnstattableError(addErr) {
		return addErr
	}

	// Sandbox / SIP / quarantine denied stat on a tracked file during
	// checkout. Same root cause as `git add -A` unstattable: the kernel
	// refuses to expose the file to the daemon process. Skip-worktree
	// the offending paths and re-run with --no-checkout, then materialise
	// the rest. This keeps a Maestro session alive on repos that ship
	// `.env.example` or other deny-pattern tracked files.
	//
	// Pre-cleanup: `git worktree add` left a half-created directory
	// behind, so the --no-checkout retry would otherwise hit "fatal:
	// '<path>' already exists".
	if rmErr := os.RemoveAll(worktreePath); rmErr != nil {
		wm.Log(core.LogLevelWarn,
			"worktree_add_partial_cleanup_failed command=%s path=%s error=%v "+
				"(--no-checkout retry may still hit `path already exists`)",
			commandID, worktreePath, rmErr)
	}
	_ = wm.gitRun("worktree", "prune", "-v")

	// `git worktree add -b <branch>` creates the ref *before* populating the
	// working tree, so the failed first attempt left the branch behind. Neither
	// the directory removal nor `worktree prune` deletes a branch, so the
	// `--no-checkout -b <branch>` retry below would fail with
	// "fatal: a branch named '<branch>' already exists" — making worker
	// worktree creation impossible under any sandbox/SIP that denies stat on a
	// tracked file (e.g. a committed `.env`). Drop the orphaned branch first.
	// Only fires for the worker shape (`-b`); the integration shape has no
	// `-b` and references a pre-existing branch that must be preserved.
	if orphanBranch := branchCreatedByWorktreeAddArgs(addArgs); orphanBranch != "" {
		if delErr := wm.gitRun("branch", "-D", orphanBranch); delErr != nil {
			wm.Log(core.LogLevelDebug,
				"worktree_add_orphan_branch_cleanup command=%s branch=%s error=%v "+
					"(branch may not have been created by the failed attempt; retry will surface real errors)",
				commandID, orphanBranch, delErr)
		}
	}

	denied := extractUnstattablePathsForWorktreeAdd(addErr.Error())
	wm.Log(core.LogLevelWarn,
		"worktree_add_unstattable_fallback command=%s path=%s denied_paths=%v "+
			"(retrying with --no-checkout; deny-pattern tracked files will be skip-worktree-marked)",
		commandID, worktreePath, denied)

	// Inject --no-checkout at the right position. addArgs always starts
	// with ["worktree", "add", ...]; we insert --no-checkout right after
	// "add". The remainder is preserved verbatim so existing flags like
	// `-b <branch>` keep their semantics.
	noCheckoutArgs := make([]string, 0, len(addArgs)+1)
	for i, a := range addArgs {
		noCheckoutArgs = append(noCheckoutArgs, a)
		if i == 1 && a == "add" {
			noCheckoutArgs = append(noCheckoutArgs, "--no-checkout")
		}
	}
	if err := wm.gitRun(noCheckoutArgs...); err != nil {
		// Even --no-checkout failed — the failure is not just about
		// stat-able files. Surface the original error so the caller
		// sees the real reason and applies whatever rollback is needed.
		return fmt.Errorf("worktree add --no-checkout fallback also failed: %w (original: %s)", err, addErr.Error())
	}

	// Mark each denied path skip-worktree so the next checkout-index
	// pass does not try to materialise it; also write to .git/info/exclude
	// so subsequent `git add -A` (run by the worker / commit pipeline)
	// does not re-trip on the same path.
	for _, p := range denied {
		if uErr := wm.gitRunInDir(worktreePath, "update-index", "--skip-worktree", "--", p); uErr != nil {
			wm.Log(core.LogLevelDebug,
				"worktree_add_skip_worktree_failed command=%s path=%s error=%v "+
					"(may not be tracked yet — checkout-index will surface real issues if any)",
				commandID, p, uErr)
		}
	}
	if appendErr := appendToGitInfoExclude(worktreePath, commandID, denied); appendErr != nil {
		wm.Log(core.LogLevelDebug,
			"worktree_add_exclude_append_failed command=%s error=%v",
			commandID, appendErr)
	}

	// Materialise the remaining tracked files. checkout-index honors
	// skip-worktree, so denied paths are bypassed cleanly. -a is "all
	// tracked", -f overwrites any leftover content from the partial
	// first attempt.
	if coErr := wm.gitRunInDir(worktreePath, "checkout-index", "-a", "-f"); coErr != nil {
		// If checkout-index also surfaced unstattable paths, append
		// them and retry once. Otherwise let the caller see it.
		if isUnstattableError(coErr) {
			extra := extractUnstattablePaths(coErr.Error())
			for _, p := range extra {
				_ = wm.gitRunInDir(worktreePath, "update-index", "--skip-worktree", "--", p)
			}
			_ = appendToGitInfoExclude(worktreePath, commandID, extra)
			if retryErr := wm.gitRunInDir(worktreePath, "checkout-index", "-a", "-f"); retryErr != nil {
				wm.Log(core.LogLevelWarn,
					"worktree_add_checkout_index_retry_failed command=%s path=%s error=%v "+
						"(worktree exists but some tracked files are not materialised; worker should still be able to operate on remaining files)",
					commandID, worktreePath, retryErr)
			}
		} else {
			wm.Log(core.LogLevelWarn,
				"worktree_add_checkout_index_failed command=%s path=%s error=%v "+
					"(worktree exists but content materialisation failed; investigate manually)",
				commandID, worktreePath, coErr)
		}
	}

	wm.Log(core.LogLevelInfo,
		"worktree_add_unstattable_recovered command=%s path=%s skipped_paths=%v "+
			"(worktree usable; deny-pattern files left out of working copy and excluded from future `git add`)",
		commandID, worktreePath, denied)
	return nil
}

// worktreePathFromArgs returns the worktree path from a `git worktree
// add` argv slice. Recognises the two shapes the daemon emits:
//   - ["worktree", "add", <path>, <branch>]                    integration
//   - ["worktree", "add", "-b", <branch>, <path>, <baseSHA>]   worker
//
// Other shapes (e.g. `--no-checkout` already present) are handled by the
// fallback's argv-rewriting logic.
func worktreePathFromArgs(args []string) string {
	if len(args) < 3 || args[0] != "worktree" || args[1] != "add" {
		return ""
	}
	// Skip flags after "add"; the first non-flag argument is the path.
	i := 2
	for i < len(args) {
		a := args[i]
		// "-b <branch>" pair: skip the flag and its value.
		if a == "-b" || a == "-B" {
			i += 2
			continue
		}
		// Other flags starting with "-" without a known value: skip alone.
		if strings.HasPrefix(a, "-") {
			i++
			continue
		}
		return a
	}
	return ""
}

// branchCreatedByWorktreeAddArgs returns the branch name a `git worktree add`
// argv would *create* — i.e. the value following `-b`/`-B`. Returns "" for the
// integration shape (`["worktree", "add", <path>, <existing-branch>]`) which
// references a pre-existing branch the fallback must never delete.
func branchCreatedByWorktreeAddArgs(args []string) string {
	for i := 2; i+1 < len(args); i++ {
		if args[i] == "-b" || args[i] == "-B" {
			return args[i+1]
		}
	}
	return ""
}

// gitSharedExcludeMu serialises read-modify-write access to the repo-shared
// `.git/info/exclude` across commands: unstattable-fallback appends and the
// per-command marker-block GC run from different scan phases / commands.
var gitSharedExcludeMu sync.Mutex

func maestroExcludeBeginMarker(commandID string) string {
	return "# maestro-exclude-begin " + commandID
}

func maestroExcludeEndMarker(commandID string) string {
	return "# maestro-exclude-end " + commandID
}

// appendToGitInfoExclude writes one anchored pattern per path to the repo's
// SHARED `.git/info/exclude` (the common-dir copy), wrapped in a marker
// block tagged with commandID so command cleanup can GC the entries
// (removeMaestroExcludeBlocks). Never touches the repo's committed
// .gitignore.
//
// Why the shared file: git does NOT read a linked worktree's per-worktree
// `info/exclude` — `info/` resolves to the common dir for every worktree
// (the only per-worktree exception is info/sparse-checkout), so writing the
// per-worktree copy silently does nothing for worker worktrees. Unstattable
// paths come from process-global sandbox deny rules (e.g. `/**/.env`), so
// the same path is unstattable in every worktree and a repo-wide exclude is
// semantically correct; the marker block bounds the pollution to the
// command's lifetime.
func appendToGitInfoExclude(worktreePath, commandID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	excludePath, err := resolveGitSharedExcludePath(worktreePath)
	if err != nil {
		return fmt.Errorf("resolve shared .git/info/exclude: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o750); err != nil {
		return fmt.Errorf("mkdir .git/info: %w", err)
	}

	gitSharedExcludeMu.Lock()
	defer gitSharedExcludeMu.Unlock()

	// #nosec G304 -- excludePath is derived from a Maestro-managed worktree
	// path (resolveGitSharedExcludePath follows the .git pointer of a
	// worktree the daemon itself created); not user-controlled.
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open .git/info/exclude: %w", err)
	}
	defer func() { _ = f.Close() }()
	var b strings.Builder
	b.WriteString(maestroExcludeBeginMarker(commandID) + "\n")
	for _, p := range paths {
		// Anchor with leading '/' so the pattern matches the exact path
		// from worktree root, not any directory of that name.
		b.WriteString("/" + strings.TrimPrefix(p, "/") + "\n")
	}
	b.WriteString(maestroExcludeEndMarker(commandID) + "\n")
	if _, werr := f.WriteString(b.String()); werr != nil {
		return fmt.Errorf("write .git/info/exclude: %w", werr)
	}
	return nil
}

// removeMaestroExcludeBlocks deletes every marker block tagged with
// commandID from the repo-shared `.git/info/exclude`. Best-effort: failures
// are logged, not returned — leftover exclude entries only hide untracked
// paths from status and must never block command cleanup.
func (wm *Manager) removeMaestroExcludeBlocks(commandID string) {
	excludePath, err := resolveGitSharedExcludePath(wm.projectRoot)
	if err != nil {
		wm.Log(core.LogLevelDebug, "exclude_block_gc_resolve_failed command=%s error=%v", commandID, err)
		return
	}

	gitSharedExcludeMu.Lock()
	defer gitSharedExcludeMu.Unlock()

	// #nosec G304 -- excludePath derives from the daemon's own projectRoot.
	data, err := os.ReadFile(excludePath)
	if err != nil {
		if !os.IsNotExist(err) {
			wm.Log(core.LogLevelDebug, "exclude_block_gc_read_failed command=%s error=%v", commandID, err)
		}
		return
	}
	begin := maestroExcludeBeginMarker(commandID)
	end := maestroExcludeEndMarker(commandID)
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	inBlock := false
	removed := false
	for _, line := range lines {
		switch {
		case line == begin:
			inBlock = true
			removed = true
		case line == end:
			inBlock = false
		case !inBlock:
			kept = append(kept, line)
		}
	}
	if !removed {
		return
	}
	// #nosec G703 -- excludePath derives from the daemon's own projectRoot.
	if err := os.WriteFile(excludePath, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		wm.Log(core.LogLevelWarn, "exclude_block_gc_write_failed command=%s error=%v", commandID, err)
		return
	}
	wm.Log(core.LogLevelInfo, "exclude_block_gc command=%s path=%s", commandID, excludePath)
}

// resolveGitSharedExcludePath returns the absolute path to the repo-shared
// `.git/info/exclude` — the only exclude file git actually reads for both
// the main worktree and all linked worktrees. For a linked worktree, `.git`
// is a file pointing at `<repo>/.git/worktrees/<name>`, whose `commondir`
// file leads back to the shared `.git`.
func resolveGitSharedExcludePath(worktreePath string) (string, error) {
	gitMarker := filepath.Join(worktreePath, ".git")
	info, err := os.Stat(gitMarker)
	if err != nil {
		return "", fmt.Errorf("stat .git: %w", err)
	}
	if info.IsDir() {
		return filepath.Join(gitMarker, "info", "exclude"), nil
	}
	// .git is a file (linked worktree). Read its `gitdir:` pointer.
	// #nosec G304 -- gitMarker is `<worktreePath>/.git`, where worktreePath
	// is provided by the daemon (Maestro-managed worktree); not user input.
	data, err := os.ReadFile(gitMarker)
	if err != nil {
		return "", fmt.Errorf("read .git pointer: %w", err)
	}
	const prefix = "gitdir: "
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		gitDir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(worktreePath, gitDir)
		}
		// gitDir is the per-worktree dir `<repo>/.git/worktrees/<name>`;
		// its `commondir` file points at the shared `.git`.
		commonDirFile := filepath.Join(gitDir, "commondir")
		// #nosec G304 G703 -- path derived from the daemon-managed gitdir above.
		cdData, err := os.ReadFile(commonDirFile)
		if err != nil {
			return "", fmt.Errorf("read commondir: %w", err)
		}
		commonDir := strings.TrimSpace(string(cdData))
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		}
		return filepath.Join(commonDir, "info", "exclude"), nil
	}
	return "", fmt.Errorf(".git pointer file did not contain `gitdir:` line")
}

// hasUnmergedFiles checks if a directory has unmerged index entries (indicating a true merge conflict).
// Uses `git ls-files -u` which is more robust than checking exit codes for automation.
// Returns (hasConflict, error) — callers must handle probe errors separately.
func (wm *Manager) hasUnmergedFiles(dir string) (bool, error) {
	output, err := wm.gitOutputInDir(dir, "ls-files", "-u")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) != "", nil
}

// getConflictFilesInDir returns the list of conflicting files in a directory.
func (wm *Manager) getConflictFilesInDir(dir string) ([]string, error) {
	output, err := wm.gitOutputInDir(dir, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, name := range strings.Split(output, "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// recoverWorktreeAfterMerge attempts to recover a worktree when merge --abort fails.
// It performs git reset --hard to preMergeHEAD, then git clean -fd to remove untracked files,
// and verifies the worktree is clean via git status --porcelain.
// Returns nil only if the worktree is confirmed clean after recovery.
func (wm *Manager) recoverWorktreeAfterMerge(worktreePath, preMergeHEAD, commandID, workerID string) error {
	// Tripwire: refuse to run destructive git ops outside the project root.
	if err := ensureWithinProjectRoot(wm.projectRoot, worktreePath); err != nil {
		wm.Log(core.LogLevelError, "merge_abort_recovery_path_guard command=%s worker=%s error=%v",
			commandID, workerID, err)
		return fmt.Errorf("worktree recovery refused: %w", err)
	}
	// Step 1: reset --hard is mandatory — it restores tracked content, index, and HEAD.
	if resetErr := wm.gitRunInDir(worktreePath, "reset", "--hard", preMergeHEAD); resetErr != nil {
		wm.Log(core.LogLevelError, "merge_abort_recovery_reset_failed command=%s worker=%s error=%v",
			commandID, workerID, resetErr)
		return fmt.Errorf("worktree recovery failed: reset --hard: %w", resetErr)
	}
	wm.Log(core.LogLevelDebug, "merge_abort_recovery_reset_ok command=%s worker=%s head=%s",
		commandID, workerID, preMergeHEAD)

	// Step 2: clean -fd to remove untracked files left by the failed merge.
	if cleanErr := wm.gitRunInDir(worktreePath, "clean", "-fd"); cleanErr != nil {
		wm.Log(core.LogLevelDebug, "merge_abort_recovery_clean_failed command=%s worker=%s error=%v",
			commandID, workerID, cleanErr)
		// Not immediately fatal — check status below to confirm.
	}

	// Step 3: verify the worktree is clean.
	statusOut, statusErr := wm.gitOutputInDir(worktreePath, "status", "--porcelain")
	if statusErr != nil {
		wm.Log(core.LogLevelError, "merge_abort_recovery_verify_failed command=%s worker=%s error=%v",
			commandID, workerID, statusErr)
		return fmt.Errorf("worktree recovery verification failed: %w", statusErr)
	}
	if strings.TrimSpace(statusOut) != "" {
		wm.Log(core.LogLevelError, "merge_abort_recovery_still_dirty command=%s worker=%s status=%q",
			commandID, workerID, strings.TrimSpace(statusOut))
		return fmt.Errorf("worktree still dirty after recovery: %s", strings.TrimSpace(statusOut))
	}

	wm.Log(core.LogLevelDebug, "merge_abort_recovery_success command=%s worker=%s", commandID, workerID)
	return nil
}
