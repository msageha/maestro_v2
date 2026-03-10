package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// QueueWriteParams is the request payload for the queue_write UDS command.
type QueueWriteParams struct {
	Target             string   `json:"target"` // "planner", "worker1", "orchestrator"
	Type               string   `json:"type"`   // "command", "task", "notification", "cancel-request"
	CommandID          string   `json:"command_id,omitempty"`
	Content            string   `json:"content,omitempty"`
	Purpose            string   `json:"purpose,omitempty"`
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty"`
	BloomLevel         int      `json:"bloom_level,omitempty"`
	BlockedBy          []string `json:"blocked_by,omitempty"`
	Constraints        []string `json:"constraints,omitempty"`
	ToolsHint          []string `json:"tools_hint,omitempty"`
	PersonaHint        string   `json:"persona_hint,omitempty"`
	SkillRefs          []string `json:"skill_refs,omitempty"`
	Priority           int      `json:"priority"`
	SourceResultID     string   `json:"source_result_id,omitempty"`
	NotificationType   string   `json:"notification_type,omitempty"`
	RequestedBy        string   `json:"requested_by,omitempty"`
	Reason             string   `json:"reason,omitempty"`
}

var validNotificationTypes = map[model.NotificationType]bool{
	model.NotificationTypeCommandCompleted: true,
	model.NotificationTypeCommandFailed:    true,
	model.NotificationTypeCommandCancelled: true,
}

func (a *API) handleQueueWrite(req *uds.Request) *uds.Response {
	var params QueueWriteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	switch params.Type {
	case "command":
		return a.handleQueueWriteCommand(params)
	case "task":
		return a.handleQueueWriteTask(params)
	case "notification":
		return a.handleQueueWriteNotification(params)
	case "cancel-request":
		return a.handleQueueWriteCancelRequest(params)
	default:
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("invalid type: %q, must be command|task|notification|cancel-request", params.Type))
	}
}

func (a *API) handleQueueWriteCommand(params QueueWriteParams) *uds.Response {
	d := a.d
	if params.Content == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "content is required for command")
	}

	if d.config.Limits.MaxEntryContentBytes > 0 && len(params.Content) > d.config.Limits.MaxEntryContentBytes {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("content exceeds max size: %d > %d bytes", len(params.Content), d.config.Limits.MaxEntryContentBytes))
	}

	a.acquireFileLock()
	defer a.releaseFileLock()
	d.lockMap.Lock("queue:planner")
	defer d.lockMap.Unlock("queue:planner")

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
		archived := archiveTerminalCommands(&cq)
		if archived > 0 {
			newData, err := yamlv3.Marshal(cq)
			if err != nil {
				return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("marshal queue after archive: %v", err))
			}
			if checkFileSizeLimit(d.config.Limits.MaxYAMLFileBytes, len(newData), len(params.Content)+200) != nil {
				return resp
			}
			d.log(LogLevelInfo, "queue_write archive_commands archived=%d", archived)
		} else {
			return resp
		}
	}

	id, err := model.GenerateID(model.IDTypeCommand)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("generate ID: %v", err))
	}

	priority := params.Priority
	if priority == 0 {
		priority = 100
	}

	now := d.clock.Now().UTC().Format(time.RFC3339)
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
	a.notifySelfWrite(queuePath, "command", cq)

	d.log(LogLevelInfo, "queue_write type=command id=%s", id)
	return uds.SuccessResponse(map[string]string{"id": id})
}

