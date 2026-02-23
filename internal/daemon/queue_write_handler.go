package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// QueueWriteParams is the request payload for the queue_write UDS command.
type QueueWriteParams struct {
	Target             string   `json:"target"`                        // "planner", "worker1", "orchestrator"
	Type               string   `json:"type"`                          // "command", "task", "notification", "cancel-request"
	CommandID          string   `json:"command_id,omitempty"`
	Content            string   `json:"content,omitempty"`
	Purpose            string   `json:"purpose,omitempty"`
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty"`
	BloomLevel         int      `json:"bloom_level,omitempty"`
	BlockedBy          []string `json:"blocked_by,omitempty"`
	Constraints        []string `json:"constraints,omitempty"`
	ToolsHint          []string `json:"tools_hint,omitempty"`
	Priority           int      `json:"priority"`
	SourceResultID     string   `json:"source_result_id,omitempty"`
	NotificationType   string   `json:"notification_type,omitempty"`
	Reason             string   `json:"reason,omitempty"`
}

var validNotificationTypes = map[string]bool{
	"command_completed": true,
	"command_failed":    true,
	"command_cancelled": true,
}

func (d *Daemon) handleQueueWrite(req *uds.Request) *uds.Response {
	var params QueueWriteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	switch params.Type {
	case "command":
		return d.handleQueueWriteCommand(params)
	case "task":
		return d.handleQueueWriteTask(params)
	case "notification":
		return d.handleQueueWriteNotification(params)
	case "cancel-request":
		return d.handleQueueWriteCancelRequest(params)
	default:
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("invalid type: %q, must be command|task|notification|cancel-request", params.Type))
	}
}

func (d *Daemon) handleQueueWriteCommand(params QueueWriteParams) *uds.Response {
	if params.Content == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "content is required for command")
	}

	if d.config.Limits.MaxEntryContentBytes > 0 && len(params.Content) > d.config.Limits.MaxEntryContentBytes {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("content exceeds max size: %d > %d bytes", len(params.Content), d.config.Limits.MaxEntryContentBytes))
	}

	d.acquireFileLock()
	defer d.releaseFileLock()

	queuePath := filepath.Join(d.maestroDir, "queue", "planner.yaml")
	cq, data, err := loadCommandQueueFile(queuePath)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}

	// Backpressure: count pending commands only (spec: pending >= limit → reject)
	if d.config.Limits.MaxPendingCommands > 0 {
		pending := 0
		for _, cmd := range cq.Commands {
			if cmd.Status == model.StatusPending {
				pending++
			}
		}
		if pending >= d.config.Limits.MaxPendingCommands {
			return uds.ErrorResponse(uds.ErrCodeBackpressure,
				fmt.Sprintf("pending commands limit reached: %d/%d", pending, d.config.Limits.MaxPendingCommands))
		}
	}

	if resp := checkFileSizeLimit(d.config.Limits.MaxYAMLFileBytes, len(data), len(params.Content)+200); resp != nil {
		return resp
	}

	id, err := model.GenerateID(model.IDTypeCommand)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("generate ID: %v", err))
	}

	priority := params.Priority
	if priority == 0 {
		priority = 100
	}

	now := time.Now().UTC().Format(time.RFC3339)
	cq.Commands = append(cq.Commands, model.Command{
		ID:        id,
		Content:   params.Content,
		Priority:  priority,
		Status:    model.StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	})

	if err := yamlutil.AtomicWrite(queuePath, cq); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("write queue: %v", err))
	}

	d.log(LogLevelInfo, "queue_write type=command id=%s", id)
	return uds.SuccessResponse(map[string]string{"id": id})
}

