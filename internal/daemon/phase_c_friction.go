package daemon

import (
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/model"
)

// C-5 friction-driven improvement loop (issue #26).
//
// The loop extends the existing C-5 repair-strategy machinery — it does NOT
// run beside it. Fingerprints are shared with the FingerprintDB, and the
// three lifecycle hooks piggyback on paths that already exist:
//
//	friction capture  → recordFrictionSignal (result time, this file)
//	application       → RepairHintForRetry   (dispatch time, phase_c_repair.go)
//	measurement       → recordRepairOutcome  (retry terminal, phase_c_repair.go)
//
// The daemon records and presents only: proposals surface in
// state/improvements.yaml and as a DATA section in Planner command
// envelopes. Applying an improvement (rewriting prompts / personas /
// workflow guidance) stays a Planner/human decision, and safety parameters,
// gate thresholds and daemon control logic are excluded targets.

// recordFrictionSignal is the friction-capture path of
// recordTaskResultLearning. Every failed / dead-lettered result is a
// friction event: worker task failures, and — via the daemon's synthetic
// results — blocked confirmation prompts (blocked_pane_timeout), runtime
// terminal errors, dispatch preflight blocks and dead letters. The event is
// keyed by the same error fingerprint the FingerprintDB stores, so the
// improvement lifecycle stays 1:1 with failure-pattern learning.
func (rh *ResultHandler) recordFrictionSignal(r *model.TaskResult, m *PhaseCManager) {
	if m == nil || m.ImprovementStore == nil {
		return
	}
	switch r.Status {
	case model.StatusFailed, model.StatusDeadLetter:
	default:
		return
	}
	fp, category := learnings.ComputeErrorFingerprint(r.Summary)
	if fp == "" {
		return
	}
	kind := learnings.ClassifyFrictionKind(r.Summary, r.Status == model.StatusDeadLetter)
	imp, transitioned := m.ImprovementStore.RecordFriction(fp, kind, category, time.Now())
	if transitioned {
		rh.log(LogLevelInfo, "improvement_%s fp=%s kind=%s task=%s command=%s occurrences=%d reopen_count=%d",
			imp.Status, fp, kind, r.TaskID, r.CommandID, imp.OccurrenceCount, imp.ReopenCount)
		return
	}
	rh.log(LogLevelDebug, "friction_event fp=%s kind=%s task=%s command=%s status=%s occurrences=%d",
		fp, kind, r.TaskID, r.CommandID, imp.Status, imp.OccurrenceCount)
}

// recordImprovementMeasure feeds a repairing retry's terminal outcome into
// the improvement measurement window (called from recordRepairOutcome once
// the retry↔fingerprint attribution is resolved). Cancelled retries measure
// nothing — they say nothing about the strategy's effect.
func (rh *ResultHandler) recordImprovementMeasure(r *model.TaskResult, m *PhaseCManager, fp string) {
	if m == nil || m.ImprovementStore == nil || fp == "" {
		return
	}
	var success bool
	switch r.Status {
	case model.StatusCompleted:
		success = true
	case model.StatusFailed, model.StatusDeadLetter:
		success = false
	default:
		return
	}
	imp, verified := m.ImprovementStore.RecordRepairOutcome(fp, success, m.metricsCountersSnapshot(), time.Now())
	if verified {
		rh.log(LogLevelInfo,
			"improvement_verified fp=%s kind=%s task=%s command=%s successes=%d recurrences=%d baseline_occurrences=%d",
			fp, imp.Kind, r.TaskID, r.CommandID,
			imp.Measure.PostApplySuccesses, imp.Measure.PostApplyRecurrences, imp.Measure.BaselineOccurrences)
	}
}

// metricsCountersSnapshot reads the current state/metrics.yaml counters for
// baseline / verification quantification. Best-effort by design: any read or
// parse failure yields zero counters and never blocks the learning path.
func (m *PhaseCManager) metricsCountersSnapshot() model.MetricsCounters {
	if m == nil || m.maestroDir == "" {
		return model.MetricsCounters{}
	}
	path := filepath.Join(m.maestroDir, "state", "metrics.yaml")
	data, err := os.ReadFile(path) //nolint:gosec // controlled maestroDir-based path
	if err != nil {
		return model.MetricsCounters{}
	}
	var mt model.Metrics
	if err := yamlv3.Unmarshal(data, &mt); err != nil {
		return model.MetricsCounters{}
	}
	return mt.Counters
}

// ImprovementProposalSection returns the injectable DATA section carrying
// the currently actionable (proposed / reopened) improvements for the
// Planner, or "" when the friction loop is disabled, injection is turned
// off (inject_count: 0), or nothing is actionable. Wired into Planner
// command dispatch via Dispatcher.SetImprovementSectionProvider.
func (m *PhaseCManager) ImprovementProposalSection() string {
	if m == nil || m.ImprovementStore == nil {
		return ""
	}
	imps := m.ImprovementStore.Actionable(m.improvementInjectCount)
	return learnings.FormatImprovementProposalSection(imps, m.improvementExcludeTargets)
}