func (a *API) handleQueueWriteTask(params QueueWriteParams) *uds.Response {
	d := a.d
	if params.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required for task")
	}
	if err := validate.ValidateID(params.CommandID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid command_id: %v", err))
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

	a.acquireFileLock()
	defer a.releaseFileLock()

	// Sanitize target: prevent directory traversal (before acquiring per-target lock)
	if filepath.Base(params.Target) != params.Target || params.Target == "." || params.Target == ".." {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("invalid target: %q", params.Target))
	}

	d.lockMap.Lock("queue:" + params.Target)
	defer d.lockMap.Unlock("queue:" + params.Target)

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
		archived := a.archiveTerminalTasks(&tq)
		if archived > 0 {
			newData, marshalErr := yamlv3.Marshal(tq)
			if marshalErr != nil {
				return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("marshal queue after archive: %v", marshalErr))
			}
			if checkFileSizeLimit(d.config.Limits.MaxYAMLFileBytes, len(newData), len(params.Content)+500) != nil {
				return resp
			}
			d.log(LogLevelInfo, "queue_write archive_tasks worker=%s archived=%d", params.Target, archived)
		} else {
			return resp
		}
	}

	id, err := model.GenerateID(model.IDTypeTask)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("generate ID: %v", err))
	}

	// Cycle detection: check if adding this task creates a circular dependency
	if len(params.BlockedBy) > 0 {
		if resp := a.detectCycleInDependencies(id, params.BlockedBy, params.CommandID, params.Target, &tq); resp != nil {
			return resp
		}
	}

	priority := params.Priority
	if priority == 0 {
		priority = 100
	}

	now := d.clock.Now().UTC().Format(time.RFC3339)
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
		PersonaHint:        params.PersonaHint,
		SkillRefs:          params.SkillRefs,
		Priority:           priority,
		Status:             model.StatusPending,
		CreatedAt:          now,
		UpdatedAt:          now,
	})

	if err := yamlutil.AtomicWrite(queuePath, tq); err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("write queue: %v", err))
	}
	a.notifySelfWrite(queuePath, "task", tq)

	d.log(LogLevelInfo, "queue_write type=task id=%s command_id=%s worker=%s", id, params.CommandID, params.Target)
	return uds.SuccessResponse(map[string]string{"id": id})
}

func (a *API) handleQueueWriteNotification(params QueueWriteParams) *uds.Response {
	d := a.d
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
	if params.NotificationType == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "notification_type is required for notification")
	}
	notifType := model.NotificationType(params.NotificationType)
	if !validNotificationTypes[notifType] {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("invalid notification_type: %q, must be command_completed|command_failed|command_cancelled", notifType))
	}

	if d.config.Limits.MaxEntryContentBytes > 0 && len(params.Content) > d.config.Limits.MaxEntryContentBytes {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("content exceeds max size: %d > %d bytes", len(params.Content), d.config.Limits.MaxEntryContentBytes))
	}

	a.acquireFileLock()
	defer a.releaseFileLock()
	d.lockMap.Lock("queue:orchestrator")
	defer d.lockMap.Unlock("queue:orchestrator")

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
		archived := archiveTerminalNotifications(&nq)
		if archived > 0 {
			newData, marshalErr := yamlv3.Marshal(nq)
			if marshalErr != nil {
				return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("marshal queue after archive: %v", marshalErr))
			}
			if checkFileSizeLimit(d.config.Limits.MaxYAMLFileBytes, len(newData), len(params.Content)+300) != nil {
				return resp
			}
			d.log(LogLevelInfo, "queue_write archive_notifications archived=%d", archived)
		} else {
			return resp
		}
	}

	id, err := model.GenerateID(model.IDTypeNotification)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("generate ID: %v", err))
	}

	priority := params.Priority
	if priority == 0 {
		priority = 100
	}

	now := d.clock.Now().UTC().Format(time.RFC3339)
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
	a.notifySelfWrite(queuePath, "notification", nq)

	d.log(LogLevelInfo, "queue_write type=notification id=%s command_id=%s source_result_id=%s", id, params.CommandID, params.SourceResultID)
	return uds.SuccessResponse(map[string]string{"id": id})
}

// handleQueueWriteCancelRequest processes cancel-request queue writes.
// Cancel-request is exempt from backpressure checks because it modifies
// existing state (e.g. setting cancel.requested on a submitted command, or
// marking an unsubmitted planner entry as cancelled) rather than adding a
// new entry to the queue. Applying backpressure here would prevent users
// from cancelling commands when the system is already overloaded, which is
// the exact scenario where cancellation is most needed.
func (a *API) handleQueueWriteCancelRequest(params QueueWriteParams) *uds.Response {
	d := a.d
	if params.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required for cancel-request")
	}
	if err := validate.ValidateID(params.CommandID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid command_id: %v", err))
	}

	// Check if state exists (submitted command)
	statePath := filepath.Join(d.maestroDir, "state", "commands", params.CommandID+".yaml")
	if _, err := os.Stat(statePath); err == nil {
		return a.cancelRequestSubmitted(params, statePath)
	}

	// Un-submitted: cancel in planner queue
	return a.cancelRequestUnsubmitted(params)
}

