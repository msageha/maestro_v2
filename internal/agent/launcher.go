package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/uds"
)

// validRoleName permits only alphanumeric, underscore, and hyphen characters.
var validRoleName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validTmuxPane matches the expected TMUX_PANE format: %<number> (e.g. %0, %1, %123).
var validTmuxPane = regexp.MustCompile(`^%\d+$`)

// knownRoles lists all valid role names. Unknown roles are rejected (fail-closed).
var knownRoles = map[string]bool{
	"orchestrator": true,
	"planner":      true,
	"worker":       true,
}

// allowedToolsByRole defines the tools each role is permitted to use.
// Orchestrator is restricted to the narrow delegation/status surface described
// in templates/instructions/orchestrator.md. Planner is restricted to maestro
// CLI commands and .maestro status files.
//
// Workers have no tool restriction (they need full access for task execution).
//
// F-002 invariant: changes to allowedToolsByRole or the sibling deny lists
// (appendDisallowedTools / workerDisallowedTools) must ship with matching
// templates/instructions/{orchestrator,planner,worker,maestro}.md updates.
var allowedToolsByRole = map[string][]string{
	"orchestrator": {
		"Bash(maestro queue write planner --type command:*)",
		"Bash(maestro skill list:*)",
		"Bash(maestro plan request-cancel:*)",
		"Read(.maestro/dashboard.md)",
		"Read(.maestro/results/planner.yaml)",
		"Read(.maestro/config.yaml)",
		// state/continuous.yaml is needed for the Continuous Mode
		// pre-generation gate described in templates/instructions/orchestrator.md.
		// Without this, the Orchestrator cannot verify paused/stopped status
		// before auto-generating the next command.
		"Read(.maestro/state/continuous.yaml)",
	},
	"planner": {
		"Bash(maestro:*)",
		"Read(.maestro/**)",
		"Read(.maestro/dashboard.md)",
		"Read(.maestro/config.yaml)",
		"Read(.maestro/results/*)",
	},
	// worker: unrestricted (empty means all tools allowed)
}

const dangerousPermissionBypassFlag = "--dangerously-skip-permissions"

// Launch reads tmux user variables for the current pane and launches the
// appropriate agent CLI with the correct model and system prompt.
// The runtime is read from the @runtime pane variable (set by formation).
// Managed roles require claude-code because role enforcement depends on
// claude-code tool flags and PreToolUse hooks.
func Launch(maestroDir string) error {
	paneTarget, err := currentPaneTarget()
	if err != nil {
		return fmt.Errorf("determine pane: %w", err)
	}

	_, role, agentModel, agentRuntime, err := readPaneVars(paneTarget)
	if err != nil {
		return err
	}

	systemPrompt, err := buildSystemPrompt(maestroDir, role)
	if err != nil {
		return fmt.Errorf("build system prompt: %w", err)
	}
	maestroPath := ResolvedBinaryPath()
	systemPrompt = appendRuntimeCLIPathInstruction(systemPrompt, maestroPath, maestroDir)

	if agentRuntime != model.RuntimeClaudeCode {
		return launchAlternativeRuntime(agentRuntime, agentModel, role, systemPrompt)
	}

	// claude-code path: build claude-specific args and exec.
	basePromptMode := "append" // default
	if cfg, err := loadBasePromptMode(maestroDir, role); err == nil {
		basePromptMode = cfg
	} else {
		slog.Warn("loadBasePromptMode failed, using default", "error", err, "default", basePromptMode)
	}

	args, err := buildLaunchArgs(role, agentModel, systemPrompt, basePromptMode)
	if err != nil {
		return fmt.Errorf("build launch args: %w", err)
	}
	args = appendWorkspaceReadAllowances(args, maestroDir, role)
	args = appendResolvedMaestroBashAllowances(args, role, maestroPath)

	if role == "worker" {
		args, err = applyWorkerPolicy(maestroDir, args)
		if err != nil {
			return err
		}
	}

	// Resolve claude to an absolute path to avoid PATH-hijacking attacks.
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("resolve claude executable: %w", err)
	}

	env, err := buildLaunchEnvForAgent(os.Environ(), role, maestroDir)
	if err != nil {
		return fmt.Errorf("build launch env: %w", err)
	}

	// Execute claude CLI.
	cmd := exec.Command(claudePath, args...) //nolint:gosec // claudePath is resolved via LookPath; args are constructed from validated config
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return runIgnoringSIGINT(cmd)
}

// launchAlternativeRuntime dispatches non-claude-code runtimes for managed
// roles. Orchestrator and planner remain hard-rejected: those roles operate
// directly on the project root with maestro-CLI access, and the existing
// validate_run_on_main pre-flight is not enough to bound their blast radius.
// Workers may run codex / gemini freely; the validate_run_on_main pre-flight
// is the cross-runtime safety net that backstops them.
func launchAlternativeRuntime(agentRuntime, agentModel, role, systemPrompt string) error {
	if role == "orchestrator" || role == "planner" {
		return fmt.Errorf(
			"role %q cannot run on runtime %q: tool-based role enforcement "+
				"and policy hooks are only available on claude-code. "+
				"Configure agents.%s.model to a Claude model (opus, sonnet, haiku)",
			role, agentRuntime, role)
	}
	if role == "worker" {
		return launchAlternativeWorker(agentRuntime, agentModel, systemPrompt)
	}
	return fmt.Errorf("unsupported runtime %q for unmanaged role %q", agentRuntime, role)
}

