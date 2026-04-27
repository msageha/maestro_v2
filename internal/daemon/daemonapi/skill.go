package daemonapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

type SkillApproveParams struct {
	CandidateID string `json:"candidate_id"`
	SkillName   string `json:"skill_name,omitempty"`
}

type SkillRejectParams struct {
	CandidateID string `json:"candidate_id"`
}

var skillNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
var nonAlphaNumPattern = regexp.MustCompile(`[^a-z0-9]+`)

type Skill struct {
	maestroDir string
	lockMap    *lock.MutexMap
	logInfof   Logf
	logWarnf   Logf
}

func NewSkill(maestroDir string, lockMap *lock.MutexMap, logInfof, logWarnf Logf) *Skill {
	return &Skill{
		maestroDir: maestroDir,
		lockMap:    lockMap,
		logInfof:   logInfof,
		logWarnf:   logWarnf,
	}
}

func (h *Skill) HandleApprove(req *uds.Request) *uds.Response {
	var params SkillApproveParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}
	if params.CandidateID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "candidate_id is required")
	}

	h.lockMap.Lock("state:skill_candidates")
	defer h.lockMap.Unlock("state:skill_candidates")

	candidates, candidatesPath, idx, errResp := h.findPendingCandidate(params.CandidateID)
	if errResp != nil {
		return errResp
	}
	candidate := &candidates[idx]

	skillName := params.SkillName
	if skillName == "" {
		skillName = skillSlugify(candidate.Content)
	}
	if skillName == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "could not auto-generate skill name from content; provide skill_name")
	}
	if !skillNamePattern.MatchString(skillName) {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid skill name %q: must be kebab-case (a-z0-9 and hyphens, 1-64 chars)", skillName))
	}

	skillsDir := filepath.Join(h.maestroDir, "skills", "share")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("create skills directory: %v", err))
	}
	skillDir := filepath.Join(skillsDir, skillName)
	if err := os.Mkdir(skillDir, 0o755); err != nil {
		if os.IsExist(err) {
			return uds.ErrorResponse(uds.ErrCodeDuplicate, fmt.Sprintf("skill %q already exists; use a different name", skillName))
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("create skill directory: %v", err))
	}
	skillDirCreated := true
	defer func() {
		if skillDirCreated {
			if removeErr := os.RemoveAll(skillDir); removeErr != nil && h.logWarnf != nil {
				h.logWarnf("skill_cleanup_failed dir=%s error=%v", skillDir, removeErr)
			}
		}
	}()

	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(formatSkillMarkdown(skillName, candidate.Content)), 0o644); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("write SKILL.md: %v", err))
	}

	candidate.Status = "approved"
	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("update candidates: %v", err))
	}

	skillDirCreated = false
	if h.logInfof != nil {
		h.logInfof("skill_approved candidate=%s skill=%s", params.CandidateID, skillName)
	}
	return uds.SuccessResponse(map[string]string{
		"candidate_id": params.CandidateID,
		"skill_name":   skillName,
	})
}

func (h *Skill) HandleReject(req *uds.Request) *uds.Response {
	var params SkillRejectParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}
	if params.CandidateID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "candidate_id is required")
	}

	h.lockMap.Lock("state:skill_candidates")
	defer h.lockMap.Unlock("state:skill_candidates")

	candidates, candidatesPath, idx, errResp := h.findPendingCandidate(params.CandidateID)
	if errResp != nil {
		return errResp
	}
	candidates[idx].Status = "rejected"
	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("update candidates: %v", err))
	}

	if h.logInfof != nil {
		h.logInfof("skill_rejected candidate=%s", params.CandidateID)
	}
	return uds.SuccessResponse(map[string]string{"candidate_id": params.CandidateID})
}

func (h *Skill) findPendingCandidate(candidateID string) ([]model.SkillCandidate, string, int, *uds.Response) {
	candidatesPath := filepath.Join(h.maestroDir, "state", "skill_candidates.yaml")
	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		return nil, "", -1, uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("read candidates: %v", err))
	}
	for i, c := range candidates {
		if c.ID != candidateID {
			continue
		}
		if candidates[i].Status != "pending" {
			return nil, "", -1, uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("candidate %s is already %s", candidateID, candidates[i].Status))
		}
		return candidates, candidatesPath, i, nil
	}
	return nil, "", -1, uds.ErrorResponse(uds.ErrCodeNotFound, fmt.Sprintf("candidate not found: %s", candidateID))
}

func skillSlugify(content string) string {
	line := strings.SplitN(content, "\n", 2)[0]
	line = strings.TrimSpace(line)
	line = strings.TrimLeft(line, "# ")

	s := strings.ToLower(line)
	s = nonAlphaNumPattern.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 64 {
		s = strings.TrimRight(s[:64], "-")
	}
	return s
}

func formatSkillMarkdown(name, content string) string {
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
