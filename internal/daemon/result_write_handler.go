package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// sanitizeContentForLog truncates a string to maxLen and replaces control
// characters with '?' to prevent log injection when including untrusted
// content values (task content, skill candidates, summaries) in log messages.
func sanitizeContentForLog(s string) string {
	const maxLen = 200
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune('?')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fallbackRecorder records worker success/failure for health monitoring.
type fallbackRecorder interface {
	RecordSuccess(workerID string)
	RecordFailure(workerID string)
}

// circuitBreakerUpdater updates circuit breaker counters on result.
type circuitBreakerUpdater interface {
	UpdateCounterOnResult(state *model.CommandState, resultStatus model.Status, taskID string, resultID string, now time.Time) (bool, string)
	TripBreaker(state *model.CommandState, reason string, now time.Time)
}

// reviewDispatcher dispatches review requests for completed tasks.
type reviewDispatcher interface {
	Enabled() bool
	DispatchIfEligible(ctx context.Context, params ResultWriteParams)
}

// ResultWriteAPI handles the "result_write" UDS endpoint.
type ResultWriteAPI struct {
	*apiContext
	// Domain-specific deps (late-bound via closures to support test wiring
	// where Daemon fields are set after newDaemon returns).
	fallbackMgr    func() fallbackRecorder
	circuitBreaker func() circuitBreakerUpdater
	reviewCoord    func() reviewDispatcher
	triggerScan    scanTriggerFunc
	ctx            func() context.Context
}

// ResultWriteParams is the request payload for the result_write UDS command.
type ResultWriteParams struct {
	Reporter               string   `json:"reporter"`
	TaskID                 string   `json:"task_id"`
	CommandID              string   `json:"command_id"`
	LeaseEpoch             int      `json:"lease_epoch"`
	Status                 string   `json:"status"`
	Summary                string   `json:"summary"`
	FilesChanged           []string `json:"files_changed,omitempty"`
	PartialChangesPossible bool     `json:"partial_changes_possible,omitempty"`
	RetrySafe              bool     `json:"retry_safe,omitempty"`
	ExitCode               *int     `json:"exit_code,omitempty"`
	Learnings              []string `json:"learnings,omitempty"`
	SkillCandidates        []string `json:"skill_candidates,omitempty"`
}

func (h *ResultWriteAPI) handleResultWrite(req *uds.Request) *uds.Response {
	var params ResultWriteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	if params.Reporter == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "reporter is required")
	}
	if !validate.IsValidBaseName(params.Reporter) {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid reporter: %q", params.Reporter))
	}
	if params.TaskID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "task_id is required")
	}
	if params.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required")
	}
	if err := validate.ID(params.CommandID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid command_id: %v", err))
	}
	if err := validate.ID(params.TaskID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid task_id: %v", err))
	}

	resultStatus := model.Status(params.Status)
	switch resultStatus {
	case model.StatusCompleted, model.StatusFailed:
		// valid terminal statuses for worker result reporting
	default:
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("status must be completed|failed, got %q", params.Status))
	}

	// Phase A: Shared file lock + per-worker mutex (results/ + queue/ updates)
	resultWritePhaseAResult, err := h.resultWritePhaseA(params, resultStatus)
	if err != nil {
		rErr := &resultWriteError{}
		if errors.As(err, &rErr) {
			return uds.ErrorResponse(rErr.Code, rErr.Message)
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}
	resultID := resultWritePhaseAResult.resultID

	// Phase B: Per-command mutex (state/ updates)
	if err := h.resultWritePhaseB(params, resultID, resultStatus, resultWritePhaseAResult.queueWriteFailed); err != nil {
		h.logFn(LogLevelError, "result_write phase_b error task=%s command=%s: %v",
			params.TaskID, params.CommandID, err)
		return uds.ErrorResponse(uds.ErrCodeInternal,
			fmt.Sprintf("state update failed: %v (result %s committed, run 'maestro plan rebuild' to fix)", err, resultID))
	}

	// Fallback tracking: record success/failure for worker health monitoring.
	if fm := h.fallbackMgr(); fm != nil {
		switch resultStatus {
		case model.StatusCompleted:
			fm.RecordSuccess(params.Reporter)
		case model.StatusFailed:
			fm.RecordFailure(params.Reporter)
		}
	}

	// Retry registration (state then queue — correct lock order).
	h.handleRetryRegistration(resultWritePhaseAResult, params)

	// Best-effort writes (learnings, skill candidates) with lease epoch guard.
	rejectionID := h.handleBestEffortWrites(params, resultID, resultStatus)

	// Phase C: Trigger scan (best effort dependency unblocking).
	if h.triggerScan != nil {
		h.triggerScan(h.ctx())
	}

	h.logFn(LogLevelInfo, "result_write result_id=%s task=%s command=%s status=%s reporter=%s",
		resultID, params.TaskID, params.CommandID, params.Status, params.Reporter)
	respPayload := map[string]string{"result_id": resultID}
	if rejectionID != "" {
		respPayload["lease_rejection_id"] = rejectionID
		respPayload["lease_rejection_warning"] =
			"learnings/skill_candidates rejected: lease revoked; recorded as " + rejectionID
	}
	return uds.SuccessResponse(respPayload)
}

