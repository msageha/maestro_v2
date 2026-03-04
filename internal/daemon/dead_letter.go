package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// maxDeadLetterArchiveFiles is the maximum number of dead letter archive files to retain.
const maxDeadLetterArchiveFiles = 100

// DeadLetterProcessor handles queue entries that have exceeded max retry attempts.
type DeadLetterProcessor struct {
	maestroDir string
	config     model.Config
	lockMap    *lock.MutexMap
	dl         *DaemonLogger
	logger     *log.Logger
	logLevel   LogLevel
	clock      Clock

	// pendingNotifications collects orchestrator notifications generated
	// during command dead-letter post-processing. They are buffered here
	// instead of being written to disk to avoid a race with the in-memory
	// notification queue flush in PeriodicScan.
	pendingNotifications []model.Notification
}

// DeadLetterResult describes a single dead-letter action.
type DeadLetterResult struct {
	QueueType string
	EntryID   string
	CommandID string
	TaskID    string
	Reason    string
}

// NewDeadLetterProcessor creates a new DeadLetterProcessor.
func NewDeadLetterProcessor(
	maestroDir string,
	cfg model.Config,
	lockMap *lock.MutexMap,
	logger *log.Logger,
	logLevel LogLevel,
) *DeadLetterProcessor {
	return &DeadLetterProcessor{
		maestroDir: maestroDir,
		config:     cfg,
		lockMap:    lockMap,
		dl:         NewDaemonLoggerFromLegacy("dead_letter", logger, logLevel),
		logger:     logger,
		logLevel:   logLevel,
		clock:      RealClock{},
	}
}

// DrainPendingNotifications returns and clears buffered orchestrator notifications
// generated during command dead-letter post-processing. The caller should append
// these to the in-memory notification queue.
func (dlp *DeadLetterProcessor) DrainPendingNotifications() []model.Notification {
	result := dlp.pendingNotifications
	dlp.pendingNotifications = nil
	return result
}

// ProcessCommandDeadLetters checks planner queue for dead-letter candidates.
// Mutates the in-memory CommandQueue and marks dirty if modified.
func (dlp *DeadLetterProcessor) ProcessCommandDeadLetters(cq *model.CommandQueue, dirty *bool) []DeadLetterResult {
	maxAttempts := dlp.config.Retry.CommandDispatch
	if maxAttempts <= 0 {
		return nil
	}

	var results []DeadLetterResult
	var kept []model.Command

	for i := range cq.Commands {
		cmd := &cq.Commands[i]
		if cmd.Status != model.StatusPending || cmd.Attempts < maxAttempts {
			kept = append(kept, *cmd)
			continue
		}

		now := dlp.clock.Now().UTC().Format(time.RFC3339)
		reason := fmt.Sprintf("attempts (%d) >= max_attempts (%d) for command_dispatch", cmd.Attempts, maxAttempts)

		cmd.Status = model.StatusDeadLetter
		cmd.DeadLetteredAt = &now
		cmd.DeadLetterReason = &reason

		// Archive
		if err := dlp.archiveDeadLetter("planner", cmd.ID, cmd, reason); err != nil {
			dlp.log(LogLevelError, "archive_dead_letter planner command=%s error=%v", cmd.ID, err)
			dlp.log(LogLevelWarn, "archive_failed_entry_details queue=planner command=%s status=%s attempts=%d reason=%s",
				cmd.ID, cmd.Status, cmd.Attempts, reason)
		}

		// Post-processing: update state if exists
		dlp.commandDeadLetterPostProcess(cmd.ID, reason)

		results = append(results, DeadLetterResult{
			QueueType: "planner",
			EntryID:   cmd.ID,
			CommandID: cmd.ID,
			Reason:    reason,
		})

		dlp.log(LogLevelWarn, "dead_letter planner command=%s attempts=%d reason=%s", cmd.ID, cmd.Attempts, reason)
	}

	if len(results) > 0 {
		cq.Commands = kept
		*dirty = true
	}
	return results
}

