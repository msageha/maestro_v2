package validate

// Worker PreToolUse policy — Go-side scaffold for F-025 step 3.
//
// SCOPE (intentionally narrow): this file defines the input/output contract
// (HookInput / HookDecision) and a small set of rules that mirror the most
// load-bearing bash checks in `internal/agent/worker_policy_hook.sh`. The
// goal is to land the type vocabulary the eventual `maestro hook
// policy-check` CLI subcommand will use, plus enough rules that a parity
// test against the bash hook can be written.
//
// IMPORTANT: this function is NOT yet wired into the Worker hook execution
// path. Step 4 (parity test, bash↔Go diff CI) and Step 7 (feature-flag
// switchover) of `docs/maestro-review/F-025-migration-plan.md` MUST land
// before the Worker hook stops shelling out to bash. Anything that calls
// CheckWorkerPolicy today is doing observability / parity comparison work,
// not enforcement.

import (
	"regexp"
	"strings"
)

// HookInput is the structured form of the PreToolUse hook stdin payload
// (currently parsed by `worker_policy_hook.sh` via jq). Keys mirror Claude
// Code's PreToolUse contract; only fields the policy actually inspects are
// modeled — additional fields in the JSON are ignored.
//
// RunOnMain captures the result of the bash-side `tmux display-message
// '#{@run_on_main}'` probe. The probe semantics are operator-policy heavy
// (fail-closed when tmux is unreachable); resolving it here keeps
// CheckWorkerPolicy a pure function of its input.
type HookInput struct {
	// ToolName is the Claude Code tool being invoked ("Bash", "Write",
	// "Edit", etc.). Empty when the payload omitted .tool_name.
	ToolName string

	// Command is the Bash tool's command string (.tool_input.command).
	// Only meaningful when ToolName=="Bash"; empty for other tools.
	Command string

	// FilePath is the Write/Edit tool's target path (.tool_input.file_path).
	// Only meaningful when ToolName=="Write" or "Edit".
	FilePath string

	// RunOnMain is the resolved value of the @run_on_main pane variable.
	// True means "this Worker is pointed at the main worktree for read-only
	// verification — block any mutation". The bash hook fails closed
	// (RunOnMain=true) when tmux is unreachable; callers replicating that
	// path should set this field accordingly before calling CheckWorkerPolicy.
	RunOnMain bool

	// ProjectRoot is the absolute path to the project root used for
	// path-confinement checks (D002, WT001). Set by the launcher /
	// hook-wrapper, NOT discovered inside CheckWorkerPolicy, so the policy
	// can be tested without filesystem access.
	ProjectRoot string
}

// HookDecision is the result of a single CheckWorkerPolicy call. Allow=true
// means "no rule fired"; Allow=false means at least one rule denied and
// Reason is the operator-facing string the bash hook would have printed via
// the `deny` helper.
type HookDecision struct {
	Allow  bool
	Reason string
}

// allowDecision is the canonical Allow value used to make call sites read
// `return allowDecision` instead of zero-value struct literals.
var allowDecision = HookDecision{Allow: true}

// denyDecision constructs a Deny decision with the supplied reason. The
// reason string SHOULD match the bash hook's `deny "..."` argument so the
// parity test (F-025 step 4) can compare strings byte-for-byte.
func denyDecision(reason string) HookDecision {
	return HookDecision{Allow: false, Reason: reason}
}

// CheckWorkerPolicy evaluates the worker PreToolUse policy and returns the
// hook decision the Bash script would produce for the same input.
//
// The implementation is intentionally a small subset of
// `worker_policy_hook.sh` — only the rules already covered here have parity
// guarantees. Adding a rule MUST land along with a test asserting both the
// allow path and a deny path so the parity table grows monotonically.
func CheckWorkerPolicy(in HookInput) HookDecision {
	switch in.ToolName {
	case "Bash":
		return checkBashPolicy(in)
	case "Write", "Edit":
		// Write/Edit rules are not yet implemented in this scaffold; defer
		// to bash for now (returning allow here would create a false
		// positive against the bash baseline that step-4 parity catches).
		return allowDecision
	default:
		// Tools the bash hook leaves unchecked: Allow.
		return allowDecision
	}
}

