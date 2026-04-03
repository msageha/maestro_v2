package daemon

import (
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

const (
	// maxNotifyAttempts is the maximum number of notification delivery attempts
	// before giving up. Prevents infinite retries when tmux is unavailable.
	maxNotifyAttempts = 5

	// notifyBackoffInitial is the initial backoff delay after a failed notification.
	// Exponential backoff: 1s → 2s → 4s → 8s → 16s (capped at notifyBackoffMax).
	notifyBackoffInitial = 1 * time.Second

	// notifyBackoffMax is the maximum backoff delay between retry attempts.
	notifyBackoffMax = 30 * time.Second

	// maxResultLoopIterations is the maximum number of iterations allowed per
	// processResultFile call. Prevents unbounded CPU/memory consumption when
	// results are added faster than they are processed.
	// Remaining results will be handled on the next scan cycle.
	maxResultLoopIterations = 1000

	// maxAtomicWriteFailures is the maximum number of consecutive AtomicWrite
	// failures before the processing loop aborts. Prevents infinite loops when
	// persistent write errors occur (e.g., disk full).
	maxAtomicWriteFailures = 3
)

// ResultHandler monitors results/ and delivers notifications to agents.
// Worker results → Planner (side-channel via agent_executor).
// Planner results → Orchestrator (queue write).
type ResultHandler struct {
	maestroDir        string
	config            model.Config
	lockMap           *lock.MutexMap
	dl                *DaemonLogger
	logger            *log.Logger
	logLevel          LogLevel
	clock             Clock
	execProvider      *ExecutorProvider
	continuousHandler *ContinuousHandler
	eventBus          *events.Bus

	mu sync.RWMutex // protects continuousHandler, eventBus
}

// NewResultHandler creates a new ResultHandler with a shared ExecutorProvider.
func NewResultHandler(
	maestroDir string,
	cfg model.Config,
	lockMap *lock.MutexMap,
	logger *log.Logger,
	logLevel LogLevel,
	ep *ExecutorProvider,
) *ResultHandler {
	return &ResultHandler{
		maestroDir:   maestroDir,
		config:       cfg,
		lockMap:      lockMap,
		dl:           NewDaemonLoggerFromLegacy("result_handler", logger, logLevel),
		logger:       logger,
		logLevel:     logLevel,
		clock:        ep.clock,
		execProvider: ep,
	}
}

// SetExecutorFactory overrides the executor factory for testing.
// Resets the cached executor so the new factory is used on next call.
// This method exists solely for test injection; production code uses the factory
// provided at construction time.
func (rh *ResultHandler) SetExecutorFactory(f ExecutorFactory) {
	rh.execProvider.SetFactory(f)
}

// getExecutor returns the shared executor instance, creating it lazily on first call.
// The Executor is safe for concurrent use (log.Logger uses internal mutex,
// os.File in append mode is POSIX-safe, all other fields are immutable).
func (rh *ResultHandler) getExecutor() (AgentExecutor, error) {
	return rh.execProvider.GetExecutor()
}

// SetContinuousHandler wires the continuous handler for iteration tracking.
func (rh *ResultHandler) SetContinuousHandler(ch *ContinuousHandler) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.continuousHandler = ch
}

// SetEventBus sets the event bus for publishing events.
func (rh *ResultHandler) SetEventBus(bus *events.Bus) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.eventBus = bus
}

// getEventBus returns the event bus with proper synchronization.
func (rh *ResultHandler) getEventBus() *events.Bus {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	return rh.eventBus
}

// getContinuousHandler returns the continuous handler with proper synchronization.
func (rh *ResultHandler) getContinuousHandler() *ContinuousHandler {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	return rh.continuousHandler
}

// HandleResultFileEvent processes a single results/ file change from fsnotify.
// Called from QueueHandler.HandleFileEvent (NOT under fileMu).
func (rh *ResultHandler) HandleResultFileEvent(filePath string) {
	base := filepath.Base(filePath)
	if !strings.HasSuffix(base, ".yaml") {
		return
	}

	name := strings.TrimSuffix(base, ".yaml")
	if name == "planner" {
		n := rh.processCommandResultFile()
		if n > 0 {
			rh.log(LogLevelInfo, "result_event_notify file=%s notified=%d", base, n)
		}
	} else if strings.HasPrefix(name, "worker") {
		n := rh.processWorkerResultFile(name)
		if n > 0 {
			rh.log(LogLevelInfo, "result_event_notify file=%s notified=%d", base, n)
		}
	}
}

