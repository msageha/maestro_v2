package daemon

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/envelope"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

const (
	// maxNotifyAttempts is the maximum number of notification delivery attempts
	// before giving up. Prevents infinite retries when tmux is unavailable.
	// Why: 3 attempts with exponential backoff covers ~7s of retries, enough
	// to ride out transient tmux hangs without blocking the scan loop.
	maxNotifyAttempts = 3

	// notifyBackoffInitial is the initial backoff delay after a failed notification.
	// Exponential backoff: 1s → 2s → 4s (capped at notifyBackoffMax).
	notifyBackoffInitial = 1 * time.Second

	// notifyBackoffMax is the maximum backoff delay between retry attempts.
	// Why: 5s cap keeps total retry time within ~7s, preventing backoff from
	// exceeding the typical scan interval or stalling the notification pipeline.
	notifyBackoffMax = 5 * time.Second

	// maxResultLoopIterations is the maximum number of iterations allowed per
	// processResultFile call. Prevents unbounded CPU/memory consumption when
	// results are added faster than they are processed.
	// Remaining results will be handled on the next scan cycle.
	maxResultLoopIterations = 1000

	// maxAtomicWriteFailures is the maximum number of consecutive AtomicWrite
	// failures before the processing loop aborts. Prevents infinite loops when
	// persistent write errors occur (e.g., disk full).
	// Why: 3 retries is enough to distinguish transient I/O hiccups from
	// persistent failures (e.g., disk full) without excessive delay.
	maxAtomicWriteFailures = 3

	// defaultNotificationPriority is the default priority assigned to new
	// orchestrator notifications. Higher values are processed first.
	// Why: 100 provides a mid-range default that leaves room for both
	// higher-priority (urgent) and lower-priority (batch) notifications.
	defaultNotificationPriority = 100
)

// ResultHandler monitors results/ and delivers notifications to agents.
// Worker results → Planner (side-channel via agent_executor).
// Planner results → Orchestrator (queue write).
// baseHandler.mu protects continuousHandler, eventBus.
type ResultHandler struct {
	baseHandler
	lockMap           *lock.MutexMap
	continuousHandler ContinuousAdvancer
	eventBus          EventPublisher
}

// NewResultHandler creates a new ResultHandler.
func NewResultHandler(
	maestroDir string,
	cfg model.Config,
	lockMap *lock.MutexMap,
	logger *log.Logger,
	logLevel LogLevel,
	ep ExecutorGetter,
	clock Clock,
) *ResultHandler {
	return &ResultHandler{
		baseHandler: baseHandler{
			maestroDir:   maestroDir,
			config:       cfg,
			dl:           NewDaemonLoggerFromLegacy("result_handler", logger, logLevel),
			logger:       logger,
			logLevel:     logLevel,
			clock:        clock,
			execProvider: ep,
		},
		lockMap: lockMap,
	}
}

// SetContinuousHandler wires the continuous handler for iteration tracking.
func (rh *ResultHandler) SetContinuousHandler(ch ContinuousAdvancer) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.continuousHandler = ch
}

// SetEventBus sets the event bus for publishing events.
func (rh *ResultHandler) SetEventBus(bus EventPublisher) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.eventBus = bus
}

// getEventBus returns the event bus with proper synchronization.
func (rh *ResultHandler) getEventBus() EventPublisher {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	return rh.eventBus
}