func (d *Daemon) handleQueueWriteTask(params QueueWriteParams) *uds.Response {
	if params.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required for task")
	}
	if params.Content == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "content is required for task")
	}
	if params.Purpose == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "purpose is required for task")
	}
	if params.AcceptanceCriteria == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "acceptance_criteria is required for task")
	}
	if params.BloomLevel < 1 || params.BloomLevel > 6 {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("bloom_level must be 1-6, got %d", params.BloomLevel))
	}
	if params.Target == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "target is required for task")
	}

	if d.config.Limits.MaxEntryContentBytes > 0 && len(params.Content) > d.config.Limits.MaxEntryContentBytes {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("content exceeds max size: %d > %d bytes", len(params.Content), d.config.Limits.MaxEntryContentBytes))
	}

	// blocked_by validation: ID format and no duplicates
	if resp := validateBlockedBy(params.BlockedBy); resp != nil {
		return resp
	}

	d.acquireFileLock()
	defer d.releaseFileLock()

	// Sanitize target: prevent directory traversal
	if filepath.Base(params.Target) != params.Target || params.Target == "." || params.Target == ".." {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("invalid target: %q", params.Target))
	}

	queuePath := filepath.Join(d.maestroDir, "queue", params.Target+".yaml")
	tq, data, err := loadTaskQueueFile(queuePath)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}

	// Backpressure: count pending tasks only (spec: pending >= limit → reject)
	if d.config.Limits.MaxPendingTasksPerWorker > 0 {
		pending := 0
		for _, task := range tq.Tasks {
			if task.Status == model.StatusPending {
				pending++
			}
		}
		if pending >= d.config.Limits.MaxPendingTasksPerWorker {
			return uds.ErrorResponse(uds.ErrCodeBackpressure,
				fmt.Sprintf("pending tasks limit for %s reached: %d/%d", params.Target, pending, d.config.Limits.MaxPendingTasksPerWorker))
		}
	}

	if resp := checkFileSizeLimit(d.config.Limits.MaxYAMLFileBytes, len(data), len(params.Content)+500); resp != nil {
		return resp
	}

	id, err := model.GenerateID(model.IDTypeTask)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("generate ID: %v", err))
	}

	priority := params.Priority
	if priority == 0 {
		priority = 100
	}

	now := time.Now().UTC().Format(time.RFC3339)
	tq.Tasks = append(tq.Tasks, model.Task{
		ID:                 id,
		CommandID:          params.CommandID,
		Purpose:            params.Purpose,
		Content:            params.Content,
		AcceptanceCriteria: params.AcceptanceCriteria,
		Constraints:        params.Constraints,
		BlockedBy:          params.BlockedBy,
		BloomLevel:         params.BloomLevel,
		ToolsHint:          params.ToolsHint,
		Priority:           priority,
		Status:             model.StatusPending,
		CreatedAt:          now,
		UpdatedAt:          now,
	})

	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("write queue: %v", err))
	}

	d.log(LogLevelInfo, "queue_write type=task id=%s command_id=%s worker=%s", id, params.CommandID, params.Target)
	return uds.SuccessResponse(map[string]string{"id": id})
}

func (d *Daemon) handleQueueWriteNotification(params QueueWriteParams) *uds.Response {
	if params.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required for notification")
	}
	if params.Content == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "content is required for notification")
	}
	if params.SourceResultID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "source_result_id is required for notification")
	}

	// notification_type validation
	notifType := params.NotificationType
	if notifType == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "notification_type is required for notification")
	}
	if !validNotificationTypes[notifType] {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("invalid notification_type: %q, must be command_completed|command_failed|command_cancelled", notifType))
	}

	if d.config.Limits.MaxEntryContentBytes > 0 && len(params.Content) > d.config.Limits.MaxEntryContentBytes {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("content exceeds max size: %d > %d bytes", len(params.Content), d.config.Limits.MaxEntryContentBytes))
	}

	d.acquireFileLock()
	defer d.releaseFileLock()

	queuePath := filepath.Join(d.maestroDir, "queue", "orchestrator.yaml")
	nq, data, err := loadNotificationQueueFile(queuePath)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}

	// Idempotency: check if source_result_id already exists
	for _, ntf := range nq.Notifications {
		if ntf.SourceResultID == params.SourceResultID {
			d.log(LogLevelInfo, "queue_write type=notification duplicate source_result_id=%s existing_id=%s", params.SourceResultID, ntf.ID)
			return uds.SuccessResponse(map[string]string{"id": ntf.ID, "duplicate": "true"})
		}
	}

	if resp := checkFileSizeLimit(d.config.Limits.MaxYAMLFileBytes, len(data), len(params.Content)+300); resp != nil {
		return resp
	}

	id, err := model.GenerateID(model.IDTypeNotification)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("generate ID: %v", err))
	}

	priority := params.Priority
	if priority == 0 {
		priority = 100
	}

	now := time.Now().UTC().Format(time.RFC3339)
	nq.Notifications = append(nq.Notifications, model.Notification{
		ID:             id,
		CommandID:      params.CommandID,
		Type:           notifType,
		SourceResultID: params.SourceResultID,
		Content:        params.Content,
		Priority:       priority,
		Status:         model.StatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	})

	if err := yamlutil.AtomicWrite(queuePath, nq); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("write queue: %v", err))
	}

	d.log(LogLevelInfo, "queue_write type=notification id=%s command_id=%s source_result_id=%s", id, params.CommandID, params.SourceResultID)
	return uds.SuccessResponse(map[string]string{"id": id})
}

