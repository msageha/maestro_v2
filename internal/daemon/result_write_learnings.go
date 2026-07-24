package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// persistLeaseRejection records a RejectedSubmission entry in the reporter's
// results file when a best-effort lease epoch check detects that the worker
// no longer holds a valid lease. The function takes the canonical
// queue→result lock order and re-verifies the lease epoch under the lock so
// the persisted record is authoritative (not based on the unlocked advisory
// read). Duplicate stale-worker retries with identical payload are
// suppressed via a content fingerprint (DedupKey) so the rejection log
// cannot grow unbounded.
//
// Returns the persisted rejection ID, or "" with an error if the persist
// failed. If the post-lock re-check shows the lease epoch now matches
// (race: revoke rolled back, or task archived), the function returns
// ("", nil) and the caller should treat best-effort writes as still
// rejected — the original advisory check has already gated them and we
// avoid creating a misleading audit record.
func (h *ResultWriteAPI) persistLeaseRejection(params ResultWriteParams, advisoryReason string) (string, bool, error) {
	h.acquireFileLock()
	defer h.releaseFileLock()

	queueLockKey := "queue:" + params.Reporter
	h.lockMap.Lock(queueLockKey)
	defer h.lockMap.Unlock(queueLockKey)

	resultLockKey := "result:" + params.Reporter
	h.lockMap.Lock(resultLockKey)
	defer h.lockMap.Unlock(resultLockKey)

	// Re-verify lease epoch mismatch authoritatively under the queue lock.
	queueLeaseEpoch, authoritativeReason, falsePositive := h.reVerifyLeaseEpoch(params, advisoryReason)
	if falsePositive {
		return "", false, nil
	}

	rf, resultPath, err := h.loadOrInitResultFile(params.Reporter)
	if err != nil {
		return "", false, err
	}

	return h.deduplicateAndPersistRejection(params, rf, resultPath, queueLeaseEpoch, authoritativeReason)
}

// reVerifyLeaseEpoch re-reads the task queue under the lock and checks whether
// the lease epoch mismatch still holds. Returns the authoritative queue epoch,
// the reason string, and a boolean indicating false positive (epoch now matches).
func (h *ResultWriteAPI) reVerifyLeaseEpoch(params ResultWriteParams, advisoryReason string) (int, string, bool) {
	queuePath := taskQueuePath(h.maestroDir, params.Reporter)
	queueLeaseEpoch := -1
	authoritativeReason := advisoryReason

	data, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		return queueLeaseEpoch, authoritativeReason, false
	}

	var tq model.TaskQueue
	if perr := yamlv3.Unmarshal(data, &tq); perr != nil {
		return queueLeaseEpoch, authoritativeReason, false
	}

	found := false
	for _, task := range tq.Tasks {
		if task.ID == params.TaskID {
			found = true
			queueLeaseEpoch = task.LeaseEpoch
			if task.LeaseEpoch == params.LeaseEpoch {
				// Race: epoch matches under the lock — false positive.
				return queueLeaseEpoch, "", true
			}
			authoritativeReason = fmt.Sprintf(
				"lease_epoch_mismatch_under_lock: queue=%d request=%d",
				task.LeaseEpoch, params.LeaseEpoch)
			break
		}
	}
	if !found {
		authoritativeReason = "task_archived_after_lease_revoke"
	}
	return queueLeaseEpoch, authoritativeReason, false
}

// loadOrInitResultFile loads the reporter's result file, initializing schema
// defaults if the file doesn't exist yet.
func (h *ResultWriteAPI) loadOrInitResultFile(reporter string) (model.TaskResultFile, string, error) {
	resultPath := resultFilePath(h.maestroDir, reporter)
	var rf model.TaskResultFile
	if data, err := os.ReadFile(resultPath); err == nil { //nolint:gosec // resultPath is constructed from a controlled application results directory
		if perr := yamlv3.Unmarshal(data, &rf); perr != nil {
			return rf, resultPath, fmt.Errorf("parse results file: %w", perr)
		}
	} else if !os.IsNotExist(err) {
		return rf, resultPath, fmt.Errorf("read results file: %w", err)
	}

	if rf.SchemaVersion == 0 {
		rf.SchemaVersion = 1
		rf.FileType = "result_task"
	}
	return rf, resultPath, nil
}

