package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
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

		if qh.dependencyResolver.stateReader != nil {
			phaseStatus, err := qh.dependencyResolver.GetPhaseStatus(sig.CommandID, sig.PhaseID)
			if err != nil {
				errMsg := err.Error()
				if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "not exist") || os.IsNotExist(err) {
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

// upsertPlannerSignal adds a signal or skips if one already exists for the same key.
func (qh *QueueHandler) upsertPlannerSignal(sq *model.PlannerSignalQueue, dirty *bool, sig model.PlannerSignal) {
	for _, existing := range sq.Signals {
		if existing.CommandID == sig.CommandID &&
			existing.PhaseID == sig.PhaseID &&
			existing.Kind == sig.Kind {
			qh.log(LogLevelDebug, "planner_signal_dedup kind=%s command=%s phase=%s",
				sig.Kind, sig.CommandID, sig.PhaseID)
			return
		}
	}
	if sq.SchemaVersion == 0 {
		sq.SchemaVersion = 1
		sq.FileType = "planner_signal_queue"
	}
	sq.Signals = append(sq.Signals, sig)
	*dirty = true
}

// deliverPlannerSignal attempts delivery to the planner with a short-probe config.
func (qh *QueueHandler) deliverPlannerSignal(ctx context.Context, commandID, message string) error {
	shortCfg := qh.config.Watcher
	shortCfg.BusyCheckMaxRetries = 1
	shortCfg.BusyCheckInterval = 1
	shortCfg.IdleStableSec = 1

	exec, err := qh.dispatcher.executorFactory(qh.maestroDir, shortCfg, qh.config.Logging.Level)
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}
	defer func() { _ = exec.Close() }()

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

// computeSignalBackoff returns the backoff duration for the given attempt count.
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
	return time.Duration(backoffSec) * time.Second
}

// isAgentBusy probes agent busy state via executor. Returns false if no checker is set.
func (qh *QueueHandler) isAgentBusy(ctx context.Context, agentID string) bool {
	if qh.busyChecker != nil {
		return qh.busyChecker(agentID)
	}

	// Default: use shared agent executor to probe busy state
	exec, err := qh.dispatcher.getExecutor()
	if err != nil {
		qh.log(LogLevelWarn, "busy_probe_failed agent=%s error=%v", agentID, err)
		return false
	}

	result := exec.Execute(agent.ExecRequest{
		Context: ctx,
		AgentID: agentID,
		Mode:    agent.ModeIsBusy,
	})

	return result.Success // Success=true means busy
}

// clearAgent sends /clear to the specified agent pane to reset a stuck session.
func (qh *QueueHandler) clearAgent(ctx context.Context, agentID string) {
	exec, err := qh.dispatcher.getExecutor()
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