// launchAlternativeWorker resolves the runtime command for codex / gemini
// workers and exec's it.
func launchAlternativeWorker(agentRuntime, agentModel, systemPrompt string) error {
	rl := NewRuntimeLauncher()
	cmdName, args, err := rl.GetCommand(agentRuntime, RuntimeLaunchOptions{
		Model:  agentModel,
		Prompt: systemPrompt,
	})
	if err != nil {
		return fmt.Errorf("resolve runtime %q: %w", agentRuntime, err)
	}
	binPath, err := exec.LookPath(cmdName)
	if err != nil {
		return fmt.Errorf("resolve %s executable: %w", cmdName, err)
	}
	env, err := buildAlternativeWorkerEnv(agentRuntime)
	if err != nil {
		// Best-effort: log and fall back to the inherited env. The runtime
		// will simply re-prompt for trust at startup if the override could
		// not be installed; that is no worse than the pre-fix behaviour.
		slog.Warn("codex trust pre-config skipped", "error", err)
		env = os.Environ()
	}

	cmd := exec.Command(binPath, args...) //nolint:gosec // binPath is resolved via LookPath; args come from the registered RuntimeDef
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return runIgnoringSIGINT(cmd)
}

// buildAlternativeWorkerEnv returns the env slice to pass to the runtime's
// exec.Command. For codex it points CODEX_HOME at a per-worker temp dir
// containing a config.toml that pre-trusts the worker pane's working
// directory. For other runtimes it falls back to the inherited environment.
// Callers handle a non-nil error by logging and reverting to os.Environ().
func buildAlternativeWorkerEnv(agentRuntime string) ([]string, error) {
	if agentRuntime != model.RuntimeCodex {
		return os.Environ(), nil
	}
	codexHome, err := prepareCodexHomeForCurrentWorker()
	if err != nil {
		return nil, err
	}
	if codexHome == "" {
		return os.Environ(), nil
	}
	env := os.Environ()
	// Replace any inherited CODEX_HOME so this worker's overrides win.
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "CODEX_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "CODEX_HOME="+codexHome)
	return out, nil
}

// prepareCodexHomeForCurrentWorker materialises a per-worker CODEX_HOME so
// codex skips the first-run "Do you trust the contents of this directory?"
// prompt in maestro worker panes. codex 0.125.0 still surfaces that prompt
// in panes opened by tmux even with `--dangerously-bypass-approvals-and-
// sandbox` set, and the `-c projects."<path>".trust_level="trusted"`
// command-line override is parsed inconsistently for paths with embedded
// quotes (the 2026-04 audit reproduced both: codex worker panes idled on
// the trust prompt while dispatch_task_success was logged). Writing the
// trust entry into a real config file under a dedicated CODEX_HOME bypasses
// the `-c` parser entirely and matches the documented codex config schema.
//
// Layout:
//
//	<CODEX_HOME>/config.toml      ← user's original + appended trust entries
//	<CODEX_HOME>/<other entries>  ← symlinked from ~/.codex (auth.json,
//	                                 cache, history, etc.)
//
// The dir is keyed by (PID, agent_id) so concurrent workers and concurrent
// formations do not contend for the same path. We intentionally skip any
// CODEX_HOME setup when the user has no ~/.codex (codex's own defaults
// will then apply, including the trust prompt — but in that case the user
// has never run codex interactively either, so the trust prompt is the
// expected and correct first-run flow).
func prepareCodexHomeForCurrentWorker() (string, error) {
	userCodex := defaultUserCodexHome()
	if userCodex == "" {
		return "", nil
	}
	if _, err := os.Stat(userCodex); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("stat user CODEX_HOME %s: %w", userCodex, err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("absolute cwd: %w", err)
	}
	// Symlink-resolved variant matters on macOS where /tmp ↔ /private/tmp
	// and /var ↔ /private/var diverge. Trust both forms so codex matches
	// regardless of which one it canonicalises to.
	realPath, evalErr := filepath.EvalSymlinks(abs)
	if evalErr != nil {
		realPath = abs
	}

	pid := os.Getpid()
	agentID, _ := os.LookupEnv("MAESTRO_AGENT_ID") // best-effort tag for debuggability
	if agentID == "" {
		agentID = "worker"
	}
	tmpRoot := os.TempDir()
	codexHome := filepath.Join(tmpRoot, fmt.Sprintf("maestro-codex-%d-%s", pid, agentID))
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return "", fmt.Errorf("create per-worker CODEX_HOME %s: %w", codexHome, err)
	}

	// Symlink every entry from the user's ~/.codex into ours, EXCEPT
	// config.toml (which we are about to overwrite with our own copy +
	// trust appendix). This preserves auth.json, cache, history, vendor
	// imports, etc. — without those, codex either refuses to launch (no
	// auth) or behaves degraded.
	if err := mirrorCodexHomeEntries(userCodex, codexHome); err != nil {
		return "", err
	}
	if err := writeCodexConfigWithTrust(userCodex, codexHome, abs, realPath); err != nil {
		return "", err
	}
	return codexHome, nil
}

// defaultUserCodexHome returns the CODEX_HOME the user already has (env
// override wins), or ~/.codex as the documented default. Empty string
// signals "no user codex home configured" and disables the trust override.
func defaultUserCodexHome() string {
	if env := os.Getenv("CODEX_HOME"); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// mirrorCodexHomeEntries symlinks every direct child of src into dst so the
// per-worker CODEX_HOME exposes auth, cache, and other state without
// copying. Existing entries in dst are removed first so reused dirs (same
// PID + agent_id across launches) refresh stale links. config.toml is
// excluded because writeCodexConfigWithTrust writes a fresh copy.
func mirrorCodexHomeEntries(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read user codex dir %s: %w", src, err)
	}
	for _, entry := range entries {
		if entry.Name() == "config.toml" {
			continue
		}
		linkSrc := filepath.Join(src, entry.Name())
		linkDst := filepath.Join(dst, entry.Name())
		// Best-effort cleanup of a stale entry from a prior launch.
		_ = os.Remove(linkDst)
		if err := os.Symlink(linkSrc, linkDst); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", linkSrc, linkDst, err)
		}
	}
	return nil
}

