package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

// handleBestEffortWrites performs lease-epoch-guarded best-effort writes for
// learnings, skill candidates, and review dispatch. Returns a rejection ID if
// learnings/skill_candidates were dropped due to lease revocation.
func (h *ResultWriteAPI) handleBestEffortWrites(params ResultWriteParams, resultID string, resultStatus model.Status) string {
	bestEffortAllowed := true
	var rejectionID string

	// Only treat advisory skip as a "rejected drop" when at least one
	// best-effort write would actually have occurred.
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

	// Advisory review dispatch: non-blocking, best-effort.
	if rc := h.reviewCoord(); resultStatus == model.StatusCompleted && rc != nil && rc.Enabled() {
		rc.DispatchIfEligible(h.ctx(), params)
	}

	return rejectionID
}

func (h *ResultWriteAPI) resultWritePhaseB(params ResultWriteParams, resultID string, resultStatus model.Status, queueWriteFailed bool) error {
	cmdLockKey := "state:" + params.CommandID
	h.lockMap.Lock(cmdLockKey)
	defer h.lockMap.Unlock(cmdLockKey)

	statePath := commandStatePath(h.maestroDir, params.CommandID)

	return updateYAMLFile(statePath, func(state *model.CommandState) error {
		if state.CommandID == "" && state.SchemaVersion == 0 && state.TaskStates == nil {
			return fmt.Errorf("state not found for command %s", params.CommandID)
		}
		if state.TaskStates == nil {
			state.TaskStates = make(map[string]model.Status)
		}

		recordedStatus := h.resolveRecordedStatus(state, params, resultStatus)
		state.TaskStates[params.TaskID] = recordedStatus

		now := h.clock.Now()
		state.UpdatedAt = now.UTC().Format(time.RFC3339)

		h.updateCircuitBreaker(state, params, resultStatus, resultID, now)

		if state.AppliedResultIDs == nil {
			state.AppliedResultIDs = make(map[string]string)
		}
		state.AppliedResultIDs[params.TaskID] = resultID

		h.recordQueueWriteFailedSticky(state, params, resultID, queueWriteFailed)

		return nil
	})
}

// resolveRecordedStatus determines the task status to record in the command
// state, handling late-arriving results and idempotent re-submissions.
//
// Late-arriving result handling: if the plan has already failed or cancelled,
// a still-pending task's late result is coerced to cancelled so the plan view
// stays consistent. Two carve-outs:
//  1. plan_status=completed is left alone.
//  2. If TaskStates[taskID] is already terminal, keep it (idempotent re-submission).
//
// The result file (audit trail) and queue entry still record the reporter's
// original status — only the plan's TaskStates view is normalized.
func (h *ResultWriteAPI) resolveRecordedStatus(state *model.CommandState, params ResultWriteParams, resultStatus model.Status) model.Status {
	existing, hadExisting := state.TaskStates[params.TaskID]
	planFailedOrCancelled := state.PlanStatus == model.PlanStatusFailed || state.PlanStatus == model.PlanStatusCancelled
	if planFailedOrCancelled && (!hadExisting || !model.IsTerminal(existing)) {
		h.logFn(LogLevelWarn,
			"result_write late_after_plan_terminal task=%s command=%s plan_status=%s reported=%s coerced=cancelled",
			params.TaskID, params.CommandID, state.PlanStatus, resultStatus)
		return model.StatusCancelled
	}
	if hadExisting && model.IsTerminal(existing) {
		return existing
	}
	return resultStatus
}

// updateCircuitBreaker checks task-unit idempotency and updates the circuit
// breaker counters if this is a new (non-duplicate) result. The original
// reported status (not the coerced one) is passed to the CB so metrics reflect
// actual task execution outcomes.
func (h *ResultWriteAPI) updateCircuitBreaker(state *model.CommandState, params ResultWriteParams, resultStatus model.Status, resultID string, now time.Time) {
	if recordedID, ok := state.AppliedResultIDs[params.TaskID]; ok {
		if recordedID != resultID {
			h.logFn(LogLevelWarn,
				"result_write applied_result_ids drift task=%s command=%s recorded=%s incoming=%s (skipping CB counter update)",
				params.TaskID, params.CommandID, recordedID, resultID)
		}
		return
	}

	if cb := h.circuitBreaker(); cb != nil {
		tripped, reason := cb.UpdateCounterOnResult(state, resultStatus, params.TaskID, resultID, now)
		if tripped {
			cb.TripBreaker(state, reason, now)
		}
	}
}

// recordQueueWriteFailedSticky persists the H2 sticky error marker when Phase A
// failed to update the worker queue to terminal status after retry.
func (h *ResultWriteAPI) recordQueueWriteFailedSticky(state *model.CommandState, params ResultWriteParams, resultID string, queueWriteFailed bool) {
	if !queueWriteFailed {
		return
	}
	if state.QueueWriteFailed == nil {
		state.QueueWriteFailed = make(map[string]string)
	}
	state.QueueWriteFailed[params.TaskID] = params.Reporter + ":" + resultID
	h.logFn(LogLevelWarn,
		"result_write queue_write_failed_sticky task=%s command=%s worker=%s result=%s",
		params.TaskID, params.CommandID, params.Reporter, resultID)
}

// checkLeaseEpochForBestEffort performs a read-only lease epoch check against
// the queue file. Returns (skip=true, reason) only on definitive epoch mismatch.
// On I/O errors, parse errors, or task-not-found (task may have been archived
// after completion), returns (skip=false, "") to allow writes to proceed.
func (h *ResultWriteAPI) checkLeaseEpochForBestEffort(params ResultWriteParams) (skip bool, reason string) {
	queuePath := taskQueuePath(h.maestroDir, params.Reporter)
	data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		return false, ""
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		return false, ""
	}
	for _, task := range tq.Tasks {
		if task.ID == params.TaskID {
			if task.LeaseEpoch != params.LeaseEpoch {
				return true, fmt.Sprintf("lease_epoch_mismatch: queue=%d request=%d", task.LeaseEpoch, params.LeaseEpoch)
			}
			return false, ""
		}
	}
	// Task not found — may have been archived after completion; allow writes
	return false, ""
}