// deduplicateAndPersistRejection checks for duplicate rejections and, if none
// found, creates and persists a new RejectedSubmission entry.
func (h *ResultWriteAPI) deduplicateAndPersistRejection(
	params ResultWriteParams,
	rf model.TaskResultFile,
	resultPath string,
	queueLeaseEpoch int,
	authoritativeReason string,
) (string, bool, error) {
	dedupKey := computeRejectionDedupKey(params, queueLeaseEpoch)
	for _, existing := range rf.RejectedSubmissions {
		if existing.DedupKey == dedupKey {
			return existing.ID, true, nil
		}
	}

	rejectionID, err := model.GenerateID(model.IDTypeResult)
	if err != nil {
		return "", false, fmt.Errorf("generate rejection id: %w", err)
	}

	// Defensive copy slices so a later mutation of params can't alias
	// into the persisted record.
	lostLearnings := append([]string(nil), params.Learnings...)
	lostSkills := append([]string(nil), params.SkillCandidates...)

	rf.RejectedSubmissions = append(rf.RejectedSubmissions, model.RejectedSubmission{
		ID:                  rejectionID,
		TaskID:              params.TaskID,
		CommandID:           params.CommandID,
		Reporter:            params.Reporter,
		Reason:              authoritativeReason,
		RequestLeaseEpoch:   params.LeaseEpoch,
		QueueLeaseEpoch:     queueLeaseEpoch,
		LostLearnings:       lostLearnings,
		LostSkillCandidates: lostSkills,
		OriginalSummary:     params.Summary,
		DedupKey:            dedupKey,
		CreatedAt:           h.clock.Now().UTC().Format(time.RFC3339),
	})

	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		return "", false, fmt.Errorf("write results file: %w", err)
	}
	h.recordSelfWrite(resultPath, rf)
	return rejectionID, true, nil
}

// computeRejectionDedupKey produces a deterministic content fingerprint of
// the rejected submission so that an identical stale-worker retry collapses
// to the existing record. Order of learnings/skill_candidates is preserved
// (matching how the worker submitted them).
func computeRejectionDedupKey(params ResultWriteParams, queueLeaseEpoch int) string {
	h := sha256.New()
	h.Write([]byte(params.TaskID))
	h.Write([]byte{0})
	h.Write([]byte(params.CommandID))
	h.Write([]byte{0})
	h.Write([]byte(params.Reporter))
	h.Write([]byte{0})
	_, _ = fmt.Fprintf(h, "%d", params.LeaseEpoch)
	h.Write([]byte{0})
	_, _ = fmt.Fprintf(h, "%d", queueLeaseEpoch)
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(params.Learnings, "\x01")))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(params.SkillCandidates, "\x01")))
	return hex.EncodeToString(h.Sum(nil))
}

// writeLearnings appends learning entries to .maestro/state/learnings.yaml.
// Best-effort: errors are logged but do not fail the result_write.
func (h *ResultWriteAPI) writeLearnings(params ResultWriteParams, resultID string) error {
	// Lock order: leaf lock under the state:* namespace. See doc.go — must
	// be acquired in isolation (no state:{commandID} held). Phase B has
	// already released state:{commandID} by the time this is called.
	h.lockMap.Lock("state:learnings")
	defer h.lockMap.Unlock("state:learnings")

	learningsPath := learningsFilePath(h.maestroDir)
	maxEntries := h.config.Learnings.EffectiveMaxEntries()
	maxLen := h.config.Learnings.EffectiveMaxContentLength()

	// Pre-check: attempt corruption recovery if the file is unreadable.
	// ReadModifyWrite returns a parse error when the file exists but is corrupt;
	// we recover and then proceed with the normal RMW path.
	//
	// NOTE: This intentionally reads the file twice — once here to probe for
	// corruption, and again inside updateYAMLFile for the RMW cycle. The double
	// read is an accepted trade-off: recoverCorruptLearningsIfNeeded must verify
	// file health (and quarantine if corrupt) before updateYAMLFile's RMW, and
	// updateYAMLFile is a generic function shared by many callers that handles
	// its own loading. The overhead is one extra file read for the normal
	// (non-corrupt) case.
	if err := h.recoverCorruptLearningsIfNeeded(learningsPath); err != nil {
		return err
	}

	var added int
	if err := updateYAMLFile(learningsPath, func(lf *model.LearningsFile) error {
		if lf.SchemaVersion == 0 {
			lf.SchemaVersion = 1
			lf.FileType = "state_learnings"
		}

		// Build dedup set: result_id + content
		type dedupKey struct {
			resultID string
			content  string
		}
		existing := make(map[dedupKey]bool, len(lf.Learnings))
		for _, l := range lf.Learnings {
			existing[dedupKey{l.ResultID, l.Content}] = true
		}

		now := h.clock.Now().UTC().Format(time.RFC3339)
		for _, content := range params.Learnings {
			if content == "" {
				continue
			}
			truncated := truncateRunes(content, maxLen)
			key := dedupKey{resultID, truncated}
			if existing[key] {
				continue
			}
			existing[key] = true
			lf.Learnings = append(lf.Learnings, model.Learning{
				ResultID:     resultID,
				CommandID:    params.CommandID,
				Content:      truncated,
				CreatedAt:    now,
				SourceWorker: params.Reporter,
			})
			added++
		}

		if added == 0 {
			return errNoUpdate
		}

		// FIFO eviction
		if len(lf.Learnings) > maxEntries {
			lf.Learnings = lf.Learnings[len(lf.Learnings)-maxEntries:]
		}
		return nil
	}); err != nil {
		return fmt.Errorf("update learnings file: %w", err)
	}

	if added > 0 {
		h.logFn(LogLevelInfo, "learnings_written result=%s added=%d", resultID, added)
	}
	return nil
}