// writeCodexConfigWithTrust copies the user's config.toml into the per-
// worker CODEX_HOME and ensures `[projects."<path>"]` trust entries are
// present for the worker's logical and symlink-resolved working
// directories.
//
// All existing `[projects.*]` (and `[projects.*.*]`) sections in the
// user's config are stripped before the trust entries are appended. This
// is more aggressive than only stripping the exact target path, but it is
// the only way to avoid TOML duplicate-table errors for the realistic
// case where the user's config records the same cwd under a different
// path representation than codex / our launcher canonicalise to. The two
// representations that diverge in practice on macOS are
// `/var/folders/...` (returned by `cd` from a shell) and
// `/private/var/folders/...` (returned by `os.Getwd()` after Go's runtime
// resolves the leading symlink); a user who ran `codex` interactively in
// the same dir will have one form on disk and we will write the other,
// producing a parse error if both are kept.
//
// Worker isolation argument: the per-worker CODEX_HOME exists solely so
// the worker pane can run in its own cwd without a trust prompt. The
// worker never visits the user's other project paths, so dropping their
// trust entries from this file is operationally a no-op.
func writeCodexConfigWithTrust(userCodex, codexHome, logicalPath, realPath string) error {
	dst := filepath.Join(codexHome, "config.toml")
	src := filepath.Join(userCodex, "config.toml")

	var data []byte
	// src points at the user's existing CODEX_HOME/config.toml. The path
	// is constructed from a controlled application directory plus the
	// fixed "config.toml" basename, so gosec G304 (file inclusion via
	// variable) does not apply here.
	if d, err := os.ReadFile(src); err == nil { //nolint:gosec // controlled CODEX_HOME path; basename fixed
		data = d
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read user codex config: %w", err)
	}

	data = stripCodexProjectsSections(data)

	targets := []string{logicalPath}
	if realPath != "" && realPath != logicalPath {
		targets = append(targets, realPath)
	}

	var b bytes.Buffer
	b.Write(data)
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		b.WriteByte('\n')
	}
	b.WriteString("\n# Maestro auto-generated trust entries — any prior [projects.*] sections from the user's config were stripped to avoid TOML duplicate-table errors when path canonicalisation differs.\n")
	for _, t := range targets {
		fmt.Fprintf(&b, "[projects.%q]\ntrust_level = %q\n", t, "trusted")
	}

	if err := os.WriteFile(dst, b.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write per-worker codex config %s: %w", dst, err)
	}
	return nil
}

// stripCodexProjectsSections removes every standard TOML table whose
// header begins with `projects.` (e.g. `[projects."/foo"]`,
// `[projects."/foo".trusted]`) from data, returning the remainder.
//
// Array-of-tables headers (`[[projects."..."]]`) are left intact — codex
// does not use that schema for the trust system today, and we cannot
// safely fold them into our standard-table appendix. A non-projects
// header (`[settings]`, `[ui]`, ...) ends any in-progress strip.
func stripCodexProjectsSections(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	var out bytes.Buffer
	skipping := false
	// SplitAfter preserves trailing newlines on each line, so writing the
	// kept lines back verbatim reproduces the user's formatting faithfully.
	for _, line := range strings.SplitAfter(string(data), "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		if isStandardTableHeader(trimmed) {
			if isProjectsHeader(trimmed) {
				skipping = true
				continue
			}
			skipping = false
		}
		if !skipping {
			out.WriteString(line)
		}
	}
	return out.Bytes()
}

// isStandardTableHeader returns true for `[ ... ]` headers, excluding
// `[[ ... ]]` array-of-tables headers.
func isStandardTableHeader(s string) bool {
	return strings.HasPrefix(s, "[") &&
		strings.HasSuffix(s, "]") &&
		!strings.HasPrefix(s, "[[")
}

// isProjectsHeader returns true if s is `[projects.<anything>]` — covers
// quoted (`"..."`/`'...'`), bare-key, and dotted-key forms. Any header
// whose trimmed inner content begins with the literal token `projects.`
// counts; this is intentionally permissive because we want to strip the
// whole subtree regardless of which TOML quoting form the user wrote.
func isProjectsHeader(s string) bool {
	if !isStandardTableHeader(s) {
		return false
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	return inner == "projects" || strings.HasPrefix(inner, "projects.")
}

// GCStaleCodexHomes removes <TempDir>/maestro-codex-<pid>-<agent_id>
// directories whose owning PID is no longer alive. Designed to be invoked
// once per daemon startup so accumulated tempdirs from prior daemon
// sessions do not pile up under macOS /var/folders/... or Linux /tmp.
//
// Returns (removedCount, errs). errs is non-nil only when an individual
// removal fails; the GC continues across remaining entries so a single
// permission error does not block cleanup of the rest. The current daemon's
// own tempdirs are NOT touched (their PID matches os.Getpid()), so calling
// this from prepareStartup before any worker pane launches is safe.
//
// Naming contract — must match prepareCodexHomeForCurrentWorker:
//
//	maestro-codex-<pid>-<agent_id>
//
// where <pid> is a positive integer. Anything else under TempDir is
// ignored.
func GCStaleCodexHomes() (int, []error) {
	tmpRoot := os.TempDir()
	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		return 0, []error{fmt.Errorf("read tmp dir %s: %w", tmpRoot, err)}
	}
	var errs []error
	removed := 0
	currentPID := os.Getpid()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "maestro-codex-") {
			continue
		}
		if !entry.IsDir() {
			continue
		}
		rest := strings.TrimPrefix(name, "maestro-codex-")
		// rest format: "<pid>-<agent_id>"
		dash := strings.IndexByte(rest, '-')
		if dash <= 0 {
			continue // malformed; safer to leave alone
		}
		pidStr := rest[:dash]
		var pid int
		if _, parseErr := fmt.Sscanf(pidStr, "%d", &pid); parseErr != nil || pid <= 0 {
			continue
		}
		if pid == currentPID {
			continue // the current daemon's own dirs (not yet created at startup, but be defensive)
		}
		if processAlive(pid) {
			continue // some other live process still owns this dir
		}
		target := filepath.Join(tmpRoot, name)
		if err := os.RemoveAll(target); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", target, err))
			continue
		}
		removed++
	}
	return removed, errs
}

