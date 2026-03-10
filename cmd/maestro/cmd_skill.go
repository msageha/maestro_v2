package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// runSkill dispatches skill subcommands.
func runSkill(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro skill: missing subcommand\nusage: maestro skill <list|candidates|approve|reject> [options]"}
	}
	switch args[0] {
	case "list":
		return runSkillList(args[1:])
	case "candidates":
		return runSkillCandidates(args[1:])
	case "approve":
		return runSkillApprove(args[1:])
	case "reject":
		return runSkillReject(args[1:])
	default:
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill: unknown subcommand: %s\nusage: maestro skill <list|candidates|approve|reject> [options]", args[0])}
	}
}

// runSkillList lists all registered skills.
func runSkillList(args []string) error {
	fs := newFlagSet("maestro skill list")
	var role string
	fs.StringVar(&role, "role", "", "Filter skills by role name (e.g., worker, planner)")
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill list: %v\nusage: maestro skill list [--role <role>]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill list: unexpected argument: %s", fs.Arg(0))}
	}

	// Validate --role to prevent path traversal.
	if role != "" && (role == "." || role == ".." || strings.Contains(role, "/") || strings.Contains(role, "\\")) {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill list: invalid --role value: %s", role)}
	}

	maestroDir, err := requireMaestroDir("skill list")
	if err != nil {
		return err
	}

	skillsDir := filepath.Join(maestroDir, "skills")
	// Use ListSkillsWithRole to include role-specific and shared skills.
	// When role is empty, only shared and flat skills are listed.
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
	fs := newFlagSet("maestro skill candidates")
	var statusFilter string
	fs.StringVar(&statusFilter, "status", "", "Filter by status: pending|approved|rejected")

	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill candidates: %v\nusage: maestro skill candidates [--status pending|approved|rejected]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill candidates: unexpected argument: %s", fs.Arg(0))}
	}

	if statusFilter != "" && statusFilter != "pending" && statusFilter != "approved" && statusFilter != "rejected" {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill candidates: invalid --status value: %s (must be pending, approved, or rejected)", statusFilter)}
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
		content := c.Content
		if len(content) > 80 {
			content = content[:77] + "..."
		}
		// Replace newlines for single-line display and sanitize
		content = strings.ReplaceAll(content, "\n", " ")
		fmt.Printf("%s\t%s\t%d\t%s\n", c.ID, c.Status, c.Occurrences, sanitizeForTerminal(content))
	}
	return nil
}

// runSkillApprove approves a skill candidate via the daemon UDS API.
func runSkillApprove(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro skill approve: missing candidate-id\nusage: maestro skill approve <candidate-id> [--name <skill-name>]"}
	}

	candidateID := args[0]
	if err := validate.ValidateID(candidateID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: invalid candidate-id: %v", err)}
	}

	fs := newFlagSet("maestro skill approve")
	var skillName string
	fs.StringVar(&skillName, "name", "", "Skill name in kebab-case (a-z0-9 and hyphens, 1-64 chars)")

	if err := fs.Parse(args[1:]); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: %v\nusage: maestro skill approve <candidate-id> [--name <skill-name>]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: unexpected argument: %s", fs.Arg(0))}
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

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("skill_approve", params)
	if err != nil {
		return fmt.Errorf("maestro skill approve: %w", err)
	}

	if !resp.Success {
		code := ""
		msg := "unknown error"
		if resp.Error != nil {
			code = resp.Error.Code
			msg = resp.Error.Message
		}
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: [%s] %s", code, msg)}
	}

	var result map[string]string
	if err := json.Unmarshal(resp.Data, &result); err == nil {
		fmt.Printf("approved %s as skill %q\n", candidateID, result["skill_name"])
	}
	return nil
}

// runSkillReject rejects a skill candidate via the daemon UDS API.
func runSkillReject(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro skill reject: missing candidate-id\nusage: maestro skill reject <candidate-id>"}
	}

	candidateID := args[0]
	if err := validate.ValidateID(candidateID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill reject: invalid candidate-id: %v", err)}
	}

	fs := newFlagSet("maestro skill reject")
	if err := fs.Parse(args[1:]); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill reject: %v", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill reject: unexpected argument: %s", fs.Arg(0))}
	}

	maestroDir, err := requireMaestroDir("skill reject")
	if err != nil {
		return err
	}

	params := map[string]string{
		"candidate_id": candidateID,
	}

	client := uds.NewClient(filepath.Join(maestroDir, uds.DefaultSocketName))
	resp, err := client.SendCommand("skill_reject", params)
	if err != nil {
		return fmt.Errorf("maestro skill reject: %w", err)
	}

	if !resp.Success {
		code := ""
		msg := "unknown error"
		if resp.Error != nil {
			code = resp.Error.Code
			msg = resp.Error.Message
		}
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill reject: [%s] %s", code, msg)}
	}

	fmt.Printf("rejected %s\n", candidateID)
	return nil
}

// sanitizeForTerminal removes control characters (except space) to prevent terminal injection.
func sanitizeForTerminal(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		if r == '\t' || r == '\n' || (r >= 0x20 && r != 0x7f) {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
