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
func (qs *QueueStoreImpl) LoadCommandQueue() (model.CommandQueue, string, error) {
	path := commandQueuePath(qs.maestroDir)
	var cq model.CommandQueue

	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		if !os.IsNotExist(err) {
			qs.log(LogLevelWarn, "load_commands error=%v", err)
		}
		return cq, "", nil
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
			return salvaged, path, nil
		}
		// No entries salvaged — attempt backup recovery
		recovered, recoverErr := qs.recoverFromBackup(path, "planner.yaml")
		if recoverErr != nil {
			return cq, "", fmt.Errorf("command queue corrupted and recovery failed: %w", recoverErr)
		}
		if err := yamlv3.Unmarshal(recovered, &cq); err != nil {
			return cq, "", fmt.Errorf("command queue backup unmarshal failed: %w", err)
		}
		qs.log(LogLevelWarn, "command_queue_restored_from_backup path=%s", path)
		return cq, path, nil
	}
	return cq, path, nil
}

// LoadAllTaskQueues loads all worker task queues from .maestro/queue/worker*.yaml files.
func (qs *QueueStoreImpl) LoadAllTaskQueues() (map[string]*taskQueueEntry, error) {
	queueDir := queueDirPath(qs.maestroDir)
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		qs.log(LogLevelWarn, "read_queue_dir error=%v", err)
		return nil, nil
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
			// Quarantine original corrupted file
			qs.quarantineFile(data, name)
			// Attempt backup recovery
			recovered, recoverErr := qs.recoverFromBackup(path, name)
			if recoverErr != nil {
				return nil, fmt.Errorf("task queue %s corrupted and recovery failed: %w", name, recoverErr)
			}
			if err := yamlv3.Unmarshal(recovered, &tq); err != nil {
				return nil, fmt.Errorf("task queue %s backup unmarshal failed: %w", name, err)
			}
			qs.log(LogLevelWarn, "task_queue_restored_from_backup file=%s", name)
		}

		result[path] = &taskQueueEntry{Queue: tq, Path: path}
	}
	return result, nil
}

// LoadNotificationQueue loads the notification queue from .maestro/queue/orchestrator.yaml.
func (qs *QueueStoreImpl) LoadNotificationQueue() (model.NotificationQueue, string, error) {
	path := notificationQueuePath(qs.maestroDir)
	var nq model.NotificationQueue

	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		if !os.IsNotExist(err) {
			qs.log(LogLevelWarn, "load_notifications error=%v", err)
		}
		return nq, "", nil
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
			return salvaged, path, nil
		}
		// No entries salvaged — attempt backup recovery
		recovered, recoverErr := qs.recoverFromBackup(path, "orchestrator.yaml")
		if recoverErr != nil {
			return nq, "", fmt.Errorf("notification queue corrupted and recovery failed: %w", recoverErr)
		}
		if err := yamlv3.Unmarshal(recovered, &nq); err != nil {
			return nq, "", fmt.Errorf("notification queue backup unmarshal failed: %w", err)
		}
		qs.log(LogLevelWarn, "notification_queue_restored_from_backup path=%s", path)
		return nq, path, nil
	}
	return nq, path, nil
}

// LoadPlannerSignalQueue loads .maestro/queue/planner_signals.yaml.
func (qs *QueueStoreImpl) LoadPlannerSignalQueue() (model.PlannerSignalQueue, string, error) {
	path := signalQueuePath(qs.maestroDir)
	var sq model.PlannerSignalQueue

	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		if !os.IsNotExist(err) {
			qs.log(LogLevelWarn, "load_planner_signals error=%v", err)
		}
		return sq, "", nil
	}

	if err := yamlv3.Unmarshal(data, &sq); err != nil {
		qs.log(LogLevelError, "parse_planner_signals error=%v path=%s", err, path)
		// Quarantine original corrupted file
		qs.quarantineFile(data, "planner_signals.yaml")
		// Attempt backup recovery
		recovered, recoverErr := qs.recoverFromBackup(path, "planner_signals.yaml")
		if recoverErr != nil {
			return sq, "", fmt.Errorf("planner signal queue corrupted and recovery failed: %w", recoverErr)
		}
		if err := yamlv3.Unmarshal(recovered, &sq); err != nil {
			return sq, "", fmt.Errorf("planner signal queue backup unmarshal failed: %w", err)
		}
		qs.log(LogLevelWarn, "planner_signal_queue_restored_from_backup path=%s", path)
		return sq, path, nil
	}
	return sq, path, nil
}

