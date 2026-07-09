package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/envelope"
	"github.com/msageha/maestro_v2/internal/model"
)

// C-5 repair-strategy loop bookkeeping (issue #43 stage 3).
//
// The FingerprintDB records failure patterns at result time, but closing the
// self-improvement loop needs two links the DB itself cannot provide:
//
//  1. failure attribution — which fingerprint did task X fail with, so a
//     retry of X can look the pattern up at dispatch time;
//  2. success attribution — which fingerprint was a completed retry task
//     repairing, so RecordRepairSuccess can credit the pattern (and adopt
//     the retry's summary as the pattern's repair strategy).
//
// Both maps are in-memory and bounded (FIFO eviction), mirroring the
// taskBloom precedent: a daemon restart between failure and retry loses at
// most one attribution, which delays learning by one occurrence instead of
// corrupting it.

// repairFingerprintMaxEntries bounds each of the two attribution maps.
const repairFingerprintMaxEntries = 512

// repairStrategyMaxLen caps the summary text adopted as a repair strategy so
// a verbose worker summary cannot bloat the persisted FingerprintDB.
const repairStrategyMaxLen = 240

// boundedFPMap is a small FIFO-bounded string map used for the two
// attribution tables. Not safe for concurrent use — callers hold repairMu.
type boundedFPMap struct {
	entries map[string]string
	order   []string
	cap     int
}

func newBoundedFPMap(capacity int) *boundedFPMap {
	return &boundedFPMap{entries: make(map[string]string), cap: capacity}
}

func (b *boundedFPMap) set(key, value string) {
	if _, exists := b.entries[key]; !exists {
		for len(b.entries) >= b.cap && len(b.order) > 0 {
			oldest := b.order[0]
			b.order = b.order[1:]
			delete(b.entries, oldest)
		}
		b.order = append(b.order, key)
	}
	b.entries[key] = value
}

func (b *boundedFPMap) get(key string) string {
	return b.entries[key]
}

func (b *boundedFPMap) consume(key string) string {
	v, ok := b.entries[key]
	if !ok {
		return ""
	}
	delete(b.entries, key)
	// The order slice keeps the consumed key (lazy deletion); eviction
	// skips keys no longer present in the map.
	return v
}

// ensureRepairMaps lazily initialises the attribution tables. Callers hold
// repairMu.
func (m *PhaseCManager) ensureRepairMaps() {
	if m.taskFailureFP == nil {
		m.taskFailureFP = newBoundedFPMap(repairFingerprintMaxEntries)
	}
	if m.retryFP == nil {
		m.retryFP = newBoundedFPMap(repairFingerprintMaxEntries)
	}
}

// RecordTaskFailureFingerprint remembers which fingerprint taskID failed
// with, so a later retry of that task can resolve the failure pattern.
// No-op on a nil manager or when the FingerprintDB is disabled.
func (m *PhaseCManager) RecordTaskFailureFingerprint(taskID, fp string) {
	if m == nil || m.FingerprintDB == nil || taskID == "" || fp == "" {
		return
	}
	m.repairMu.Lock()
	defer m.repairMu.Unlock()
	m.ensureRepairMaps()
	m.taskFailureFP.set(taskID, fp)
}

// RecordRetryFingerprint links a dispatched retry task to its predecessor's
// failure fingerprint (resolved via the task's OriginalTaskID at dispatch
// classification). Consumed when the retry terminates so a completed repair
// credits the pattern.
func (m *PhaseCManager) RecordRetryFingerprint(retryTaskID, originalTaskID string) {
	if m == nil || m.FingerprintDB == nil || retryTaskID == "" || originalTaskID == "" {
		return
	}
	m.repairMu.Lock()
	defer m.repairMu.Unlock()
	m.ensureRepairMaps()
	if fp := m.taskFailureFP.get(originalTaskID); fp != "" {
		m.retryFP.set(retryTaskID, fp)
	}
}

// ConsumeRetryFingerprint returns and releases the fingerprint the given
// retry task was repairing, or "" when the task was not a tracked retry.
func (m *PhaseCManager) ConsumeRetryFingerprint(taskID string) string {
	if m == nil || taskID == "" {
		return ""
	}
	m.repairMu.Lock()
	defer m.repairMu.Unlock()
	m.ensureRepairMaps()
	return m.retryFP.consume(taskID)
}

// RepairHintForRetry returns an injectable DATA section carrying the proven
// repair strategy for the failure the retry task addresses, or "" when the
// task is not a retry, the predecessor's fingerprint is unknown, or the
// pattern has no successful repair on record (SuggestStrategy gate).
func (m *PhaseCManager) RepairHintForRetry(task *model.Task) string {
	if m == nil || m.FingerprintDB == nil || task == nil || task.OriginalTaskID == "" {
		return ""
	}
	m.repairMu.Lock()
	m.ensureRepairMaps()
	fp := m.taskFailureFP.get(task.OriginalTaskID)
	m.repairMu.Unlock()
	if fp == "" {
		return ""
	}
	strategy, ok := m.FingerprintDB.SuggestStrategy(fp)
	if !ok || strategy == "" {
		return ""
	}
	pattern, ok := m.FingerprintDB.Query(fp)
	if !ok {
		return ""
	}
	return learnings.FormatRepairStrategySection(pattern.ErrorCategory, strategy, pattern.SuccessRate, pattern.OccurrenceCount)
}

// recordRepairOutcome closes the C-5 loop at result time: a completed retry
// credits its predecessor's failure pattern (RecordRepairSuccess adopts the
// retry's summary as the pattern's strategy when none is recorded yet); any
// terminal outcome releases the attribution entry.
func (rh *ResultHandler) recordRepairOutcome(r *model.TaskResult, m *PhaseCManager) {
	if m == nil || m.FingerprintDB == nil || !model.IsTerminal(r.Status) {
		return
	}
	fp := m.ConsumeRetryFingerprint(r.TaskID)
	if fp == "" || r.Status != model.StatusCompleted {
		return
	}
	strategy := envelope.NewRawContent(r.Summary).Sanitize().String()
	if len(strategy) > repairStrategyMaxLen {
		strategy = strategy[:repairStrategyMaxLen]
	}
	m.FingerprintDB.RecordRepairSuccess(fp, strategy)
	rh.log(LogLevelInfo, "fingerprint_repair_success task=%s command=%s fp=%s",
		r.TaskID, r.CommandID, fp)
}