// ScanAllResults scans all results/ files for unnotified entries.
// Called from QueueHandler.PeriodicScan (step 2.5).
func (rh *ResultHandler) ScanAllResults() int {
	resultsDir := filepath.Join(rh.maestroDir, "results")
	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			rh.log(LogLevelWarn, "scan_results read_dir error=%v", err)
		}
		return 0
	}

	total := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		baseName := strings.TrimSuffix(name, ".yaml")
		if baseName == "planner" {
			total += rh.processCommandResultFile()
		} else if strings.HasPrefix(baseName, "worker") {
			total += rh.processWorkerResultFile(baseName)
		}
	}
	return total
}

// --- Generic result processing ---

// resultFileSpec defines the type-specific behavior for processResultFile.
type resultFileSpec[T any, PT interface {
	*T
	model.Notifiable
}, F any] struct {
	lockKey    string
	resultPath string
	label      string // entity label for log messages (e.g. "worker=worker1", "planner")

	loadFile   func(path string) (*F, error)
	getResults func(f *F) []T
	findByID   func(f *F, id string) PT

	notify    func(r PT) error // execute notification delivery
	onSuccess func(r PT)       // post-success action (publish event, advance continuous handler)

	logSuccess func(r PT)
	logFailure func(r PT, err error)
}

// processResultFile is the generic notification processing loop shared by
// processWorkerResultFile and processCommandResultFile.
func processResultFile[T any, PT interface {
	*T
	model.Notifiable
}, F any](rh *ResultHandler, spec resultFileSpec[T, PT, F]) int {
	notified := 0
	attempted := make(map[string]bool)
	writeFailures := 0

	for iter := 0; iter < maxResultLoopIterations; iter++ {
		// Phase 1: Acquire lease under lock
		rh.lockMap.Lock(spec.lockKey)

		rf, err := spec.loadFile(spec.resultPath)
		if err != nil {
			rh.lockMap.Unlock(spec.lockKey)
			rh.log(LogLevelWarn, "load_results %s error=%v", spec.label, err)
			return notified
		}

		results := spec.getResults(rf)
		idx := findUnnotifiedExcluding[T, PT](results, attempted, rh.isLeaseExpired)
		if idx < 0 {
			rh.lockMap.Unlock(spec.lockKey)
			return notified
		}

		entry := PT(&results[idx])
		resultID := entry.GetResultID()
		attempted[resultID] = true

		rh.acquireNotifyLease(entry)
		if err := yamlutil.AtomicWrite(spec.resultPath, rf); err != nil {
			rh.lockMap.Unlock(spec.lockKey)
			rh.log(LogLevelError, "write_lease %s result=%s error=%v", spec.label, resultID, err)
			return notified
		}
		rh.lockMap.Unlock(spec.lockKey)

		// Phase 2: Execute notification (outside lock)
		notifyErr := spec.notify(entry)

		// Phase 3: Update result under lock
		rh.lockMap.Lock(spec.lockKey)
		rf, err = spec.loadFile(spec.resultPath)
		if err != nil {
			rh.lockMap.Unlock(spec.lockKey)
			rh.log(LogLevelError, "reload_results %s error=%v", spec.label, err)
			return notified
		}

		entry = spec.findByID(rf, resultID)
		if entry == nil {
			rh.lockMap.Unlock(spec.lockKey)
			rh.log(LogLevelWarn, "result_disappeared %s result=%s", spec.label, resultID)
			continue
		}

		if notifyErr != nil {
			rh.markNotifyFailure(entry, notifyErr.Error())
			spec.logFailure(entry, notifyErr)
		} else {
			rh.markNotifySuccess(entry)
			notified++
			spec.logSuccess(entry)
			if spec.onSuccess != nil {
				spec.onSuccess(entry)
			}
		}

		if err := yamlutil.AtomicWrite(spec.resultPath, rf); err != nil {
			writeFailures++
			rh.log(LogLevelError, "write_result %s error=%v failures=%d/%d", spec.label, err, writeFailures, maxAtomicWriteFailures)
			if writeFailures >= maxAtomicWriteFailures {
				rh.lockMap.Unlock(spec.lockKey)
				rh.log(LogLevelError, "write_result_abort %s consecutive_failures=%d", spec.label, writeFailures)
				return notified
			}
		} else {
			writeFailures = 0
		}
		rh.lockMap.Unlock(spec.lockKey)
	}

	if len(attempted) >= maxResultLoopIterations {
		rh.log(LogLevelWarn, "process_result iteration_limit %s processed=%d limit=%d", spec.label, notified, maxResultLoopIterations)
	}
	return notified
}

