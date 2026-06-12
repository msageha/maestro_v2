package daemon

import (
	"errors"
	"os"

	"github.com/msageha/maestro_v2/internal/model"
)

// queueTaskAliveChecker reads the worker's queue file under the standard
// "queue:<worker>" lock and reports whether a specific task is still
// owned by the dispatch attempt identified by expectedLeaseEpoch.
//
// The checker is wired into the dispatcher's inline retry loop so a
// retry that races a successful worker completion (or a fencing-bumped
// lease epoch) aborts before sending another paste-Enter wave to a pane
// that would re-dispatch the same envelope.
type queueTaskAliveChecker struct {
	qh *QueueHandler
}

// newQueueTaskAliveChecker constructs a TaskAliveChecker backed by qh's
// maestroDir + lockMap. qh must remain valid for the dispatcher's
// lifetime; the checker holds the bare pointer rather than copying
// fields so reconfiguration (rare; restart-only in practice) propagates
// without re-wiring.
func newQueueTaskAliveChecker(qh *QueueHandler) *queueTaskAliveChecker {
	return &queueTaskAliveChecker{qh: qh}
}

// IsDispatchActive returns true when the queue entry for taskID is still
// in_progress and its lease epoch matches the expectedLeaseEpoch
// (i.e. no concurrent fencing reject bumped the epoch out from under
// the dispatch). Returns true on read errors so a transient stat or
// parse failure does not abort the retry loop spuriously — the next
// iteration's check will pick up real state changes.
func (c *queueTaskAliveChecker) IsDispatchActive(workerID, taskID string, expectedLeaseEpoch int) bool {
	if c == nil || c.qh == nil {
		// Misconfigured wiring: keep the legacy behaviour (always
		// retry). The startup code path ensures this never happens for
		// production daemons.
		return true
	}
	queuePath := taskQueuePath(c.qh.maestroDir, workerID)

	lockKey := "queue:" + workerID
	c.qh.lockMap.Lock(lockKey)
	defer c.qh.lockMap.Unlock(lockKey)

	tq, _, err := loadYAMLFile[model.TaskQueue](queuePath, true)
	if err != nil {
		// Read error: be conservative and let the retry continue. The
		// alternative (treating IO errors as "task gone") would mask a
		// disk hiccup as a permanent abort.
		if !errors.Is(err, os.ErrNotExist) {
			c.qh.log(LogLevelDebug,
				"dispatch_alive_check_read_failed worker=%s task=%s error=%v",
				workerID, taskID, err)
		}
		return true
	}
	for i := range tq.Tasks {
		t := &tq.Tasks[i]
		if t.ID != taskID {
			continue
		}
		// Terminal in queue → worker / scanner has moved on.
		if model.IsTerminal(t.Status) {
			return false
		}
		// Status reverted to pending (lease released, dispatch failed
		// out of band, or another path moved the task back). Continuing
		// to retry would race the next scan's re-dispatch.
		if t.Status != model.StatusInProgress {
			return false
		}
		// Fencing-bumped lease epoch means a different dispatch
		// attempt now owns this task; this dispatcher's writes would
		// be rejected by Phase C anyway.
		if t.LeaseEpoch != expectedLeaseEpoch {
			return false
		}
		return true
	}
	// Task no longer present in the queue (cancelled, dead-lettered,
	// retry rewrite). Definitely not ours to drive.
	return false
}
