package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

func (h *QueueWriteAPI) handleQueueWriteCommand(params QueueWriteParams) *uds.Response {
	if resp := validateRequired(params.Content, "content", "command"); resp != nil {
		return resp
	}
	if resp := validateContentSize(params.Content, h.config.Limits.MaxEntryContentBytes); resp != nil {
		return resp
	}
	if resp := validatePersonaAndSkillRefs(params.PersonaHint, params.SkillRefs); resp != nil {
		return resp
	}
	if resp := h.checkContinuousGate(); resp != nil {
		return resp
	}

	return h.withQueueLocks([]string{"queue:planner"}, func() *uds.Response {
		return h.executeCommandWrite(params)
	})
}

// checkContinuousGate rejects command writes when continuous mode is enabled in config
// but the current state is paused/stopped. This provides a hard enforcement layer
// complementing the orchestrator.md pre-generation gate: even if the Orchestrator
// LLM ignores its instructions, the daemon will refuse to enqueue auto-generated
// commands while continuous mode is not actively running.
//
// The gate is a no-op when:
//   - continuous.enabled=false (feature not in use)
//   - state file missing (never initialised; treated as disabled)
//   - status == running (normal operation)
func (h *QueueWriteAPI) checkContinuousGate() *uds.Response {
	if h.config == nil || !h.config.Continuous.Enabled {
		return nil
	}
	statePath := filepath.Join(h.maestroDir, "state", "continuous.yaml")
	data, err := os.ReadFile(statePath) //nolint:gosec // path derived from daemon-controlled state directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		h.logFn(LogLevelWarn, "continuous gate: read state failed, allowing write: %v", err)
		return nil
	}
	var state model.Continuous
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		h.logFn(LogLevelWarn, "continuous gate: parse state failed, allowing write: %v", err)
		return nil
	}
	switch state.Status {
	case model.ContinuousStatusPaused:
		reason := ""
		if state.PausedReason != nil {
			reason = *state.PausedReason
		}
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("continuous mode is paused (reason=%q); resume before enqueueing new commands", reason))
	case model.ContinuousStatusStopped:
		return uds.ErrorResponse(uds.ErrCodeValidation,
			"continuous mode is stopped; restart via 'maestro up --continuous' before enqueueing new commands")
	}
	return nil
}

// executeCommandWrite performs the locked portion of command queue write.
func (h *QueueWriteAPI) executeCommandWrite(params QueueWriteParams) *uds.Response {
	queuePath := commandQueuePath(h.maestroDir)
	cq, data, err := loadCommandQueueFile(queuePath)
	if err != nil {
		return internalErrorf("%v", err)
	}

	if resp := checkCommandBackpressure(cq.Commands, h.config.Limits.MaxPendingCommands); resp != nil {
		return resp
	}

	if resp := ensureCommandCapacity(h, &cq, data, len(params.Content)+200); resp != nil {
		return resp
	}

	entry, err := buildCommandEntry(params, h.clock)
	if err != nil {
		return internalErrorf("generate ID: %v", err)
	}
	cq.Commands = append(cq.Commands, entry)

	if resp := h.writeAndNotify(queuePath, "command", cq); resp != nil {
		return resp
	}

	h.logFn(LogLevelInfo, "queue_write type=command id=%s", entry.ID)
	return uds.SuccessResponse(map[string]string{"id": entry.ID})
}

// checkCommandBackpressure rejects if pending commands reach the configured limit.
func checkCommandBackpressure(commands []model.Command, maxPending int) *uds.Response {
	if maxPending <= 0 {
		return nil
	}
	pending := 0
	for _, cmd := range commands {
		if cmd.Status == model.StatusPending {
			pending++
		}
	}
	if pending >= maxPending {
		return uds.ErrorResponse(uds.ErrCodeBackpressure,
			fmt.Sprintf("pending commands limit reached: %d/%d", pending, maxPending))
	}
	return nil
}

// ensureCommandCapacity checks file size limit and archives terminal commands if needed.
func ensureCommandCapacity(h *QueueWriteAPI, cq *model.CommandQueue, data []byte, estimatedAddition int) *uds.Response {
	return ensureCapacityWithArchive(
		h.config.Limits.MaxYAMLFileBytes, data, estimatedAddition,
		func() int { return archiveTerminalCommands(cq) },
		func() ([]byte, error) { return yamlv3.Marshal(*cq) },
		func(f string, a ...any) { h.logFn(LogLevelInfo, f, a...) },
		"commands",
	)
}

// buildCommandEntry constructs a new model.Command from the given params.
func buildCommandEntry(params QueueWriteParams, clock Clock) (model.Command, error) {
	id, err := model.GenerateID(model.IDTypeCommand)
	if err != nil {
		return model.Command{}, err
	}

	priority := resolvePriority(params.Priority)
	now := formatNowUTC(clock)
	return model.Command{
		ID:        id,
		Content:   params.Content,
		SkillRefs: params.SkillRefs,
		Priority:  priority,
		Status:    model.StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}