// processWorkerResultFile processes worker results using the notification lease pattern.
// Notifies Planner of each unnotified result.
func (rh *ResultHandler) processWorkerResultFile(workerID string) int {
	resultPath := filepath.Join(rh.maestroDir, "results", workerID+".yaml")
	label := "worker=" + workerID

	return processResultFile(rh, resultFileSpec[model.TaskResult, *model.TaskResult, model.TaskResultFile]{
		lockKey:    "result:" + workerID,
		resultPath: resultPath,
		label:      label,
		loadFile:   rh.loadTaskResultFile,
		getResults: func(f *model.TaskResultFile) []model.TaskResult { return f.Results },
		findByID:   func(f *model.TaskResultFile, id string) *model.TaskResult { return findResultByID[model.TaskResult, *model.TaskResult](f.Results, id) },

		notify: func(r *model.TaskResult) error {
			return rh.notifyPlannerOfWorkerResult(r.CommandID, r.TaskID, workerID, string(r.Status))
		},
		onSuccess: func(r *model.TaskResult) {
			if bus := rh.getEventBus(); bus != nil {
				bus.Publish(events.EventTaskCompleted, map[string]any{
					"task_id":    r.TaskID,
					"command_id": r.CommandID,
					"worker_id":  workerID,
					"status":     string(r.Status),
				})
			}
		},
		logSuccess: func(r *model.TaskResult) {
			rh.log(LogLevelInfo, "notify_planner_success worker=%s task=%s command=%s", workerID, r.TaskID, r.CommandID)
		},
		logFailure: func(r *model.TaskResult, notifyErr error) {
			if r.NotifyAttempts >= maxNotifyAttempts {
				rh.log(LogLevelError, "notify_exhausted worker=%s task=%s command=%s attempts=%d last_error=%v",
					workerID, r.TaskID, r.CommandID, r.NotifyAttempts, notifyErr)
			} else {
				rh.log(LogLevelWarn, "notify_planner_failed worker=%s task=%s error=%v attempts=%d/%d next_retry_in=%s",
					workerID, r.TaskID, notifyErr, r.NotifyAttempts, maxNotifyAttempts, rh.notifyBackoff(r.NotifyAttempts))
			}
		},
	})
}

// processCommandResultFile processes planner results using the notification lease pattern.
// Notifies Orchestrator via queue write.
func (rh *ResultHandler) processCommandResultFile() int {
	resultPath := filepath.Join(rh.maestroDir, "results", "planner.yaml")

	return processResultFile(rh, resultFileSpec[model.CommandResult, *model.CommandResult, model.CommandResultFile]{
		lockKey:    "result:planner",
		resultPath: resultPath,
		label:      "planner",
		loadFile:   rh.loadCommandResultFile,
		getResults: func(f *model.CommandResultFile) []model.CommandResult { return f.Results },
		findByID:   func(f *model.CommandResultFile, id string) *model.CommandResult { return findResultByID[model.CommandResult, *model.CommandResult](f.Results, id) },

		notify: func(r *model.CommandResult) error {
			return rh.notifyOrchestratorOfCommandResult(r.ID, r.CommandID, r.Status)
		},
		onSuccess: func(r *model.CommandResult) {
			if ch := rh.getContinuousHandler(); ch != nil {
				if err := ch.CheckAndAdvance(r.CommandID, r.Status); err != nil {
					rh.log(LogLevelWarn, "continuous_advance command=%s error=%v", r.CommandID, err)
				}
			}
		},
		logSuccess: func(r *model.CommandResult) {
			rh.log(LogLevelInfo, "notify_orchestrator_success command=%s status=%s", r.CommandID, r.Status)
		},
		logFailure: func(r *model.CommandResult, notifyErr error) {
			if r.NotifyAttempts >= maxNotifyAttempts {
				rh.log(LogLevelError, "notify_exhausted command=%s attempts=%d last_error=%v",
					r.CommandID, r.NotifyAttempts, notifyErr)
			} else {
				rh.log(LogLevelWarn, "notify_orchestrator_failed command=%s error=%v attempts=%d/%d next_retry_in=%s",
					r.CommandID, notifyErr, r.NotifyAttempts, maxNotifyAttempts, rh.notifyBackoff(r.NotifyAttempts))
			}
		},
	})
}