// ProcessTaskDeadLetters checks a worker task queue for dead-letter candidates.
func (dlp *DeadLetterProcessor) ProcessTaskDeadLetters(tq *taskQueueEntry, dirty *bool) []DeadLetterResult {
	maxAttempts := dlp.config.Retry.TaskDispatch
	if maxAttempts <= 0 {
		return nil
	}

	var results []DeadLetterResult
	var kept []model.Task

	workerID := workerIDFromPath(tq.Path)

	for i := range tq.Queue.Tasks {
		task := &tq.Queue.Tasks[i]
		if task.Status != model.StatusPending || task.Attempts < maxAttempts {
			kept = append(kept, *task)
			continue
		}

		now := dlp.clock.Now().UTC().Format(time.RFC3339)
		reason := fmt.Sprintf("attempts (%d) >= max_attempts (%d) for task_dispatch", task.Attempts, maxAttempts)

		task.Status = model.StatusDeadLetter
		task.DeadLetteredAt = &now
		task.DeadLetterReason = &reason

		// Archive
		if err := dlp.archiveDeadLetter(workerID, task.ID, task, reason); err != nil {
			dlp.log(LogLevelError, "archive_dead_letter %s task=%s error=%v", workerID, task.ID, err)
			dlp.log(LogLevelWarn, "archive_failed_entry_details queue=%s task=%s command=%s status=%s attempts=%d reason=%s",
				workerID, task.ID, task.CommandID, task.Status, task.Attempts, reason)
		}

		// Post-processing: update state + synthetic result
		dlp.taskDeadLetterPostProcess(task.CommandID, task.ID, workerID)

		results = append(results, DeadLetterResult{
			QueueType: workerID,
			EntryID:   task.ID,
			CommandID: task.CommandID,
			TaskID:    task.ID,
			Reason:    reason,
		})

		dlp.log(LogLevelWarn, "dead_letter %s task=%s command=%s attempts=%d", workerID, task.ID, task.CommandID, task.Attempts)
	}

	if len(results) > 0 {
		tq.Queue.Tasks = kept
		*dirty = true
	}
	return results
}

// ProcessNotificationDeadLetters checks orchestrator notification queue for dead-letter candidates.
func (dlp *DeadLetterProcessor) ProcessNotificationDeadLetters(nq *model.NotificationQueue, dirty *bool) []DeadLetterResult {
	maxAttempts := dlp.config.Retry.OrchestratorNotificationDispatch
	if maxAttempts <= 0 {
		return nil
	}

	var results []DeadLetterResult
	var kept []model.Notification

	for i := range nq.Notifications {
		ntf := &nq.Notifications[i]
		if ntf.Status != model.StatusPending || ntf.Attempts < maxAttempts {
			kept = append(kept, *ntf)
			continue
		}

		now := dlp.clock.Now().UTC().Format(time.RFC3339)
		reason := fmt.Sprintf("attempts (%d) >= max_attempts (%d) for orchestrator_notification_dispatch", ntf.Attempts, maxAttempts)

		ntf.Status = model.StatusDeadLetter
		ntf.DeadLetteredAt = &now
		ntf.DeadLetterReason = &reason

		// Archive
		if err := dlp.archiveDeadLetter("orchestrator", ntf.ID, ntf, reason); err != nil {
			dlp.log(LogLevelError, "archive_dead_letter orchestrator notification=%s error=%v", ntf.ID, err)
			dlp.log(LogLevelWarn, "archive_failed_entry_details queue=orchestrator notification=%s command=%s status=%s attempts=%d reason=%s",
				ntf.ID, ntf.CommandID, ntf.Status, ntf.Attempts, reason)
		}

		results = append(results, DeadLetterResult{
			QueueType: "orchestrator",
			EntryID:   ntf.ID,
			CommandID: ntf.CommandID,
			Reason:    reason,
		})

		dlp.log(LogLevelWarn, "dead_letter orchestrator notification=%s command=%s attempts=%d", ntf.ID, ntf.CommandID, ntf.Attempts)
	}

	if len(results) > 0 {
		nq.Notifications = kept
		*dirty = true
	}
	return results
}

