package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
)

// processPlannerSignalsDeferred evaluates signals but defers tmux delivery to Phase B.
func (qh *QueueHandler) processPlannerSignalsDeferred(sq *model.PlannerSignalQueue, dirty *bool, work *deferredWork) {
	now := qh.clock.Now().UTC()
	var retained []model.PlannerSignal

	for i := range sq.Signals {
		sig := &sq.Signals[i]

		if sig.NextAttemptAt != nil {
			nextAt, err := time.Parse(time.RFC3339, *sig.NextAttemptAt)
			if err == nil && nextAt.After(now) {
				retained = append(retained, *sig)
				continue
			}
		}

		if qh.dependencyResolver.stateReader != nil && sig.PhaseID != "" {
			// Phase-level signals: check phase existence (orphan) and staleness
			phaseStatus, err := qh.dependencyResolver.GetPhaseStatus(sig.CommandID, sig.PhaseID)
			if err != nil {
				if errors.Is(err, core.ErrPhaseNotFound) || errors.Is(err, core.ErrStateNotFound) || os.IsNotExist(err) {
					qh.log(core.LogLevelInfo, "signal_orphaned_removed kind=%s command=%s phase=%s error=%v",
						sig.Kind, sig.CommandID, sig.PhaseID, err)
					*dirty = true
					continue
				}
				qh.log(core.LogLevelWarn, "signal_phase_check command=%s phase=%s error=%v",
					sig.CommandID, sig.PhaseID, err)
				retained = append(retained, *sig)
				continue
			}

			if sig.Kind == "awaiting_fill" && phaseStatus != model.PhaseStatusAwaitingFill {
				qh.log(core.LogLevelInfo, "signal_stale_removed kind=%s command=%s phase=%s current_status=%s",
					sig.Kind, sig.CommandID, sig.PhaseID, phaseStatus)
				*dirty = true
				continue
			}

			if sig.Kind == "fill_timeout" && phaseStatus != model.PhaseStatusTimedOut {
				qh.log(core.LogLevelInfo, "signal_stale_removed kind=%s command=%s phase=%s current_status=%s",
					sig.Kind, sig.CommandID, sig.PhaseID, phaseStatus)
				*dirty = true
				continue
			}
		} else if qh.dependencyResolver.stateReader != nil && sig.PhaseID == "" {
			// Command-level signals (e.g. circuit_breaker_tripped): check command existence only
			_, err := qh.dependencyResolver.stateReader.GetCommandPhases(sig.CommandID)
			if err != nil {
				if errors.Is(err, core.ErrStateNotFound) || os.IsNotExist(err) {
					qh.log(core.LogLevelInfo, "signal_orphaned_removed kind=%s command=%s (command not found)",
						sig.Kind, sig.CommandID)
					*dirty = true
					continue
				}
				qh.log(core.LogLevelWarn, "signal_command_check command=%s error=%v",
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
type signalKey struct {
	CommandID string
	PhaseID   string
	Kind      string
}

// buildSignalIndex builds a lookup index from the current signals slice for O(1) dedup.
func buildSignalIndex(signals []model.PlannerSignal) map[signalKey]struct{} {
	idx := make(map[signalKey]struct{}, len(signals))
	for _, s := range signals {
		idx[signalKey{CommandID: s.CommandID, PhaseID: s.PhaseID, Kind: s.Kind}] = struct{}{}
	}
	return idx
}

// upsertPlannerSignal adds a signal or skips if one already exists for the same key.
// Uses signalIndex for O(1) lookup; caller must pass the index built via buildSignalIndex.
func (qh *QueueHandler) upsertPlannerSignal(sq *model.PlannerSignalQueue, dirty *bool, sig model.PlannerSignal, signalIndex map[signalKey]struct{}) {
	key := signalKey{CommandID: sig.CommandID, PhaseID: sig.PhaseID, Kind: sig.Kind}
	if _, exists := signalIndex[key]; exists {
		qh.log(core.LogLevelDebug, "planner_signal_dedup kind=%s command=%s phase=%s",
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
	exec, err := qh.dispatcher.getExecutor()
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
	jittered := time.Duration(float64(base) * (0.75 + rand.Float64()*0.5))
	return jittered
}

// isAgentBusy probes agent busy state via executor.
// Returns (busy, undecided). When undecided=true, busy is false.
func (qh *QueueHandler) isAgentBusy(ctx context.Context, agentID string) (busy, undecided bool) {
	if qh.busyChecker != nil {
		return qh.busyChecker(agentID), false
	}

	// Default: use shared agent executor to probe busy state
	exec, err := qh.dispatcher.getExecutor()
	if err != nil {
		qh.log(core.LogLevelWarn, "busy_probe_executor_error agent=%s error=%v (treating as undecided)", agentID, err)
		return false, true
	}

	result := exec.Execute(agent.ExecRequest{
		Context: ctx,
		AgentID: agentID,
		Mode:    agent.ModeIsBusy,
	})

	// VerdictUndecided: neither extend nor release; defer to next scan cycle.
	if result.Error != nil && errors.Is(result.Error, agent.ErrBusyUndecided) {
		qh.log(core.LogLevelInfo, "busy_probe_undecided agent=%s", agentID)
		return false, true
	}

	return result.Success, false // Success=true means busy
}

// clearAgent sends /clear to the specified agent pane to reset a stuck session.
func (qh *QueueHandler) clearAgent(ctx context.Context, agentID string) {
	exec, err := qh.dispatcher.getExecutor()
	if err != nil {
		qh.log(core.LogLevelWarn, "clear_agent create_executor error=%v", err)
		return
	}

	result := exec.Execute(agent.ExecRequest{
		Context: ctx,
		AgentID: agentID,
		Mode:    agent.ModeClear,
	})
	if result.Error != nil {
		qh.log(core.LogLevelWarn, "clear_agent agent=%s error=%v", agentID, result.Error)
	} else {
		qh.log(core.LogLevelInfo, "clear_agent agent=%s success", agentID)
	}
}
