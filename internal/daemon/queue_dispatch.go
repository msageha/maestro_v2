package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// processPlannerSignalsDeferred evaluates signals but defers tmux delivery to Phase B.
func (qh *QueueHandler) processPlannerSignalsDeferred(sq *model.PlannerSignalQueue, dirty *bool, work *deferredWork) {
	now := qh.clock.Now().UTC()
	retained := make([]model.PlannerSignal, 0, len(sq.Signals))

	for i := range sq.Signals {
		sig := &sq.Signals[i]

		if sig.NextAttemptAt != nil {
			nextAt, err := time.Parse(time.RFC3339, *sig.NextAttemptAt)
			if err == nil && nextAt.After(now) {
				retained = append(retained, *sig)
				continue
			}
		}

		if qh.dependencyResolver.HasStateReader() && sig.PhaseID != "" {
			// Phase-level signals: check phase existence (orphan) and staleness
			phaseStatus, err := qh.dependencyResolver.GetPhaseStatus(sig.CommandID, sig.PhaseID)
			if err != nil {
				if errors.Is(err, ErrPhaseNotFound) || errors.Is(err, ErrStateNotFound) || os.IsNotExist(err) {
					qh.log(LogLevelInfo, "signal_orphaned_removed kind=%s command=%s phase=%s error=%v",
						sig.Kind, sig.CommandID, sig.PhaseID, err)
					*dirty = true
					continue
				}
				qh.log(LogLevelWarn, "signal_phase_check command=%s phase=%s error=%v",
					sig.CommandID, sig.PhaseID, err)
				retained = append(retained, *sig)
				continue
			}

			if sig.Kind == "awaiting_fill" && phaseStatus != model.PhaseStatusAwaitingFill {
				qh.log(LogLevelInfo, "signal_stale_removed kind=%s command=%s phase=%s current_status=%s",
					sig.Kind, sig.CommandID, sig.PhaseID, phaseStatus)
				*dirty = true
				continue
			}

			if sig.Kind == "fill_timeout" && phaseStatus != model.PhaseStatusTimedOut {
				qh.log(LogLevelInfo, "signal_stale_removed kind=%s command=%s phase=%s current_status=%s",
					sig.Kind, sig.CommandID, sig.PhaseID, phaseStatus)
				*dirty = true
				continue
			}
		} else if qh.dependencyResolver.HasStateReader() && sig.PhaseID == "" {
			// Command-level signals (e.g. circuit_breaker_tripped): check command existence only
			_, err := qh.dependencyResolver.GetStateReader().GetCommandPhases(sig.CommandID)
			if err != nil {
				if errors.Is(err, ErrStateNotFound) || os.IsNotExist(err) {
					qh.log(LogLevelInfo, "signal_orphaned_removed kind=%s command=%s (command not found)",
						sig.Kind, sig.CommandID)
					*dirty = true
					continue
				}
				qh.log(LogLevelWarn, "signal_command_check command=%s error=%v",
					sig.CommandID, err)
				retained = append(retained, *sig)
				continue
			}
		}

		// Defer delivery to Phase B
		work.signals = append(work.signals, signalDeliveryItem{
			CommandID: sig.CommandID,
			PhaseID:   sig.PhaseID,
			Kind:      sig.Kind,
			Message:   sig.Message,
		})
		retained = append(retained, *sig)
	}

	sq.Signals = retained
}

// signalKey is the deduplication key for PlannerSignal.
// WorkerID is always part of the key so that per-worker signals (commit_failed,
// merge_conflict, and the conflict-resolution kinds) each retain a distinct
// entry. Phase-level signals (PhaseID set, WorkerID empty) continue to dedup
// the same way they did before because their WorkerID field is empty.
type signalKey struct {
	CommandID          string
	PhaseID            string
	Kind               string
	WorkerID           string
	ConflictGeneration string
}

// signalDedupKey returns the worker-scoped dedup key for a signal.
// ConflictGeneration is included so that a re-detected merge conflict against
// a different integration HEAD or worker SHA registers as a fresh signal
// instead of being suppressed by the previous (now-stale) entry. Non-resolver
// signals leave ConflictGeneration empty, preserving the prior dedup behavior.
func signalDedupKey(s model.PlannerSignal) signalKey {
	return signalKey{
		CommandID:          s.CommandID,
		PhaseID:            s.PhaseID,
		Kind:               s.Kind,
		WorkerID:           s.WorkerID,
		ConflictGeneration: s.ConflictGeneration,
	}
}