// --- Generic notification lease helpers ---

// findUnnotifiedExcluding finds the first unnotified result not in the exclude set
// whose lease is either unset or expired.
func findUnnotifiedExcluding[T any, PT interface {
	*T
	model.Notifiable
}](results []T, exclude map[string]bool, isLeaseExpired func(*string) bool) int {
	for i := range results {
		r := PT(&results[i])
		if r.IsNotified() {
			continue
		}
		if exclude[r.GetResultID()] {
			continue
		}
		if r.GetNotifyAttempts() >= maxNotifyAttempts {
			continue
		}
		if r.GetNotifyLeaseOwner() == nil || isLeaseExpired(r.GetNotifyLeaseExpiresAt()) {
			return i
		}
	}
	return -1
}

// findResultByID returns a pointer to the result with the given ID, or nil.
func findResultByID[T any, PT interface {
	*T
	model.Notifiable
}](results []T, id string) PT {
	for i := range results {
		r := PT(&results[i])
		if r.GetResultID() == id {
			return r
		}
	}
	return nil
}

// notifyBackoff computes the exponential backoff delay for the given attempt count
// with ±25% uniform jitter to prevent thundering herd on recovery.
// Formula: min(notifyBackoffInitial * 2^(attempts-1), notifyBackoffMax) * (0.75 + rand*0.5).
func (rh *ResultHandler) notifyBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := notifyBackoffInitial * time.Duration(1<<(attempts-1))
	if delay > notifyBackoffMax {
		delay = notifyBackoffMax
	}
	jittered := time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5))
	return jittered
}

func (rh *ResultHandler) notifyLeaseSec() int {
	if rh.config.Watcher.NotifyLeaseSec > 0 {
		return rh.config.Watcher.NotifyLeaseSec
	}
	return 120
}

func (rh *ResultHandler) leaseOwner() string {
	return fmt.Sprintf("daemon:%d", os.Getpid())
}

func (rh *ResultHandler) isLeaseExpired(expiresAt *string) bool {
	if expiresAt == nil {
		return true
	}
	// Try RFC3339Nano first (used by backoff), then RFC3339 (used by leases).
	t, err := time.Parse(time.RFC3339Nano, *expiresAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339, *expiresAt)
		if err != nil {
			return true
		}
	}
	return rh.clock.Now().UTC().After(t)
}

// acquireNotifyLease sets the lease owner and expiration on a Notifiable result.
func (rh *ResultHandler) acquireNotifyLease(r model.Notifiable) {
	owner := rh.leaseOwner()
	expiresAt := rh.clock.Now().UTC().Add(time.Duration(rh.notifyLeaseSec()) * time.Second).Format(time.RFC3339)
	r.AcquireLease(owner, expiresAt)
}

// markNotifySuccess marks a Notifiable result as successfully notified.
func (rh *ResultHandler) markNotifySuccess(r model.Notifiable) {
	now := rh.clock.Now().UTC().Format(time.RFC3339)
	r.MarkNotified(now)
}

// markNotifyFailure marks a Notifiable result as failed with backoff lease.
func (rh *ResultHandler) markNotifyFailure(r model.Notifiable, errMsg string) {
	backoff := rh.notifyBackoff(r.GetNotifyAttempts())
	// Use RFC3339Nano to preserve sub-second precision (backoff jitter can be <1s).
	expiresAt := rh.clock.Now().UTC().Add(backoff).Format(time.RFC3339Nano)
	r.MarkNotifyFailure(errMsg, "backoff", expiresAt)
}

// --- Notification delivery ---

// notifyPlannerOfWorkerResult sends a task_result notification to Planner via agent_executor.
func (rh *ResultHandler) notifyPlannerOfWorkerResult(commandID, taskID, workerID, taskStatus string) error {
	exec, err := rh.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	message := agent.BuildTaskResultNotification(commandID, taskID, workerID, taskStatus)

	result := exec.Execute(agent.ExecRequest{
		AgentID:   "planner",
		Message:   message,
		Mode:      agent.ModeDeliver,
		TaskID:    taskID,
		CommandID: commandID,
	})

	if result.Error != nil {
		return result.Error
	}
	return nil
}

