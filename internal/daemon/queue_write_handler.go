package daemon

import (
	"encoding/json"
	"fmt"
	"os"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
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
	// SystemCaller gates the queue_write task path. Task ID minting is the
	// Planner's responsibility; the queue_write task entrypoint is reserved
	// for internal/test usage and is NOT exposed via the maestro CLI. Callers
	// must set this to a recognised model.TaskIDCaller (typically
	// TaskIDCallerSystemInternal) or the request is rejected.
	SystemCaller string `json:"system_caller,omitempty"`
}

var validNotificationTypes = map[model.NotificationType]bool{
	model.NotificationTypeCommandCompleted: true,
	model.NotificationTypeCommandFailed:    true,
	model.NotificationTypeCommandCancelled: true,
}

// QueueWriteAPI handles the "queue_write" UDS endpoint.
type QueueWriteAPI struct {
	*apiContext
}

func (h *QueueWriteAPI) handleQueueWrite(req *uds.Request) *uds.Response {
	var params QueueWriteParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid params: %v", err))
	}

	switch params.Type {
	case "command":
		return h.handleQueueWriteCommand(params)
	case "task":
		return h.handleQueueWriteTask(params)
	case "notification":
		return h.handleQueueWriteNotification(params)
	case "cancel-request":
		return h.handleQueueWriteCancelRequest(params)
	default:
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("invalid type: %q, must be command|task|notification|cancel-request", params.Type))
	}
}

// --- Lock helpers ---

// withQueueLocks acquires the shared file lock and all per-resource lockMap keys,
// executes fn, then releases everything in reverse order via defer.
// This consolidates the acquireFileLock + lockMap.Lock pattern used by all write handlers.
func (h *QueueWriteAPI) withQueueLocks(targets []string, fn func() *uds.Response) *uds.Response {
	h.acquireFileLock()
	defer h.releaseFileLock()
	for _, t := range targets {
		h.lockMap.Lock(t)
		defer h.lockMap.Unlock(t)
	}
	return fn()
}

// --- Error response helpers ---

// validateRequired returns a validation error response if field is empty.
func validateRequired(field, name, entityType string) *uds.Response {
	if field == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("%s is required for %s", name, entityType))
	}
	return nil
}

// validateContentSize returns a validation error response if content exceeds maxBytes.
func validateContentSize(content string, maxBytes int) *uds.Response {
	if maxBytes > 0 && len(content) > maxBytes {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("content exceeds max size: %d > %d bytes", len(content), maxBytes))
	}
	return nil
}

// internalErrorf returns an internal error response with a formatted message.
func internalErrorf(format string, args ...any) *uds.Response {
	return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf(format, args...))
}

// --- File I/O helpers ---

// loadQueueFile reads and unmarshals a YAML queue file via loadYAMLFile. If
// the file does not exist, it returns a zero-value T with defaults applied.
// setDefaults is called on every successful path to ensure SchemaVersion/FileType are set.
func loadQueueFile[T any](path string, setDefaults func(*T)) (T, []byte, error) {
	result, data, err := loadYAMLFile[T](path, true)
	if err != nil {
		return result, data, err
	}
	setDefaults(&result)
	return result, data, nil
}

func loadCommandQueueFile(path string) (model.CommandQueue, []byte, error) {
	return loadQueueFile(path, func(cq *model.CommandQueue) {
		if cq.SchemaVersion == 0 {
			cq.SchemaVersion = 1
			cq.FileType = "queue_command"
		}
	})
}

func loadTaskQueueFile(path string) (model.TaskQueue, []byte, error) {
	return loadQueueFile(path, func(tq *model.TaskQueue) {
		if tq.SchemaVersion == 0 {
			tq.SchemaVersion = 1
			tq.FileType = "queue_task"
		}
	})
}

func loadNotificationQueueFile(path string) (model.NotificationQueue, []byte, error) {
	return loadQueueFile(path, func(nq *model.NotificationQueue) {
		if nq.SchemaVersion == 0 {
			nq.SchemaVersion = 1
			nq.FileType = "queue_notification"
		}
	})
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
func (h *QueueWriteAPI) archiveTerminalTasks(tq *model.TaskQueue) int {
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
			terminal = readCommandPlanTerminal(h.maestroDir, task.CommandID)
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

// readCommandPlanTerminal reads a command's state file and returns whether
// plan_status is terminal.
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
func readCommandPlanTerminal(maestroDir, commandID string) bool {
	statePath := commandStatePath(maestroDir, commandID)
	data, err := os.ReadFile(statePath) //nolint:gosec // statePath is constructed from a controlled application state directory
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
