package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/model"
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

// SkillAPI handles the "skill_approve" and "skill_reject" UDS endpoints.
type SkillAPI struct {
	*apiContext
}

func (h *SkillAPI) handleSkillApprove(req *uds.Request) *uds.Response {
	var params SkillApproveParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	if params.CandidateID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "candidate_id is required")
	}

	// Acquire skill_candidates lock to serialize with writeSkillCandidates in result_write.
	// Lock order: leaf lock under the state:* namespace; see daemon/doc.go.
	// Acquired in isolation — no state:{commandID} is held on this path.
	h.lockMap.Lock("state:skill_candidates")
	defer h.lockMap.Unlock("state:skill_candidates")

	candidates, candidatesPath, idx, errResp := h.findPendingCandidate(params.CandidateID)
	if errResp != nil {
		return errResp
	}
	candidate := &candidates[idx]

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

	// Create skill directory atomically. os.Mkdir fails with os.ErrExist if the
	// directory already exists, avoiding the TOCTOU race between Stat and MkdirAll.
	skillsDir := filepath.Join(h.maestroDir, "skills", "share")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil { //nolint:gosec // 0755 is appropriate for a skills directory
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("create skills directory: %v", err))
	}
	skillDir := filepath.Join(skillsDir, skillName)
	if err := os.Mkdir(skillDir, 0o755); err != nil { //nolint:gosec // 0755 is appropriate for a skills directory
		if os.IsExist(err) {
			return uds.ErrorResponse(uds.ErrCodeDuplicate, fmt.Sprintf("skill %q already exists; use a different name", skillName))
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("create skill directory: %v", err))
	}
	skillDirCreated := true
	defer func() {
		if skillDirCreated {
			if removeErr := os.RemoveAll(skillDir); removeErr != nil {
				h.logFn(LogLevelWarn, "skill_cleanup_failed dir=%s error=%v", skillDir, removeErr)
			}
		}
	}()

	skillContent := daemonFormatSkillMD(skillName, candidate.Content)
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(skillContent), 0o644); err != nil { //nolint:gosec // skill files are user-readable documentation
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("write SKILL.md: %v", err))
	}

	// Update candidate status
	candidate.Status = "approved"
	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("update candidates: %v", err))
	}

	// All operations succeeded; prevent cleanup from removing the directory.
	skillDirCreated = false

	h.logFn(LogLevelInfo, "skill_approved candidate=%s skill=%s", params.CandidateID, skillName)
	return uds.SuccessResponse(map[string]string{
		"candidate_id": params.CandidateID,
		"skill_name":   skillName,
	})
}

func (h *SkillAPI) handleSkillReject(req *uds.Request) *uds.Response {
	var params SkillRejectParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	if params.CandidateID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "candidate_id is required")
	}

	// Acquire skill_candidates lock to serialize with writeSkillCandidates in result_write.
	// Lock order: leaf lock under the state:* namespace; see daemon/doc.go.
	// Acquired in isolation — no state:{commandID} is held on this path.
	h.lockMap.Lock("state:skill_candidates")
	defer h.lockMap.Unlock("state:skill_candidates")

	candidates, candidatesPath, idx, errResp := h.findPendingCandidate(params.CandidateID)
	if errResp != nil {
		return errResp
	}
	candidate := &candidates[idx]

	candidate.Status = "rejected"
	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("update candidates: %v", err))
	}

	h.logFn(LogLevelInfo, "skill_rejected candidate=%s", params.CandidateID)
	return uds.SuccessResponse(map[string]string{
		"candidate_id": params.CandidateID,
	})
}

// findPendingCandidate reads skill candidates, finds one by ID, and verifies
// it is in "pending" status. Caller must hold the "state:skill_candidates" lock.
func (h *SkillAPI) findPendingCandidate(candidateID string) ([]model.SkillCandidate, string, int, *uds.Response) {
	candidatesPath := filepath.Join(h.maestroDir, "state", "skill_candidates.yaml")
	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		return nil, "", -1, uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("read candidates: %v", err))
	}

	idx := -1
	for i, c := range candidates {
		if c.ID == candidateID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, "", -1, uds.ErrorResponse(uds.ErrCodeNotFound, fmt.Sprintf("candidate not found: %s", candidateID))
	}

	if candidates[idx].Status != "pending" {
		return nil, "", -1, uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("candidate %s is already %s", candidateID, candidates[idx].Status))
	}

	return candidates, candidatesPath, idx, nil
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
	fmt.Fprintf(&sb, "name: %s\n", name)
	sb.WriteString("description: Auto-approved from skill candidate\n")
	sb.WriteString("---\n")
	sb.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}
