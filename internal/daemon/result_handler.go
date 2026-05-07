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

	// gateNotify, when set, is called for each unnotified result before
	// the lease is acquired. Returning false defers this entry to the
	// next scan tick — no lease is taken and NotifyAttempts is not
	// incremented. Used to delay notify_planner_success while a daemon-
	// owned step (verify_pending / repair_pending) is still in flight
	// (Report 2026-05-05 P0). nil means "always notify now".
	gateNotify func(r PT) bool

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
//
// phase1Skip means "this candidate is not yet ready to notify; mark it
// attempted-this-tick and continue the loop to the next candidate". The
// next periodic scan will retry the entry.
const (
	// phase1Proceed indicates the lease was acquired and notification should proceed.
	phase1Proceed = iota
	// phase1Done indicates no more unnotified results; the caller should return.
	phase1Done
	// phase1Error indicates a fatal error; the caller should return.
	phase1Error
	// phase1Skip means the candidate is deferred (gateNotify returned
	// false). The lock has been released and the candidate is recorded
	// in the per-call attempted set so the loop moves on without
	// re-selecting it; no lease was acquired.
	phase1Skip
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

	// Gate before lease acquisition: when the daemon is still mid-flight
	// on a step that owns the task's terminal status (typically
	// verify_pending → verify_runner → repair_pending|completed), notifying
	// the Planner now leaks a misleading "task succeeded" signal that
	// races phase-completion gating (Report 2026-05-05 P0). Releasing the
	// lock without taking a lease keeps NotifyAttempts unchanged so the
	// next scan tick retries cleanly.
	if spec.gateNotify != nil && !spec.gateNotify(entry) {
		rh.lockMap.Unlock(spec.lockKey)
		return nil, resultID, phase1Skip
	}

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

// Notification dead-letter helpers (sweepExhaustedNotifications,
// applyDeadLetterTransitions, archiveAndInvokeDeadLetterCallbacks,
// archiveNotifyDeadLetter) live in result_dead_letter.go.

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
		switch outcome {
		case phase1Proceed:
			// fall through to Phase 2 below.
		case phase1Skip:
			// gateNotify deferred this entry; pick the next candidate.
			rh.log(LogLevelDebug,
				"notify_deferred %s result=%s (gateNotify returned false; will retry on the next scan tick)",
				spec.label, resultID)
			continue
		default:
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

		// Defer the Planner notification while the daemon-owned verify
		// step is still in flight. Worker reports completed → Phase B
		// parks the task at verify_pending → §S1-1 verify_runner runs
		// (only on RunOnIntegration / RunOnMain after the 2026-05-05
		// redesign) → outcome lands the task at completed or
		// repair_pending. Notifying the Planner before that lands leaks
		// a "task succeeded" signal that races phase_complete gating
		// (Report 2026-05-05 P0 — final integration_verify phase
		// flagged terminal before verify_runner_passed because Planner
		// already saw success).
		gateNotify: func(r *model.TaskResult) bool {
			return rh.taskTerminalForNotify(r)
		},

		notify: func(r *model.TaskResult) error {
			// Verify failure routes the original task to
			// cancelled-as-superseded with a freshly enqueued retry
			// task. The retry's eventual notify is the canonical
			// signal for the Planner; emitting an additional
			// notify_planner_success here would carry the stale
			// worker-reported `completed` status and tell the Planner
			// "the task succeeded" while the state machine has
			// actually scheduled a repair (Report 2026-05-05 P1).
			// Silently mark the original as notified by returning nil
			// without contacting the Planner pane — markNotifySuccess
			// will set Notified=true and the file sweeper will not
			// retry it.
			if rh.taskSupersededByRetry(r.CommandID, r.TaskID) {
				rh.log(LogLevelInfo,
					"notify_planner_skipped_superseded worker=%s task=%s command=%s "+
						"(verify failure scheduled a retry; original result silently acked)",
					workerID, r.TaskID, r.CommandID)
				return nil
			}
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
			// "Success" here means the orchestrator notification was
			// delivered, not that the command itself succeeded. Status-
			// dispatched log keys keep operator-facing logs honest:
			// scanning daemon.log for `notify_orchestrator_success` used
			// to surface cancelled / failed commands as well, which made
			// audit trails misleading (Report 2026-05-06 P0 — a
			// content-filter-driven cancel showed up under
			// "notify_orchestrator_success status=cancelled").
			switch r.Status {
			case model.StatusCompleted:
				rh.log(LogLevelInfo, "notify_orchestrator_completed command=%s status=%s", r.CommandID, r.Status)
			case model.StatusCancelled:
				rh.log(LogLevelWarn, "notify_orchestrator_cancelled command=%s status=%s", r.CommandID, r.Status)
			case model.StatusFailed, model.StatusDeadLetter, model.StatusAborted:
				rh.log(LogLevelWarn, "notify_orchestrator_failed_status command=%s status=%s", r.CommandID, r.Status)
			default:
				rh.log(LogLevelInfo, "notify_orchestrator_delivered command=%s status=%s", r.CommandID, r.Status)
			}
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

// taskTerminalForNotify reports whether the Planner can be told this
// task's final outcome now. Used by processWorkerResultFile.gateNotify
// to defer notify_planner_success while the daemon-owned pipeline is
// still in flight.
//
// Two-layer gating (post-2026-05-06 P0-A REGRESSION redesign):
//
//  1. **Result-entry verify-pipeline marker** (primary, race-proof):
//     When Phase A writes the result entry, it stamps RunOnIntegration /
//     RunOnMain from the queue task and leaves VerifyOutcomeAppliedAt
//     blank for entries that must wait for the daemon-owned verify
//     pipeline. applyVerifyOutcome stamps VerifyOutcomeAppliedAt only
//     after the verify outcome has been committed to state. The gate
//     defers any RunOnIntegration / RunOnMain entry whose
//     VerifyOutcomeAppliedAt is nil — independent of state-file races.
//  2. **State-file allowlist** (back-stop): IsTerminal(state[taskID])
//     is required. This catches the no-stamp / non-verify entry path
//     (failure / cancellation reported by the worker, normal worker
//     completed → verify_skipped_normal_worker_task) and stale stamps
//     where the state machine has not yet caught up.
//
// Why two layers: prior to the verify-pipeline marker, the gate
// depended only on state[taskID]==terminal. A state-file race window
// (Report 2026-05-05 P0-A REGRESSION) let the gate fire before
// verify_runner_passed because the state read momentarily showed
// terminal-by-some-other-path. The marker pins "verify outcome has
// been applied" to the result entry itself, so even a state-file
// race can no longer slip an early notify through. The state allowlist
// remains because the Planner notification carries the result entry's
// recorded Status; we still want state to confirm the task reached a
// terminal slot before we deliver that status.
//
// The next-scan retry path is bounded by retry.result_notify_*
// (notify backoff exponentially out to maxNotifyAttempts), so a state
// file that genuinely never appears still dead-letters cleanly without
// a deadlock.
func (rh *ResultHandler) taskTerminalForNotify(entry *model.TaskResult) bool {
	if entry == nil || entry.CommandID == "" || entry.TaskID == "" {
		// Defensive: caller passed an unrecognisable identifier.
		// Allow notify so the legacy "no gating" baseline still applies
		// (rare; if hit it is almost certainly a wiring bug elsewhere
		// rather than a real lifecycle state).
		return true
	}
	if !rh.notifyVerifyPipelineReady(entry) {
		return false
	}
	return rh.notifyTaskStateTerminal(entry)
}

// notifyVerifyPipelineReady is Layer 1 of the notify gate: a RunOnIntegration
// / RunOnMain task that the worker reported as completed must not be notified
// until applyVerifyOutcome (or the silent-ack supersede path) has stamped the
// VerifyOutcomeAppliedAt marker on the result entry.
func (rh *ResultHandler) notifyVerifyPipelineReady(entry *model.TaskResult) bool {
	if !(entry.RunOnIntegration || entry.RunOnMain) || entry.Status != model.StatusCompleted {
		return true
	}
	if entry.VerifyOutcomeAppliedAt != nil && *entry.VerifyOutcomeAppliedAt != "" {
		return true
	}
	rh.log(LogLevelDebug,
		"notify_gate_defer_verify_pipeline task=%s command=%s result=%s "+
			"(RunOnIntegration=%t RunOnMain=%t VerifyOutcomeAppliedAt=nil — "+
			"awaiting applyVerifyOutcome / supersede stamp)",
		entry.TaskID, entry.CommandID, entry.ID,
		entry.RunOnIntegration, entry.RunOnMain)
	return false
}

// notifyTaskStateTerminal is Layer 2 of the notify gate (back-stop): consult
// the state file and require the task's tracked state to be terminal before
// we let the notify proceed. Any read/parse/lookup miss defers to next scan.
func (rh *ResultHandler) notifyTaskStateTerminal(entry *model.TaskResult) bool {
	statePath := commandStatePath(rh.maestroDir, entry.CommandID)
	data, err := os.ReadFile(statePath) //nolint:gosec // controlled application state path
	if err != nil {
		rh.log(LogLevelDebug,
			"notify_gate_defer_state_unreadable task=%s command=%s result=%s error=%v",
			entry.TaskID, entry.CommandID, entry.ID, err)
		return false
	}
	if len(data) == 0 {
		rh.log(LogLevelDebug,
			"notify_gate_defer_state_empty task=%s command=%s result=%s",
			entry.TaskID, entry.CommandID, entry.ID)
		return false
	}
	var cs model.CommandState
	if err := yamlv3.Unmarshal(data, &cs); err != nil {
		rh.log(LogLevelDebug,
			"notify_gate_defer_state_parse_error task=%s command=%s result=%s error=%v",
			entry.TaskID, entry.CommandID, entry.ID, err)
		return false
	}
	status, ok := cs.TaskStates[entry.TaskID]
	if !ok {
		rh.log(LogLevelDebug,
			"notify_gate_defer_task_missing task=%s command=%s result=%s",
			entry.TaskID, entry.CommandID, entry.ID)
		return false
	}
	if !model.IsTerminal(status) {
		rh.log(LogLevelDebug,
			"notify_gate_defer_state_nonterminal task=%s command=%s result=%s state=%s",
			entry.TaskID, entry.CommandID, entry.ID, status)
		return false
	}
	return true
}

// taskSupersededByRetry reports whether the given task has been
// terminal-cancelled with a "superseded_by_*" reason, meaning a retry
// task has been scheduled and the Planner will hear about the retry's
// outcome rather than this original result. Used to silently mark the
// original result as notified so it doesn't sit in the queue (the
// notify message would carry a stale "completed" status — the worker
// reported completed before verify overruled — and confuse the Planner).
func (rh *ResultHandler) taskSupersededByRetry(commandID, taskID string) bool {
	if commandID == "" || taskID == "" {
		return false
	}
	statePath := commandStatePath(rh.maestroDir, commandID)
	data, err := os.ReadFile(statePath) //nolint:gosec // controlled application state path
	if err != nil || len(data) == 0 {
		return false
	}
	var cs model.CommandState
	if err := yamlv3.Unmarshal(data, &cs); err != nil {
		return false
	}
	if cs.TaskStates[taskID] != model.StatusCancelled {
		return false
	}
	reason, ok := cs.CancelledReasons[taskID]
	if !ok {
		return false
	}
	return strings.HasPrefix(reason, "superseded_by_")
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

// Phase C learning helpers (recordTaskResultLearning,
// recordFingerprintCapture, recordBanditReward, recordSearchTreeOutcome,
// recordEvolutionSignal, workerModelName) live in result_learning.go.

// Notification delivery and orchestrator-queue write helpers
// (notifyPlannerOfWorkerResultWithRetry, notifyPlannerOfWorkerResult,
// notifyOrchestratorOfCommandResult, writeNotificationToOrchestratorQueue,
// notificationAction / notificationActionAppend|Superseded|Dedup,
// notificationTypeForStatus, indexNotificationBySourceResult,
// supersedeOrSkipNotification, appendNewNotification,
// WriteNotificationToOrchestratorQueue, emitResultDeadLetterNotification)
// live in result_notification.go.

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
