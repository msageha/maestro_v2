package daemon

import (
	"fmt"

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

	return h.withQueueLocks([]string{"queue:planner"}, func() *uds.Response {
		return h.executeCommandWrite(params)
	})
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
