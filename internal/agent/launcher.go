package agent

import (
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

// launchAlternativeRuntime rejects non-claude-code managed roles. Config
// validation should prevent this path; the launcher keeps the check as defense
// in depth for stale tmux panes or hand-edited pane variables.
func launchAlternativeRuntime(agentRuntime, _, role, _ string) error {
	if role == "orchestrator" || role == "planner" || role == "worker" {
		return fmt.Errorf(
			"role %q cannot run on runtime %q: tool-based role enforcement "+
				"and policy hooks are only available on claude-code. "+
				"Configure agents.%s.model to a Claude model (opus, sonnet, haiku)",
			role, agentRuntime, role)
	}
	return fmt.Errorf("unsupported runtime %q for unmanaged role %q", agentRuntime, role)
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
	implementation := model.PolicyHookImplementationBash
	if cfg, err := model.LoadConfig(maestroDir); err == nil {
		implementation = cfg.Agents.Workers.EffectivePolicyHookImplementation()
	} else {
		slog.Warn("load worker policy hook implementation failed, using bash", "error", err, "default", implementation)
	}

	pc := NewPolicyChecker(maestroDir)
	scriptPath, err := pc.WriteHookScriptWithOptions(HookScriptOptions{
		Implementation: implementation,
		MaestroBinary:  ResolvedBinaryPath(),
	})
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
func ResolvedLaunchCommand() string {
	return ResolvedBinaryPath() + " agent launch"
}

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
	body := "#!/bin/sh\n" +
		"export " + uds.CallerRoleEnv + "=" + shellSingleQuote(role) + "\n" +
		"export MAESTRO_DIR=" + shellSingleQuote(canonicalDir) + "\n" +
		"exec " + shellSingleQuote(execPath) + " \"$@\"\n"
	if err := os.WriteFile(wrapperPath, []byte(body), 0o700); err != nil {
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
	result := make([]string, len(env))
	copy(result, env)
	for i, e := range result {
		if strings.HasPrefix(e, prefix) {
			result[i] = prefix + value
			return result
		}
	}
	return append(result, prefix+value)
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