// notifyOrchestratorOfCommandResult writes a notification to queue/orchestrator.yaml.
func (rh *ResultHandler) notifyOrchestratorOfCommandResult(resultID, commandID string, status model.Status) error {
	if err := rh.writeNotificationToOrchestratorQueue(resultID, commandID, status); err != nil {
		return fmt.Errorf("write orchestrator notification: %w", err)
	}
	return nil
}

// writeNotificationToOrchestratorQueue directly writes a notification to queue/orchestrator.yaml.
// Uses source_result_id idempotency to prevent duplicate notifications.
func (rh *ResultHandler) writeNotificationToOrchestratorQueue(resultID, commandID string, status model.Status) error {
	queuePath := filepath.Join(rh.maestroDir, "queue", "orchestrator.yaml")

	// Serialize access to orchestrator queue file to prevent lost-update races
	// between fsnotify event path (no fileMu) and PeriodicScan path.
	rh.lockMap.Lock("queue:orchestrator")
	defer rh.lockMap.Unlock("queue:orchestrator")

	return updateYAMLFile(queuePath, func(nq *model.NotificationQueue) error {
		if nq.SchemaVersion == 0 {
			nq.SchemaVersion = 1
			nq.FileType = "queue_notification"
		}

		// Idempotency: check if source_result_id already exists
		for _, ntf := range nq.Notifications {
			if ntf.SourceResultID == resultID {
				rh.log(LogLevelDebug, "orchestrator_notification_duplicate source_result_id=%s", resultID)
				return errNoUpdate
			}
		}

		id, err := model.GenerateID(model.IDTypeNotification)
		if err != nil {
			return fmt.Errorf("generate notification ID: %w", err)
		}

		// Map result status to notification type
		notifType := model.NotificationTypeCommandCompleted
		switch status {
		case model.StatusFailed:
			notifType = model.NotificationTypeCommandFailed
		case model.StatusCancelled:
			notifType = model.NotificationTypeCommandCancelled
		}

		now := rh.clock.Now().UTC().Format(time.RFC3339)
		content := fmt.Sprintf("command %s %s", commandID, status)

		nq.Notifications = append(nq.Notifications, model.Notification{
			ID:             id,
			CommandID:      commandID,
			Type:           notifType,
			SourceResultID: resultID,
			Content:        content,
			Priority:       100,
			Status:         model.StatusPending,
			CreatedAt:      now,
			UpdatedAt:      now,
		})

		return nil
	})
}

// WriteNotificationToOrchestratorQueue is the exported wrapper for writeNotificationToOrchestratorQueue.
// Used by the reconcile package via the ResultNotifier interface.
func (rh *ResultHandler) WriteNotificationToOrchestratorQueue(resultID, commandID string, status model.Status) error {
	return rh.writeNotificationToOrchestratorQueue(resultID, commandID, status)
}

// --- File I/O helpers ---

func (rh *ResultHandler) loadTaskResultFile(path string) (*model.TaskResultFile, error) {
	return loadResultFile(path, "result_task", func() *model.TaskResultFile { return &model.TaskResultFile{} })
}

func (rh *ResultHandler) loadCommandResultFile(path string) (*model.CommandResultFile, error) {
	return loadResultFile(path, "result_command", func() *model.CommandResultFile { return &model.CommandResultFile{} })
}

// loadResultFile is a generic file loader for result files.
func loadResultFile[F interface{ *model.TaskResultFile | *model.CommandResultFile }](path, defaultFileType string, newFile func() F) (F, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			rf := newFile()
			// Use type switch to set defaults
			switch v := any(rf).(type) {
			case *model.TaskResultFile:
				v.SchemaVersion = 1
				v.FileType = defaultFileType
			case *model.CommandResultFile:
				v.SchemaVersion = 1
				v.FileType = defaultFileType
			}
			return rf, nil
		}
		return newFile(), fmt.Errorf("read %s: %w", path, err)
	}
	rf := newFile()
	if err := yamlv3.Unmarshal(data, rf); err != nil {
		return newFile(), fmt.Errorf("parse %s: %w", path, err)
	}
	return rf, nil
}

// --- Logging ---

func (rh *ResultHandler) log(level LogLevel, format string, args ...any) {
	rh.dl.Logf(level, format, args...)
}
