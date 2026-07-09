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

// terminalResultInfo carries a terminal result's status together with its
// lease epoch and creation timestamp so queue reconciliation can fence out
// results that predate the queue row's current dispatch attempt (see
// StaleResult).
type terminalResultInfo struct {
	Status model.Status
	// LeaseEpoch is the queue row's lease epoch recorded at result-write
	// time. 0 means the result predates the field (D-F9); the fence then
	// falls back to the CreatedAt timestamp comparison.
	LeaseEpoch int
	CreatedAt  string
}

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
	// StaleResult, when non-nil, reports whether the terminal result predates
	// the item's current attempt so applying it would clobber a live
	// re-dispatch. result_write fences its terminal transitions with
	// status+epoch+owner; the reconcile-side fence compares the result's
	// persisted lease epoch against the row's current epoch, falling back
	// to the CreatedAt-vs-dispatch-timestamp comparison for results written
	// before the epoch was persisted (D-F9).
	StaleResult func(*T, terminalResultInfo) bool
}

// reconcileTerminalQueueItems processes queue items, updating those whose terminal result
// exists in terminalResults but whose queue status hasn't been updated yet.
// Returns whether modifications were made, the repairs, and the set of repaired command IDs.
func reconcileTerminalQueueItems[T any](
	items []T,
	terminalResults map[string]terminalResultInfo,
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
		result, found := terminalResults[matchKey]
		if !found {
			continue
		}

		// Fencing: a result written before the item's current dispatch belongs
		// to an earlier attempt. Applying it would terminal-ize a freshly
		// re-dispatched row and strand the live worker.
		if accessor.StaleResult != nil && accessor.StaleResult(item, result) {
			run.Log(core.LogLevelWarn,
				"%s stale_result_fenced queue=%s id=%s result_status=%s result_created_at=%s "+
					"(result predates the row's current dispatch; leaving queue row for the live attempt)",
				patternName, queueName, matchKey, result.Status, result.CreatedAt)
			continue
		}

		commandID := accessor.CommandID(item)
		taskID := accessor.TaskID(item)

		run.Log(core.LogLevelWarn, "%s result_terminal_queue_mismatch queue=%s id=%s result_status=%s",
			patternName, queueName, matchKey, result.Status)

		accessor.ApplyUpdate(item, result.Status, now)
		modified = true
		repairedCommands[commandID] = true

		repairs = append(repairs, Repair{
			Pattern:   patternName,
			CommandID: commandID,
			TaskID:    taskID,
			Detail:    fmt.Sprintf("queue %s updated to %s", queueName, result.Status),
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
	terminalResults map[string]terminalResultInfo,
	unmarshalQueue func(data []byte) (Q, []T, error),
	setItems func(Q, []T) Q,
	accessor queueItemAccessor[T],
) (repairs []Repair, repairedCommands map[string]bool, err error) {
	run.Deps.LockMap.Lock("queue:" + queueName)
	defer run.Deps.LockMap.Unlock("queue:" + queueName)

	queueData, err := os.ReadFile(queuePath) //nolint:gosec // queuePath is constructed from a controlled application queue directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read queue %s: %w", queueName, err)
	}

	queue, items, err := unmarshalQueue(queueData)
	if err != nil {
		return nil, nil, fmt.Errorf("parse queue %s: %w", queueName, err)
	}

	modified, repairs, repairedCommands := reconcileTerminalQueueItems(
		items, terminalResults, accessor, run, patternName, queueName,
	)

	if modified {
		queue = setItems(queue, items)
		if err := yamlutil.AtomicWrite(queuePath, queue); err != nil {
			run.Log(core.LogLevelError, "%s write_queue queue=%s error=%v", patternName, queueName, err)
			return nil, nil, fmt.Errorf("write queue %s: %w", queueName, err)
		}
	}

	return repairs, repairedCommands, nil
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
		// Epoch fence (D-F9): a result that persisted the lease epoch of the
		// attempt it belongs to is stale exactly when that epoch is lower
		// than the row's current epoch — a same-epoch result always applies
		// (crash-recovery repairs of the same attempt must not be fenced).
		// Results with no recorded epoch (0, written before the field
		// existed) fall back to the timestamp fence: a result strictly older
		// than the row's in_progress transition was written by a previous
		// attempt of the same task ID (crash recovery results always
		// postdate their own dispatch). Same-timestamp results pass so
		// second-granularity RFC3339 does not fence a legitimate
		// crash-recovery repair.
		StaleResult: func(t *model.Task, result terminalResultInfo) bool {
			if result.LeaseEpoch > 0 {
				return result.LeaseEpoch < t.LeaseEpoch
			}
			if t.InProgressAt == nil || result.CreatedAt == "" {
				return false
			}
			inProgressAt, err := time.Parse(time.RFC3339, *t.InProgressAt)
			if err != nil {
				return false
			}
			createdAt, err := time.Parse(time.RFC3339, result.CreatedAt)
			if err != nil {
				return false
			}
			return createdAt.Before(inProgressAt)
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
