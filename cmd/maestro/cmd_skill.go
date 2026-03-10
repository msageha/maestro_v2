package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
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
	fs.StringVar(&role, "role", "", "")
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
	skills, err := skill.ListSkillsWithRole(skillsDir, role)
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
	fs.StringVar(&statusFilter, "status", "", "")

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

// runSkillApprove approves a skill candidate and saves it as a SKILL.md file.
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
	fs.StringVar(&skillName, "name", "", "")

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

	candidatesPath := filepath.Join(maestroDir, "state", "skill_candidates.yaml")
	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		return fmt.Errorf("maestro skill approve: %w", err)
	}

	idx := -1
	for i, c := range candidates {
		if c.ID == candidateID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: candidate not found: %s", candidateID)}
	}

	candidate := &candidates[idx]
	if candidate.Status != "pending" {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: candidate %s is already %s", candidateID, candidate.Status)}
	}

	// Determine skill name
	if skillName == "" {
		skillName = slugify(candidate.Content)
	}
	if skillName == "" {
		return &CLIError{Code: 1, Msg: "maestro skill approve: could not auto-generate skill name from content; use --name to specify one"}
	}

	// Validate skill name as a safe identifier
	if !isValidSkillName(skillName) {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: invalid skill name %q: must be kebab-case (a-z0-9 and hyphens, 1-64 chars)", skillName)}
	}

	// Check for name collision
	skillsDir := filepath.Join(maestroDir, "skills")
	skillDir := filepath.Join(skillsDir, skillName)
	if _, err := os.Stat(skillDir); err == nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill approve: skill %q already exists; use --name to specify a different name", skillName)}
	}

	// Create skill directory and write SKILL.md
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return fmt.Errorf("maestro skill approve: create skill directory: %w", err)
	}

	skillContent := formatSkillMD(skillName, candidate.Content)
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(skillContent), 0o644); err != nil {
		// Rollback: remove the created directory
		_ = os.RemoveAll(skillDir)
		return fmt.Errorf("maestro skill approve: write SKILL.md: %w", err)
	}

	// Update candidate status
	candidate.Status = "approved"
	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		// Rollback: remove the skill directory since the status update failed
		_ = os.RemoveAll(skillDir)
		return fmt.Errorf("maestro skill approve: update candidates: %w", err)
	}

	fmt.Printf("approved %s as skill %q\n", candidateID, skillName)
	return nil
}

// runSkillReject rejects a skill candidate.
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

	candidatesPath := filepath.Join(maestroDir, "state", "skill_candidates.yaml")
	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		return fmt.Errorf("maestro skill reject: %w", err)
	}

	idx := -1
	for i, c := range candidates {
		if c.ID == candidateID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill reject: candidate not found: %s", candidateID)}
	}

	candidate := &candidates[idx]
	if candidate.Status != "pending" {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro skill reject: candidate %s is already %s", candidateID, candidate.Status)}
	}

	candidate.Status = "rejected"
	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		return fmt.Errorf("maestro skill reject: update candidates: %w", err)
	}

	fmt.Printf("rejected %s\n", candidateID)
	return nil
}

// skillNamePattern allows kebab-case names: lowercase alphanumeric with hyphens, 1-64 chars.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

func isValidSkillName(name string) bool {
	return skillNamePattern.MatchString(name)
}

// nonAlphaNum matches any non-alphanumeric character.
var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

// slugify generates a kebab-case skill name from content text.
func slugify(content string) string {
	// Use first line as source
	line := strings.SplitN(content, "\n", 2)[0]
	line = strings.TrimSpace(line)
	// Strip markdown heading prefix
	line = strings.TrimLeft(line, "# ")

	s := strings.ToLower(line)
	s = nonAlphaNum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")

	// Cap length
	if len(s) > 64 {
		s = s[:64]
		s = strings.TrimRight(s, "-")
	}

	return s
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

// formatSkillMD creates a SKILL.md with YAML frontmatter from a skill name and content body.
func formatSkillMD(name, content string) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", name))
	sb.WriteString(fmt.Sprintf("description: Auto-approved from skill candidate\n"))
	sb.WriteString("---\n")
	sb.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}
