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
	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/envelope"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
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
	phaseC            *PhaseCManager
	// shutdownCtx is the daemon's shutdown context; when non-nil, retry
	// loops use it as the parent so they cancel promptly on shutdown.
	shutdownCtx context.Context
	// modelSelector receives reward feedback when tasks complete. Nil-safe:
	// when unset, no rewards are recorded (static selection remains in effect).
	modelSelector *banditModelSelector
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

// SetPhaseCManager wires the Phase C component bundle so result processing
// can record failure fingerprints and feed bandit reward signals. Safe to
// call with nil (used for tests and when Phase C is disabled).
func (rh *ResultHandler) SetPhaseCManager(m *PhaseCManager) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.phaseC = m
}

// getPhaseC returns the Phase C manager with proper synchronization. nil-safe.
func (rh *ResultHandler) getPhaseC() *PhaseCManager {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	return rh.phaseC
}

// SetModelSelector wires the adaptive model selector so task outcomes can
// feed rewards back into the bandit. Safe to call with nil.
func (rh *ResultHandler) SetModelSelector(s *banditModelSelector) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.modelSelector = s
}

// getModelSelector returns the adaptive model selector. nil-safe.
func (rh *ResultHandler) getModelSelector() *banditModelSelector {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	return rh.modelSelector
}

// SetShutdownContext wires the daemon's shutdown context so result-notify
// retry loops can cancel promptly on daemon shutdown instead of running to
// completion under context.Background.
func (rh *ResultHandler) SetShutdownContext(ctx context.Context) {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	rh.shutdownCtx = ctx
}

// parentContext returns the daemon shutdown context when set, otherwise
// context.Background(). Never returns nil.
func (rh *ResultHandler) parentContext() context.Context {
	rh.mu.RLock()
	defer rh.mu.RUnlock()
	if rh.shutdownCtx != nil {
		return rh.shutdownCtx
	}
	return context.Background()
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

	// onDeadLetter is invoked once per entry transitioned into the notify
	// dead-letter terminal state (NotifyAttempts >= maxNotifyAttempts). It runs
	// outside any result-file lock so it is free to acquire other queue locks.
	// Used to surface the loss to the orchestrator / archive the entry.
	onDeadLetter func(r PT)
}

// Phase 1 outcome codes returned by processResultPhase1AcquireLease.
// They are plain ints (the function returns int) rather than a typed alias —
// callers pattern-match against these constants with bare ==/switch.
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
			rh.log(LogLevelWarn, "write_result_abort %s consecutive_failures=%d threshold_reached", spec.label, writeFailures)
			return writeFailures, success, phase3Abort
		}
	} else {
		writeFailures = 0
	}
	rh.lockMap.Unlock(spec.lockKey)
	return writeFailures, success, phase3OK
}