// recoverFromBackup attempts to restore a queue file from its .bak backup.
// Returns the raw backup content on success, or an error if no backup exists
// or the backup is also corrupted.
func (qs *QueueStoreImpl) recoverFromBackup(path, name string) ([]byte, error) {
	if err := yamlutil.RestoreFromBackup(path); err != nil {
		qs.log(LogLevelError, "backup_recovery_failed file=%s error=%v", name, err)
		return nil, fmt.Errorf("backup recovery for %s: %w", name, err)
	}
	restored, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		return nil, fmt.Errorf("read restored file %s: %w", name, err)
	}
	return restored, nil
}

// FlushQueues writes dirty queues to disk atomically.
//
// Lock order (see doc.go): queue:{worker} is level 1, so all queue:* locks
// are peers. Within FlushQueues the acquisition order is:
//   queue:planner → queue:{worker}… → queue:orchestrator → queue:planner_signals
// This is consistent with the write-handler ordering (queue_write_handler.go).
func (qs *QueueStoreImpl) FlushQueues(
	commandQueue model.CommandQueue, commandPath string, commandsDirty bool,
	taskQueues map[string]*taskQueueEntry, taskDirty map[string]bool,
	notificationQueue model.NotificationQueue, notificationPath string, notificationsDirty bool,
	signalQueue model.PlannerSignalQueue, signalPath string, signalsDirty bool,
) {
	if commandsDirty && commandPath != "" {
		// lockMap protects against concurrent writes from queue_write_command
		// which acquires queue:planner via withQueueLocks.
		func() {
			qs.lockMap.Lock("queue:planner")
			defer qs.lockMap.Unlock("queue:planner")
			if err := yamlutil.AtomicWrite(commandPath, commandQueue); err != nil {
				qs.log(LogLevelError, "write_commands error=%v", err)
			}
		}()
	}
	for queueFile, tq := range taskQueues {
		if taskDirty[queueFile] {
			// lockMap protects against concurrent writes from queue_write_task
			// which acquires queue:{workerID} via withQueueLocks.
			func() {
				workerID := workerIDFromPath(queueFile)
				if workerID == "" {
					qs.log(LogLevelError, "write_tasks cannot determine worker ID from path=%s", queueFile)
					return
				}
				qs.lockMap.Lock("queue:" + workerID)
				defer qs.lockMap.Unlock("queue:" + workerID)
				if err := yamlutil.AtomicWrite(queueFile, tq.Queue); err != nil {
					qs.log(LogLevelError, "write_tasks file=%s error=%v", queueFile, err)
				}
			}()
		}
	}
	if notificationsDirty && notificationPath != "" {
		// lockMap is required here even though callers hold scanMu.Lock():
		// ResultHandler writes orchestrator.yaml via lockMap without scanMu,
		// so lockMap prevents concurrent atomic rewrites and lost updates.
		// Uses a closure+defer to match the lockMap pattern used by worker
		// queue writes (withQueueLocks), ensuring Unlock on panic.
		func() {
			qs.lockMap.Lock("queue:orchestrator")
			defer qs.lockMap.Unlock("queue:orchestrator")
			if err := yamlutil.AtomicWrite(notificationPath, notificationQueue); err != nil {
				qs.log(LogLevelError, "write_notifications error=%v", err)
			}
		}()
	}
	if signalsDirty {
		p := signalPath
		if p == "" {
			p = signalQueuePath(qs.maestroDir)
		}
		// lockMap protects against concurrent writes from signal_store_yaml
		// which acquires queue:planner_signals.
		func() {
			qs.lockMap.Lock("queue:planner_signals")
			defer qs.lockMap.Unlock("queue:planner_signals")
			if len(signalQueue.Signals) == 0 {
				_ = os.Remove(p)
			} else {
				if err := yamlutil.AtomicWrite(p, signalQueue); err != nil {
					qs.log(LogLevelError, "write_planner_signals error=%v", err)
				}
			}
		}()
	}
}

