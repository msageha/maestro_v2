package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// QueueStoreImpl provides queue file I/O operations: loading, flushing,
// quarantine recovery, and salvaging corrupted YAML entries.
type QueueStoreImpl struct {
	maestroDir string
	config     model.Config
	clock      Clock
	lockMap    *lock.MutexMap
	dl         *DaemonLogger
}

// NewQueueStore creates a new QueueStoreImpl.
func NewQueueStore(maestroDir string, cfg model.Config, clock Clock, lockMap *lock.MutexMap, dl *DaemonLogger) *QueueStoreImpl {
	return &QueueStoreImpl{
		maestroDir: maestroDir,
		config:     cfg,
		clock:      clock,
		lockMap:    lockMap,
		dl:         dl,
	}
}

// LoadCommandQueue loads the command queue from .maestro/queue/planner.yaml.
func (qs *QueueStoreImpl) LoadCommandQueue() (model.CommandQueue, string) {
	path := filepath.Join(qs.maestroDir, "queue", "planner.yaml")
	var cq model.CommandQueue

	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		if !os.IsNotExist(err) {
			qs.log(LogLevelWarn, "load_commands error=%v", err)
		}
		return cq, ""
	}

	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		qs.log(LogLevelError, "parse_commands error=%v path=%s", err, path)
		// Quarantine original corrupted file
		qs.quarantineFile(data, "planner.yaml")
		// Attempt per-entry salvage via Node parsing
		salvaged := qs.salvageCommandQueue(data)
		if len(salvaged.Commands) > 0 {
			qs.log(LogLevelWarn, "command_queue_salvaged recovered=%d path=%s", len(salvaged.Commands), path)
			if writeErr := yamlutil.AtomicWrite(path, salvaged); writeErr != nil {
				qs.log(LogLevelError, "write_salvaged_queue error=%v", writeErr)
			}
			return salvaged, path
		}
		// No entries salvaged — reset to empty
		emptyCQ := model.CommandQueue{
			SchemaVersion: 1,
			FileType:      "command_queue",
		}
		if writeErr := yamlutil.AtomicWrite(path, emptyCQ); writeErr != nil {
			qs.log(LogLevelError, "overwrite_corrupted_queue error=%v", writeErr)
		} else {
			qs.log(LogLevelInfo, "corrupted_queue_reset path=%s", path)
		}
		return cq, ""
	}
	return cq, path
}

// LoadAllTaskQueues loads all worker task queues from .maestro/queue/worker*.yaml files.
func (qs *QueueStoreImpl) LoadAllTaskQueues() map[string]*taskQueueEntry {
	queueDir := filepath.Join(qs.maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		qs.log(LogLevelWarn, "read_queue_dir error=%v", err)
		return nil
	}

	result := make(map[string]*taskQueueEntry)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "worker") || !strings.HasSuffix(name, ".yaml") {
			continue
		}

		path := filepath.Join(queueDir, name)
		data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
		if err != nil {
			qs.log(LogLevelWarn, "load_task_queue file=%s error=%v", name, err)
			continue
		}

		var tq model.TaskQueue
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
			qs.log(LogLevelError, "parse_task_queue file=%s error=%v", name, err)
			continue
		}

		result[path] = &taskQueueEntry{Queue: tq, Path: path}
	}
	return result
}

// LoadNotificationQueue loads the notification queue from .maestro/queue/orchestrator.yaml.
func (qs *QueueStoreImpl) LoadNotificationQueue() (model.NotificationQueue, string) {
	path := filepath.Join(qs.maestroDir, "queue", "orchestrator.yaml")
	var nq model.NotificationQueue

	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		if !os.IsNotExist(err) {
			qs.log(LogLevelWarn, "load_notifications error=%v", err)
		}
		return nq, ""
	}

	if err := yamlv3.Unmarshal(data, &nq); err != nil {
		qs.log(LogLevelError, "parse_notifications error=%v path=%s", err, path)
		// Quarantine original corrupted file
		qs.quarantineFile(data, "orchestrator.yaml")
		// Attempt per-entry salvage via Node parsing
		salvaged := qs.salvageNotificationQueue(data)
		if len(salvaged.Notifications) > 0 {
			qs.log(LogLevelWarn, "notification_queue_salvaged recovered=%d path=%s", len(salvaged.Notifications), path)
			qs.lockMap.Lock("queue:orchestrator")
			writeErr := yamlutil.AtomicWrite(path, salvaged)
			qs.lockMap.Unlock("queue:orchestrator")
			if writeErr != nil {
				qs.log(LogLevelError, "write_salvaged_notifications error=%v", writeErr)
			}
			return salvaged, path
		}
		// No entries salvaged — reset to empty
		emptyNQ := model.NotificationQueue{
			SchemaVersion: 1,
			FileType:      "queue_notification",
		}
		qs.lockMap.Lock("queue:orchestrator")
		writeErr := yamlutil.AtomicWrite(path, emptyNQ)
		qs.lockMap.Unlock("queue:orchestrator")
		if writeErr != nil {
			qs.log(LogLevelError, "overwrite_corrupted_notifications error=%v", writeErr)
		} else {
			qs.log(LogLevelInfo, "corrupted_notifications_reset path=%s", path)
		}
		return emptyNQ, path
	}
	return nq, path
}