// checkBashPolicy implements the Bash branch of the worker policy. Rules
// follow the order they appear in worker_policy_hook.sh so the parity test
// can compare match-priority too.
func checkBashPolicy(in HookInput) HookDecision {
	cmd := in.Command
	if cmd == "" {
		return allowDecision
	}

	// C1: backtick command substitution is unconditionally denied. The bash
	// hook does this with `printf '\x60'` to avoid embedding a literal
	// backtick inside its own here-doc; in Go we just compare to "`".
	if strings.Contains(cmd, "`") {
		return denyDecision("C1: Blocked command containing backtick command substitution")
	}

	// C1: ANSI-C quoting ($'...') can encode arbitrary bytes (\x41 etc.)
	// to bypass downstream pattern checks. Bash regex anchors the `$'` on
	// either start-of-string or a shell-significant separator so casual
	// uses like `echo a$b` are not mistaken for a $'...' opening.
	if c1ANSICQuoting.MatchString(cmd) {
		return denyDecision("C1: Blocked command containing ANSI-C quoting")
	}

	// H-1: process substitution (<(cmd) or >(cmd)) is denied
	// unconditionally. Only Bash supports it, and it is overwhelmingly
	// used to feed the output/input of an arbitrary subshell into another
	// command — the same threat model as the deny-by-default $() rule.
	if h1ProcessSubstitution.MatchString(cmd) {
		return denyDecision("H1-PS: Blocked process substitution (<(cmd) or >(cmd))")
	}

	// D001: `rm -rf` (or any [rR]+f / f+[rR] flag combination, optional
	// long-form --recursive/--force, optional whitespace) targeting a
	// system / home / root directory. Mirrors the three bash regexes:
	//   1. rm -[..R..]f.. <target>
	//   2. rm -[..]f..[R].. <target>
	//   3. rm + (--recursive|-rR-form) + (--force|-f-form) + <target>
	if reason, ok := rmRecursiveForceTargetsSystemReason(cmd); ok {
		return denyDecision(reason)
	}

	// D004: git reset --hard is denied unconditionally. Workers stash
	// instead. Bash equivalent: `git\s+reset\s+--hard`.
	if d004GitResetHard.MatchString(cmd) {
		return denyDecision("D004: Blocked git reset --hard (use git stash)")
	}

	// Worker: all forms of `git push` are denied. Mirrors the bash hook's
	// stand-alone block at L243-255 of worker_policy_hook.sh; the long
	// reason string (referencing F-027 / worker.md / maestro.md Tier3
	// alternatives) is replicated verbatim so parity tests can compare
	// reasons byte-for-byte once Step 6 enables that comparison.
	if workerGitPush.MatchString(cmd) {
		return denyDecision("Worker git push is prohibited (all git push operations are blocked for Workers, including --force-with-lease — Tier3 alternative does not apply to this role; see maestro.md F-027 / worker.md)")
	}

	// D005: privilege escalation. `sudo` / `su` must appear at the head of
	// a fresh command segment (start-of-string or after `;` / `|` / `&&`)
	// to avoid mistaking benign substrings like `mysql_user` for `su`.
	if d005Sudo.MatchString(cmd) {
		return denyDecision("D005: Blocked sudo (privilege escalation)")
	}
	if d005Su.MatchString(cmd) {
		return denyDecision("D005: Blocked su (privilege escalation)")
	}

	// D006: process destruction. Same shell-separator anchor as D005.
	if d006Kill.MatchString(cmd) {
		return denyDecision("D006: Blocked kill/killall (process destruction)")
	}
	if d006Pkill.MatchString(cmd) {
		return denyDecision("D006: Blocked pkill (process destruction)")
	}

	// D008: remote code execution via pipe. `curl URL | bash` etc.
	if d008CurlPipeShell.MatchString(cmd) {
		return denyDecision("D008: Blocked remote code execution (curl/wget piped to shell)")
	}

	return allowDecision
}

// shellSeparatorAnchor names the regex fragment the bash hook spells as
// `(^|;|\||&&)` — i.e. "this token must appear at the start of a fresh
// shell-command segment, anchored by start-of-string or by one of the
// segment-separating operators". Many D00x rules need it; centralising
// the literal keeps every translation visibly identical to the bash side.
const shellSeparatorAnchor = `(?:^|;|\||&&)`

// c1ANSICQuoting matches the ANSI-C quoting opener (`$'`) anchored at
// start-of-string or after one of the shell-significant separators bash
// considers a "fresh token" boundary (space, `;`, `|`, `&`, `(`, `{`,
// `"`, `'`, `=`). This is a verbatim translation of the hook's
// `(^|[[:space:];|&({\"'=])[$]'` pattern.
var c1ANSICQuoting = regexp.MustCompile(`(?:^|[\s;|&({"'=])\$'`)

// h1ProcessSubstitution matches `<(` or `>(` anywhere in the command.
// The bash equivalent is `[<>]\(`.
var h1ProcessSubstitution = regexp.MustCompile(`[<>]\(`)

// d004GitResetHard matches `git reset --hard` with whitespace tolerance,
// matching the bash hook's `git\s+reset\s+--hard` pattern.
var d004GitResetHard = regexp.MustCompile(`git\s+reset\s+--hard`)

// workerGitPush matches `git push` followed by whitespace or end-of-string
// so `git pushed-feature-branch` (a benign substring) doesn't trip. Mirrors
// the hook's `git\s+push(\s|$)` pattern.
var workerGitPush = regexp.MustCompile(`git\s+push(?:\s|$)`)

// d005Sudo / d005Su anchor `sudo` / `su` to a fresh shell segment so a
// substring like `mysql_user` does not collide with `su`. Mirrors the
// hook's `(^|;|\||&&)\s*sudo\s` and `(^|;|\||&&)\s*su\s` patterns.
var (
	d005Sudo = regexp.MustCompile(shellSeparatorAnchor + `\s*sudo\s`)
	d005Su   = regexp.MustCompile(shellSeparatorAnchor + `\s*su\s`)
)