func (d *Daemon) handleQueueWriteCancelRequest(params QueueWriteParams) *uds.Response {
	if params.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required for cancel-request")
	}

	// Check if state exists (submitted command)
	statePath := filepath.Join(d.maestroDir, "state", "commands", params.CommandID+".yaml")
	if _, err := os.Stat(statePath); err == nil {
		return d.cancelRequestSubmitted(params, statePath)
	}

	// Un-submitted: cancel in planner queue
	return d.cancelRequestUnsubmitted(params)
}

// cancelRequestSubmitted handles cancel-request for already-submitted commands.
// Sets cancel.requested=true on the state file (idempotent).
// Lock order: fileMu → lockMap (consistent with PeriodicScan → Reconciler).
func (d *Daemon) cancelRequestSubmitted(params QueueWriteParams, statePath string) *uds.Response {
	// Acquire locks in consistent order: fileMu → lockMap to prevent deadlock.
	d.acquireFileLock()
	defer d.releaseFileLock()

	d.lockMap.Lock("state:" + params.CommandID)
	defer d.lockMap.Unlock("state:" + params.CommandID)

	data, err := os.ReadFile(statePath)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("read state: %v", err))
	}

	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("parse state: %v", err))
	}

	// Idempotent: already requested → skip
	if state.Cancel.Requested {
		d.log(LogLevelInfo, "queue_write type=cancel-request command=%s already_requested", params.CommandID)
		return uds.SuccessResponse(map[string]string{"command_id": params.CommandID, "status": "already_requested"})
	}

	now := time.Now().UTC().Format(time.RFC3339)
	requestedBy := "orchestrator"
	state.Cancel.Requested = true
	state.Cancel.RequestedAt = &now
	state.Cancel.RequestedBy = &requestedBy
	state.Cancel.Reason = &params.Reason
	state.UpdatedAt = now

	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("write state: %v", err))
	}

	// Also update queue/planner.yaml cancel metadata (already under fileMu)
	queuePath := filepath.Join(d.maestroDir, "queue", "planner.yaml")
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
						d.log(LogLevelError, "queue_write cancel_planner_queue_update error=%v", err)
					}
				}
				break
			}
		}
	}

	d.log(LogLevelInfo, "queue_write type=cancel-request command=%s submitted=true", params.CommandID)
	return uds.SuccessResponse(map[string]string{"command_id": params.CommandID, "status": "cancel_requested"})
}

// cancelRequestUnsubmitted handles cancel-request for un-submitted commands.
// Directly cancels the command in queue/planner.yaml.
func (d *Daemon) cancelRequestUnsubmitted(params QueueWriteParams) *uds.Response {
	d.acquireFileLock()
	defer d.releaseFileLock()

	queuePath := filepath.Join(d.maestroDir, "queue", "planner.yaml")
	cq, _, err := loadCommandQueueFile(queuePath)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}

	now := time.Now().UTC().Format(time.RFC3339)
	requestedBy := "orchestrator"
	found := false

	for i := range cq.Commands {
		if cq.Commands[i].ID == params.CommandID {
			// Terminal guard: already terminal → skip
			if model.IsTerminal(cq.Commands[i].Status) {
				d.log(LogLevelInfo, "queue_write type=cancel-request command=%s already_terminal=%s", params.CommandID, cq.Commands[i].Status)
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
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("write queue: %v", err))
	}

	d.log(LogLevelInfo, "queue_write type=cancel-request command=%s submitted=false cancelled", params.CommandID)
	return uds.SuccessResponse(map[string]string{"command_id": params.CommandID, "status": "cancelled"})
}