// sweepExhaustedNotifications transitions results whose NotifyAttempts reached
// the retry ceiling into a terminal notify-dead-letter state. Without this sweep,
// findUnnotifiedExcluding would silently skip them forever, causing completed
// results to be stranded in results/ with no downstream signalling.
//
// For each transitioned result it:
//   - Marks NotifyDeadLettered on disk (persists via AtomicWrite).
//   - Archives the entry into .maestro/dead_letters/.
//   - Invokes spec.onDeadLetter (which typically emits an orchestrator notification).
func sweepExhaustedNotifications[T any, PT interface {
	*T
	model.Notifiable
}, F any](rh *ResultHandler, spec *resultFileSpec[T, PT, F]) {
	rh.lockMap.Lock(spec.lockKey)
	rf, err := spec.loadFile(spec.resultPath)
	if err != nil {
		rh.lockMap.Unlock(spec.lockKey)
		return
	}
	results := spec.getResults(rf)

	// Collect IDs of entries that transitioned in this pass so we can run
	// onDeadLetter callbacks after releasing the lock.
	transitioned := make([]string, 0)
	now := rh.clock.Now().UTC().Format(time.RFC3339)
	changed := false
	for i := range results {
		r := PT(&results[i])
		if r.IsNotified() || r.IsNotifyDeadLettered() {
			continue
		}
		if r.GetNotifyAttempts() < maxNotifyAttempts {
			continue
		}
		reason := fmt.Sprintf("notify_attempts (%d) >= max_notify_attempts (%d) for %s",
			r.GetNotifyAttempts(), maxNotifyAttempts, spec.label)
		r.MarkNotifyDeadLetter(now, reason)
		transitioned = append(transitioned, r.GetResultID())
		changed = true
		rh.log(LogLevelError, "notify_dead_letter %s result=%s reason=%s",
			spec.label, r.GetResultID(), reason)
	}
	if changed {
		if err := yamlutil.AtomicWrite(spec.resultPath, rf); err != nil {
			rh.log(LogLevelError, "sweep_dead_letter_write %s error=%v", spec.label, err)
			rh.lockMap.Unlock(spec.lockKey)
			return
		}
	}
	rh.lockMap.Unlock(spec.lockKey)

	if len(transitioned) == 0 {
		return
	}

	// Archive + invoke callback outside the lock. Reload once for stable snapshots.
	rh.lockMap.Lock(spec.lockKey)
	rf2, err := spec.loadFile(spec.resultPath)
	rh.lockMap.Unlock(spec.lockKey)
	if err != nil {
		rh.log(LogLevelWarn, "sweep_dead_letter_reload %s error=%v", spec.label, err)
		return
	}
	for _, id := range transitioned {
		entry := spec.findByID(rf2, id)
		if entry == nil {
			continue
		}
		reason := ""
		if r := entry.GetNotifyDeadLetterReason(); r != nil {
			reason = *r
		}
		if err := rh.archiveNotifyDeadLetter(spec.label, id, entry, reason); err != nil {
			rh.log(LogLevelError, "archive_notify_dead_letter %s result=%s error=%v",
				spec.label, id, err)
		}
		if spec.onDeadLetter != nil {
			spec.onDeadLetter(entry)
		}
	}
}

// archiveNotifyDeadLetter writes a dead-letter archive for a result whose
// notification retries were exhausted. Mirrors DeadLetterProcessor.archiveDeadLetter
// in file layout so operators can correlate entries from a single dead_letters/ view.
func (rh *ResultHandler) archiveNotifyDeadLetter(label, entryID string, entry any, reason string) error {
	archiveDir := filepath.Join(rh.maestroDir, "dead_letters")
	if err := os.MkdirAll(archiveDir, 0755); err != nil { //nolint:gosec // 0755 matches dispatch dead-letter archive perms
		return fmt.Errorf("create dead_letters dir: %w", err)
	}
	type archiveEntry struct {
		SchemaVersion  int    `yaml:"schema_version"`
		FileType       string `yaml:"file_type"`
		QueueType      string `yaml:"queue_type"`
		Entry          any    `yaml:"entry"`
		DeadLetteredAt string `yaml:"dead_lettered_at"`
		Reason         string `yaml:"reason"`
	}
	now := rh.clock.Now().UTC()
	a := archiveEntry{
		SchemaVersion:  1,
		FileType:       "dead_letter",
		QueueType:      "result:" + label,
		Entry:          entry,
		DeadLetteredAt: now.Format(time.RFC3339),
		Reason:         reason,
	}
	filename := fmt.Sprintf("result_%s_%s_%s.yaml",
		strings.ReplaceAll(label, "=", "_"),
		now.Format("20060102T150405Z"),
		entryID,
	)
	return yamlutil.AtomicWrite(filepath.Join(archiveDir, filename), a)
}

