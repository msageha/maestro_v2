package daemon

import (
	"fmt"
	"os"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/daemonapi"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// QueueWriteParams aliases daemonapi.QueueWriteParams so daemon-internal
// callers can keep their existing import surface after the request body
// decoding moved into daemonapi.
type QueueWriteParams = daemonapi.QueueWriteParams

var validNotificationTypes = map[model.NotificationType]bool{
	model.NotificationTypeCommandCompleted: true,
	model.NotificationTypeCommandFailed:    true,
	model.NotificationTypeCommandCancelled: true,
}

// QueueWriteAPI handles the "queue_write" UDS endpoint.
type QueueWriteAPI struct {
	*apiContext
}

// validatePersonaAndSkillRefs rejects unsafe persona_hint / skill_refs values
// at the UDS boundary. plan submit / plan add-task already enforce the same
// rules in internal/plan/validate.go, but the queue_write entrypoints used to
// trust their input — meaning a system-internal task or a command write could
// land an identifier containing path separators or null bytes that the
// dispatcher then injected into prompts and skill resolution paths. Mirroring
// the planner-side check here closes the gap so every code path that reaches
// the queue files goes through the same identifier sanitisation.
func validatePersonaAndSkillRefs(personaHint string, skillRefs []string) *uds.Response {
	if personaHint != "" {
		if !validate.IsValidIdentifier(personaHint) {
			return uds.ErrorResponse(uds.ErrCodeValidation,
				fmt.Sprintf("invalid persona_hint %q: must not contain '/', '\\', or null bytes", personaHint))
		}
	}
	for i, ref := range skillRefs {
		if !validate.IsValidIdentifier(ref) {
			return uds.ErrorResponse(uds.ErrCodeValidation,
				fmt.Sprintf("invalid skill_refs[%d] %q: must not contain '/', '\\', or null bytes", i, ref))
		}
	}
	return nil
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
	if err := validate.ID(commandID); err != nil {
		return false
	}
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

// writeAndNotify atomically writes a queue file, notifies self-write, and
// returns an error response on failure. This consolidates the repeated
// AtomicWrite + notifySelfWrite pattern used by command, task, and notification
// write handlers.
func (h *QueueWriteAPI) writeAndNotify(queuePath, queueType string, data any) *uds.Response {
	// Pre-record self-write hash BEFORE the file write to close the race
	// window where fsnotify observes the AtomicWrite before Record() runs.
	// notifySelfWrite() below will re-record (idempotent) and publish the event.
	h.recordSelfWrite(queuePath, data)
	if err := yamlutil.AtomicWrite(queuePath, data); err != nil {
		return internalErrorf("write queue: %v", err)
	}
	h.notifySelfWrite(queuePath, queueType, data)
	return nil
}

// ensureCapacityWithArchive checks whether the queue file can accommodate
// estimatedAddition bytes. If it cannot, it calls archiveFn to remove terminal
// entries, re-marshals the queue, and re-checks. Returns nil when the write is
// safe, or an error response when the file remains too large.
//
// This consolidates the repeated "check → archive → re-marshal → re-check"
// pattern used by command, task, and notification write handlers.
func ensureCapacityWithArchive(
	maxYAMLBytes int,
	currentData []byte,
	estimatedAddition int,
	archiveFn func() int,
	marshalFn func() ([]byte, error),
	logFn func(string, ...any),
	label string,
) *uds.Response {
	resp := checkFileSizeLimit(maxYAMLBytes, len(currentData), estimatedAddition)
	if resp == nil {
		return nil
	}
	archived := archiveFn()
	if archived == 0 {
		return resp
	}
	newData, err := marshalFn()
	if err != nil {
		return internalErrorf("marshal queue after archive: %v", err)
	}
	if checkFileSizeLimit(maxYAMLBytes, len(newData), estimatedAddition) != nil {
		return resp
	}
	logFn("queue_write archive_%s archived=%d", label, archived)
	return nil
}

// resolvePriority returns the given priority, or model.DefaultPriority if zero.
func resolvePriority(priority int) int {
	if priority == 0 {
		return model.DefaultPriority
	}
	return priority
}

// formatNowUTC returns the current UTC time formatted as RFC3339.
func formatNowUTC(c Clock) string {
	return c.Now().UTC().Format(time.RFC3339)
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
