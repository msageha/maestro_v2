package daemonapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// SkillApproveParams is the body of a skill-approve UDS request.
type SkillApproveParams struct {
	CandidateID string `json:"candidate_id"`
	SkillName   string `json:"skill_name,omitempty"`
	Description string `json:"description,omitempty"`
	// Force bypasses the value gates (occurrences >= 2, no similar existing
	// skill). It never bypasses a hard name collision with an existing
	// library skill, nor the anatomy validator.
	Force bool `json:"force,omitempty"`
}

// SkillApproveResult is the success payload of a skill-approve request.
type SkillApproveResult struct {
	CandidateID string `json:"candidate_id"`
	SkillName   string `json:"skill_name"`
	// StagedPath is the project-root-relative path of the generated draft.
	StagedPath string `json:"staged_path"`
	// Warnings are advisory anatomy findings on the generated draft.
	Warnings []string `json:"warnings,omitempty"`
	// SimilarSkills lists existing library skills resembling the candidate
	// (present when Force was used to approve past the dedup gate, or when
	// similarity was below the blocking threshold at approval time).
	SimilarSkills []string `json:"similar_skills,omitempty"`
	// PromotionHint tells the operator how to promote the draft. Promotion
	// is always a human git operation; the daemon never commits.
	PromotionHint string `json:"promotion_hint"`
}

// SkillRejectParams is the body of a skill-reject UDS request.
type SkillRejectParams struct {
	CandidateID string `json:"candidate_id"`
}

var skillNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
var nonAlphaNumPattern = regexp.MustCompile(`[^a-z0-9]+`)

// minApproveOccurrences is the value gate for approval without --force: a
// pattern must have been observed at least twice ("repeats twice+" rule from
// the skill-factory design) before it is worth staging.
const minApproveOccurrences = 2

// Skill handles skill candidate approval / rejection UDS requests.
//
// Approval is deliberately NOT a write into the live skill library
// (<maestroDir>/skills/ or skills.extra_dirs). It generates a complete
// SKILL.md draft under <maestroDir>/state/skill_staging/<name>/, gates it
// through the skill-anatomy validator, and reports the result to the
// operator. Promoting a draft into a version-controlled library is a human
// git operation.
type Skill struct {
	maestroDir string
	// extraDirs returns the configured skills.extra_dirs (evaluated per
	// request so a config reload is picked up).
	extraDirs func() []string
	lockMap   *lock.MutexMap
	logInfof  Logf
	logWarnf  Logf
}

// NewSkill constructs a Skill handler. lockMap is shared with the daemon
// so candidate state mutations serialize against other consumers. extraDirs
// supplies the configured skills.extra_dirs for dedup against the full skill
// library; a nil func is treated as "no extra dirs".
func NewSkill(maestroDir string, extraDirs func() []string, lockMap *lock.MutexMap, logInfof, logWarnf Logf) *Skill {
	if extraDirs == nil {
		extraDirs = func() []string { return nil }
	}
	return &Skill{
		maestroDir: maestroDir,
		extraDirs:  extraDirs,
		lockMap:    lockMap,
		logInfof:   logInfof,
		logWarnf:   logWarnf,
	}
}