// recoverCorruptLearningsIfNeeded checks if the learnings file is corrupt and
// attempts quarantine recovery. Returns an error only if recovery itself fails.
func (h *ResultWriteAPI) recoverCorruptLearningsIfNeeded(learningsPath string) error {
	data, err := os.ReadFile(learningsPath) //nolint:gosec // learningsPath is constructed from a controlled application state directory
	if err != nil {
		return nil // missing or unreadable — updateYAMLFile will handle
	}
	var probe model.LearningsFile
	if yamlv3.Unmarshal(data, &probe) == nil {
		return nil // file is valid
	}
	h.logFn(LogLevelWarn, "learnings_file_corrupt, recovering")
	if recErr := yamlutil.RecoverCorruptedFile(h.maestroDir, learningsPath, "state_learnings"); recErr != nil {
		return fmt.Errorf("recover learnings file: %w", recErr)
	}
	return nil
}

// writeSkillCandidates merges skill candidate entries into .maestro/state/skill_candidates.yaml.
// Best-effort: errors are logged but do not fail the result_write.
func (h *ResultWriteAPI) writeSkillCandidates(params ResultWriteParams) error {
	// Lock order: leaf lock under the state:* namespace. See doc.go — must
	// be acquired in isolation (no state:{commandID} held). Phase B has
	// already released state:{commandID} by the time this is called.
	h.lockMap.Lock("state:skill_candidates")
	defer h.lockMap.Unlock("state:skill_candidates")

	candidatesPath := filepath.Join(h.maestroDir, "state", "skill_candidates.yaml")
	now := h.clock.Now().UTC().Format(time.RFC3339)

	candidates, err := skill.ReadCandidates(candidatesPath)
	if err != nil {
		return fmt.Errorf("read skill candidates: %w", err)
	}

	idFunc := func() (string, error) {
		return model.GenerateID(model.IDTypeSkillCandidate)
	}

	// Registration-time dedup hint: annotate each new candidate with the
	// existing library skills (bundled catalog + skills.extra_dirs) that
	// already cover a similar pattern, so the operator can reject
	// re-proposals of what the 31+ shipped skills already handle.
	library := skill.ListAllSkills(h.skillSearchDirs(), nil)

	added := 0
	for _, content := range params.SkillCandidates {
		if content == "" {
			continue
		}
		before := len(candidates)
		similar := skill.FindSimilarSkills(content, library, skill.LibraryDedupThreshold)
		candidates, err = skill.AddOrUpdateCandidate(candidates, content, params.CommandID, now, idFunc, similar)
		if err != nil {
			h.logFn(LogLevelError, "skill_candidate_add_failed content=%q: %v", sanitizeContentForLog(content), err)
			continue
		}
		if len(candidates) > before {
			// Retention cap: prune terminal (approved/rejected) entries oldest
			// first so the state file stays structurally below the read-size
			// guard; pending entries are protected. When pending entries alone
			// fill the cap, the new registration (the appended tail) is
			// skipped with a WARN — never a silent drop.
			var pruned int
			candidates, pruned = skill.EnforceCandidateCap(candidates, skill.MaxCandidates)
			if pruned > 0 {
				h.logFn(LogLevelInfo, "skill_candidates_pruned command=%s pruned=%d max=%d", params.CommandID, pruned, skill.MaxCandidates)
			}
			if len(candidates) > skill.MaxCandidates {
				candidates = candidates[:len(candidates)-1]
				h.logFn(LogLevelWarn,
					"skill_candidate_skipped_capacity command=%s max=%d: pending candidates fill the retention cap; approve or reject existing candidates to admit new ones, skipped content=%q",
					params.CommandID, skill.MaxCandidates, sanitizeContentForLog(content))
				continue
			}
			added++
		}
	}

	if err := skill.WriteCandidates(candidatesPath, candidates); err != nil {
		return fmt.Errorf("write skill candidates: %w", err)
	}

	h.logFn(LogLevelInfo, "skill_candidates_written command=%s added=%d total=%d", params.CommandID, added, len(candidates))
	return nil
}

// skillSearchDirs returns the precedence-ordered skill source directories
// (configured skills.extra_dirs, then the bundled <maestroDir>/skills
// catalog), mirroring the dispatch-time resolution in dispatch/envelope.go.
func (h *ResultWriteAPI) skillSearchDirs() []string {
	bundledDir := filepath.Join(h.maestroDir, "skills")
	projectRoot := filepath.Dir(h.maestroDir)
	return skill.ResolveSearchDirs(h.config.Skills.ExtraDirs, projectRoot, bundledDir, nil)
}
