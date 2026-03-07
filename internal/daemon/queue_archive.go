package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// --- File I/O helpers ---

func (qh *QueueHandler) loadCommandQueue() (model.CommandQueue, string) {
	path := filepath.Join(qh.maestroDir, "queue", "planner.yaml")
	var cq model.CommandQueue

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			qh.log(core.LogLevelWarn, "load_commands error=%v", err)
		}
		return cq, ""
	}

	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		qh.log(core.LogLevelError, "parse_commands error=%v path=%s", err, path)
		// Quarantine original corrupted file
		qh.quarantineFile(data, "planner.yaml")
		// Attempt per-entry salvage via Node parsing
		salvaged := qh.salvageCommandQueue(data)
		if len(salvaged.Commands) > 0 {
			qh.log(core.LogLevelWarn, "command_queue_salvaged recovered=%d path=%s", len(salvaged.Commands), path)
			if writeErr := yamlutil.AtomicWrite(path, salvaged); writeErr != nil {
				qh.log(core.LogLevelError, "write_salvaged_queue error=%v", writeErr)
			}
			return salvaged, path
		}
		// No entries salvaged — reset to empty
		emptyCQ := model.CommandQueue{
			SchemaVersion: 1,
			FileType:      "queue_command",
		}
		if writeErr := yamlutil.AtomicWrite(path, emptyCQ); writeErr != nil {
			qh.log(core.LogLevelError, "overwrite_corrupted_queue error=%v", writeErr)
		} else {
			qh.log(core.LogLevelInfo, "corrupted_queue_reset path=%s", path)
		}
		return cq, ""
	}
	return cq, path
}

// quarantineFile saves corrupted file data to the quarantine directory for later inspection.
func (qh *QueueHandler) quarantineFile(data []byte, name string) {
	quarantineDir := filepath.Join(qh.maestroDir, "quarantine")
	if err := os.MkdirAll(quarantineDir, 0755); err != nil {
		qh.log(core.LogLevelError, "create_quarantine_dir error=%v", err)
		return
	}
	ts := qh.clock.Now().UTC().Format("20060102T150405Z")
	qPath := filepath.Join(quarantineDir, fmt.Sprintf("%s_%s", ts, name))
	if err := os.WriteFile(qPath, data, 0644); err != nil {
		qh.log(core.LogLevelError, "quarantine_write error=%v", err)
	} else {
		qh.log(core.LogLevelInfo, "quarantined file=%s to=%s", name, qPath)
	}
	qh.cleanupQuarantine(quarantineDir)
}

// salvageCommandQueue attempts per-entry recovery from corrupted command queue YAML.
func (qh *QueueHandler) salvageCommandQueue(data []byte) model.CommandQueue {
	result := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
	}

	var node yamlv3.Node
	if err := yamlv3.Unmarshal(data, &node); err != nil {
		return result
	}

	if node.Kind != yamlv3.DocumentNode || len(node.Content) == 0 {
		return result
	}
	root := node.Content[0]
	if root.Kind != yamlv3.MappingNode {
		return result
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "commands" {
			seqNode := root.Content[i+1]
			if seqNode.Kind != yamlv3.SequenceNode {
				break
			}
			for _, cmdNode := range seqNode.Content {
				cmdBytes, err := yamlv3.Marshal(cmdNode)
				if err != nil {
					continue
				}
				var cmd model.Command
				if err := yamlv3.Unmarshal(cmdBytes, &cmd); err != nil {
					qh.log(core.LogLevelWarn, "salvage_command_skip error=%v", err)
					continue
				}
				if cmd.ID != "" {
					result.Commands = append(result.Commands, cmd)
				}
			}
			break
		}
	}
	return result
}

// salvageNotificationQueue attempts per-entry recovery from corrupted notification queue YAML.
func (qh *QueueHandler) salvageNotificationQueue(data []byte) model.NotificationQueue {
	result := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "queue_notification",
	}

	var node yamlv3.Node
	if err := yamlv3.Unmarshal(data, &node); err != nil {
		return result
	}

	if node.Kind != yamlv3.DocumentNode || len(node.Content) == 0 {
		return result
	}
	root := node.Content[0]
	if root.Kind != yamlv3.MappingNode {
		return result
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "notifications" {
			seqNode := root.Content[i+1]
			if seqNode.Kind != yamlv3.SequenceNode {
				break
			}
			for _, ntfNode := range seqNode.Content {
				ntfBytes, err := yamlv3.Marshal(ntfNode)
				if err != nil {
					continue
				}
				var ntf model.Notification
				if err := yamlv3.Unmarshal(ntfBytes, &ntf); err != nil {
					qh.log(core.LogLevelWarn, "salvage_notification_skip error=%v", err)
					continue
				}
				if ntf.ID != "" && ntf.CommandID != "" && ntf.SourceResultID != "" && model.ValidateNotificationType(ntf.Type) == nil {
					result.Notifications = append(result.Notifications, ntf)
				}
			}
			break
		}
	}
	return result
}

// cleanupQuarantine removes the oldest files when the quarantine directory exceeds the configured limit.
// Files are sorted by name (timestamp-based naming ensures chronological order).
func (qh *QueueHandler) cleanupQuarantine(quarantineDir string) {
	entries, err := os.ReadDir(quarantineDir)
	if err != nil {
		return
	}
	limit := qh.config.Limits.EffectiveMaxQuarantineFiles()
	if len(entries) <= limit {
		return
	}
	// os.ReadDir returns entries sorted by name; timestamp-based names ensure oldest first
	toRemove := len(entries) - limit
	for i := 0; i < toRemove; i++ {
		path := filepath.Join(quarantineDir, entries[i].Name())
		if err := os.Remove(path); err != nil {
			qh.log(core.LogLevelWarn, "quarantine_cleanup_failed path=%s error=%v", path, err)
		} else {
			qh.log(core.LogLevelDebug, "quarantine_cleanup_removed path=%s", path)
		}
	}
}

