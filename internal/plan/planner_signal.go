package plan

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"syscall"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// plannerSignalQueuePath returns the on-disk location of the planner signal
// queue. Mirrors internal/daemon/paths.go:signalQueuePath so the plan package
// can write to the same file when the daemon is not in the loop.
func plannerSignalQueuePath(maestroDir string) string {
	return filepath.Join(maestroDir, "queue", "planner_signals.yaml")
}

// emitPausedForReplanSignal appends a paused_for_replan PlannerSignal to the
// shared signal queue. Used as a recovery escape hatch when phase-fill
// rollback itself fails: the on-disk queue/state is now ambiguous, so we
// surface a structured signal to the planner instead of letting the command
// silently rot.
//
// Locking:
//   - Callers MUST NOT hold the state lock; this helper acquires
//     `queue:planner_signals` and the canonical lock order is
//     queue → state → result.
//   - In-process: lockMap acquires `queue:planner_signals`.
//   - Cross-process: a flock on locks/planner_signals.yaml.flock guards
//     against other processes (Daemon, parallel CLI invocations).
//
// All failures are logged and swallowed — the caller has already returned an
// error to the user; the signal is purely a best-effort recovery hint.
func emitPausedForReplanSignal(maestroDir, commandID, phaseID, reason string, lockMap *lock.MutexMap) {
	if lockMap == nil {
		slog.Error("paused_for_replan_signal_skipped_no_lockmap",
			"command_id", commandID, "phase_id", phaseID, "reason", reason)
		return
	}

	unlockKey := lockQueueKeys(lockMap, []string{"queue:planner_signals"})
	defer unlockKey()

	flockFile, flockErr := acquireFlock(queueFlockPath(maestroDir, "planner_signals.yaml"), syscall.LOCK_EX)
	if flockErr != nil {
		slog.Error("paused_for_replan_signal_flock_failed",
			"command_id", commandID, "phase_id", phaseID, "reason", reason, "error", flockErr)
		return
	}
	defer releaseFlock(flockFile)

	now := nowUTC()
	sig := model.PlannerSignal{
		Kind:      "paused_for_replan",
		CommandID: commandID,
		PhaseID:   phaseID,
		Message: fmt.Sprintf(
			"[maestro] kind:paused_for_replan command_id:%s phase_id:%s\nreason: %s\nnext_action: replan or fail the command",
			commandID, phaseID, reason),
		Reason:    reason,
		CreatedAt: now,
		UpdatedAt: now,
	}

	path := plannerSignalQueuePath(maestroDir)
	err := yamlutil.ReadModifyWrite[model.PlannerSignalQueue](path, func(sq *model.PlannerSignalQueue) error {
		if sq.SchemaVersion == 0 {
			sq.SchemaVersion = 1
			sq.FileType = "planner_signal_queue"
		}
		if plannerSignalDuplicate(sq.Signals, sig) {
			return yamlutil.ErrNoUpdate
		}
		sq.Signals = append(sq.Signals, sig)
		return nil
	})
	if err != nil && !errors.Is(err, yamlutil.ErrNoUpdate) {
		slog.Error("paused_for_replan_signal_write_failed",
			"command_id", commandID, "phase_id", phaseID, "reason", reason, "error", err)
		return
	}
	slog.Warn("paused_for_replan_signal_queued",
		"command_id", commandID, "phase_id", phaseID, "reason", reason)
}

// phaseSignalID derives a stable PhaseID from a phase name for planner
// signals emitted from phase-fill recovery paths. The "__phase_" prefix
// distinguishes phase-scoped signals from task-scoped ones (which use
// "__task_<task_id>") and from worker-scoped ones.
func phaseSignalID(phaseName string) string {
	return "__phase_" + phaseName
}

// plannerSignalDuplicate reports whether sig is already present in signals
// under the same dedup key (kind + command_id + phase_id + worker_id +
// conflict_generation). Mirrors daemon/queue_dispatch.go:signalDedupKey so
// emit-from-plan and emit-from-daemon do not produce duplicate entries.
func plannerSignalDuplicate(signals []model.PlannerSignal, sig model.PlannerSignal) bool {
	for _, existing := range signals {
		if existing.Kind == sig.Kind &&
			existing.CommandID == sig.CommandID &&
			existing.PhaseID == sig.PhaseID &&
			existing.WorkerID == sig.WorkerID &&
			existing.ConflictGeneration == sig.ConflictGeneration {
			return true
		}
	}
	return false
}