// archiveDeadLetter writes a dead-letter archive file.
// entryID is included in the filename to prevent same-second collisions.
func (dlp *DeadLetterProcessor) archiveDeadLetter(queueType string, entryID string, entry interface{}, reason string) error {
	archiveDir := filepath.Join(dlp.maestroDir, "dead_letters")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return fmt.Errorf("create dead_letters dir: %w", err)
	}

	type archiveEntry struct {
		SchemaVersion  int         `yaml:"schema_version"`
		FileType       string      `yaml:"file_type"`
		QueueType      string      `yaml:"queue_type"`
		Entry          interface{} `yaml:"entry"`
		DeadLetteredAt string      `yaml:"dead_lettered_at"`
		Reason         string      `yaml:"reason"`
	}

	now := dlp.clock.Now().UTC()
	archive := archiveEntry{
		SchemaVersion:  1,
		FileType:       "dead_letter",
		QueueType:      queueType,
		Entry:          entry,
		DeadLetteredAt: now.Format(time.RFC3339),
		Reason:         reason,
	}

	filename := fmt.Sprintf("%s_%s_%s.yaml", queueType, now.Format("20060102T150405Z"), entryID)
	archivePath := filepath.Join(archiveDir, filename)

	if err := yamlutil.AtomicWrite(archivePath, archive); err != nil {
		return err
	}

	dlp.pruneDeadLetterArchives(archiveDir)
	return nil
}

// pruneDeadLetterArchives removes the oldest archive files when the count exceeds the limit.
// Uses file modification time for ordering to correctly handle mixed queue type prefixes.
func (dlp *DeadLetterProcessor) pruneDeadLetterArchives(archiveDir string) {
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return
	}

	type fileWithTime struct {
		name    string
		modTime time.Time
	}

	var yamlFiles []fileWithTime
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		yamlFiles = append(yamlFiles, fileWithTime{name: e.Name(), modTime: info.ModTime()})
	}

	if len(yamlFiles) <= maxDeadLetterArchiveFiles {
		return
	}

	// Sort by modification time ascending — oldest first
	sort.Slice(yamlFiles, func(i, j int) bool {
		return yamlFiles[i].modTime.Before(yamlFiles[j].modTime)
	})

	toRemove := len(yamlFiles) - maxDeadLetterArchiveFiles
	for i := 0; i < toRemove; i++ {
		path := filepath.Join(archiveDir, yamlFiles[i].name)
		if err := os.Remove(path); err != nil {
			dlp.log(LogLevelWarn, "prune_dead_letter_archive file=%s error=%v", yamlFiles[i].name, err)
		}
	}

	dlp.log(LogLevelInfo, "pruned_dead_letter_archives removed=%d remaining=%d", toRemove, maxDeadLetterArchiveFiles)
}

// commandDeadLetterPostProcess updates state for a dead-lettered command.
func (dlp *DeadLetterProcessor) commandDeadLetterPostProcess(commandID, reason string) {
	statePath := filepath.Join(dlp.maestroDir, "state", "commands", commandID+".yaml")

	lockKey := "state:" + commandID
	dlp.lockMap.Lock(lockKey)
	defer dlp.lockMap.Unlock(lockKey)

	data, err := os.ReadFile(statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			dlp.log(LogLevelError, "dead_letter_post_process read_state command=%s error=%v", commandID, err)
		}
		// State missing or unreadable — still notify orchestrator
		dlp.bufferDeadLetterOrchestratorNotification(commandID, reason)
		return
	}

	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		dlp.log(LogLevelError, "dead_letter_post_process parse_state command=%s error=%v", commandID, err)
		// Parse failure — still notify orchestrator
		dlp.bufferDeadLetterOrchestratorNotification(commandID, reason)
		return
	}

	if model.IsPlanTerminal(state.PlanStatus) {
		return
	}

	state.PlanStatus = model.PlanStatusFailed
	now := dlp.clock.Now().UTC().Format(time.RFC3339)
	state.UpdatedAt = now
	if err := yamlutil.AtomicWrite(statePath, &state); err != nil {
		dlp.log(LogLevelError, "dead_letter_state_update command=%s error=%v", commandID, err)
	}

	dlp.bufferDeadLetterOrchestratorNotification(commandID, reason)
}

