package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runSkill dispatches skill subcommands.
func (a *cliApp) runSkill(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro skill: missing subcommand\nusage: maestro skill <list|candidates|approve|reject> [options]"}
	}
	switch args[0] {
	case "list":
		return runSkillList(args[1:])
	case "candidates":
		return runSkillCandidates(args[1:])
	case "approve":
		return a.runSkillApprove(args[1:])
	case "reject":
		return a.runSkillReject(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill: unknown subcommand: %s\nusage: maestro skill <list|candidates|approve|reject> [options]", args[0])}
	}
}

// runSkillList lists all registered skills for a given role.
func runSkillList(args []string) error {
	cmd := NewCommand("maestro skill list", "maestro skill list --role <role>")
	var role string
	cmd.RequiredString(&role, "role", "Role name (required, e.g., worker, planner, orchestrator)")
	if err := cmd.Parse(args); err != nil {
		return err
	}

	// Validate --role to prevent path traversal.
	if role == "." || role == ".." || strings.Contains(role, "/") || strings.Contains(role, "\\") {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill list: invalid --role value: %s", role)}
	}

	maestroDir, err := requireMaestroDir("skill list")
	if err != nil {
		return err
	}

	skillsDir := filepath.Join(maestroDir, "skills")
	// Use ListSkillsWithRole to include role-specific and shared skills.
	skills, err := skill.ListSkillsWithRole(skillsDir, role, nil)
	if err != nil {
		return fmt.Errorf("maestro skill list: %w", err)
	}

	if len(skills) == 0 {
		fmt.Println("No skills found.")
		return nil
	}

	for _, s := range skills {
		name := s.Name
		if name == "" {
			name = s.ID
		}
		desc := s.Description
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Printf("%s\t%s\n", sanitizeForTerminal(name), sanitizeForTerminal(desc))
	}
	return nil
}

// runSkillCandidates lists skill candidates from state/skill_candidates.yaml.
func runSkillCandidates(args []string) error {
	cmd := NewCommand("maestro skill candidates", "maestro skill candidates [--status pending|approved|rejected]")
	var statusFilter string
	cmd.StringVar(&statusFilter, "status", "", "Filter by status: pending|approved|rejected")

	if err := cmd.Parse(args); err != nil {
		return err
	}

	if statusFilter != "" && statusFilter != "pending" && statusFilter != "approved" && statusFilter != "rejected" {
		return cmd.Errorf("invalid --status value: %s (must be pending, approved, or rejected)", statusFilter)
	}

	maestroDir, err := requireMaestroDir("skill candidates")
	if err != nil {
		return err
	}

	candidatesPath := filepath.Join(maestroDir, "state", "skill_candidates.yaml")
	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		return fmt.Errorf("maestro skill candidates: %w", err)
	}

	if len(candidates) == 0 {
		fmt.Println("No skill candidates found.")
		return nil
	}

	for _, c := range candidates {
		if statusFilter != "" && c.Status != statusFilter {
			continue
		}
		// Truncate content for display
		const maxContentDisplayWidth = 80 // maximum rune width for candidate content display
		content := c.Content
		if runes := []rune(content); len(runes) > maxContentDisplayWidth {
			content = string(runes[:maxContentDisplayWidth-3]) + "..."
		}
		// Replace newlines for single-line display and sanitize
		content = strings.ReplaceAll(content, "\n", " ")
		fmt.Printf("%s\t%s\t%d\t%s\n", c.ID, c.Status, c.Occurrences, sanitizeForTerminal(content))
	}
	return nil
}

// runSkillApprove approves a skill candidate via the daemon UDS API.
func (a *cliApp) runSkillApprove(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro skill approve: missing candidate-id\nusage: maestro skill approve <candidate-id> [--name <skill-name>]"}
	}

	candidateID := args[0]
	if err := validate.ID(candidateID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: invalid candidate-id: %v", err)}
	}

	cmd := NewCommand("maestro skill approve", "maestro skill approve <candidate-id> [--name <skill-name>]")
	var skillName string
	cmd.StringVar(&skillName, "name", "", "Skill name in kebab-case (a-z0-9 and hyphens, 1-64 chars)")

	if err := cmd.Parse(args[1:]); err != nil {
		return err
	}

	maestroDir, err := requireMaestroDir("skill approve")
	if err != nil {
		return err
	}

	params := map[string]string{
		"candidate_id": candidateID,
	}
	if skillName != "" {
		params["skill_name"] = skillName
	}

	client := a.newDaemonClient(maestroDir)
	resp, err := client.SendCommand("skill_approve", params)
	if err != nil {
		return fmt.Errorf("maestro skill approve: %w", err)
	}

	if !resp.Success {
		code, msg := udsErrorInfo(resp)
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: [%s] %s", code, msg)}
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return fmt.Errorf("maestro skill approve: unmarshal response: %w", err)
	}
	if result == nil {
		return &CLIError{Code: 1, Msg: "maestro skill approve: response missing skill_name"}
	}
	skillName, ok := result["skill_name"]
	if !ok {
		return &CLIError{Code: 1, Msg: "maestro skill approve: response missing skill_name"}
	}
	fmt.Printf("approved %s as skill %q\n", candidateID, skillName)
	return nil
}

// runSkillReject rejects a skill candidate via the daemon UDS API.
func (a *cliApp) runSkillReject(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro skill reject: missing candidate-id\nusage: maestro skill reject <candidate-id>"}
	}

	candidateID := args[0]
	if err := validate.ID(candidateID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill reject: invalid candidate-id: %v", err)}
	}

	cmd := NewCommand("maestro skill reject", "maestro skill reject <candidate-id>")
	if err := cmd.Parse(args[1:]); err != nil {
		return err
	}

	maestroDir, err := requireMaestroDir("skill reject")
	if err != nil {
		return err
	}

	params := map[string]string{
		"candidate_id": candidateID,
	}

	client := a.newDaemonClient(maestroDir)
	resp, err := client.SendCommand("skill_reject", params)
	if err != nil {
		return fmt.Errorf("maestro skill reject: %w", err)
	}

	if !resp.Success {
		code, msg := udsErrorInfo(resp)
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill reject: [%s] %s", code, msg)}
	}

	fmt.Printf("rejected %s\n", candidateID)
	return nil
}

