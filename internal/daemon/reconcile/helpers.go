package reconcile

import (
	"fmt"
	"os"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// queueItemAccessor defines how to access and update a queue item for terminal result reconciliation.
type queueItemAccessor[T any] struct {
	// ShouldProcess returns true if this item should be checked against terminal results.
	ShouldProcess func(*T) bool
	// MatchKey returns the key to look up in the terminal results map.
	MatchKey func(*T) string
	// CommandID returns the command ID associated with this item.
	CommandID func(*T) string
	// TaskID returns the task ID for this item (empty string if not applicable).
	TaskID func(*T) string
	// ApplyUpdate updates the item's status, clears lease fields, and sets UpdatedAt.
	ApplyUpdate func(*T, model.Status, string)
}

// reconcileTerminalQueueItems processes queue items, updating those whose terminal result
// exists in terminalResults but whose queue status hasn't been updated yet.
// Returns whether modifications were made, the repairs, and the set of repaired command IDs.
func reconcileTerminalQueueItems[T any](
	items []T,
	terminalResults map[string]model.Status,
	accessor queueItemAccessor[T],
	run *Run,
	patternName RepairPatternID,
	queueName string,
) (modified bool, repairs []Repair, repairedCommands map[string]bool) {
	repairedCommands = make(map[string]bool)
	now := run.Deps.Clock.Now().UTC().Format(time.RFC3339)

	for i := range items {
		item := &items[i]
		if !accessor.ShouldProcess(item) {
			continue
		}

		matchKey := accessor.MatchKey(item)
		resultStatus, found := terminalResults[matchKey]
		if !found {
			continue
		}

		commandID := accessor.CommandID(item)
		taskID := accessor.TaskID(item)

		run.log(core.LogLevelWarn, "%s result_terminal_queue_mismatch queue=%s id=%s result_status=%s",
			patternName, queueName, matchKey, resultStatus)

		accessor.ApplyUpdate(item, resultStatus, now)
		modified = true
		repairedCommands[commandID] = true

		repairs = append(repairs, Repair{
			Pattern:   patternName,
			CommandID: commandID,
			TaskID:    taskID,
			Detail:    fmt.Sprintf("queue %s updated to %s", queueName, resultStatus),
		})
	}

	return modified, repairs, repairedCommands
}

// reconcileTerminalQueue is a higher-level helper that locks a queue file, reads it,
// reconciles terminal results against queue items, and writes back if modified.
// Q is the queue file type, T is the queue item type.
func reconcileTerminalQueue[Q any, T any](
	run *Run,
	patternName RepairPatternID,
	queueName string,
	queuePath string,
	terminalResults map[string]model.Status,
	unmarshalQueue func(data []byte) (Q, []T, error),
	setItems func(Q, []T) Q,
	accessor queueItemAccessor[T],
) (repairs []Repair, repairedCommands map[string]bool) {
	run.Deps.LockMap.Lock("queue:" + queueName)
	defer run.Deps.LockMap.Unlock("queue:" + queueName)

	queueData, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		return nil, nil
	}

	queue, items, err := unmarshalQueue(queueData)
	if err != nil {
		return nil, nil
	}

	modified, repairs, repairedCommands := reconcileTerminalQueueItems(
		items, terminalResults, accessor, run, patternName, queueName,
	)

	if modified {
		queue = setItems(queue, items)
		if err := yamlutil.AtomicWrite(queuePath, queue); err != nil {
			run.log(core.LogLevelError, "%s write_queue queue=%s error=%v", patternName, queueName, err)
			return nil, nil
		}
	}

	return repairs, repairedCommands
}

// taskQueueAccessor returns a queueItemAccessor for model.Task items (used by R1).
func taskQueueAccessor() queueItemAccessor[model.Task] {
	return queueItemAccessor[model.Task]{
		ShouldProcess: func(t *model.Task) bool {
			return t.Status == model.StatusInProgress
		},
		MatchKey:  func(t *model.Task) string { return t.ID },
		CommandID: func(t *model.Task) string { return t.CommandID },
		TaskID:    func(t *model.Task) string { return t.ID },
		ApplyUpdate: func(t *model.Task, status model.Status, now string) {
			t.Status = status
			t.LeaseOwner = nil
			t.LeaseExpiresAt = nil
			t.UpdatedAt = now
		},
	}
}

// commandQueueAccessor returns a queueItemAccessor for model.Command items (used by R3).
func commandQueueAccessor() queueItemAccessor[model.Command] {
	return queueItemAccessor[model.Command]{
		ShouldProcess: func(c *model.Command) bool {
			return !model.IsTerminal(c.Status)
		},
		MatchKey:  func(c *model.Command) string { return c.ID },
		CommandID: func(c *model.Command) string { return c.ID },
		TaskID:    func(_ *model.Command) string { return "" },
		ApplyUpdate: func(c *model.Command, status model.Status, now string) {
			c.Status = status
			c.LeaseOwner = nil
			c.LeaseExpiresAt = nil
			c.UpdatedAt = now
		},
	}
}

// unmarshalTaskQueue parses a TaskQueue YAML and returns the queue and its items.
func unmarshalTaskQueue(data []byte) (model.TaskQueue, []model.Task, error) {
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		return tq, nil, err
	}
	return tq, tq.Tasks, nil
}

// setTaskQueueItems returns the TaskQueue with updated items.
func setTaskQueueItems(tq model.TaskQueue, items []model.Task) model.TaskQueue {
	tq.Tasks = items
	return tq
}

// unmarshalCommandQueue parses a CommandQueue YAML and returns the queue and its items.
func unmarshalCommandQueue(data []byte) (model.CommandQueue, []model.Command, error) {
	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return cq, nil, err
	}
	return cq, cq.Commands, nil
}

// setCommandQueueItems returns the CommandQueue with updated items.
func setCommandQueueItems(cq model.CommandQueue, items []model.Command) model.CommandQueue {
	cq.Commands = items
	return cq
}