func (qh *QueueHandler) loadAllTaskQueues() map[string]*taskQueueEntry {
	queueDir := filepath.Join(qh.maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		qh.log(core.LogLevelWarn, "read_queue_dir error=%v", err)
		return nil
	}

	result := make(map[string]*taskQueueEntry)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		path := filepath.Join(queueDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			qh.log(core.LogLevelWarn, "load_task_queue file=%s error=%v", name, err)
			continue
		}

		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			qh.log(core.LogLevelError, "parse_task_queue file=%s error=%v", name, err)
			continue
		}

		result[path] = &taskQueueEntry{Queue: tq, Path: path}
	}
	return result
}

func (qh *QueueHandler) loadNotificationQueue() (model.NotificationQueue, string) {
	path := filepath.Join(qh.maestroDir, "queue", "orchestrator.yaml")
	var nq model.NotificationQueue

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			qh.log(core.LogLevelWarn, "load_notifications error=%v", err)
		}
		return nq, ""
	}

	if err := yamlv3.Unmarshal(data, &nq); err != nil {
		qh.log(core.LogLevelError, "parse_notifications error=%v path=%s", err, path)
		// Quarantine original corrupted file
		qh.quarantineFile(data, "orchestrator.yaml")
		// Attempt per-entry salvage via Node parsing
		salvaged := qh.salvageNotificationQueue(data)
		if len(salvaged.Notifications) > 0 {
			qh.log(core.LogLevelWarn, "notification_queue_salvaged recovered=%d path=%s", len(salvaged.Notifications), path)
			qh.lockMap.Lock("queue:orchestrator")
			writeErr := yamlutil.AtomicWrite(path, salvaged)
			qh.lockMap.Unlock("queue:orchestrator")
			if writeErr != nil {
				qh.log(core.LogLevelError, "write_salvaged_notifications error=%v", writeErr)
			}
			return salvaged, path
		}
		// No entries salvaged — reset to empty
		emptyNQ := model.NotificationQueue{
			SchemaVersion: 1,
			FileType:      "queue_notification",
		}
		qh.lockMap.Lock("queue:orchestrator")
		writeErr := yamlutil.AtomicWrite(path, emptyNQ)
		qh.lockMap.Unlock("queue:orchestrator")
		if writeErr != nil {
			qh.log(core.LogLevelError, "overwrite_corrupted_notifications error=%v", writeErr)
		} else {
			qh.log(core.LogLevelInfo, "corrupted_notifications_reset path=%s", path)
		}
		return emptyNQ, path
	}
	return nq, path
}

// loadPlannerSignalQueue loads .maestro/queue/planner_signals.yaml.
func (qh *QueueHandler) loadPlannerSignalQueue() (model.PlannerSignalQueue, string) {
	path := filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
	var sq model.PlannerSignalQueue

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			qh.log(core.LogLevelWarn, "load_planner_signals error=%v", err)
		}
		return sq, ""
	}

	if err := yamlv3.Unmarshal(data, &sq); err != nil {
		qh.log(core.LogLevelError, "parse_planner_signals error=%v", err)
		return sq, ""
	}
	return sq, path
}

// flushQueues writes dirty queues to disk atomically.
func (qh *QueueHandler) flushQueues(
	commandQueue model.CommandQueue, commandPath string, commandsDirty bool,
	taskQueues map[string]*taskQueueEntry, taskDirty map[string]bool,
	notificationQueue model.NotificationQueue, notificationPath string, notificationsDirty bool,
	signalQueue model.PlannerSignalQueue, signalPath string, signalsDirty bool,
) {
	if commandsDirty && commandPath != "" {
		if err := yamlutil.AtomicWrite(commandPath, commandQueue); err != nil {
			qh.log(core.LogLevelError, "write_commands error=%v", err)
		}
	}
	for queueFile, tq := range taskQueues {
		if taskDirty[queueFile] {
			if err := yamlutil.AtomicWrite(queueFile, tq.Queue); err != nil {
				qh.log(core.LogLevelError, "write_tasks file=%s error=%v", queueFile, err)
			}
		}
	}
	if notificationsDirty && notificationPath != "" {
		// lockMap is required here even though callers hold scanMu.Lock():
		// ResultHandler writes orchestrator.yaml via lockMap without scanMu,
		// so lockMap prevents concurrent atomic rewrites and lost updates.
		qh.lockMap.Lock("queue:orchestrator")
		if err := yamlutil.AtomicWrite(notificationPath, notificationQueue); err != nil {
			qh.log(core.LogLevelError, "write_notifications error=%v", err)
		}
		qh.lockMap.Unlock("queue:orchestrator")
	}
	if signalsDirty {
		p := signalPath
		if p == "" {
			p = filepath.Join(qh.maestroDir, "queue", "planner_signals.yaml")
		}
		if len(signalQueue.Signals) == 0 {
			_ = os.Remove(p)
		} else {
			if err := yamlutil.AtomicWrite(p, signalQueue); err != nil {
				qh.log(core.LogLevelError, "write_planner_signals error=%v", err)
			}
		}
	}
}

func safeStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// workerIDFromPath extracts the worker ID from a queue file path.
// e.g., "/path/to/queue/worker1.yaml" -> "worker1"
func workerIDFromPath(path string) string {
	base := filepath.Base(path) // "worker1.yaml"
	if !strings.HasPrefix(base, "worker") || !strings.HasSuffix(base, ".yaml") {
		return ""
	}
	return strings.TrimSuffix(base, ".yaml") // "worker1"
}