type resultWriteError struct {
	Code    string
	Message string
}

func (e *resultWriteError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// handleRetryRegistration registers a retry task in state and queue if phaseA
// determined one is needed. Runs after phaseA has released queue+result locks,
// so acquiring state(L2) then queue(L1) does not violate canonical order.
func (h *ResultWriteAPI) handleRetryRegistration(phaseAResult *resultWritePhaseAResult, params ResultWriteParams) {
	if phaseAResult.retryTask == nil {
		return
	}

	retryTask := phaseAResult.retryTask
	retryHandler := NewTaskRetryHandler(h.maestroDir, *h.config, h.lockMap, h.logger, h.logLevel)

	// First register in state (acquires state lock)
	if err := retryHandler.RegisterRetryTaskInState(retryTask, params.CommandID); err != nil {
		h.logFn(LogLevelError, "register_retry_task_failed task=%s command=%s error=%v", retryTask.ID, params.CommandID, err)
		return
	}

	// Then add to queue (acquires queue lock independently)
	if err := retryHandler.AddRetryTaskToQueue(retryTask, params.Reporter); err != nil {
		// M2 note: A compensating delete on the state entry was considered
		// but is unsafe. AddRetryTaskToQueue ultimately calls AtomicWrite,
		// which can return an error after os.Rename has already committed
		// the new queue file (e.g. when the post-rename syncDir() fails).
		// In that case the queue would already contain the retry task and
		// rolling back the state entry would orphan the queue entry. Since
		// the daemon cannot distinguish a pre-rename failure from a
		// post-rename failure, we leave the state entry in place and mark
		// it as RetryEnqueueFailed so the R1 reconciler can either
		// re-enqueue it or transition it to dead_letter.
		h.logFn(LogLevelError, "add_retry_task_failed task=%s worker=%s command=%s error=%v "+
			"(task registered in state but enqueue failed; R1 reconciler will re-enqueue or mark failed)",
			retryTask.ID, params.Reporter, params.CommandID, err)
		if markErr := retryHandler.MarkRetryEnqueueFailed(retryTask.ID, params.Reporter, params.CommandID); markErr != nil {
			h.logFn(LogLevelError, "mark_retry_enqueue_failed task=%s command=%s error=%v",
				retryTask.ID, params.CommandID, markErr)
		}
	} else {
		h.logFn(LogLevelInfo, "task_retry_scheduled task=%s retry_id=%s attempt=%d",
			params.TaskID, retryTask.ID, retryTask.Attempts)
	}
}

// truncateRunes truncates a string to at most maxRunes runes.
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes])
}
