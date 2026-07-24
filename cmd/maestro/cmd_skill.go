package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/msageha/maestro_v2/internal/daemon/daemonapi"
	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/model"
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

	// Search the same precedence-ordered sources as dispatch-time skill
	// resolution: configured skills.extra_dirs first, bundled catalog last.
	// A missing config.yaml just means "no extra dirs" so listing keeps
	// working on partially set-up projects.
	var extraDirs []string
	cfg, cfgErr := model.LoadConfig(maestroDir)
	switch {
	case cfgErr == nil:
		extraDirs = cfg.Skills.ExtraDirs
	case errors.Is(cfgErr, os.ErrNotExist):
		// no config: bundled catalog only
	default:
		return fmt.Errorf("maestro skill list: load config: %w", cfgErr)
	}

	bundledDir := filepath.Join(maestroDir, "skills")
	skillsDirs := skill.ResolveSearchDirs(extraDirs, filepath.Dir(maestroDir), bundledDir, nil)
	skills, err := skill.ListSkillsWithRoleDirs(skillsDirs, role, nil)
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
		line := fmt.Sprintf("%s\t%s\t%d\t%s", c.ID, c.Status, c.Occurrences, sanitizeForTerminal(content))
		if len(c.SimilarSkills) > 0 {
			line += fmt.Sprintf("\tsimilar=%s", sanitizeForTerminal(strings.Join(c.SimilarSkills, ",")))
		}
		if c.StagedPath != "" {
			line += fmt.Sprintf("\tstaged=%s", sanitizeForTerminal(c.StagedPath))
		}
		fmt.Println(line)
	}
	return nil
}

// runSkillApprove approves a skill candidate via the daemon UDS API. The
// daemon stages a validated SKILL.md draft under state/skill_staging/;
// promotion into a live skill library is a follow-up human git operation.
func (a *cliApp) runSkillApprove(args []string) error {
	usage := "maestro skill approve <candidate-id> [--name <skill-name>] [--description <text>] [--force]"
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro skill approve: missing candidate-id\nusage: " + usage}
	}

	cmd := NewCommand("maestro skill approve", usage)
	var skillName string
	var description string
	var force bool
	cmd.StringVar(&skillName, "name", "", "Skill name in kebab-case (a-z0-9 and hyphens, 1-64 chars)")
	cmd.StringVar(&description, "description", "", "Frontmatter description for the staged draft (default: derived from candidate content)")
	cmd.BoolVar(&force, "force", false, "Bypass the occurrences>=2 and similar-skill dedup gates (never bypasses name collisions or the anatomy validator)")

	candidateID := args[0]
	if strings.HasPrefix(candidateID, "-") {
		if candidateID == "-h" || candidateID == "--help" {
			return cmd.Parse(args)
		}
		return cmd.UsageErrorf("missing candidate-id — the first argument must be the candidate ID, got flag %q", candidateID)
	}
	if err := validate.ID(candidateID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: invalid candidate-id: %v", err)}
	}

	if err := cmd.Parse(args[1:]); err != nil {
		return err
	}

	maestroDir, err := requireMaestroDir("skill approve")
	if err != nil {
		return err
	}

	params := daemonapi.SkillApproveParams{
		CandidateID: candidateID,
		SkillName:   skillName,
		Description: description,
		Force:       force,
	}

	client := a.newDaemonClient(maestroDir)
	resp, err := client.SendCommand("skill_approve", params)
	if err != nil {
		return fmt.Errorf("maestro skill approve: %w", err)
	}

	if !resp.Success {
		return udsCLIError("maestro skill approve", resp)
	}

	var result daemonapi.SkillApproveResult
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return fmt.Errorf("maestro skill approve: unmarshal response: %w", err)
	}
	if result.SkillName == "" {
		return &CLIError{Code: 1, Msg: "maestro skill approve: response missing skill_name"}
	}
	fmt.Printf("approved %s as skill %q\n", candidateID, sanitizeForTerminal(result.SkillName))
	if result.StagedPath != "" {
		fmt.Printf("staged: %s\n", sanitizeForTerminal(result.StagedPath))
	}
	for _, w := range result.Warnings {
		fmt.Printf("warning: %s\n", sanitizeForTerminal(w))
	}
	if len(result.SimilarSkills) > 0 {
		fmt.Printf("similar existing skills: %s\n", sanitizeForTerminal(strings.Join(result.SimilarSkills, ", ")))
	}
	if result.PromotionHint != "" {
		fmt.Printf("next: %s\n", sanitizeForTerminal(result.PromotionHint))
	}
	return nil
}

// runSkillReject rejects a skill candidate via the daemon UDS API.
func (a *cliApp) runSkillReject(args []string) error {
	if len(args) < 1 {
		return &CLIError{Code: 1, Msg: "maestro skill reject: missing candidate-id\nusage: maestro skill reject <candidate-id>"}
	}

	cmd := NewCommand("maestro skill reject", "maestro skill reject <candidate-id>")

	candidateID := args[0]
	if strings.HasPrefix(candidateID, "-") {
		if candidateID == "-h" || candidateID == "--help" {
			return cmd.Parse(args)
		}
		return cmd.UsageErrorf("missing candidate-id — the first argument must be the candidate ID, got flag %q", candidateID)
	}
	if err := validate.ID(candidateID); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill reject: invalid candidate-id: %v", err)}
	}

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
		return udsCLIError("maestro skill reject", resp)
	}

	fmt.Printf("rejected %s\n", candidateID)
	return nil
}