// processAlive reports whether the given PID is reachable. Uses signal 0,
// which performs the kernel's permission/existence check without actually
// delivering a signal. ESRCH (no such process) is the canonical "dead"
// answer; any other error (EPERM in particular) means the process exists
// but we are not allowed to signal it — still alive from the GC's
// perspective.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	return true
}

// readPaneVars reads and validates the tmux user variables (agent_id, role, model, runtime)
// from the given pane target. The runtime variable is non-fatal: missing or empty values
// fall back to model.DefaultRuntime() ("claude-code") without error.
func readPaneVars(paneTarget string) (agentID, role, agentModel, agentRuntime string, err error) {
	agentID, err = tmux.GetUserVar(paneTarget, "agent_id")
	if err != nil {
		return "", "", "", "", fmt.Errorf("read @agent_id: %w", err)
	}
	if agentID == "" {
		return "", "", "", "", fmt.Errorf("@agent_id is empty for pane %s", sanitizeForLog(paneTarget))
	}

	role, err = tmux.GetUserVar(paneTarget, "role")
	if err != nil {
		return "", "", "", "", fmt.Errorf("read @role: %w", err)
	}
	if role == "" {
		return "", "", "", "", fmt.Errorf("@role is empty for pane %s", sanitizeForLog(paneTarget))
	}
	if !validRoleName.MatchString(role) {
		return "", "", "", "", fmt.Errorf("invalid role name %q: must be alphanumeric, underscore, or hyphen", sanitizeForLog(role))
	}

	agentModel, err = tmux.GetUserVar(paneTarget, "model")
	if err != nil {
		return "", "", "", "", fmt.Errorf("read @model: %w", err)
	}

	// Runtime is optional: unset or empty falls back to the default without error.
	agentRuntime, err = tmux.GetUserVar(paneTarget, "runtime")
	if err != nil {
		slog.Warn("read @runtime failed, using default", "error", err, "default", model.DefaultRuntime())
	}
	if agentRuntime == "" {
		agentRuntime = model.DefaultRuntime()
	}

	// For claude-code, the model must be non-empty.
	// For other runtimes, empty model is allowed (the runtime uses its own default).
	if agentModel == "" && agentRuntime == model.RuntimeClaudeCode {
		return "", "", "", "", fmt.Errorf("@model is empty for pane %s", sanitizeForLog(paneTarget))
	}

	return agentID, role, agentModel, agentRuntime, nil
}

// applyWorkerPolicy appends the worker-specific policy hook settings to the
// CLI args. HookSettings produces merged JSON containing both Notification
// disablement and PreToolUse policy hook.
func applyWorkerPolicy(maestroDir string, args []string) ([]string, error) {
	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScript()
	if err != nil {
		return nil, fmt.Errorf("write policy hook script: %w", err)
	}
	hookSettings, err := pc.HookSettings(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("build policy hook settings: %w", err)
	}
	return append(args, "--settings", hookSettings), nil
}

// runIgnoringSIGINT runs the command while ignoring SIGINT so that only the
// child process (claude) handles Ctrl+C. Without this, the Go runtime
// terminates the parent on SIGINT, orphaning the child.
func runIgnoringSIGINT(cmd *exec.Cmd) error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	defer func() {
		signal.Stop(sigCh)
		close(sigCh) // Unblock the drain goroutine so it can exit.
	}()
	go func() {
		for range sigCh { //nolint:revive // intentional drain: claude handles SIGINT directly
		}
	}()
	err := cmd.Run()
	if err != nil {
		// Log signal-terminated exits at warn level to aid diagnosis of unexpected
		// SIGKILL / SIGSEGV events (e.g. OOM killer, sandbox violations).
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
			ws, ok := exitErr.Sys().(interface {
				Signaled() bool
				Signal() syscall.Signal
			})
			if ok && ws.Signaled() {
				slog.Warn("agent process terminated by signal",
					"signal", ws.Signal().String(),
					"cmd", cmd.Path)
			}
		}
	}
	return err
}

// buildLaunchArgs constructs the CLI arguments for the claude command.
// basePromptMode controls the system prompt flag: "replace" uses --system-prompt,
// "append" (or empty) uses --append-system-prompt.
func buildLaunchArgs(role, agentModel, systemPrompt, basePromptMode string) ([]string, error) {
	if !knownRoles[role] {
		return nil, fmt.Errorf("unknown role %q: rejected (fail-closed)", role)
	}

	args := launchArgsCore(role, agentModel, systemPrompt, basePromptMode)
	args = appendAllowedTools(args, role)
	args = appendDisallowedTools(args, role)
	args = appendNotificationSettings(args, role)
	return args, nil
}

// launchArgsCore returns the common positional flags shared by every role:
// model, system-prompt (replace or append mode per basePromptMode), and the
// dangerously-skip-permissions bypass.
func launchArgsCore(role, agentModel, systemPrompt, basePromptMode string) []string {
	_ = role // role currently does not influence core flags but kept for symmetry
	promptFlag := "--append-system-prompt"
	if basePromptMode == "replace" {
		promptFlag = "--system-prompt"
	}
	return []string{
		"--model", agentModel,
		promptFlag, systemPrompt,
		dangerousPermissionBypassFlag,
	}
}

// appendAllowedTools applies the role-specific allow-list when one is
// configured. Workers pass through without an allow-list because the worker
// surface is intentionally unrestricted.
func appendAllowedTools(args []string, role string) []string {
	tools, ok := allowedToolsByRole[role]
	if !ok || len(tools) == 0 {
		return args
	}
	return append(args, "--allowedTools", strings.Join(tools, ","))
}

