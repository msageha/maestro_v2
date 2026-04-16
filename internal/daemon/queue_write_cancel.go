package daemon

import (
	"fmt"
	"os"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// resolveRequestedBy returns val if non-empty, otherwise returns defaultVal.
func resolveRequestedBy(val, defaultVal string) string {
	if val == "" {
		return defaultVal
	}
	return val
}

// handleQueueWriteCancelRequest processes cancel-request queue writes.
//
// H7 (cancel route unification): this handler is the SINGLE canonical entry
// point for command cancellation. Both `maestro plan request-cancel` (the
// recommended CLI surface) and the deprecated `maestro queue write planner
// --type cancel-request` route reach this function. There is no separate
// state mutation path that bypasses this handler. The deprecation warning
// is emitted at the CLI layer (cmd_queue.go) so operators migrate, but the
// behavior is identical to keep both routes consistent.
//
// Cancel-request is exempt from backpressure checks because it modifies
// existing state (e.g. setting cancel.requested on a submitted command, or
// marking an unsubmitted planner entry as cancelled) rather than adding a
// new entry to the queue. Applying backpressure here would prevent users
// from cancelling commands when the system is already overloaded, which is
// the exact scenario where cancellation is most needed.
func (h *QueueWriteAPI) handleQueueWriteCancelRequest(params QueueWriteParams) *uds.Response {
	if resp := validateRequired(params.CommandID, "command_id", "cancel-request"); resp != nil {
		return resp
	}
	if err := validate.ID(params.CommandID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid command_id: %v", err))
	}

	// Check if state exists (submitted command)
	statePath := commandStatePath(h.maestroDir, params.CommandID)
	if _, err := os.Stat(statePath); err == nil {
		return h.cancelRequestSubmitted(params, statePath)
	}

	// Un-submitted: cancel in planner queue
	return h.cancelRequestUnsubmitted(params)
}

// cancelRequestSubmitted handles cancel-request for already-submitted commands.
// Sets cancel.requested=true on the state file (idempotent).
// Lock order: scanMu.RLock → queue:planner → state:{cmd} (consistent with PeriodicScan).
func (h *QueueWriteAPI) cancelRequestSubmitted(params QueueWriteParams, statePath string) *uds.Response {
	return h.withQueueLocks([]string{"queue:planner", "state:" + params.CommandID}, func() *uds.Response {
		return h.executeCancelSubmitted(params, statePath)
	})
}

// executeCancelSubmitted performs the locked portion of submitted cancel-request.
func (h *QueueWriteAPI) executeCancelSubmitted(params QueueWriteParams, statePath string) *uds.Response {
	var alreadyRequested bool
	now := formatNowUTC(h.clock)
	requestedBy := resolveRequestedBy(params.RequestedBy, "orchestrator")

	if err := updateYAMLFile(statePath, func(state *model.CommandState) error {
		if state.Cancel.Requested {
			alreadyRequested = true
			return errNoUpdate
		}
		state.Cancel.Requested = true
		state.Cancel.RequestedAt = &now
		state.Cancel.RequestedBy = &requestedBy
		state.Cancel.Reason = &params.Reason
		state.UpdatedAt = now
		return nil
	}); err != nil {
		return internalErrorf("update state: %v", err)
	}

	if alreadyRequested {
		h.logFn(LogLevelInfo, "queue_write type=cancel-request command=%s already_requested", params.CommandID)
		return uds.SuccessResponse(map[string]string{"command_id": params.CommandID, "status": "already_requested"})
	}

	// Also update queue/planner.yaml cancel metadata (already under fileMu)
	queuePath := commandQueuePath(h.maestroDir)
	cq, _, err := loadCommandQueueFile(queuePath)
	if err == nil {
		for i := range cq.Commands {
			if cq.Commands[i].ID == params.CommandID {
				if !model.IsTerminal(cq.Commands[i].Status) {
					cq.Commands[i].CancelReason = &params.Reason
					cq.Commands[i].CancelRequestedAt = &now
					cq.Commands[i].CancelRequestedBy = &requestedBy
					cq.Commands[i].UpdatedAt = now
					if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
						h.logFn(LogLevelError, "queue_write cancel_planner_queue_update error=%v", err)
					} else {
						h.notifySelfWrite(queuePath, "cancel-request", cq)
					}
				}
				break
			}
		}
	}

	h.logFn(LogLevelInfo, "queue_write type=cancel-request command=%s submitted=true", params.CommandID)
	return uds.SuccessResponse(map[string]string{"command_id": params.CommandID, "status": "cancel_requested"})
}

// cancelRequestUnsubmitted handles cancel-request for un-submitted commands.
// Directly cancels the command in queue/planner.yaml.
func (h *QueueWriteAPI) cancelRequestUnsubmitted(params QueueWriteParams) *uds.Response {
	return h.withQueueLocks([]string{"queue:planner"}, func() *uds.Response {
		return h.executeCancelUnsubmitted(params)
	})
}

// executeCancelUnsubmitted performs the locked portion of unsubmitted cancel-request.
func (h *QueueWriteAPI) executeCancelUnsubmitted(params QueueWriteParams) *uds.Response {
	queuePath := commandQueuePath(h.maestroDir)
	cq, _, err := loadCommandQueueFile(queuePath)
	if err != nil {
		return internalErrorf("%v", err)
	}

	now := formatNowUTC(h.clock)
	requestedBy := resolveRequestedBy(params.RequestedBy, "orchestrator")
	found := false

	for i := range cq.Commands {
		if cq.Commands[i].ID == params.CommandID {
			// Terminal guard: already terminal → skip
			if model.IsTerminal(cq.Commands[i].Status) {
				h.logFn(LogLevelInfo, "queue_write type=cancel-request command=%s already_terminal=%s", params.CommandID, cq.Commands[i].Status)
				return uds.SuccessResponse(map[string]string{"command_id": params.CommandID, "status": string(cq.Commands[i].Status)})
			}
			cq.Commands[i].Status = model.StatusCancelled
			cq.Commands[i].CancelReason = &params.Reason
			cq.Commands[i].CancelRequestedAt = &now
			cq.Commands[i].CancelRequestedBy = &requestedBy
			cq.Commands[i].LeaseOwner = nil
			cq.Commands[i].LeaseExpiresAt = nil
			cq.Commands[i].UpdatedAt = now
			found = true
			break
		}
	}

	if !found {
		return uds.ErrorResponse(uds.ErrCodeNotFound,
			fmt.Sprintf("command %s not found in planner queue", params.CommandID))
	}

	if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
		return internalErrorf("write queue: %v", err)
	}
	h.notifySelfWrite(queuePath, "cancel-request", cq)

	h.logFn(LogLevelInfo, "queue_write type=cancel-request command=%s submitted=false cancelled", params.CommandID)
	return uds.SuccessResponse(map[string]string{"command_id": params.CommandID, "status": "cancelled"})
}