// processResultFile is the generic notification processing loop shared by
// processWorkerResultFile and processCommandResultFile.
func processResultFile[T any, PT interface {
	*T
	model.Notifiable
}, F any](rh *ResultHandler, spec resultFileSpec[T, PT, F]) int {
	// Transition exhausted entries into dead-letter terminal state before the
	// normal scan so they no longer consume findUnnotifiedExcluding iterations
	// and their loss is surfaced to the orchestrator.
	sweepExhaustedNotifications[T, PT, F](rh, &spec)

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
	if !validate.IsValidBaseName(workerID) {
		rh.log(LogLevelWarn, "process_worker_result invalid_worker_id=%q", workerID)
		return 0
	}
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
			rh.recordTaskResultLearning(r, workerID)
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
		onDeadLetter: func(r *model.TaskResult) {
			// Synthesize a command_failed notification so the Orchestrator is
			// told that a worker task result was lost (Planner could never be
			// notified). Using the result's ID as source keeps dedup stable.
			content := fmt.Sprintf("task result %s (command=%s worker=%s task=%s) dead-lettered: notify retries exhausted",
				r.ID, r.CommandID, workerID, r.TaskID)
			if err := rh.emitResultDeadLetterNotification(r.ID, r.CommandID, content); err != nil {
				rh.log(LogLevelError, "dead_letter_notify_failed worker=%s task=%s error=%v",
					workerID, r.TaskID, err)
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
			// Release per-command evolution bookkeeping now that the
			// command has reached a terminal state and the orchestrator
			// has been notified; keeps daemon memory bounded across
			// long-running sessions.
			if m := rh.getPhaseC(); m != nil {
				m.ResetEvolutionState(r.CommandID)
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
		onDeadLetter: func(r *model.CommandResult) {
			// Writing to orchestrator queue is the exact path that just failed
			// for this command. We still try once more via a dedicated
			// notification that carries a distinct source_result_id so the
			// Orchestrator sees it as a new entry; if this final attempt also
			// fails, the archive under dead_letters/ remains the record.
			content := fmt.Sprintf("command result %s (command=%s) dead-lettered: notify retries exhausted",
				r.ID, r.CommandID)
			if err := rh.emitResultDeadLetterNotification(r.ID, r.CommandID, content); err != nil {
				rh.log(LogLevelError, "dead_letter_notify_failed command=%s error=%v",
					r.CommandID, err)
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
		if r.IsNotifyDeadLettered() {
			// Terminal: notification retries exhausted; handled by sweepExhaustedNotifications.
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

// recordTaskResultLearning feeds a completed worker task result into the
// Phase C learning components:
//   - FingerprintDB (C-5): failed results contribute a failure fingerprint
//     and category derived from the result Summary.
//   - Bandit model selector (C-2): completed/failed/dead-lettered results
//     reward the worker's model arm.
//   - Search tree & Thompson sampler (C-4): backpropagate the task outcome
//     and reward the widen/deepen decision recorded at dispatch.
//
// Each component path is independent and nil-safe; disabling one does not
// skip the others. Two nil-guard styles coexist intentionally:
//   - FingerprintDB access is a *field read* so the call site must check
//     `m != nil` explicitly before dereferencing.
//   - ObserveTaskOutcome / RecordTaskCompletionNovelty / PlanRetryMutations
//     are *method calls* on *PhaseCManager that tolerate a nil receiver,
//     so the call site omits the guard and lets the callee short-circuit.
//
// Keep this split when adding new signals — do not collapse to a single
// `if m != nil { ... }` wrapper, as that hides the callee's nil contract.
func (rh *ResultHandler) recordTaskResultLearning(r *model.TaskResult, workerID string) {
	if r == nil {
		return
	}
	m := rh.getPhaseC()

	// C-5 fingerprint capture (requires FingerprintDB).
	if m != nil && m.FingerprintDB != nil {
		// TaskResult encodes failure detail in Summary (no dedicated LastError
		// field); success results typically have benign summaries we ignore.
		errMsg := r.Summary
		switch r.Status {
		case model.StatusFailed, model.StatusDeadLetter:
			fp, category := learnings.ComputeErrorFingerprint(errMsg)
			if fp != "" {
				// Strategy is empty at capture time; the planner/retry logic may
				// attach one later via Store(fp, category, strategy).
				m.FingerprintDB.Store(fp, category, "")
				rh.log(LogLevelInfo, "fingerprint_store worker=%s task=%s command=%s category=%s fp=%s",
					workerID, r.TaskID, r.CommandID, category, fp)
			}
		}
	}

	// C-2 bandit reward (requires adaptive model selector). Reward policy:
	// Completed=1.0, Failed/DeadLetter=0.0, anything else ignored. Mirrors
	// REQUIREMENTS §5-7 — see internal/daemon/model_selector.go.
	sel := rh.getModelSelector()
	if sel != nil {
		modelName := rh.workerModelName(workerID)
		if modelName != "" {
			switch r.Status {
			case model.StatusCompleted:
				sel.RecordResult(modelName, 1.0)
			case model.StatusFailed, model.StatusDeadLetter:
				sel.RecordResult(modelName, 0.0)
			}
		}
	}

	// C-4 search tree / Thompson sampler backpropagation (nil-safe on manager
	// and components). Status mapping matches the bandit path above.
	switch r.Status {
	case model.StatusCompleted:
		m.ObserveTaskOutcome(r.TaskID, 1.0, true)
	case model.StatusFailed, model.StatusDeadLetter:
		m.ObserveTaskOutcome(r.TaskID, 0.0, false)
	}

	// C-1 EvolutionEngine integration:
	//  - Completed: check novelty of the summary against prior completions.
	//    A non-novel signal means the daemon has converged on a repeated
	//    solution, which is surfaced as an observability log so operators
	//    can notice when exploration has stalled.
	//  - Failed/DeadLetter: increment per-command failure count and, once
	//    the threshold is crossed, run PlanMutations to emit a retry
	//    strategy plan (observability only — the existing retry path in
	//    plan/retry.go performs the actual retries; this log lets operators
	//    see which mutation strategies the evolution engine would pick).
	switch r.Status {
	case model.StatusCompleted:
		if novel, evaluated := m.RecordTaskCompletionNovelty(r.CommandID, r.Summary); evaluated {
			rh.log(LogLevelInfo, "evolution_novelty command=%s task=%s novel=%t",
				r.CommandID, r.TaskID, novel)
		}
	case model.StatusFailed, model.StatusDeadLetter:
		// parentCount is bounded below by 2 so the cross-strategy slot is
		// eligible once the threshold is crossed; the engine itself drops
		// cross when parentCount < 2.
		if slots, planned := m.PlanRetryMutations(r.CommandID, 2); planned {
			rh.log(LogLevelInfo, "evolution_retry_plan command=%s task=%s slots=%d",
				r.CommandID, r.TaskID, len(slots))
		}
	}
}

// workerModelName resolves the model name configured for the given worker.
// Returns empty when the worker is unknown and no default is configured.
// Duplicates plan.GetWorkerModel's logic to keep daemon decoupled from plan.
func (rh *ResultHandler) workerModelName(workerID string) string {
	cfg := rh.config.Agents.Workers
	if cfg.Boost {
		return "opus"
	}
	if m, ok := cfg.Models[workerID]; ok && m != "" {
		return m
	}
	if cfg.DefaultModel != "" {
		return cfg.DefaultModel
	}
	return "sonnet"
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

	parent := rh.parentContext()
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			rh.log(LogLevelInfo, "result_notify_inline_retry attempt=%d/%d task=%s command=%s error=%v",
				attempt+1, maxRetries+1, taskID, commandID, lastErr)
			select {
			case <-parent.Done():
				return fmt.Errorf("result notify aborted during retry: %w", parent.Err())
			case <-time.After(retryDelay):
			}
		}

		if err := parent.Err(); err != nil {
			return fmt.Errorf("result notify aborted: %w", err)
		}
		ctx, cancel := context.WithTimeout(parent, attemptTimeout)
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

// emitResultDeadLetterNotification writes a one-shot notification into
// queue/orchestrator.yaml telling the Orchestrator that a result (task or
// command) has been dead-lettered because notification retries were exhausted.
// Dedup key: a synthetic SourceResultID derived from resultID to survive
// repeated sweep passes without emitting duplicates.
func (rh *ResultHandler) emitResultDeadLetterNotification(resultID, commandID, content string) error {
	queuePath := notificationQueuePath(rh.maestroDir)
	sourceID := "result_dl_" + resultID

	rh.lockMap.Lock("queue:orchestrator")
	defer rh.lockMap.Unlock("queue:orchestrator")

	return updateYAMLFile(queuePath, func(nq *model.NotificationQueue) error {
		if nq.SchemaVersion == 0 {
			nq.SchemaVersion = 1
			nq.FileType = "queue_notification"
		}

		for i := range nq.Notifications {
			if nq.Notifications[i].SourceResultID == sourceID &&
				nq.Notifications[i].Type == model.NotificationTypeCommandFailed {
				return errNoUpdate
			}
		}

		id, err := model.GenerateID(model.IDTypeNotification)
		if err != nil {
			return fmt.Errorf("generate notification ID: %w", err)
		}
		now := rh.clock.Now().UTC().Format(time.RFC3339)
		nq.Notifications = append(nq.Notifications, model.Notification{
			ID:             id,
			CommandID:      commandID,
			Type:           model.NotificationTypeCommandFailed,
			SourceResultID: sourceID,
			Content:        content,
			Priority:       defaultNotificationPriority,
			Status:         model.StatusPending,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
		return nil
	})
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