// cancelRequestSubmitted handles cancel-request for already-submitted commands.
// Sets cancel.requested=true on the state file (idempotent).
// Lock order: scanMu.RLock → queue:planner → state:{cmd} (consistent with PeriodicScan).
func (a *API) cancelRequestSubmitted(params QueueWriteParams, statePath string) *uds.Response {
	d := a.d
	a.acquireFileLock()
	defer a.releaseFileLock()
	d.lockMap.Lock("queue:planner")
	defer d.lockMap.Unlock("queue:planner")

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

	now := d.clock.Now().UTC().Format(time.RFC3339)
	requestedBy := params.RequestedBy
	if requestedBy == "" {
		requestedBy = "orchestrator"
	}
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
					} else {
						a.notifySelfWrite(queuePath, "cancel-request", cq)
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
func (a *API) cancelRequestUnsubmitted(params QueueWriteParams) *uds.Response {
	d := a.d
	a.acquireFileLock()
	defer a.releaseFileLock()
	d.lockMap.Lock("queue:planner")
	defer d.lockMap.Unlock("queue:planner")

	queuePath := filepath.Join(d.maestroDir, "queue", "planner.yaml")
	cq, _, err := loadCommandQueueFile(queuePath)
	if err != nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, err.Error())
	}

	now := d.clock.Now().UTC().Format(time.RFC3339)
	requestedBy := params.RequestedBy
	if requestedBy == "" {
		requestedBy = "orchestrator"
	}
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
	a.notifySelfWrite(queuePath, "cancel-request", cq)

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

// --- Cycle detection helpers ---

// detectCycleInDependencies checks if adding a task with the given blocked_by
// would create a circular dependency among all non-terminal tasks for the same command.
// It collects tasks from all worker queues and the already-loaded target queue.
func (a *API) detectCycleInDependencies(newTaskID string, newBlockedBy []string, commandID string, targetWorker string, targetQueue *model.TaskQueue) *uds.Response {
	d := a.d
	// Build dependency graph: taskID → list of task IDs it depends on
	deps := make(map[string][]string)
	deps[newTaskID] = newBlockedBy

	// Collect non-terminal tasks from the already-loaded target queue
	for _, task := range targetQueue.Tasks {
		if task.CommandID != commandID || model.IsTerminal(task.Status) {
			continue
		}
		if len(task.BlockedBy) > 0 {
			deps[task.ID] = task.BlockedBy
		}
	}

	// Collect non-terminal tasks from other worker queues (enumerate from disk)
	queueDir := filepath.Join(d.maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		d.log(LogLevelWarn, "cycle_detection read_queue_dir error=%v", err)
		// Continue with what we have — don't block task submission on dir read failure
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		workerName := strings.TrimSuffix(name, ".yaml")
		if workerName == targetWorker {
			continue // already loaded above
		}
		path := filepath.Join(queueDir, name)
		tq, _, err := loadTaskQueueFile(path)
		if err != nil {
			d.log(LogLevelWarn, "cycle_detection load_queue file=%s error=%v", name, err)
			continue
		}
		for _, task := range tq.Tasks {
			if task.CommandID != commandID || model.IsTerminal(task.Status) {
				continue
			}
			if len(task.BlockedBy) > 0 {
				deps[task.ID] = task.BlockedBy
			}
		}
	}

	// Run DFS-based cycle detection
	if cycle := detectCycleDFS(deps); len(cycle) > 0 {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("circular dependency detected: %s", strings.Join(cycle, " -> ")))
	}
	return nil
}