// getContinuousHandler returns the continuous handler with proper synchronization.
func (rh *ResultHandler) getContinuousHandler() ContinuousAdvancer {
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
	resultsDir := resultsDirPath(rh.maestroDir)
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

// phase1Result holds the outcome of processResultPhase1AcquireLease.
type phase1Result[PT any] int

const (
	// phase1Proceed indicates the lease was acquired and notification should proceed.
	phase1Proceed = iota
	// phase1Done indicates no more unnotified results; the caller should return.
	phase1Done
	// phase1Error indicates a fatal error; the caller should return.
	phase1Error
)

// processResultPhase1AcquireLease loads the result file, finds the next unnotified
// entry, acquires a lease, and writes the file. The lock is held on entry and
// released before returning. On phase1Proceed the caller should proceed to Phase 2;
// on phase1Done/phase1Error the caller should return notified.
func processResultPhase1AcquireLease[T any, PT interface {
	*T
	model.Notifiable
}, F any](rh *ResultHandler, spec *resultFileSpec[T, PT, F], attempted map[string]bool) (PT, string, int) {
	rf, err := spec.loadFile(spec.resultPath)
	if err != nil {
		rh.lockMap.Unlock(spec.lockKey)
		rh.log(LogLevelWarn, "load_results %s error=%v", spec.label, err)
		return nil, "", phase1Error
	}

	results := spec.getResults(rf)
	idx := findUnnotifiedExcluding[T, PT](results, attempted, rh.isLeaseExpired)
	if idx < 0 {
		rh.lockMap.Unlock(spec.lockKey)
		return nil, "", phase1Done
	}

	entry := PT(&results[idx])
	resultID := entry.GetResultID()
	attempted[resultID] = true

	rh.acquireNotifyLease(entry)
	if err := yamlutil.AtomicWrite(spec.resultPath, rf); err != nil {
		rh.lockMap.Unlock(spec.lockKey)
		rh.log(LogLevelError, "write_lease %s result=%s error=%v", spec.label, resultID, err)
		return nil, "", phase1Error
	}
	rh.lockMap.Unlock(spec.lockKey)
	return entry, resultID, phase1Proceed
}

// phase3Result holds the outcome of processResultPhase3UpdateResult.
type phase3Result int

const (
	// phase3Continue indicates the loop should continue to the next iteration.
	phase3Continue phase3Result = iota
	// phase3Abort indicates a fatal write error; the caller should return.
	phase3Abort
	// phase3OK indicates success; the loop should continue normally.
	phase3OK
)

// processResultPhase3UpdateResult reloads the result file, marks the entry as
// success or failure, and writes it back. The lock is held on entry and released
// before returning. Returns the updated writeFailures count, whether notification
// succeeded (for incrementing notified), and a flow-control signal.
func processResultPhase3UpdateResult[T any, PT interface {
	*T
	model.Notifiable
}, F any](rh *ResultHandler, spec *resultFileSpec[T, PT, F], resultID string, notifyErr error, writeFailures int) (int, bool, phase3Result) {
	rf, err := spec.loadFile(spec.resultPath)
	if err != nil {
		rh.lockMap.Unlock(spec.lockKey)
		rh.log(LogLevelError, "reload_results %s error=%v", spec.label, err)
		return writeFailures, false, phase3Abort
	}

	entry := spec.findByID(rf, resultID)
	if entry == nil {
		rh.lockMap.Unlock(spec.lockKey)
		rh.log(LogLevelWarn, "result_disappeared %s result=%s", spec.label, resultID)
		return writeFailures, false, phase3Continue
	}

	success := false
	if notifyErr != nil {
		rh.markNotifyFailure(entry, notifyErr.Error())
		spec.logFailure(entry, notifyErr)
	} else {
		rh.markNotifySuccess(entry)
		success = true
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
			return writeFailures, success, phase3Abort
		}
	} else {
		writeFailures = 0
	}
	rh.lockMap.Unlock(spec.lockKey)
	return writeFailures, success, phase3OK
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
		entry, resultID, outcome := processResultPhase1AcquireLease[T, PT](rh, &spec, attempted)
		if outcome != phase1Proceed {
			return notified
		}

		// Phase 2: Execute notification (outside lock)
		notifyErr := spec.notify(entry)

		// Phase 3: Update result under lock
		rh.lockMap.Lock(spec.lockKey)
		var success bool
		var p3 phase3Result
		writeFailures, success, p3 = processResultPhase3UpdateResult[T, PT](rh, &spec, resultID, notifyErr, writeFailures)
		if success {
			notified++
		}
		if p3 == phase3Abort {
			return notified
		}
	}

	if len(attempted) >= maxResultLoopIterations {
		rh.log(LogLevelWarn, "process_result iteration_limit %s processed=%d limit=%d", spec.label, notified, maxResultLoopIterations)
	}
	return notified
}

