package daemon

import (
	"fmt"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
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

	if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
		return internalErrorf("write queue: %v", err)
	}
	h.notifySelfWrite(queuePath, "command", cq)

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
	resp := checkFileSizeLimit(h.config.Limits.MaxYAMLFileBytes, len(data), estimatedAddition)
	if resp == nil {
		return nil
	}
	archived := archiveTerminalCommands(cq)
	if archived == 0 {
		return resp
	}
	newData, err := yamlv3.Marshal(*cq)
	if err != nil {
		return internalErrorf("marshal queue after archive: %v", err)
	}
	if checkFileSizeLimit(h.config.Limits.MaxYAMLFileBytes, len(newData), estimatedAddition) != nil {
		return resp
	}
	h.logFn(LogLevelInfo, "queue_write archive_commands archived=%d", archived)
	return nil
}

// buildCommandEntry constructs a new model.Command from the given params.
func buildCommandEntry(params QueueWriteParams, clock Clock) (model.Command, error) {
	id, err := model.GenerateID(model.IDTypeCommand)
	if err != nil {
		return model.Command{}, err
	}

	priority := params.Priority
	if priority == 0 {
		priority = model.DefaultPriority
	}

	now := clock.Now().UTC().Format(time.RFC3339)
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
