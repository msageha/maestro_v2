package agent

import (
	"context"
	"fmt"
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
	// Determine current pane target
	paneTarget, err := currentPaneTarget()
	if err != nil {
		return fmt.Errorf("determine pane: %w", err)
	}

	agentID, err := tmux.GetUserVar(paneTarget, "agent_id")
	if err != nil {
		return fmt.Errorf("read @agent_id: %w", err)
	}
	if agentID == "" {
		return fmt.Errorf("@agent_id is empty for pane %s", paneTarget)
	}

	role, err := tmux.GetUserVar(paneTarget, "role")
	if err != nil {
		return fmt.Errorf("read @role: %w", err)
	}
	if role == "" {
		return fmt.Errorf("@role is empty for pane %s", paneTarget)
	}
	if !validRoleName.MatchString(role) {
		return fmt.Errorf("invalid role name %q: must be alphanumeric, underscore, or hyphen", role)
	}

	model, err := tmux.GetUserVar(paneTarget, "model")
	if err != nil {
		return fmt.Errorf("read @model: %w", err)
	}
	if model == "" {
		return fmt.Errorf("@model is empty for pane %s", paneTarget)
	}

	// Build system prompt
	systemPrompt, err := buildSystemPrompt(maestroDir, role)
	if err != nil {
		return fmt.Errorf("build system prompt: %w", err)
	}

	// Determine base_prompt_mode from config
	basePromptMode := "append" // default
	if cfg, err := loadBasePromptMode(maestroDir, role); err == nil {
		basePromptMode = cfg
	}

	args := buildLaunchArgs(role, model, systemPrompt, basePromptMode)

	// For workers, set up a single --settings containing both Notification
	// disablement and PreToolUse policy hook. HookSettings produces the merged
	// JSON so we do NOT add a separate --settings in buildLaunchArgs (workers
	// are excluded from the Notification-only --settings there).
	if role == "worker" {
		pc := NewPolicyChecker(maestroDir)
		scriptPath, err := pc.WriteHookScript()
		if err != nil {
			return fmt.Errorf("write policy hook script: %w", err)
		}
		hookSettings, err := pc.HookSettings(scriptPath)
		if err != nil {
			return fmt.Errorf("build policy hook settings: %w", err)
		}
		args = append(args, "--settings", hookSettings)
	}

	// Execute claude CLI.
	// Clear CLAUDECODE env var to allow launching inside a parent Claude Code
	// session (e.g. when maestro is invoked from Claude Code CLI).
	cmd := exec.Command("claude", args...)
	// Propagate the agent role to child processes via MAESTRO_AGENT_ROLE so
	// that any maestro CLI invocations they spawn carry an authenticated role
	// hint to the daemon (used for recovery API trust boundaries).
	cmd.Env = append(filterEnv(os.Environ(), "CLAUDECODE"), uds.CallerRoleEnv+"="+role)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Ignore SIGINT so that only the child process (claude) handles Ctrl+C.
	// Without this, the Go runtime terminates this process on SIGINT, orphaning
	// the claude child and breaking the daemon's ensureWorkingDir exit sequence
	// (Ctrl+C → Ctrl+D) which relies on claude exiting cleanly back to the shell.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			// Intentionally ignored — claude handles SIGINT directly.
		}
	}()

	return cmd.Run()
}

// buildLaunchArgs constructs the CLI arguments for the claude command.
// basePromptMode controls the system prompt flag: "replace" uses --system-prompt,
// "append" (or empty) uses --append-system-prompt.
func buildLaunchArgs(role, agentModel, systemPrompt, basePromptMode string) []string {
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
				"Read(.maestro/queues/**)",
				"Read(.maestro/results/**)",
				"Read(.maestro/locks/**)",
				"Read(.maestro/logs/**)",
				"Read(.maestro/config.yaml)",
			}, ","))
	}

	// Disable Notification hooks for non-orchestrator, non-worker roles.
	// Workers get a merged --settings (Notification + PreToolUse) in Launch(),
	// so we skip them here to avoid passing two --settings flags.
	if role != "orchestrator" && role != "worker" {
		args = append(args, "--settings", `{"hooks":{"Notification":[]}}`)
	}

	return args
}

// buildSystemPrompt combines maestro.md + instructions/{role}.md.
func buildSystemPrompt(maestroDir, role string) (string, error) {
	// Read maestro.md
	maestroPath := filepath.Join(maestroDir, "maestro.md")
	maestroContent, err := os.ReadFile(maestroPath)
	if err != nil {
		return "", fmt.Errorf("read maestro.md: %w", err)
	}

	// Read instructions/{role}.md
	instructionsPath := filepath.Join(maestroDir, "instructions", role+".md")
	instructionsContent, err := os.ReadFile(instructionsPath)
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
		return "", fmt.Errorf("TMUX_PANE environment variable not set (not running inside tmux?)")
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