// appendDisallowedTools applies role-scoped tool blocks. Currently:
//   - planner: blocks operator-only recovery commands while keeping the
//     standard maestro Bash surface available.
//   - worker:  blocks destructive tmux subcommands, recovery API escape
//     hatches (D009), and direct reads under .maestro/.
//
// Notes preserved from the original implementation:
//   - Planner intentionally retains `plan resume-merge` and `plan
//     add-retry-task` (primary recovery / retry mechanisms).
//   - Workers also block the legacy `maestro resolve-conflict` spelling as
//     defense-in-depth in case `content` reintroduces it.
func appendDisallowedTools(args []string, role string) []string {
	switch role {
	case "planner":
		return append(args, "--disallowedTools", "Bash(maestro plan unquarantine:*)")
	case "worker":
		return append(args, "--disallowedTools", strings.Join(workerDisallowedTools, ","))
	default:
		return args
	}
}

// workerDisallowedTools lists the tool patterns Claude CLI must hard-block
// for Workers. The textual prohibitions in worker.md (D006, .maestro/ access)
// are not enforced by the CLI; this list provides the technical guardrail.
var workerDisallowedTools = []string{
	"Bash(tmux kill-server:*)",
	"Bash(tmux kill-session:*)",
	"Bash(tmux kill-pane:*)",
	"Bash(tmux kill-window:*)",
	// D009: recovery API escape hatches are operator-only. Workers must
	// never invoke these even if a future content payload tries to embed
	// them.
	"Bash(maestro plan unquarantine:*)",
	"Bash(maestro plan resume-merge:*)",
	"Bash(maestro plan resolve-conflict:*)",
	// Legacy form (no `plan` segment): unreachable via the current CLI
	// router but blocked here too as defense-in-depth in case `content`
	// reintroduces the historical spelling.
	"Bash(maestro resolve-conflict:*)",
	"Read(.maestro/state/**)",
	"Read(.maestro/queue/**)",
	"Read(.maestro/results/**)",
	"Read(.maestro/locks/**)",
	"Read(.maestro/logs/**)",
	"Read(.maestro/config.yaml)",
	"Read(.maestro/dashboard.md)",
}

// appendNotificationSettings disables Claude Code Notification hooks for
// internal autonomous agents.
//
// Behaviour by role:
//   - orchestrator: user-facing agent — keep user hooks intact.
//   - worker:       Notification=[] is merged with the PreToolUse policy hook
//     into a single --settings flag by HookSettings()
//     (policy_checker.go); we add nothing here.
//   - planner / other internal roles: emit a Notification=[] only.
//
// Sandbox settings are intentionally NOT injected here. Passing a sandbox
// section via --settings overrides the user's global sandbox.enabled:false
// and disables the /sandbox runtime command. The
// allowAllUnixSockets entry required for the daemon UDS connection
// (.maestro/daemon.sock) must live in the user's global
// ~/.claude/settings.json or the project's .claude/settings.json instead.
func appendNotificationSettings(args []string, role string) []string {
	switch role {
	case "orchestrator", "worker":
		return args
	default:
		return append(args, "--settings", `{"hooks":{"Notification":[]}}`)
	}
}

func appendWorkspaceReadAllowances(args []string, maestroDir, role string) []string {
	if role != "planner" && role != "orchestrator" {
		return args
	}
	extra := workspaceReadAllowances(maestroDir, role)
	if len(extra) == 0 {
		return args
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--allowedTools" {
			args[i+1] = appendToolList(args[i+1], extra)
			return args
		}
	}
	return append(args, "--allowedTools", strings.Join(extra, ","))
}

func workspaceReadAllowances(maestroDir, role string) []string {
	if maestroDir == "" {
		return nil
	}
	paths := canonicalMaestroDirs(maestroDir)
	if len(paths) == 0 {
		return nil
	}
	var tools []string
	add := func(tool string) {
		for _, existing := range tools {
			if existing == tool {
				return
			}
		}
		tools = append(tools, tool)
	}
	for _, dir := range paths {
		if role == "planner" {
			add("Read(" + filepath.ToSlash(filepath.Join(dir, "dashboard.md")) + ")")
			add("Read(" + filepath.ToSlash(filepath.Join(dir, "config.yaml")) + ")")
			add("Read(" + filepath.ToSlash(filepath.Join(dir, "results")) + "/*)")
			add("Read(" + filepath.ToSlash(dir) + "/**)")
			continue
		}
		for _, rel := range []string{
			"dashboard.md",
			"results/planner.yaml",
			"config.yaml",
			"state/continuous.yaml",
		} {
			add("Read(" + filepath.ToSlash(filepath.Join(dir, rel)) + ")")
		}
	}
	return tools
}

func canonicalMaestroDirs(maestroDir string) []string {
	var dirs []string
	add := func(path string) {
		if path == "" {
			return
		}
		path = filepath.Clean(path)
		for _, existing := range dirs {
			if existing == path {
				return
			}
		}
		dirs = append(dirs, path)
	}
	add(maestroDir)
	if abs, err := filepath.Abs(maestroDir); err == nil {
		add(abs)
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			add(resolved)
		}
	}
	return dirs
}

func appendToolList(existing string, extra []string) string {
	seen := make(map[string]bool)
	var tools []string
	for _, tool := range strings.Split(existing, ",") {
		tool = strings.TrimSpace(tool)
		if tool == "" || seen[tool] {
			continue
		}
		seen[tool] = true
		tools = append(tools, tool)
	}
	for _, tool := range extra {
		tool = strings.TrimSpace(tool)
		if tool == "" || seen[tool] {
			continue
		}
		seen[tool] = true
		tools = append(tools, tool)
	}
	return strings.Join(tools, ",")
}

func appendResolvedMaestroBashAllowances(args []string, role, maestroPath string) []string {
	if maestroPath == "" || maestroPath == "maestro" {
		return args
	}
	var extra []string
	switch role {
	case "orchestrator":
		extra = []string{
			"Bash(" + maestroPath + " queue write planner --type command:*)",
			"Bash(" + maestroPath + " skill list:*)",
			"Bash(" + maestroPath + " plan request-cancel:*)",
		}
	case "planner":
		extra = []string{"Bash(" + maestroPath + ":*)"}
	default:
		return args
	}
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--allowedTools" {
			args[i+1] = appendToolList(args[i+1], extra)
			return args
		}
	}
	return append(args, "--allowedTools", strings.Join(extra, ","))
}

