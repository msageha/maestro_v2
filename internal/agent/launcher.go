package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/msageha/maestro_v2/internal/tmux"
)

// validRoleName permits only alphanumeric, underscore, and hyphen characters.
var validRoleName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

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

	// Execute claude CLI
	cmd := exec.Command("claude",
		"--model", model,
		"--append-system-prompt", systemPrompt,
		"--dangerously-skip-permissions",
	)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
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

// currentPaneTarget returns the current pane in "session:window.pane" format.
func currentPaneTarget() (string, error) {
	cmd := exec.Command("tmux", "display-message", "-p", "#{session_name}:#{window_index}.#{pane_index}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux display-message: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
