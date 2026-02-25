package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/tmux"
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
	"orchestrator": {"Bash(maestro:*)", "Read(.maestro/**)"},
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

	args := buildLaunchArgs(role, model, systemPrompt)

	// Execute claude CLI.
	// Clear CLAUDECODE env var to allow launching inside a parent Claude Code
	// session (e.g. when maestro is invoked from Claude Code CLI).
	cmd := exec.Command("claude", args...)
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// buildLaunchArgs constructs the CLI arguments for the claude command.
func buildLaunchArgs(role, agentModel, systemPrompt string) []string {
	args := []string{
		"--model", agentModel,
		"--append-system-prompt", systemPrompt,
		"--dangerously-skip-permissions",
	}

	// Apply tool restrictions for non-worker roles
	if tools, ok := allowedToolsByRole[role]; ok && len(tools) > 0 {
		args = append(args, "--allowedTools", strings.Join(tools, ","))
	}

	// Disable Notification hooks for non-orchestrator roles via deep merge.
	// PreToolUse/PostToolUse are preserved; only Notification is cleared.
	if role != "orchestrator" {
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