// buildSystemPrompt combines maestro.md + instructions/{role}.md.
func buildSystemPrompt(maestroDir, role string) (string, error) {
	// Read maestro.md
	maestroPath := filepath.Join(maestroDir, "maestro.md")
	maestroContent, err := os.ReadFile(maestroPath) //nolint:gosec // maestroPath is constructed from a controlled application directory
	if err != nil {
		return "", fmt.Errorf("read maestro.md: %w", err)
	}

	// Read instructions/{role}.md
	instructionsPath := filepath.Join(maestroDir, "instructions", role+".md")
	instructionsContent, err := os.ReadFile(instructionsPath) //nolint:gosec // instructionsPath is constructed from a controlled application directory
	if err != nil {
		return "", fmt.Errorf("read instructions/%s.md: %w", role, err)
	}

	// Concatenate with separator
	var sb strings.Builder
	sb.Write(maestroContent)
	sb.WriteString("\n\n---\n\n")
	sb.Write(instructionsContent)

	return sb.String(), nil
}

func appendRuntimeCLIPathInstruction(prompt, maestroPath, maestroDir string) string {
	if maestroPath == "" || maestroPath == "maestro" {
		return prompt
	}
	activeDir := maestroDir
	if canonical, err := canonicalMaestroDir(maestroDir); err == nil {
		activeDir = canonical
	}
	return prompt + "\n\n---\n\n## Runtime CLI Path\n\n" +
		"All `maestro` CLI commands in these instructions MUST be executed with this exact binary path:\n\n" +
		"```\n" + maestroPath + "\n```\n\n" +
		"Do not rely on the shell `PATH`; Claude Code shell snapshots may resolve an older `maestro` binary.\n" +
		"Do not `cd` to the binary directory before running it. Keep the task workspace as cwd; `MAESTRO_DIR` is already set for daemon routing.\n\n" +
		"The active `.maestro` directory for Read operations is:\n\n" +
		"```\n" + filepath.ToSlash(activeDir) + "\n```\n\n" +
		"Never read `.maestro` from the binary source directory or the user home directory.\n"
}

// LaunchCommand is the shell command to start an agent process in a tmux pane
// using bare PATH resolution. Prefer ResolvedLaunchCommand() when the absolute
// binary path is available to prevent version skew.
const LaunchCommand = "maestro agent launch"

// ResolvedLaunchCommand returns the shell command to start an agent process
// using the absolute path of the current maestro binary. This prevents the
// common "binary version skew" failure where the pane shell's PATH resolves an
// older maestro (e.g. a globally-installed /usr/local/bin/maestro) instead of
// the binary that started the formation (e.g. a freshly-built /tmp/bin/maestro).
// Without this, flags added in the new binary (such as --run-on-integration) are
// silently missing in the pane's execution context.
//
// Falls back to LaunchCommand (bare PATH resolution) if os.Executable() fails or
// the resolved path does not exist on disk.
//
// Deprecated: prefer ResolvedLaunchCommandFor(maestroDir) so that the pane's
// `maestro agent launch` invocation always sees MAESTRO_DIR. Retained for
// callers that genuinely cannot supply a maestroDir (legacy tests).
func ResolvedLaunchCommand() string {
	return ResolvedBinaryPath() + " agent launch"
}

// ResolvedLaunchCommandFor returns the shell command to start an agent process
// in a tmux pane, with MAESTRO_DIR pinned to the supplied project-root
// .maestro/ directory.
//
// Why the env prefix exists: when a worker pane sits inside a worktree whose
// tracked file set includes a partial .maestro/ subtree (e.g. the user has a
// global gitignore that masks .maestro/* but force-tracks .maestro/config.yaml
// so it reappears in worktree checkouts), the bootstrap `maestro agent launch`
// process would otherwise fall back to findMaestroDir's cwd walk-up and stop
// at that partial .maestro/. The agent launcher then fails with
// `.maestro/maestro.md: no such file or directory` because the partial dir
// has no maestro.md. Setting MAESTRO_DIR via env prefix forces findMaestroDir
// onto the env-var branch, which points at the real project root.
//
// If maestroDir is empty (test paths, edge cases) or fails canonicalisation,
// this falls back to the bare ResolvedLaunchCommand() so behaviour is no
// worse than before.
func ResolvedLaunchCommandFor(maestroDir string) string {
	cmd := ResolvedLaunchCommand()
	if maestroDir == "" {
		return cmd
	}
	canonical, err := canonicalMaestroDir(maestroDir)
	if err != nil {
		return cmd
	}
	return "MAESTRO_DIR=" + shellSingleQuote(canonical) + " " + cmd
}

// ResolvedBinaryPath returns the absolute, symlink-resolved path of the
// currently-running maestro binary, falling back to the literal string
// "maestro" when the path cannot be determined or no longer exists. The
// resolved path is used by formation wiring to call the same binary
// version that started the daemon, defeating PATH-based version skew.
func ResolvedBinaryPath() string {
	execPath, err := os.Executable()
	if err != nil || execPath == "" {
		return "maestro"
	}
	// Resolve symlinks so agents can locate the real binary directory for PATH.
	if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = resolved
	}
	// Verify the path still exists (guard against deleted-but-still-running binary).
	if _, err := os.Stat(execPath); err != nil {
		return "maestro"
	}
	return execPath
}