// bufferDeadLetterOrchestratorNotification creates a command_failed notification
// for a dead-lettered command and buffers it for later appending to the in-memory
// notification queue (see DrainPendingNotifications).
func (dlp *DeadLetterProcessor) bufferDeadLetterOrchestratorNotification(commandID, reason string) {
	id, err := model.GenerateID(model.IDTypeNotification)
	if err != nil {
		dlp.log(LogLevelError, "dead_letter_generate_notification_id error=%v", err)
		return
	}

	sourceResultID := fmt.Sprintf("res_dl_%s", commandID)

	now := dlp.clock.Now().UTC().Format(time.RFC3339)
	dlp.pendingNotifications = append(dlp.pendingNotifications, model.Notification{
		ID:             id,
		CommandID:      commandID,
		Type:           "command_failed",
		SourceResultID: sourceResultID,
		Content:        fmt.Sprintf("command %s dead-lettered: %s", commandID, reason),
		Priority:       100,
		Status:         model.StatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
}

// taskDeadLetterPostProcess updates state and writes synthetic result for a dead-lettered task.
//
// Lock ordering: state:{commandID} → result:{workerID}
// The state lock is held until the synthetic result is written to prevent a
// TOCTOU race where Complete() could aggregate results before the synthetic
// result exists (CR-013).
func (dlp *DeadLetterProcessor) taskDeadLetterPostProcess(commandID, taskID, workerID string) {
	statePath := filepath.Join(dlp.maestroDir, "state", "commands", commandID+".yaml")

	// Acquire state lock first (held through both state update and result write).
	lockKey := "state:" + commandID
	dlp.lockMap.Lock(lockKey)
	defer dlp.lockMap.Unlock(lockKey)

	// Phase 1: Update state — task_states[taskID] = failed
	data, err := os.ReadFile(statePath)
	if err == nil {
		var state model.CommandState
		if err := yamlv3.Unmarshal(data, &state); err == nil {
			if state.TaskStates == nil {
				state.TaskStates = make(map[string]model.Status)
			}
			if !model.IsTerminal(state.TaskStates[taskID]) {
				state.TaskStates[taskID] = model.StatusFailed
				now := dlp.clock.Now().UTC().Format(time.RFC3339)
				state.UpdatedAt = now
				if err := yamlutil.AtomicWrite(statePath, &state); err != nil {
					dlp.log(LogLevelError, "dead_letter_state_task_update command=%s task=%s error=%v", commandID, taskID, err)
				}
			}
		}
	}

	// Phase 2: Write synthetic failed result to results/worker{N}.yaml
	// Lock order: state:{commandID} (held above) → result:{workerID}
	resultPath := filepath.Join(dlp.maestroDir, "results", workerID+".yaml")

	resultLockKey := "result:" + workerID
	dlp.lockMap.Lock(resultLockKey)
	defer dlp.lockMap.Unlock(resultLockKey)

	var rf model.TaskResultFile
	resultData, err := os.ReadFile(resultPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return
		}
		rf.SchemaVersion = 1
		rf.FileType = "result_task"
	} else {
		if err := yamlv3.Unmarshal(resultData, &rf); err != nil {
			return
		}
	}

	// Check idempotency (don't add duplicate synthetic result)
	for _, r := range rf.Results {
		if r.TaskID == taskID && model.IsTerminal(r.Status) {
			return
		}
	}

	resID, err := model.GenerateID(model.IDTypeResult)
	if err != nil {
		return
	}

	now := dlp.clock.Now().UTC().Format(time.RFC3339)
	rf.Results = append(rf.Results, model.TaskResult{
		ID:        resID,
		TaskID:    taskID,
		CommandID: commandID,
		Status:    model.StatusFailed,
		Summary:   "dead-lettered: dispatch retry exhausted",
		CreatedAt: now,
	})

	if err := yamlutil.AtomicWrite(resultPath, rf); err != nil {
		dlp.log(LogLevelError, "dead_letter_synthetic_result worker=%s task=%s error=%v", workerID, taskID, err)
	}
}

func (dlp *DeadLetterProcessor) log(level LogLevel, format string, args ...any) {
	dlp.dl.Logf(level, format, args...)
}