// --- Validation helpers ---

func validateBlockedBy(blockedBy []string) *uds.Response {
	seen := make(map[string]bool)
	for _, id := range blockedBy {
		if !model.ValidateID(id) {
			return uds.ErrorResponse(uds.ErrCodeValidation,
				fmt.Sprintf("invalid blocked_by ID format: %q", id))
		}
		idType, _ := model.ParseIDType(id)
		if idType != model.IDTypeTask {
			return uds.ErrorResponse(uds.ErrCodeValidation,
				fmt.Sprintf("blocked_by must reference task IDs, got %q (type: %s)", id, idType))
		}
		if seen[id] {
			return uds.ErrorResponse(uds.ErrCodeValidation,
				fmt.Sprintf("duplicate blocked_by ID: %q", id))
		}
		seen[id] = true
	}
	return nil
}

// --- File lock helpers ---

// acquireFileLock acquires the shared file mutex to serialize with QueueHandler's PeriodicScan.
func (d *Daemon) acquireFileLock() {
	if d.handler != nil {
		d.handler.LockFiles()
	}
}

// releaseFileLock releases the shared file mutex.
func (d *Daemon) releaseFileLock() {
	if d.handler != nil {
		d.handler.UnlockFiles()
	}
}

// --- File I/O helpers ---

func loadCommandQueueFile(path string) (model.CommandQueue, []byte, error) {
	var cq model.CommandQueue
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cq.SchemaVersion = 1
			cq.FileType = "queue_command"
			return cq, nil, nil
		}
		return cq, nil, fmt.Errorf("read queue %s: %w", path, err)
	}
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return cq, data, fmt.Errorf("parse queue %s: %w", path, err)
	}
	if cq.SchemaVersion == 0 {
		cq.SchemaVersion = 1
		cq.FileType = "queue_command"
	}
	return cq, data, nil
}

func loadTaskQueueFile(path string) (model.TaskQueue, []byte, error) {
	var tq model.TaskQueue
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			tq.SchemaVersion = 1
			tq.FileType = "queue_task"
			return tq, nil, nil
		}
		return tq, nil, fmt.Errorf("read queue %s: %w", path, err)
	}
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		return tq, data, fmt.Errorf("parse queue %s: %w", path, err)
	}
	if tq.SchemaVersion == 0 {
		tq.SchemaVersion = 1
		tq.FileType = "queue_task"
	}
	return tq, data, nil
}

func loadNotificationQueueFile(path string) (model.NotificationQueue, []byte, error) {
	var nq model.NotificationQueue
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			nq.SchemaVersion = 1
			nq.FileType = "queue_notification"
			return nq, nil, nil
		}
		return nq, nil, fmt.Errorf("read queue %s: %w", path, err)
	}
	if err := yamlv3.Unmarshal(data, &nq); err != nil {
		return nq, data, fmt.Errorf("parse queue %s: %w", path, err)
	}
	if nq.SchemaVersion == 0 {
		nq.SchemaVersion = 1
		nq.FileType = "queue_notification"
	}
	return nq, data, nil
}

func checkFileSizeLimit(maxBytes, currentSize, estimatedAddition int) *uds.Response {
	if maxBytes <= 0 {
		return nil
	}
	if currentSize+estimatedAddition > maxBytes {
		return uds.ErrorResponse(uds.ErrCodeBackpressure,
			fmt.Sprintf("queue file would exceed max size: ~%d > %d bytes", currentSize+estimatedAddition, maxBytes))
	}
	return nil
}