// buildLaunchEnv constructs the environment for the agent CLI process.
//   - Clears CLAUDECODE to allow launching inside a parent Claude Code session
//     (e.g. when maestro is invoked from Claude Code CLI).
//   - Strips dangerous env var prefixes to prevent library injection / path hijacking.
//   - Sets MAESTRO_AGENT_ROLE for role-based trust boundaries.
//   - Prepends the current maestro binary's directory to PATH so that agents
//     agent process calls the same maestro version used by formation. This is the
//     second line of defence against binary version skew: even if the shell somehow
//     resolves an old maestro for `maestro agent launch`, any subsequent `maestro`
//     calls made by the AI (plan add-task, queue write, etc.) will still find the
//     correct binary first.
//
// Note: workspace trust dialog bypass is handled at the formation level
// (auto-accept after agent launch), not via environment variables. Claude Code
// does not expose an env var to skip the trust dialog; --dangerously-skip-permissions
// only covers per-tool permission checks.
//
// dangerousEnvPrefixes lists environment variable prefixes that must be
// stripped from child processes to prevent library injection or path hijacking.
var dangerousEnvPrefixes = []string{
	"DYLD_",           // macOS dynamic linker injection (DYLD_INSERT_LIBRARIES, etc.)
	"LD_PRELOAD",      // Linux shared library injection
	"LD_LIBRARY_PATH", // Linux library path override
	"GIT_EXEC_PATH",   // git executable path override
	"GIT_DIR",         // git directory override (could redirect operations)
}

func buildLaunchEnv(base []string, role string) []string {
	env := filterEnv(base, "CLAUDECODE")
	env = filterDangerousEnv(env)
	env = append(env, uds.CallerRoleEnv+"="+role)
	// Prepend the current binary's directory to PATH so agents call the correct
	// maestro version. Only applied when the binary path can be determined.
	if execPath, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
			execPath = resolved
		}
		if binDir := filepath.Dir(execPath); binDir != "" && binDir != "." {
			env = prependToPath(env, binDir)
		}
	}
	return env
}

func buildLaunchEnvForAgent(base []string, role, maestroDir string) ([]string, error) {
	env := buildLaunchEnv(base, role)
	if maestroDir == "" {
		return env, nil
	}
	canonicalDir, err := canonicalMaestroDir(maestroDir)
	if err != nil {
		return nil, err
	}
	env = setEnv(env, "MAESTRO_DIR", canonicalDir)

	// Default GOCACHE to a project-local path so the agent's `go build`,
	// `go test`, etc. don't hit `permission denied` against
	// ~/Library/Caches/go-build inside the claude-code sandbox.
	// The 2026-04-28 retest2 reported Workers manually re-running with
	// `GOCACHE=$TMPDIR/go-cache` after the first build failed; pinning a
	// safe default here removes that workaround. Operators that need a
	// shared cache (CI, dev shells) can still override by exporting
	// GOCACHE before launching the daemon — explicit env beats our default.
	if !envHasKey(env, "GOCACHE") {
		env = setEnv(env, "GOCACHE", filepath.Join(canonicalDir, "cache", "go-build"))
	}

	// Force TERM to a usable terminfo entry when the inherited value is
	// missing or stuck at "dumb". 2026-04-28 retest3 surfaced two noise
	// modes that both trace back to TERM:
	//   - starship in the agent pane prints "unable to determine terminal
	//     type" each time the prompt redraws because it cannot read the
	//     terminfo capabilities for "dumb";
	//   - operators monitoring CLI output saw the starship banner mixed
	//     into the structured maestro output, complicating log parsing.
	// "xterm-256color" is the broadest-compat entry that ships with
	// macOS and most Linux distributions; tmux itself rewrites the value
	// inside its panes to "tmux-256color" on capable systems, so this
	// only kicks in when the operator's outer shell already lost the
	// real value (e.g. starting from an SSH session with TERM=dumb).
	if v := envValue(env, "TERM"); v == "" || v == "dumb" {
		env = setEnv(env, "TERM", "xterm-256color")
	}
	// Move mise's cache to a project-local writable path. The default
	// ~/.cache/mise often hits "Operation not permitted" inside the
	// claude-code sandbox (retest3 Planner pane), and the WARN line
	// every shell init pollutes daemon log captures. Operators using
	// mise for language version pinning still get the cache benefit —
	// just under .maestro/cache/mise instead of $HOME/.cache/mise.
	if !envHasKey(env, "MISE_CACHE_DIR") {
		env = setEnv(env, "MISE_CACHE_DIR", filepath.Join(canonicalDir, "cache", "mise"))
	}
	wrapperDir, err := ensureRoleMaestroWrapper(maestroDir, role)
	if err != nil {
		return nil, err
	}
	return prependToPath(env, wrapperDir), nil
}