// d006Kill / d006Pkill use the same anchor as D005. Bash equivalents:
// `(^|;|\||&&)\s*kill(all)?\s` and `(^|;|\||&&)\s*pkill\s`.
var (
	d006Kill  = regexp.MustCompile(shellSeparatorAnchor + `\s*kill(?:all)?\s`)
	d006Pkill = regexp.MustCompile(shellSeparatorAnchor + `\s*pkill\s`)
)

// d008CurlPipeShell matches `curl ... | bash` / `wget ... | sh` etc. Bash
// equivalent: `(curl|wget)\s.*\|\s*(ba)?sh`.
var d008CurlPipeShell = regexp.MustCompile(`(?:curl|wget)\s.*\|\s*(?:ba)?sh`)

// d001SystemTarget matches the same set of "system/home/root" path tokens
// the bash hook uses (`/`, `~`, `/Users`, `/home`, `/root`, `/opt`). Keep
// this in lockstep with worker_policy_hook.sh's D001 / D002 path patterns.
//
// Anchoring rules:
//   - `/` followed by whitespace or end-of-line catches `rm -rf /` and
//     `rm -rf / && ...` while letting `rm -rf /tmp/x` pass through.
//   - `~` matches the literal home shorthand.
//   - The named directories MUST match as a leading path token (followed by
//     `/`, whitespace, or end) so `/optional` does not trigger.
var d001SystemTarget = regexp.MustCompile(
	`(?:^|\s)(?:/(?:\s|$)|~|/Users(?:/|\s|$)|/home(?:/|\s|$)|/root(?:/|\s|$)|/opt(?:/|\s|$))`,
)

// rmRecursiveForceFlagPair tests whether cmd contains `rm` together with
// both a recursive flag (`-r`, `-R`, `-rf`, `-Rf`, `--recursive`, …) and a
// force flag (`-f`, `-rf`, `--force`, …) anywhere on the command line. The
// bash hook uses three separate regexes for the combined / split / long
// form cases; we approximate the union by checking the three components
// independently and requiring all three to be present.
func rmRecursiveForceTargetsSystemReason(cmd string) (string, bool) {
	if d001ShortRecursiveForceSystem.MatchString(cmd) {
		return "D001: Blocked rm -rf targeting system/home directory", true
	}
	if d001ShortForceRecursiveSystem.MatchString(cmd) {
		return "D001: Blocked rm -rf targeting system/home directory", true
	}
	if d001LongRecursiveForceSystem.MatchString(cmd) {
		return "D001: Blocked rm --recursive --force targeting system/home directory", true
	}
	if rmInvocation.MatchString(cmd) &&
		rmRecursiveFlag.MatchString(cmd) &&
		rmForceFlag.MatchString(cmd) &&
		d001SystemTarget.MatchString(cmd) {
		return "D001: Blocked rm with recursive+force targeting system/home directory", true
	}
	return "", false
}

var (
	d001ShortRecursiveForceSystem = regexp.MustCompile(`(?i)rm\s+-[a-zA-Z]*[rR][a-zA-Z]*f[a-zA-Z]*\s+(?:/\s|/$|~|/Users|/home|/root|/opt)`)
	d001ShortForceRecursiveSystem = regexp.MustCompile(`(?i)rm\s+-[a-zA-Z]*f[a-zA-Z]*[rR][a-zA-Z]*\s+(?:/\s|/$|~|/Users|/home|/root|/opt)`)
	d001LongRecursiveForceSystem  = regexp.MustCompile(`(?i)rm\s+.*--(?:recursive|force)\s+.*--(?:recursive|force).*\s+(?:/\s|/$|~|/Users|/home|/root|/opt)`)

	// rmInvocation matches a top-level `rm` token (start, after a shell
	// separator, or path-prefixed). Mirrors the bash hook's `rm\s` plus the
	// path-prefixed `*/rm` form found in the D002 word loop.
	rmInvocation = regexp.MustCompile(`(?:^|[;|&(\s])(?:/[^\s]*/)?rm(?:\s|$)`)

	// rmRecursiveFlag matches any short or long form of the recursive
	// flag. Short forms like `-rf`, `-Rf`, `-vRf` are covered by the
	// `[a-zA-Z]*[rR]` class. NOTE: no `\b` after the class — `\b` would
	// fail to fire when the character immediately following `r/R` is also a
	// word character (e.g. `-rf` where `r` is followed by `f`), missing
	// exactly the most common D001 pattern.
	rmRecursiveFlag = regexp.MustCompile(`(?:-[a-zA-Z]*[rR]|--recursive)`)

	// rmForceFlag matches any short or long form of the force flag,
	// matching the same shape as rmRecursiveFlag and following the same
	// no-`\b` reasoning.
	rmForceFlag = regexp.MustCompile(`(?:-[a-zA-Z]*f|--force)`)
)
