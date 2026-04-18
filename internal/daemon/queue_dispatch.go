package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
)

// stepPlannerSignalsDeferred evaluates signals but defers tmux delivery to Phase B.
// commandQueue is used to suppress stale publish_completed signals for commands
// that have already reached a terminal status (e.g. plan complete was called
// between signal creation and the current evaluation).
func (qh *QueueHandler) stepPlannerSignalsDeferred(sq *model.PlannerSignalQueue, dirty *bool, work *deferredWork, commandQueue model.CommandQueue) {
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

		if qh.dependencyResolver.HasStateReader() && sig.PhaseID != "" && !strings.HasPrefix(sig.PhaseID, "__") {
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
		} else if qh.dependencyResolver.HasStateReader() && (sig.PhaseID == "" || strings.HasPrefix(sig.PhaseID, "__")) {
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

			// Suppress publish_completed signals for commands that are already
			// terminal. This closes the race window where plan complete is called
			// between Phase C signal creation and Phase A signal evaluation,
			// preventing the Planner from issuing a redundant second plan complete.
			if sig.Kind == "publish_completed" && isCommandTerminalInQueue(commandQueue, sig.CommandID) {
				qh.log(LogLevelInfo, "signal_stale_removed kind=%s command=%s (command already terminal)",
					sig.Kind, sig.CommandID)
				*dirty = true
				continue
			}

			// Suppress publish_failed signals — the Daemon handles publish
			// failure retries automatically (recordPublishFailure / backoff).
			// R8 (NotifyPublishQuarantined) escalates to the Planner only
			// when retries are exhausted. Delivering publish_failed to the
			// Planner causes it to attempt plan_submit / add_retry_task
			// which fails because the Worker task completed successfully.
			if sig.Kind == "publish_failed" {
				qh.log(LogLevelInfo, "signal_suppressed kind=%s command=%s (daemon handles publish retry)",
					sig.Kind, sig.CommandID)
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

// deliverPlannerSignal attempts delivery to the planner with inline retries.
// On transient failure (e.g., planner busy), retries up to SignalInlineRetries
// times with a short delay between attempts, each bounded by SignalDeliveryTimeoutSec.
// This avoids the full scan-cycle delay for the common case where the planner is
// only briefly busy.
func (qh *QueueHandler) deliverPlannerSignal(ctx context.Context, commandID, message string) error {
	maxRetries := qh.config.Retry.EffectiveSignalInlineRetries()
	retryDelay := time.Duration(qh.config.Retry.EffectiveSignalInlineRetryDelaySec()) * time.Second
	attemptTimeout := time.Duration(qh.config.Retry.EffectiveSignalDeliveryTimeoutSec()) * time.Second

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			qh.log(LogLevelInfo, "signal_inline_retry attempt=%d/%d command=%s error=%v",
				attempt+1, maxRetries+1, commandID, lastErr)
			select {
			case <-ctx.Done():
				return fmt.Errorf("signal delivery cancelled: %w", ctx.Err())
			case <-time.After(retryDelay):
			}
		}

		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err := qh.deliverPlannerSignalOnce(attemptCtx, commandID, message)
		cancel()

		if err == nil {
			if attempt > 0 {
				qh.log(LogLevelInfo, "signal_inline_retry_success command=%s total_attempts=%d", commandID, attempt+1)
				qh.scanExecutor.scanCounters.SignalInlineRetrySuccesses++
			}
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// deliverPlannerSignalOnce performs a single delivery attempt using the shared executor.
func (qh *QueueHandler) deliverPlannerSignalOnce(ctx context.Context, commandID, message string) error {
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
// Uses a shorter base (2s) for the first 3 attempts to recover faster from
// transient planner-busy failures, then reverts to the standard 5s base.
func (qh *QueueHandler) computeSignalBackoff(attempts int) time.Duration {
	baseSec := 5
	if attempts <= 3 {
		baseSec = 2
	}
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
	if qh.scanExecutor.busyChecker != nil {
		busy := qh.scanExecutor.busyChecker.IsBusy(agentID)
		if qh.undecidedTracker != nil {
			qh.undecidedTracker.Reset(agentID)
		}
		return busy, false
	}

	exec, err := qh.execProvider.GetExecutor()
	if err != nil {
		qh.log(LogLevelWarn, "busy_probe_executor_error agent=%s error=%v (treating as undecided)", agentID, err)
		count := 0
		if qh.undecidedTracker != nil {
			count = qh.undecidedTracker.Increment(agentID)
		}
		if count >= undecidedWarnThreshold {
			qh.log(LogLevelWarn, "busy_probe_undecided_consecutive agent=%s count=%d scheduling_health_check", agentID, count)
		}
		return false, true
	}

	result := exec.Execute(agent.ExecRequest{
		Context: ctx,
		AgentID: agentID,
		Mode:    agent.ModeIsBusy,
	})

	if result.Error != nil && errors.Is(result.Error, agent.ErrBusyUndecided) {
		count := 0
		if qh.undecidedTracker != nil {
			count = qh.undecidedTracker.Increment(agentID)
		}
		if count >= undecidedPromoteThreshold {
			// Sustained undecided across multiple scan cycles: promote to idle.
			// The agent is very likely idle with stale busy-pattern output.
			qh.log(LogLevelInfo, "busy_probe_undecided_promote_idle agent=%s count=%d", agentID, count)
			if qh.undecidedTracker != nil {
				qh.undecidedTracker.Reset(agentID)
			}
			return false, false
		}
		if count >= undecidedWarnThreshold {
			qh.log(LogLevelWarn, "busy_probe_undecided_consecutive agent=%s count=%d scheduling_health_check", agentID, count)
		}
		qh.log(LogLevelInfo, "busy_probe_undecided agent=%s consecutive=%d", agentID, count)
		return false, true
	}

	if qh.undecidedTracker != nil {
		qh.undecidedTracker.Reset(agentID)
	}
	return result.Success, false
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

// isTaskDispatchCancelled checks whether a task's command has been
// cancel-requested, using both the Phase A deferred work set and the
// CancelHandler's in-memory cache (which catches cancel requests that
// arrived after Phase A collected dispatch items).
func (qh *QueueHandler) isTaskDispatchCancelled(item dispatchItem, pa *phaseAResult) bool {
	if item.Task == nil {
		return false
	}
	cmdID := item.Task.CommandID
	if pa.work.cancelledCommandIDs != nil {
		if _, ok := pa.work.cancelledCommandIDs[cmdID]; ok {
			return true
		}
	}
	return qh.cancelHandler.IsDispatchBlocked(cmdID)
}

// undecidedTracker tracks consecutive undecided busy-probe results per agent.
// When the count exceeds the threshold, a warning is logged and a health
// check (/clear) is scheduled for the next scan cycle.
type undecidedTracker struct {
	mu     sync.Mutex
	counts map[string]int
}

const undecidedWarnThreshold = 3

// undecidedPromoteThreshold is the number of consecutive undecided results
// after which the agent is treated as idle. Sustained undecided across
// multiple scan cycles (each with its own probe) strongly suggests the agent
// is idle with stale busy-pattern output in the pane.
const undecidedPromoteThreshold = 5

func newUndecidedTracker() *undecidedTracker {
	return &undecidedTracker{counts: make(map[string]int)}
}

// Increment records an undecided result and returns the new consecutive count.
func (t *undecidedTracker) Increment(agentID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[agentID]++
	return t.counts[agentID]
}

// Reset clears the consecutive undecided count for an agent.
func (t *undecidedTracker) Reset(agentID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.counts, agentID)
}

// Count returns the current consecutive undecided count for an agent.
func (t *undecidedTracker) Count(agentID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[agentID]
}