func canonicalMaestroDir(maestroDir string) (string, error) {
	dir, err := filepath.Abs(maestroDir)
	if err != nil {
		return "", fmt.Errorf("resolve maestro dir: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if info, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("stat maestro dir: %w", err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("maestro dir %q is not a directory", sanitizeForLog(dir))
	}
	return dir, nil
}

func ensureRoleMaestroWrapper(maestroDir, role string) (string, error) {
	if !knownRoles[role] {
		return "", fmt.Errorf("unknown role %q", sanitizeForLog(role))
	}
	canonicalDir, err := canonicalMaestroDir(maestroDir)
	if err != nil {
		return "", err
	}
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(execPath); err == nil {
		execPath = resolved
	}
	if _, err := os.Stat(execPath); err != nil {
		return "", fmt.Errorf("stat current executable: %w", err)
	}

	wrapperDir := filepath.Join(maestroDir, "bin", "roles", role)
	if err := os.MkdirAll(wrapperDir, 0o700); err != nil {
		return "", fmt.Errorf("create role wrapper dir: %w", err)
	}
	wrapperPath := filepath.Join(wrapperDir, "maestro")
	// 2026-04-28 E2E: when the launching binary lives in a short-lived path
	// (e.g. an in-tree build artifact like /repo/maestro that gets removed by
	// `make clean` or a worktree rotation), the wrapper's hard-coded `exec`
	// fails with shell exit 126/127 in the middle of a long agent session,
	// breaking every CLI call (`maestro plan ...`, `maestro queue ...`) the
	// agent makes. The wrapper now tries the absolute path FIRST (preserves
	// version-skew protection — same binary that started the formation), and
	// if that file no longer exists falls back to the PATH-resolved `maestro`.
	// buildLaunchEnv already prepends the launching binary's directory to PATH,
	// so when both paths are valid the fallback resolves to the same binary;
	// when the absolute path has been removed but a stable PATH-installed
	// `maestro` is still available (e.g. ~/Works/bin/maestro), the agent stays
	// functional. The warning written to stderr surfaces in the pane and the
	// agent's own logs so operators can correlate the fallback with a rebuild.
	body := "#!/bin/sh\n" +
		"export " + uds.CallerRoleEnv + "=" + shellSingleQuote(role) + "\n" +
		"export MAESTRO_DIR=" + shellSingleQuote(canonicalDir) + "\n" +
		"maestro_exec_path=" + shellSingleQuote(execPath) + "\n" +
		"if [ -x \"$maestro_exec_path\" ]; then\n" +
		"  exec \"$maestro_exec_path\" \"$@\"\n" +
		"fi\n" +
		"echo \"[maestro role wrapper] launching-binary path '$maestro_exec_path' no longer exists; falling back to PATH-resolved maestro (rebuild or worktree rotation may have removed it)\" >&2\n" +
		"exec maestro \"$@\"\n"
	// 0o700: the wrapper is a shell script that the agent's pane
	// invokes by exec, so it MUST carry the owner-execute bit.
	// gosec G306 prefers 0o600 for general writes but this file is
	// intentionally executable; the owner-only mode keeps blast
	// radius minimal.
	if err := os.WriteFile(wrapperPath, []byte(body), 0o700); err != nil { //nolint:gosec // intentionally executable per the comment above
		return "", fmt.Errorf("write role wrapper: %w", err)
	}
	return wrapperDir, nil
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// prependToPath returns a copy of env with dir prepended to the PATH entry.
// If no PATH entry is found, a new one is added using the current process's PATH.
func prependToPath(env []string, dir string) []string {
	pathPrefix := "PATH="
	for i, e := range env {
		if strings.HasPrefix(e, pathPrefix) {
			existing := e[len(pathPrefix):]
			result := make([]string, len(env))
			copy(result, env)
			result[i] = pathPrefix + dir + ":" + existing
			return result
		}
	}
	// PATH not present in env — construct from the current process environment.
	return append(env, pathPrefix+dir+":"+os.Getenv("PATH"))
}

func setEnv(env []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, len(env), len(env)+1)
	copy(result, env)
	for i, e := range result {
		if strings.HasPrefix(e, prefix) {
			result[i] = prefix + value
			return result
		}
	}
	return append(result, prefix+value)
}

// envHasKey reports whether env contains an entry beginning with name=.
// Used by buildLaunchEnvForAgent to defer to operator-supplied defaults
// for keys like GOCACHE without overwriting an explicit export.
func envHasKey(env []string, name string) bool {
	prefix := name + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// envValue returns the value of name= in env, or "" if absent. Distinct
// from envHasKey when the caller needs to make a decision based on the
// existing value (e.g. "promote TERM=dumb to a real terminfo entry").
func envValue(env []string, name string) string {
	prefix := name + "="
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, prefix); ok {
			return v
		}
	}
	return ""
}

// filterDangerousEnv removes environment variables matching dangerousEnvPrefixes
// to prevent library injection and path hijacking in child processes.
// Each entry in dangerousEnvPrefixes is matched as a prefix against the variable
// name (the part before "="). For example, "DYLD_" matches "DYLD_INSERT_LIBRARIES",
// and "LD_PRELOAD" matches both "LD_PRELOAD" and "LD_PRELOAD_32".
func filterDangerousEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		// Extract variable name (everything before the first "=").
		name := e
		if idx := strings.IndexByte(e, '='); idx >= 0 {
			name = e[:idx]
		}
		dangerous := false
		for _, prefix := range dangerousEnvPrefixes {
			if strings.HasPrefix(name, prefix) {
				dangerous = true
				break
			}
		}
		if !dangerous {
			out = append(out, e)
		}
	}
	return out
}

// filterEnv returns a copy of environ with the named variable removed.
func filterEnv(environ []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environ))
	for _, e := range environ {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// sanitizeForLog truncates a string to maxLen and removes control characters
// to prevent log injection when including untrusted values in error messages.
// Covers ASCII control chars (0x00-0x1F, 0x7F) and Unicode line/paragraph
// separators (U+2028, U+2029) which bypass unicode.IsControl() checks.
func sanitizeForLog(s string) string {
	const maxLen = 100
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			b.WriteRune('?')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// loadBasePromptMode loads config and returns the effective base_prompt_mode for the given role.
func loadBasePromptMode(maestroDir, role string) (string, error) {
	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return "", err
	}
	switch role {
	case "orchestrator":
		return cfg.Agents.Orchestrator.EffectiveBasePromptMode(), nil
	case "planner":
		return cfg.Agents.Planner.EffectiveBasePromptMode(), nil
	default:
		return cfg.Agents.Workers.EffectiveBasePromptMode(), nil
	}
}

// currentPaneTarget returns the current pane in "session:window.pane" format.
// It uses the TMUX_PANE environment variable (set per-pane by tmux) to resolve
// the correct pane target, avoiding race conditions when multiple agents are
// launched concurrently via tmux send-keys.
func currentPaneTarget() (string, error) {
	paneID := os.Getenv("TMUX_PANE")
	if paneID == "" {
		return "", fmt.Errorf("\"TMUX_PANE\" environment variable not set (not running inside tmux?)")
	}
	if !validTmuxPane.MatchString(paneID) {
		return "", fmt.Errorf("invalid TMUX_PANE format: expected %%<number>, got: %s", sanitizeForLog(paneID))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// paneID is validated above against validTmuxPane (^%[0-9]+$), so it is safe to pass as argument.
	cmd := exec.CommandContext(ctx, "tmux", "display-message", "-t", paneID, "-p", "#{session_name}:#{window_index}.#{pane_index}") //nolint:gosec // paneID validated by regex
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("tmux display-message: timeout after 5s: %w", ctx.Err())
		}
		return "", fmt.Errorf("tmux display-message: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