// buildSignalIndex builds a lookup index from the current signals slice for O(1) dedup.
func buildSignalIndex(signals []model.PlannerSignal) map[signalKey]struct{} {
	idx := make(map[signalKey]struct{}, len(signals))
	for _, s := range signals {
		idx[signalDedupKey(s)] = struct{}{}
	}
	return idx
}

// upsertPlannerSignal adds a signal or skips if one already exists for the same key.
// Uses signalIndex for O(1) lookup; caller must pass the index built via buildSignalIndex.
func (qh *QueueHandler) upsertPlannerSignal(sq *model.PlannerSignalQueue, dirty *bool, sig model.PlannerSignal, signalIndex map[signalKey]struct{}) {
	key := signalDedupKey(sig)
	if _, exists := signalIndex[key]; exists {
		qh.log(LogLevelDebug, "planner_signal_dedup kind=%s command=%s phase=%s",
			sig.Kind, sig.CommandID, sig.PhaseID)
		return
	}
	if sq.SchemaVersion == 0 {
		sq.SchemaVersion = 1
		sq.FileType = "planner_signal_queue"
	}
	sq.Signals = append(sq.Signals, sig)
	signalIndex[key] = struct{}{}
	*dirty = true
}

// deliverPlannerSignal attempts delivery to the planner using the shared executor.
func (qh *QueueHandler) deliverPlannerSignal(ctx context.Context, commandID, message string) error {
	exec, err := qh.execProvider.GetExecutor()
	if err != nil {
		return fmt.Errorf("get executor: %w", err)
	}

	result := exec.Execute(agent.ExecRequest{
		Context:   ctx,
		AgentID:   "planner",
		Message:   message,
		Mode:      agent.ModeDeliver,
		CommandID: commandID,
	})
	if result.Error != nil {
		return result.Error
	}
	return nil
}

// computeSignalBackoff returns the backoff duration for the given attempt count
// with ±25% uniform jitter to prevent thundering herd on recovery.
func (qh *QueueHandler) computeSignalBackoff(attempts int) time.Duration {
	baseSec := 5
	maxSec := qh.config.Watcher.ScanIntervalSec
	if maxSec <= 0 {
		maxSec = 10
	}

	if attempts < 1 {
		attempts = 1
	}
	backoffSec := baseSec * (1 << (attempts - 1))
	if backoffSec > maxSec {
		backoffSec = maxSec
	}
	if backoffSec < baseSec {
		backoffSec = baseSec
	}
	base := time.Duration(backoffSec) * time.Second
	jittered := time.Duration(float64(base) * (0.75 + rand.Float64()*0.5)) //nolint:gosec // math/rand is appropriate for jitter
	return jittered
}

// isAgentBusy probes agent busy state via executor.
// Returns (busy, undecided). When undecided=true, busy is false.
func (qh *QueueHandler) isAgentBusy(ctx context.Context, agentID string) (busy, undecided bool) {
	if qh.busyChecker != nil {
		return qh.busyChecker.IsBusy(agentID), false
	}

	// Default: use shared agent executor to probe busy state
	exec, err := qh.execProvider.GetExecutor()
	if err != nil {
		qh.log(LogLevelWarn, "busy_probe_executor_error agent=%s error=%v (treating as undecided)", agentID, err)
		return false, true
	}

	result := exec.Execute(agent.ExecRequest{
		Context: ctx,
		AgentID: agentID,
		Mode:    agent.ModeIsBusy,
	})

	// VerdictUndecided: neither extend nor release; defer to next scan cycle.
	if result.Error != nil && errors.Is(result.Error, agent.ErrBusyUndecided) {
		qh.log(LogLevelInfo, "busy_probe_undecided agent=%s", agentID)
		return false, true
	}

	return result.Success, false // Success=true means busy
}

// clearAgent sends /clear to the specified agent pane to reset a stuck session.
func (qh *QueueHandler) clearAgent(ctx context.Context, agentID string) {
	exec, err := qh.execProvider.GetExecutor()
	if err != nil {
		qh.log(LogLevelWarn, "clear_agent create_executor error=%v", err)
		return
	}

	result := exec.Execute(agent.ExecRequest{
		Context: ctx,
		AgentID: agentID,
		Mode:    agent.ModeClear,
	})
	if result.Error != nil {
		qh.log(LogLevelWarn, "clear_agent agent=%s error=%v", agentID, result.Error)
	} else {
		qh.log(LogLevelInfo, "clear_agent agent=%s success", agentID)
	}
}