// quarantineFile saves corrupted file data to the quarantine directory for later inspection.
func (qs *QueueStoreImpl) quarantineFile(data []byte, name string) {
	quarantineDir := quarantineDirPath(qs.maestroDir)
	if err := os.MkdirAll(quarantineDir, 0755); err != nil { //nolint:gosec // 0755 is appropriate for a quarantine directory
		qs.log(LogLevelError, "create_quarantine_dir error=%v", err)
		return
	}
	ts := qs.clock.Now().UTC().Format("20060102T150405.000000000Z")
	qPath := filepath.Join(quarantineDir, fmt.Sprintf("%s_%s", ts, name))
	if err := os.WriteFile(qPath, data, 0600); err != nil { //nolint:gosec // qPath is constructed from a validated quarantine directory and a timestamp
		qs.log(LogLevelError, "quarantine_write error=%v", err)
	} else {
		qs.log(LogLevelInfo, "quarantined file=%s to=%s", name, qPath)
	}
	qs.cleanupQuarantine(quarantineDir)
}

// salvageQueueEntries is a generic helper that recovers individual entries from
// a corrupted YAML queue file by parsing the YAML node tree. It locates the
// sequence node identified by listKey, attempts to unmarshal each entry
// independently, validates it via the validate callback, and collects valid
// entries. Invalid entries are logged via logSkip. This eliminates near-
// identical logic previously duplicated in salvageCommandQueue and
// salvageNotificationQueue.
func salvageQueueEntries[T any](data []byte, listKey string, validate func(*T) bool, logSkip func(error), qs *QueueStoreImpl) []T {
	var node yamlv3.Node
	if err := yamlv3.Unmarshal(data, &node); err != nil {
		return nil
	}

	if node.Kind != yamlv3.DocumentNode || len(node.Content) == 0 {
		return nil
	}
	root := node.Content[0]
	if root.Kind != yamlv3.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == listKey {
			seqNode := root.Content[i+1]
			if seqNode.Kind != yamlv3.SequenceNode {
				break
			}
			var results []T
			for _, entryNode := range seqNode.Content {
				entryBytes, err := yamlv3.Marshal(entryNode)
				if err != nil {
					continue
				}
				var entry T
				if err := yamlv3.Unmarshal(entryBytes, &entry); err != nil {
					logSkip(err)
					continue
				}
				if validate(&entry) {
					results = append(results, entry)
				}
			}
			return results
		}
	}
	return nil
}

// salvageCommandQueue attempts per-entry recovery from corrupted command queue YAML.
func (qs *QueueStoreImpl) salvageCommandQueue(data []byte) model.CommandQueue {
	result := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
	}
	result.Commands = salvageQueueEntries(data, "commands",
		func(cmd *model.Command) bool { return cmd.ID != "" },
		func(err error) { qs.log(LogLevelWarn, "salvage_command_skip error=%v", err) },
		qs,
	)
	return result
}

// salvageNotificationQueue attempts per-entry recovery from corrupted notification queue YAML.
func (qs *QueueStoreImpl) salvageNotificationQueue(data []byte) model.NotificationQueue {
	result := model.NotificationQueue{
		SchemaVersion: 1,
		FileType:      "queue_notification",
	}
	result.Notifications = salvageQueueEntries(data, "notifications",
		func(ntf *model.Notification) bool {
			return ntf.ID != "" && ntf.CommandID != "" && ntf.SourceResultID != "" && model.ValidateNotificationType(ntf.Type) == nil
		},
		func(err error) { qs.log(LogLevelWarn, "salvage_notification_skip error=%v", err) },
		qs,
	)
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
