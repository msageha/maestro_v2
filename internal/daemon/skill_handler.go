package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/uds"
)

// SkillApproveParams is the request payload for the skill_approve UDS command.
type SkillApproveParams struct {
	CandidateID string `json:"candidate_id"`
	SkillName   string `json:"skill_name,omitempty"`
}

// SkillRejectParams is the request payload for the skill_reject UDS command.
type SkillRejectParams struct {
	CandidateID string `json:"candidate_id"`
}

// skillNamePattern allows kebab-case names: lowercase alphanumeric with hyphens, 1-64 chars.
var daemonSkillNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// nonAlphaNumPattern matches any non-alphanumeric character.
var nonAlphaNumPattern = regexp.MustCompile(`[^a-z0-9]+`)

func (a *API) handleSkillApprove(req *uds.Request) *uds.Response {
	d := a.d

	var params SkillApproveParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	if params.CandidateID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "candidate_id is required")
	}

	// Acquire skill_candidates lock to serialize with writeSkillCandidates in result_write
	d.lockMap.Lock("state:skill_candidates")
	defer d.lockMap.Unlock("state:skill_candidates")

	candidatesPath := filepath.Join(d.maestroDir, "state", "skill_candidates.yaml")
	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("read candidates: %v", err))
	}

	idx := -1
	for i, c := range candidates {
		if c.ID == params.CandidateID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return uds.ErrorResponse(uds.ErrCodeNotFound, fmt.Sprintf("candidate not found: %s", params.CandidateID))
	}

	candidate := &candidates[idx]
	if candidate.Status != "pending" {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("candidate %s is already %s", params.CandidateID, candidate.Status))
	}

	// Determine skill name
	skillName := params.SkillName
	if skillName == "" {
		skillName = daemonSlugify(candidate.Content)
	}
	if skillName == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "could not auto-generate skill name from content; provide skill_name")
	}

	if !daemonSkillNamePattern.MatchString(skillName) {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid skill name %q: must be kebab-case (a-z0-9 and hyphens, 1-64 chars)", skillName))
	}

	// Check for name collision
	skillsDir := filepath.Join(d.maestroDir, "skills", "share")
	skillDir := filepath.Join(skillsDir, skillName)
	if _, err := os.Stat(skillDir); err == nil {
		return uds.ErrorResponse(uds.ErrCodeDuplicate, fmt.Sprintf("skill %q already exists; use a different name", skillName))
	}

	// Create skill directory and write SKILL.md
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("create skill directory: %v", err))
	}

	skillContent := daemonFormatSkillMD(skillName, candidate.Content)
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(skillContent), 0o644); err != nil {
		_ = os.RemoveAll(skillDir)
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("write SKILL.md: %v", err))
	}

	// Update candidate status
	candidate.Status = "approved"
	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		_ = os.RemoveAll(skillDir)
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("update candidates: %v", err))
	}

	d.log(LogLevelInfo, "skill_approved candidate=%s skill=%s", params.CandidateID, skillName)
	return uds.SuccessResponse(map[string]string{
		"candidate_id": params.CandidateID,
		"skill_name":   skillName,
	})
}

func (a *API) handleSkillReject(req *uds.Request) *uds.Response {
	d := a.d

	var params SkillRejectParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	if params.CandidateID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "candidate_id is required")
	}

	// Acquire skill_candidates lock to serialize with writeSkillCandidates in result_write
	d.lockMap.Lock("state:skill_candidates")
	defer d.lockMap.Unlock("state:skill_candidates")

	candidatesPath := filepath.Join(d.maestroDir, "state", "skill_candidates.yaml")
	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("read candidates: %v", err))
	}

	idx := -1
	for i, c := range candidates {
		if c.ID == params.CandidateID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return uds.ErrorResponse(uds.ErrCodeNotFound, fmt.Sprintf("candidate not found: %s", params.CandidateID))
	}

	candidate := &candidates[idx]
	if candidate.Status != "pending" {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("candidate %s is already %s", params.CandidateID, candidate.Status))
	}

	candidate.Status = "rejected"
	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("update candidates: %v", err))
	}

	d.log(LogLevelInfo, "skill_rejected candidate=%s", params.CandidateID)
	return uds.SuccessResponse(map[string]string{
		"candidate_id": params.CandidateID,
	})
}

// daemonSlugify generates a kebab-case skill name from content text.
func daemonSlugify(content string) string {
	line := strings.SplitN(content, "\n", 2)[0]
	line = strings.TrimSpace(line)
	line = strings.TrimLeft(line, "# ")

	s := strings.ToLower(line)
	s = nonAlphaNumPattern.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")

	if len(s) > 64 {
		s = s[:64]
		s = strings.TrimRight(s, "-")
	}

	return s
}

// daemonFormatSkillMD creates a SKILL.md with YAML frontmatter.
func daemonFormatSkillMD(name, content string) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", name))
	sb.WriteString("description: Auto-approved from skill candidate\n")
	sb.WriteString("---\n")
	sb.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}