// HandleApprove stages a pending skill candidate as a validated SKILL.md
// draft under <maestroDir>/state/skill_staging/<name>/ and marks the
// candidate approved. It never writes into the live skill library and never
// commits anything to git.
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

	// Value gate: a single observation is not a repeating pattern.
	if candidate.Occurrences < minApproveOccurrences && !params.Force {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf(
			"candidate %s has occurrences=%d (<%d): the pattern has not been observed repeating; use force to approve anyway",
			params.CandidateID, candidate.Occurrences, minApproveOccurrences))
	}

	// Dedup against the full library (bundled catalog + skills.extra_dirs).
	library := skill.ListAllSkills(h.skillSearchDirs(), nil)
	if ref, collides := findNameCollision(skillName, library); collides {
		return uds.ErrorResponse(uds.ErrCodeDuplicate, fmt.Sprintf(
			"skill name %q collides with existing skill %s; choose a different name or reject the candidate", skillName, ref))
	}
	similar := skill.FindSimilarSkills(candidate.Content, library, skill.LibraryDedupThreshold)
	if len(similar) > 0 && !params.Force {
		return uds.ErrorResponse(uds.ErrCodeDuplicate, fmt.Sprintf(
			"candidate %s resembles existing skill(s) %s; reject the candidate and reference the existing skill, or use force to approve anyway",
			params.CandidateID, strings.Join(similar, ", ")))
	}

	staged, err := skill.StageCandidate(skill.StagingRoot(h.maestroDir), skillName, params.Description, *candidate)
	if err != nil {
		var valErr *skill.StagingValidationError
		switch {
		case errors.Is(err, skill.ErrStagedSkillExists):
			return uds.ErrorResponse(uds.ErrCodeDuplicate, fmt.Sprintf(
				"a staged draft named %q already exists under state/%s/; promote or remove it, or choose a different name", skillName, skill.StagingDirName))
		case errors.As(err, &valErr):
			return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("generated draft failed skill-anatomy validation: %v", valErr))
		default:
			return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("stage skill draft: %v", err))
		}
	}

	projectRoot := filepath.Dir(h.maestroDir)
	stagedRel, relErr := filepath.Rel(projectRoot, staged.Path)
	if relErr != nil {
		stagedRel = staged.Path
	}

	candidate.Status = "approved"
	candidate.SkillName = skillName
	candidate.StagedPath = stagedRel
	candidate.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		// The atomic write can report an error after its rename already made
		// the new state visible (e.g. a directory-sync failure). Re-read
		// before rolling back: deleting the draft while the persisted state
		// says approved would strand the candidate without its staged file.
		if candidateApprovedOnDisk(candidatesPath, candidate.ID) {
			if h.logWarnf != nil {
				h.logWarnf("skill_approve candidate write reported %v but the approved state is persisted; keeping staged draft %s", err, staged.Path)
			}
		} else {
			// Roll back the staging directory created above: the candidate is
			// still pending on disk, and a leftover draft would make every
			// retry regenerate from the manifest anyway. Removing it keeps
			// the state and staging consistent after a transient I/O failure.
			if rmErr := os.RemoveAll(filepath.Dir(staged.Path)); rmErr != nil && h.logWarnf != nil {
				h.logWarnf("skill_approve rollback failed: staged draft %s left behind; remove it manually before retrying: %v", staged.Path, rmErr)
			}
			return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("update candidates: %v", err))
		}
	}

	warnings := make([]string, 0, len(staged.Warnings))
	for _, w := range staged.Warnings {
		warnings = append(warnings, w.String())
	}

	if h.logInfof != nil {
		h.logInfof("skill_approved candidate=%s skill=%s staged=%s warnings=%d", params.CandidateID, skillName, stagedRel, len(warnings))
	}
	return uds.SuccessResponse(SkillApproveResult{
		CandidateID:   params.CandidateID,
		SkillName:     skillName,
		StagedPath:    stagedRel,
		Warnings:      warnings,
		SimilarSkills: similar,
		PromotionHint: fmt.Sprintf(
			"レビュー後、%s を repo 管理の skill source (skills.extra_dirs の <dir>/<role>/%s/SKILL.md) へコピーして git commit する。daemon は live library へ書き込まない",
			stagedRel, skillName),
	})
}

// HandleReject marks a pending skill candidate as rejected so it is no
// longer surfaced to operators for review.
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
	candidates[idx].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("update candidates: %v", err))
	}

	if h.logInfof != nil {
		h.logInfof("skill_rejected candidate=%s", params.CandidateID)
	}
	return uds.SuccessResponse(map[string]string{"candidate_id": params.CandidateID})
}

// skillSearchDirs returns the precedence-ordered skill source directories
// (configured skills.extra_dirs, then the bundled <maestroDir>/skills
// catalog), mirroring the dispatch-time resolution in dispatch/envelope.go.
func (h *Skill) skillSearchDirs() []string {
	bundledDir := filepath.Join(h.maestroDir, "skills")
	projectRoot := filepath.Dir(h.maestroDir)
	return skill.ResolveSearchDirs(h.extraDirs(), projectRoot, bundledDir, nil)
}

// findNameCollision reports whether skillName is already taken by a library
// skill (by directory ID or frontmatter name) and returns its reference.
func findNameCollision(skillName string, library []skill.LibrarySkill) (string, bool) {
	for _, s := range library {
		if s.ID == skillName || s.Name == skillName {
			return s.Ref(), true
		}
	}
	return "", false
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

// candidateApprovedOnDisk re-reads the candidates state and reports whether
// the given candidate is persisted as approved. Used to disambiguate a
// write error that fired after the atomic rename made the state visible.
func candidateApprovedOnDisk(candidatesPath, candidateID string) bool {
	persisted, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		return false
	}
	for i := range persisted {
		if persisted[i].ID == candidateID {
			return persisted[i].Status == "approved"
		}
	}
	return false
}
