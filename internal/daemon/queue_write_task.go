package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	"github.com/msageha/maestro_v2/internal/validate"
)

// handleQueueWriteTask is an INTERNAL entrypoint for the queue_write "task"
// type. The Planner is the single source of truth for task creation and
// minting; this path exists only for system-internal/test usage and is not
// exposed via the maestro CLI. Callers must set params.SystemCaller to a
// recognised model.TaskIDCaller value (typically TaskIDCallerSystemInternal)
// — otherwise the request is rejected to prevent the historical
// "Planner-bypass task injection" Critical issue.
func (h *QueueWriteAPI) handleQueueWriteTask(params QueueWriteParams) *uds.Response {
	// Validate all fields before acquiring locks
	if resp := validateTaskWriteParams(params, h.config.Limits.MaxEntryContentBytes); resp != nil {
		return resp
	}

	// Sanitize target: prevent directory traversal (before acquiring per-target lock)
	if !validate.IsValidBaseName(params.Target) {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			fmt.Sprintf("invalid target: %q", params.Target))
	}

	return h.withQueueLocks([]string{"queue:" + params.Target}, func() *uds.Response {
		return h.executeTaskWrite(params)
	})
}

// executeTaskWrite performs the locked portion of task queue write.
func (h *QueueWriteAPI) executeTaskWrite(params QueueWriteParams) *uds.Response {
	queuePath := taskQueuePath(h.maestroDir, params.Target)
	tq, data, err := loadTaskQueueFile(queuePath)
	if err != nil {
		return internalErrorf("%v", err)
	}

	// Backpressure: count pending tasks only (spec: pending >= limit → reject)
	if h.config.Limits.MaxPendingTasksPerWorker > 0 {
		pending := 0
		for _, task := range tq.Tasks {
			if task.Status == model.StatusPending {
				pending++
			}
		}
		if pending >= h.config.Limits.MaxPendingTasksPerWorker {
			return uds.ErrorResponse(uds.ErrCodeBackpressure,
				fmt.Sprintf("pending tasks limit for %s reached: %d/%d", params.Target, pending, h.config.Limits.MaxPendingTasksPerWorker))
		}
	}

	if resp := ensureCapacityWithArchive(
		h.config.Limits.MaxYAMLFileBytes, data, len(params.Content)+500,
		func() int { return h.archiveTerminalTasks(&tq) },
		func() ([]byte, error) { return yamlv3.Marshal(tq) },
		func(f string, a ...any) { h.logFn(LogLevelInfo, f, a...) },
		fmt.Sprintf("tasks worker=%s", params.Target),
	); resp != nil {
		return resp
	}

	id, err := model.NewTaskID(model.TaskIDCaller(params.SystemCaller))
	if err != nil {
		return internalErrorf("generate ID: %v", err)
	}

	// Cycle detection: check if adding this task creates a circular dependency
	if len(params.BlockedBy) > 0 {
		if resp := h.detectCycleInDependencies(id, params.BlockedBy, params.CommandID, params.Target, &tq); resp != nil {
			return resp
		}
	}

	priority := resolvePriority(params.Priority)
	now := formatNowUTC(h.clock)
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

	if resp := h.writeAndNotify(queuePath, "task", tq); resp != nil {
		return resp
	}

	h.logFn(LogLevelInfo, "queue_write type=task id=%s command_id=%s worker=%s", id, params.CommandID, params.Target)
	return uds.SuccessResponse(map[string]string{"id": id})
}

// --- Validation helpers ---

// validateTaskWriteParams validates all fields of a task queue_write request
// before any file locks are acquired. Returns nil if all checks pass.
func validateTaskWriteParams(params QueueWriteParams, maxEntryContentBytes int) *uds.Response {
	// The queue_write task UDS path is reserved for system-internal/test
	// callers ONLY. Planner / Daemon-retry callers must mint task IDs via
	// model.NewTaskID directly inside their own packages — they must NOT
	// reach the queue via this UDS entrypoint. Accepting only
	// TaskIDCallerSystemInternal here makes "planner-submit" / "daemon-retry"
	// values un-spoofable through UDS, which closes the Planner-bypass hole.
	caller := model.TaskIDCaller(params.SystemCaller)
	if caller != model.TaskIDCallerSystemInternal {
		return uds.ErrorResponse(uds.ErrCodeValidation,
			"queue_write type=task is internal-only; task creation must go through Planner (plan submit / plan retry-task). Only system-internal callers may use this path.")
	}
	if params.CommandID == "" {
		return uds.ErrorResponse(uds.ErrCodeValidation, "command_id is required for task")
	}
	if err := validate.ID(params.CommandID); err != nil {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("invalid command_id: %v", err))
	}
	if resp := validateRequired(params.Content, "content", "task"); resp != nil {
		return resp
	}
	if resp := validateRequired(params.Purpose, "purpose", "task"); resp != nil {
		return resp
	}
	if resp := validateRequired(params.AcceptanceCriteria, "acceptance_criteria", "task"); resp != nil {
		return resp
	}
	if params.BloomLevel < 1 || params.BloomLevel > 6 {
		return uds.ErrorResponse(uds.ErrCodeValidation, fmt.Sprintf("bloom_level must be 1-6, got %d", params.BloomLevel))
	}
	if resp := validateRequired(params.Target, "target", "task"); resp != nil {
		return resp
	}
	if resp := validateContentSize(params.Content, maxEntryContentBytes); resp != nil {
		return resp
	}
	if resp := validateBlockedBy(params.BlockedBy); resp != nil {
		return resp
	}
	return nil
}

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
func (h *QueueWriteAPI) detectCycleInDependencies(newTaskID string, newBlockedBy []string, commandID string, targetWorker string, targetQueue *model.TaskQueue) *uds.Response {
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
	queueDir := queueDirPath(h.maestroDir)
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		h.logFn(LogLevelWarn, "cycle_detection read_queue_dir error=%v", err)
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
			h.logFn(LogLevelWarn, "cycle_detection load_queue file=%s error=%v", name, err)
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