// detectCycleDFS finds a cycle in the dependency graph using DFS with white/gray/black coloring.
// Returns the cycle path if found, or nil if no cycle exists.
func detectCycleDFS(deps map[string][]string) []string {
	const (
		white = 0 // unvisited
		gray  = 1 // in current DFS path
		black = 2 // fully explored
	)

	color := make(map[string]int)
	parent := make(map[string]string)

	var dfs func(node string) []string
	dfs = func(node string) []string {
		color[node] = gray
		for _, dep := range deps[node] {
			if color[dep] == black {
				continue
			}
			if color[dep] == gray {
				// Found cycle — reconstruct path
				cycle := []string{dep}
				current := node
				for current != dep {
					cycle = append(cycle, current)
					current = parent[current]
				}
				cycle = append(cycle, dep)
				// Reverse to get forward order
				for i, j := 0, len(cycle)-1; i < j; i, j = i+1, j-1 {
					cycle[i], cycle[j] = cycle[j], cycle[i]
				}
				return cycle
			}
			// white — unvisited but only if it's in our graph
			if _, inGraph := deps[dep]; !inGraph {
				continue // dangling reference, skip
			}
			parent[dep] = node
			if result := dfs(dep); result != nil {
				return result
			}
		}
		color[node] = black
		return nil
	}

	// Start DFS from all nodes in the graph
	for node := range deps {
		if color[node] == white {
			if result := dfs(node); result != nil {
				return result
			}
		}
	}
	return nil
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

// archiveTerminalCommands removes terminal-status commands from the queue in-place.
// Returns the number of entries removed.
func archiveTerminalCommands(cq *model.CommandQueue) int {
	kept := cq.Commands[:0]
	archived := 0
	for _, cmd := range cq.Commands {
		if model.IsTerminal(cmd.Status) {
			archived++
			continue
		}
		kept = append(kept, cmd)
	}
	cq.Commands = kept
	return archived
}

// archiveTerminalTasks removes terminal-status tasks whose command plan_status is also terminal.
func (a *API) archiveTerminalTasks(tq *model.TaskQueue) int {
	planTerminalCache := make(map[string]bool)
	kept := tq.Tasks[:0]
	archived := 0
	for _, task := range tq.Tasks {
		if !model.IsTerminal(task.Status) {
			kept = append(kept, task)
			continue
		}
		terminal, ok := planTerminalCache[task.CommandID]
		if !ok {
			terminal = a.isCommandPlanTerminal(task.CommandID)
			planTerminalCache[task.CommandID] = terminal
		}
		if terminal {
			archived++
			continue
		}
		kept = append(kept, task)
	}
	tq.Tasks = kept
	return archived
}

// isCommandPlanTerminal checks if a command's plan_status is terminal.
//
// This is a read-only check and intentionally does NOT acquire the per-command
// state lock. Because all state file writes use AtomicWrite (write-to-temp +
// rename), concurrent reads always see a complete, consistent snapshot.
// A slightly stale read is acceptable here — the worst case is that we skip
// archiving a task whose command has just become terminal, which will be
// caught on the next archive pass.
//
// Acquiring the state lock here would create a lock-order inversion:
// this function is called under queue:{target} (via archiveTerminalTasks),
// but other code paths (retry, submit) acquire state:{commandID} before
// queue:{workerID}. Omitting the lock avoids the deadlock risk (CR-011).
func (a *API) isCommandPlanTerminal(commandID string) bool {
	statePath := filepath.Join(a.d.maestroDir, "state", "commands", commandID+".yaml")
	data, err := os.ReadFile(statePath)
	if err != nil {
		return false
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return false
	}
	return model.IsPlanTerminal(state.PlanStatus)
}

// archiveTerminalNotifications removes completed/dead_letter notifications from the queue.
func archiveTerminalNotifications(nq *model.NotificationQueue) int {
	kept := nq.Notifications[:0]
	archived := 0
	for _, ntf := range nq.Notifications {
		if ntf.Status == model.StatusCompleted || ntf.Status == model.StatusDeadLetter {
			archived++
			continue
		}
		kept = append(kept, ntf)
	}
	nq.Notifications = kept
	return archived
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