// LoadPlannerSignalQueue loads .maestro/queue/planner_signals.yaml.
func (qs *QueueStoreImpl) LoadPlannerSignalQueue() (model.PlannerSignalQueue, string) {
	path := filepath.Join(qs.maestroDir, "queue", "planner_signals.yaml")
	var sq model.PlannerSignalQueue

	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		if !os.IsNotExist(err) {
			qs.log(LogLevelWarn, "load_planner_signals error=%v", err)
		}
		return sq, ""
	}

	if err := yamlv3.Unmarshal(data, &sq); err != nil {
		qs.log(LogLevelError, "parse_planner_signals error=%v", err)
		return sq, ""
	}
	return sq, path
}

// FlushQueues writes dirty queues to disk atomically.
func (qs *QueueStoreImpl) FlushQueues(
	commandQueue model.CommandQueue, commandPath string, commandsDirty bool,
	taskQueues map[string]*taskQueueEntry, taskDirty map[string]bool,
	notificationQueue model.NotificationQueue, notificationPath string, notificationsDirty bool,
	signalQueue model.PlannerSignalQueue, signalPath string, signalsDirty bool,
) {
	if commandsDirty && commandPath != "" {
		if err := yamlutil.AtomicWrite(commandPath, commandQueue); err != nil {
			qs.log(LogLevelError, "write_commands error=%v", err)
		}
	}
	for queueFile, tq := range taskQueues {
		if taskDirty[queueFile] {
			if err := yamlutil.AtomicWrite(queueFile, tq.Queue); err != nil {
				qs.log(LogLevelError, "write_tasks file=%s error=%v", queueFile, err)
			}
		}
	}
	if notificationsDirty && notificationPath != "" {
		// lockMap is required here even though callers hold scanMu.Lock():
		// ResultHandler writes orchestrator.yaml via lockMap without scanMu,
		// so lockMap prevents concurrent atomic rewrites and lost updates.
		qs.lockMap.Lock("queue:orchestrator")
		if err := yamlutil.AtomicWrite(notificationPath, notificationQueue); err != nil {
			qs.log(LogLevelError, "write_notifications error=%v", err)
		}
		qs.lockMap.Unlock("queue:orchestrator")
	}
	if signalsDirty {
		p := signalPath
		if p == "" {
			p = filepath.Join(qs.maestroDir, "queue", "planner_signals.yaml")
		}
		if len(signalQueue.Signals) == 0 {
			_ = os.Remove(p)
		} else {
			if err := yamlutil.AtomicWrite(p, signalQueue); err != nil {
				qs.log(LogLevelError, "write_planner_signals error=%v", err)
			}
		}
	}
}

// quarantineFile saves corrupted file data to the quarantine directory for later inspection.
func (qs *QueueStoreImpl) quarantineFile(data []byte, name string) {
	quarantineDir := filepath.Join(qs.maestroDir, "quarantine")
	if err := os.MkdirAll(quarantineDir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for a quarantine directory
		qs.log(LogLevelError, "create_quarantine_dir error=%v", err)
		return
	}
	ts := qs.clock.Now().UTC().Format("20060102T150405Z")
	qPath := filepath.Join(quarantineDir, fmt.Sprintf("%s_%s", ts, name))
	if err := os.WriteFile(qPath, data, 0600); err != nil { //nolint:gosec // qPath is constructed from a validated quarantine directory and a timestamp
		qs.log(LogLevelError, "quarantine_write error=%v", err)
	} else {
		qs.log(LogLevelInfo, "quarantined file=%s to=%s", name, qPath)
	}
	qs.cleanupQuarantine(quarantineDir)
}

// salvageCommandQueue attempts per-entry recovery from corrupted command queue YAML.
func (qs *QueueStoreImpl) salvageCommandQueue(data []byte) model.CommandQueue {
	result := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
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
					qs.log(LogLevelWarn, "salvage_command_skip error=%v", err)
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
func (qs *QueueStoreImpl) salvageNotificationQueue(data []byte) model.NotificationQueue {
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
					qs.log(LogLevelWarn, "salvage_notification_skip error=%v", err)
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
func (qs *QueueStoreImpl) cleanupQuarantine(quarantineDir string) {
	entries, err := os.ReadDir(quarantineDir)
	if err != nil {
		return
	}
	limit := qs.config.Limits.EffectiveMaxQuarantineFiles()
	if len(entries) <= limit {
		return
	}
	// os.ReadDir returns entries sorted by name; timestamp-based names ensure oldest first
	toRemove := len(entries) - limit
	for i := 0; i < toRemove; i++ {
		path := filepath.Join(quarantineDir, entries[i].Name())
		if err := os.Remove(path); err != nil {
			qs.log(LogLevelWarn, "quarantine_cleanup_failed path=%s error=%v", path, err)
		} else {
			qs.log(LogLevelDebug, "quarantine_cleanup_removed path=%s", path)
		}
	}
}

func (qs *QueueStoreImpl) log(level LogLevel, format string, args ...any) {
	qs.dl.Logf(level, format, args...)
}
