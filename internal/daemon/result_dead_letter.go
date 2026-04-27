package daemon

// Notification dead-letter handling extracted from result_handler.go (F-042
// step 4 physical file split). Covers: detecting retry-exhausted entries,
// transitioning them under the result-file lock, and archiving them outside
// the lock so the on-disk dead-letter store mirrors the in-memory state.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// sweepExhaustedNotifications transitions any non-terminal Notifiable entries
// whose notify_attempts >= maxNotifyAttempts to notify_dead_lettered, archives
// them under dead_letters/, and invokes the spec's onDeadLetter callback so
// the orchestrator (or other observer) is informed of permanent loss.
//
// Two-phase under-lock / outside-lock execution:
//   - applyDeadLetterTransitions mutates and persists state under the result
//     file lock; the second phase reloads the file under a fresh lock and
//     archives outside the lock so I/O errors do not extend the critical
//     section.
func sweepExhaustedNotifications[T any, PT interface {
	*T
	model.Notifiable
}, F any](rh *ResultHandler, spec *resultFileSpec[T, PT, F]) {
	transitioned := applyDeadLetterTransitions(rh, spec)
	if len(transitioned) == 0 {
		return
	}
	archiveAndInvokeDeadLetterCallbacks(rh, spec, transitioned)
}

// applyDeadLetterTransitions transitions every retry-exhausted result entry
// to notify_dead_lettered under the spec's lock and persists the change.
// Returns the IDs that transitioned in this pass so the caller can run
// archive + onDeadLetter callbacks after releasing the lock. F-042 step 1
// helper extracted from sweepExhaustedNotifications.
func applyDeadLetterTransitions[T any, PT interface {
	*T
	model.Notifiable
}, F any](rh *ResultHandler, spec *resultFileSpec[T, PT, F]) []string {
	rh.lockMap.Lock(spec.lockKey)
	defer rh.lockMap.Unlock(spec.lockKey)

	rf, err := spec.loadFile(spec.resultPath)
	if err != nil {
		return nil
	}
	results := spec.getResults(rf)

	transitioned := make([]string, 0)
	now := rh.clock.Now().UTC().Format(time.RFC3339)
	changed := false
	for i := range results {
		r := PT(&results[i])
		if r.IsNotified() || r.IsNotifyDeadLettered() {
			continue
		}
		if r.GetNotifyAttempts() < maxNotifyAttempts {
			continue
		}
		reason := fmt.Sprintf("notify_attempts (%d) >= max_notify_attempts (%d) for %s",
			r.GetNotifyAttempts(), maxNotifyAttempts, spec.label)
		r.MarkNotifyDeadLetter(now, reason)
		transitioned = append(transitioned, r.GetResultID())
		changed = true
		rh.log(LogLevelError, "notify_dead_letter %s result=%s reason=%s",
			spec.label, r.GetResultID(), reason)
	}
	if !changed {
		return transitioned
	}
	if err := yamlutil.AtomicWrite(spec.resultPath, rf); err != nil {
		rh.log(LogLevelError, "sweep_dead_letter_write %s error=%v", spec.label, err)
		// Persistence failed: the in-memory transition will be re-derived
		// on the next sweep, so drop the IDs to avoid invoking callbacks
		// for state that is not on disk yet.
		return nil
	}
	return transitioned
}

// archiveAndInvokeDeadLetterCallbacks reloads the result file under the
// lock, then archives each transitioned entry and runs the spec's
// onDeadLetter callback OUTSIDE the lock. F-042 step 1 helper extracted
// from sweepExhaustedNotifications. The reload is necessary because the
// transition was committed by applyDeadLetterTransitions and the in-memory
// snapshot here may be stale relative to other writers.
func archiveAndInvokeDeadLetterCallbacks[T any, PT interface {
	*T
	model.Notifiable
}, F any](rh *ResultHandler, spec *resultFileSpec[T, PT, F], transitioned []string) {
	rh.lockMap.Lock(spec.lockKey)
	rf2, err := spec.loadFile(spec.resultPath)
	rh.lockMap.Unlock(spec.lockKey)
	if err != nil {
		rh.log(LogLevelWarn, "sweep_dead_letter_reload %s error=%v", spec.label, err)
		return
	}
	for _, id := range transitioned {
		entry := spec.findByID(rf2, id)
		if entry == nil {
			continue
		}
		reason := ""
		if r := entry.GetNotifyDeadLetterReason(); r != nil {
			reason = *r
		}
		if err := rh.archiveNotifyDeadLetter(spec.label, id, entry, reason); err != nil {
			rh.log(LogLevelError, "archive_notify_dead_letter %s result=%s error=%v",
				spec.label, id, err)
		}
		if spec.onDeadLetter != nil {
			spec.onDeadLetter(entry)
		}
	}
}

// archiveNotifyDeadLetter writes a dead-letter archive for a result whose
// notification retries were exhausted. Mirrors DeadLetterProcessor.archiveDeadLetter
// in file layout so operators can correlate entries from a single dead_letters/ view.
func (rh *ResultHandler) archiveNotifyDeadLetter(label, entryID string, entry any, reason string) error {
	archiveDir := filepath.Join(rh.maestroDir, "dead_letters")
	if err := os.MkdirAll(archiveDir, 0755); err != nil { //nolint:gosec // 0755 matches dispatch dead-letter archive perms
		return fmt.Errorf("create dead_letters dir: %w", err)
	}
	type archiveEntry struct {
		SchemaVersion  int    `yaml:"schema_version"`
		FileType       string `yaml:"file_type"`
		QueueType      string `yaml:"queue_type"`
		Entry          any    `yaml:"entry"`
		DeadLetteredAt string `yaml:"dead_lettered_at"`
		Reason         string `yaml:"reason"`
	}
	now := rh.clock.Now().UTC()
	a := archiveEntry{
		SchemaVersion:  1,
		FileType:       "dead_letter",
		QueueType:      "result:" + label,
		Entry:          entry,
		DeadLetteredAt: now.Format(time.RFC3339),
		Reason:         reason,
	}
	filename := fmt.Sprintf("result_%s_%s_%s.yaml",
		strings.ReplaceAll(label, "=", "_"),
		now.Format("20060102T150405Z"),
		entryID,
	)
	return yamlutil.AtomicWrite(filepath.Join(archiveDir, filename), a)
}