// processWorkerResultFile processes worker results using the notification lease pattern.
// Notifies Planner of each unnotified result.
func (rh *ResultHandler) processWorkerResultFile(workerID string) int {
	resultPath := resultFilePath(rh.maestroDir, workerID)
	label := "worker=" + workerID

	return processResultFile(rh, resultFileSpec[model.TaskResult, *model.TaskResult, model.TaskResultFile]{
		lockKey:    "result:" + workerID,
		resultPath: resultPath,
		label:      label,
		loadFile:   rh.loadTaskResultFile,
		getResults: func(f *model.TaskResultFile) []model.TaskResult { return f.Results },
		findByID: func(f *model.TaskResultFile, id string) *model.TaskResult {
			return findResultByID[model.TaskResult, *model.TaskResult](f.Results, id)
		},

		notify: func(r *model.TaskResult) error {
			return rh.notifyPlannerOfWorkerResultWithRetry(r.CommandID, r.TaskID, workerID, string(r.Status))
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
	resultPath := resultFilePath(rh.maestroDir, "planner")

	return processResultFile(rh, resultFileSpec[model.CommandResult, *model.CommandResult, model.CommandResultFile]{
		lockKey:    "result:planner",
		resultPath: resultPath,
		label:      "planner",
		loadFile:   rh.loadCommandResultFile,
		getResults: func(f *model.CommandResultFile) []model.CommandResult { return f.Results },
		findByID: func(f *model.CommandResultFile, id string) *model.CommandResult {
			return findResultByID[model.CommandResult, *model.CommandResult](f.Results, id)
		},

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
	jittered := time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5)) //nolint:gosec // math/rand is appropriate for jitter
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

// notifyPlannerOfWorkerResultWithRetry attempts delivery to the planner with inline retries.
// On transient failure (e.g., planner busy during recovery), retries up to
// ResultNotifyInlineRetries times with a short delay between attempts, each bounded
// by ResultNotifyDeliveryTimeoutSec. This mirrors deliverPlannerSignal's inline retry
// pattern to avoid blocking for the full busy detection cycle on a single attempt.
func (rh *ResultHandler) notifyPlannerOfWorkerResultWithRetry(commandID, taskID, workerID, taskStatus string) error {
	maxRetries := rh.config.Retry.EffectiveResultNotifyInlineRetries()
	retryDelay := time.Duration(rh.config.Retry.EffectiveResultNotifyInlineRetryDelaySec()) * time.Second
	attemptTimeout := time.Duration(rh.config.Retry.EffectiveResultNotifyDeliveryTimeoutSec()) * time.Second

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			rh.log(LogLevelInfo, "result_notify_inline_retry attempt=%d/%d task=%s command=%s error=%v",
				attempt+1, maxRetries+1, taskID, commandID, lastErr)
			time.Sleep(retryDelay)
		}

		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		err := rh.notifyPlannerOfWorkerResult(ctx, commandID, taskID, workerID, taskStatus)
		cancel()

		if err == nil {
			if attempt > 0 {
				rh.log(LogLevelInfo, "result_notify_inline_retry_success task=%s command=%s total_attempts=%d",
					taskID, commandID, attempt+1)
			}
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// notifyPlannerOfWorkerResult sends a task_result notification to Planner via agent_executor.
func (rh *ResultHandler) notifyPlannerOfWorkerResult(ctx context.Context, commandID, taskID, workerID, taskStatus string) error {
	exec, err := rh.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	message := envelope.BuildTaskResultNotification(commandID, taskID, workerID, taskStatus)

	result := exec.Execute(agent.ExecRequest{
		Context:   ctx,
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
	queuePath := notificationQueuePath(rh.maestroDir)

	// Serialize access to orchestrator queue file to prevent lost-update races
	// between fsnotify event path (no fileMu) and PeriodicScan path.
	rh.lockMap.Lock("queue:orchestrator")
	defer rh.lockMap.Unlock("queue:orchestrator")

	return updateYAMLFile(queuePath, func(nq *model.NotificationQueue) error {
		if nq.SchemaVersion == 0 {
			nq.SchemaVersion = 1
			nq.FileType = "queue_notification"
		}

		// Map result status to notification type (computed early for dedup key).
		notifType := model.NotificationTypeCommandCompleted
		switch status {
		case model.StatusFailed:
			notifType = model.NotificationTypeCommandFailed
		case model.StatusCancelled:
			notifType = model.NotificationTypeCommandCancelled
		}

		now := rh.clock.Now().UTC().Format(time.RFC3339)
		content := fmt.Sprintf("command %s %s", commandID, status)

		// Idempotency: dedup key is (source_result_id, type). H3 reconcile may
		// preserve the original result ID but change the terminal status — in
		// that case the notification Type differs and we supersede the existing
		// entry in place rather than dropping the re-delivery. Resetting
		// delivery state ensures stale lease/attempts from a prior failed
		// dispatch do not block re-delivery. ID/CreatedAt/Priority are preserved
		// so downstream observers see the same notification identity.
		for i := range nq.Notifications {
			if nq.Notifications[i].SourceResultID != resultID {
				continue
			}
			if nq.Notifications[i].Type == notifType {
				rh.log(LogLevelDebug, "orchestrator_notification_duplicate source_result_id=%s", resultID)
				return errNoUpdate
			}
			rh.log(LogLevelInfo, "orchestrator_notification_supersede source_result_id=%s old_type=%s new_type=%s existing_id=%s",
				resultID, nq.Notifications[i].Type, notifType, nq.Notifications[i].ID)
			nq.Notifications[i].Type = notifType
			nq.Notifications[i].Content = content
			nq.Notifications[i].Status = model.StatusPending
			nq.Notifications[i].Attempts = 0
			nq.Notifications[i].LastError = nil
			nq.Notifications[i].DeadLetteredAt = nil
			nq.Notifications[i].DeadLetterReason = nil
			nq.Notifications[i].LeaseOwner = nil
			nq.Notifications[i].LeaseExpiresAt = nil
			nq.Notifications[i].UpdatedAt = now
			return nil
		}

		id, err := model.GenerateID(model.IDTypeNotification)
		if err != nil {
			return fmt.Errorf("generate notification ID: %w", err)
		}

		nq.Notifications = append(nq.Notifications, model.Notification{
			ID:             id,
			CommandID:      commandID,
			Type:           notifType,
			SourceResultID: resultID,
			Content:        content,
			Priority:       defaultNotificationPriority,
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
func loadResultFile[F interface {
	*model.TaskResultFile | *model.CommandResultFile
}](path, defaultFileType string, newFile func() F) (F, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
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

