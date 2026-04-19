package agent

import (
	"context"
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
// Orchestrator and Planner are restricted to:
//   - Bash(maestro:*) — only maestro CLI commands (no cat, echo, grep, etc.)
//   - Read(.maestro/**) — only .maestro/ status files (path restriction is
//     enforced at the permission-prompt level; under --dangerously-skip-permissions
//     this acts as a declarative intent only)
//
// Workers have no tool restriction (they need full access for task execution).
var allowedToolsByRole = map[string][]string{
	"orchestrator": {"Bash(maestro:*)", "Read(.maestro/dashboard.md)", "Read(.maestro/results/*)", "Read(.maestro/config.yaml)"},
	"planner":      {"Bash(maestro:*)", "Read(.maestro/**)"},
	// worker: unrestricted (empty means all tools allowed)
}

// Launch reads tmux user variables for the current pane and launches the
// appropriate Agent CLI (claude) with the correct model and system prompt.
func Launch(maestroDir string) error {
	paneTarget, err := currentPaneTarget()
	if err != nil {
		return fmt.Errorf("determine pane: %w", err)
	}

	_, role, agentModel, err := readPaneVars(paneTarget)
	if err != nil {
		return err
	}

	systemPrompt, err := buildSystemPrompt(maestroDir, role)
	if err != nil {
		return fmt.Errorf("build system prompt: %w", err)
	}

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

	// Execute claude CLI.
	cmd := exec.Command(claudePath, args...) //nolint:gosec // claudePath is resolved via LookPath; args are constructed from validated config
	cmd.Env = buildLaunchEnv(os.Environ(), role)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return runIgnoringSIGINT(cmd)
}

// readPaneVars reads and validates the tmux user variables (agent_id, role, model)
// from the given pane target.
func readPaneVars(paneTarget string) (agentID, role, model string, err error) {
	agentID, err = tmux.GetUserVar(paneTarget, "agent_id")
	if err != nil {
		return "", "", "", fmt.Errorf("read @agent_id: %w", err)
	}
	if agentID == "" {
		return "", "", "", fmt.Errorf("@agent_id is empty for pane %s", sanitizeForLog(paneTarget))
	}

	role, err = tmux.GetUserVar(paneTarget, "role")
	if err != nil {
		return "", "", "", fmt.Errorf("read @role: %w", err)
	}
	if role == "" {
		return "", "", "", fmt.Errorf("@role is empty for pane %s", sanitizeForLog(paneTarget))
	}
	if !validRoleName.MatchString(role) {
		return "", "", "", fmt.Errorf("invalid role name %q: must be alphanumeric, underscore, or hyphen", sanitizeForLog(role))
	}

	model, err = tmux.GetUserVar(paneTarget, "model")
	if err != nil {
		return "", "", "", fmt.Errorf("read @model: %w", err)
	}
	if model == "" {
		return "", "", "", fmt.Errorf("@model is empty for pane %s", sanitizeForLog(paneTarget))
	}

	return agentID, role, model, nil
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
	return cmd.Run()
}

// buildLaunchArgs constructs the CLI arguments for the claude command.
// basePromptMode controls the system prompt flag: "replace" uses --system-prompt,
// "append" (or empty) uses --append-system-prompt.
func buildLaunchArgs(role, agentModel, systemPrompt, basePromptMode string) ([]string, error) {
	if !knownRoles[role] {
		return nil, fmt.Errorf("unknown role %q: rejected (fail-closed)", role)
	}

	promptFlag := "--append-system-prompt"
	if basePromptMode == "replace" {
		promptFlag = "--system-prompt"
	}
	args := []string{
		"--model", agentModel,
		promptFlag, systemPrompt,
		"--dangerously-skip-permissions",
	}

	// Apply tool restrictions for non-worker roles
	if tools, ok := allowedToolsByRole[role]; ok && len(tools) > 0 {
		args = append(args, "--allowedTools", strings.Join(tools, ","))
	}

	// Planners: block operator-only recovery API commands.
	// Planner has Bash(maestro:*) in allowedTools, which permits all maestro
	// subcommands. These disallowedTools carve out the operator-only escape
	// hatches that only Orchestrator should invoke.
	// Note: resume-merge is intentionally NOT blocked for Planner — it is the
	// Planner's primary mechanism for triggering re-merge after a worker has
	// resolved a conflict (hybrid b+c conflict recovery path).
	// Note: add-retry-task is intentionally NOT blocked for Planner — it is
	// the Planner's standard mechanism for retrying failed tasks (see
	// planner.md "失敗タスクの処理" and verification loop sections).
	if role == "planner" {
		args = append(args, "--disallowedTools",
			"Bash(maestro plan unquarantine:*)")
	}

	// Workers: block destructive tmux commands and .maestro/ reads at the tool level.
	// The textual prohibitions in worker.md (D006, .maestro/ access) are not
	// enforced by Claude CLI; --disallowedTools provides a hard technical block.
	if role == "worker" {
		args = append(args, "--disallowedTools",
			strings.Join([]string{
				"Bash(tmux kill-server:*)",
				"Bash(tmux kill-session:*)",
				"Bash(tmux kill-pane:*)",
				"Bash(tmux kill-window:*)",
				// D009: recovery API escape hatches are operator-only.
				// Workers must never invoke these even if a future content
				// payload tries to embed them.
				"Bash(maestro plan unquarantine:*)",
				"Bash(maestro plan resume-merge:*)",
				"Bash(maestro resolve-conflict:*)",
				"Read(.maestro/state/**)",
				"Read(.maestro/queue/**)",
				"Read(.maestro/results/**)",
				"Read(.maestro/locks/**)",
				"Read(.maestro/logs/**)",
				"Read(.maestro/config.yaml)",
				"Read(.maestro/dashboard.md)",
			}, ","))
	}

	// Notification hooks: user-configured scripts that fire on Claude Code
	// events (tool calls, errors, etc.). For internal agents (planner, worker),
	// these are disabled to prevent interference with automated operation:
	//   - User notification scripts may block or produce side effects
	//   - Internal agents run autonomously and don't need user-facing alerts
	//   - Orchestrator keeps user-configured hooks since it's the user-facing agent
	//
	// Worker Notification hooks are disabled in HookSettings() (policy_checker.go),
	// merged with PreToolUse hooks into a single --settings flag.
	// allowAllUnixSockets enables the daemon UDS connection (.maestro/daemon.sock).
	switch role {
	case "orchestrator":
		args = append(args, "--settings", `{"sandbox":{"network":{"allowAllUnixSockets":true}}}`)
	case "worker":
		// Worker settings (Notification=[] + PreToolUse policy hook) are
		// handled via HookSettings() in Launch() to produce a single
		// merged --settings flag.
	default:
		// Planner and other internal roles: disable Notification hooks.
		args = append(args, "--settings", `{"sandbox":{"network":{"allowAllUnixSockets":true}},"hooks":{"Notification":[]}}`)
	}

	return args, nil
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

// buildLaunchEnv constructs the environment for the claude CLI process.
//   - Clears CLAUDECODE to allow launching inside a parent Claude Code session
//     (e.g. when maestro is invoked from Claude Code CLI).
//   - Sets CLAUDE_CODE_SANDBOXED=1 to bypass the workspace trust dialog that
//     otherwise blocks automated (headless) startup in every tmux pane. This is
//     separate from --dangerously-skip-permissions which only skips per-tool
//     permission checks; the trust dialog is an independent security layer that
//     checks project-level trust state.
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
	env = append(env, "CLAUDE_CODE_SANDBOXED=1")
	return env
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
	cmd := exec.CommandContext(ctx, "tmux", "display-message", "-t", paneID, "-p", "#{session_name}:#{window_index}.#{pane_index}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("tmux display-message: timeout after 5s: %w", ctx.Err())
		}
		return "", fmt.Errorf("tmux display-message: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
