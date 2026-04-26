package daemon

import "path/filepath"

type resultBestEffortService struct {
	api *ResultWriteAPI
}

func newResultBestEffortService(api *ResultWriteAPI) resultBestEffortService {
	return resultBestEffortService{api: api}
}

// Handle performs lease-epoch-guarded best-effort writes for learnings and
// skill candidates. Returns a rejection ID if they were dropped due to lease
// revocation.
//
// These payloads are worker self-reports that should be preserved even if
// Verify later overrules the worker's "completed" claim.
func (s resultBestEffortService) Handle(params ResultWriteParams, resultID string) string {
	h := s.api
	bestEffortAllowed := true
	var rejectionID string

	hasMeaningfulBestEffort := (len(params.Learnings) > 0 && h.config.Learnings.Enabled) ||
		len(params.SkillCandidates) > 0
	if hasMeaningfulBestEffort {
		if skip, reason := h.checkLeaseEpochForBestEffort(params); skip {
			h.logFn(LogLevelWarn, "best_effort_writes_skipped task=%s command=%s reason=%s",
				params.TaskID, params.CommandID, reason)
			bestEffortAllowed = false
			if id, persisted, perr := h.persistLeaseRejection(params, reason); perr != nil {
				h.logFn(LogLevelError,
					"lease_rejection_persist_failed task=%s command=%s reporter=%s error=%v "+
						"(learnings/skill_candidates lost; not recorded in audit trail)",
					params.TaskID, params.CommandID, params.Reporter, perr)
			} else if !persisted {
				h.logFn(LogLevelInfo,
					"lease_rejection_recheck_cleared task=%s command=%s reporter=%s "+
						"(advisory mismatch resolved under lock; best-effort writes re-enabled)",
					params.TaskID, params.CommandID, params.Reporter)
				bestEffortAllowed = true
			} else {
				rejectionID = id
				h.logFn(LogLevelWarn,
					"lease_rejection_persisted rejection_id=%s task=%s command=%s reporter=%s reason=%s "+
						"learnings_count=%d skill_candidate_count=%d",
					rejectionID, params.TaskID, params.CommandID, params.Reporter, reason,
					len(params.Learnings), len(params.SkillCandidates))
			}
		}
	}

	if bestEffortAllowed && len(params.Learnings) > 0 && h.config.Learnings.Enabled {
		learningsPath := learningsFilePath(h.maestroDir)
		if err := h.writeLearnings(params, resultID); err != nil {
			h.logFn(LogLevelWarn, "learnings_write_failed result=%s task=%s command=%s path=%s count=%d error=%v "+
				"(learnings data lost; core result already committed; manual recovery: re-submit result with same learnings)",
				resultID, params.TaskID, params.CommandID, learningsPath, len(params.Learnings), err)
		}
	}

	if bestEffortAllowed && len(params.SkillCandidates) > 0 {
		candidatesPath := filepath.Join(h.maestroDir, "state", "skill_candidates.yaml")
		if err := h.writeSkillCandidates(params); err != nil {
			h.logFn(LogLevelWarn, "skill_candidates_write_failed result=%s task=%s command=%s path=%s count=%d error=%v "+
				"(skill candidates lost; core result already committed; manual recovery: re-submit result with same skill_candidates)",
				resultID, params.TaskID, params.CommandID, candidatesPath, len(params.SkillCandidates), err)
		}
	}

	return rejectionID
}
