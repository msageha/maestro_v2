package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/daemon/paneactivity"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/metrics"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
)

// PhaseDiagnoserFunc produces a diagnosis prompt string from a completed phase's tasks.
// Returns "" if diagnosis yields no actionable information.
type PhaseDiagnoserFunc func(phase model.Phase, tasks []model.Task, results []model.TaskResult) string

// BusyChecker probes whether an agent is currently busy.
// Implementations are used to override the default executor-based probe in tests.
type BusyChecker interface {
	IsBusy(agentID string) bool
}

// BusyCheckerFunc adapts a plain function to the BusyChecker interface.
type BusyCheckerFunc func(agentID string) bool

// IsBusy calls the underlying function to check whether the given agent is busy.
func (f BusyCheckerFunc) IsBusy(agentID string) bool { return f(agentID) }

// QueueHandlerOption configures a QueueHandler after construction.
type QueueHandlerOption func(*QueueHandler)

// WithBusyChecker injects a BusyChecker to override the default executor-based probe.
func WithBusyChecker(bc BusyChecker) QueueHandlerOption {
	return func(qh *QueueHandler) {
		qh.scanExecutor.busyChecker = bc
	}
}

// QueueHandler orchestrates fsnotify event routing and periodic scan execution.
type QueueHandler struct {
	maestroDir string
	config     model.Config
	dl         *DaemonLogger
	logger     *log.Logger
	logLevel   LogLevel
	clock      Clock

	execProvider          *ExecutorProvider // shared across dispatcher, resultHandler, cancelHandler
	queueStore            QueueStore
	leaseManager          QueueLeaseManager
	dispatcher            QueueDispatcher
	dependencyResolver    QueueDependencyResolver
	cancelHandler         *CancelHandler
	resultHandler         *ResultHandler
	reconciler            *Reconciler
	deadLetterProcessor   *DeadLetterProcessor
	metricsHandler        *metrics.Handler
	circuitBreaker        *circuitbreaker.Handler
	admissionCtrl         *admission.Controller
	worktreeManager       QueueWorktreeManager
	deferredPlanCompleter DeferredPlanCompleterFunc
	lockMap               *lock.MutexMap

	// scanExecutor handles periodic scan orchestration and scan-specific state.
	scanExecutor *ScanPhaseExecutor

	// phantomSuspects tracks "state has it, queues don't" candidates across
	// scans (key: commandID+"/"+taskID). A candidate is only force-cleared
	// after being queue-absent on TWO consecutive scans: daemon-side retry
	// registration (RetryTaskAtomically, R9) writes state BEFORE the queue,
	// so a single-instant probe can catch a task mid-registration. Accessed
	// only from the serialized PeriodicScan goroutine — no lock needed.
	phantomSuspects map[string]int

	// abStateCache caches per-file parse results for readABCommandStates
	// (state-driven A/B group discovery runs every scan; most command
	// states never contain candidate_groups). Guarded by its own mutex.
	abStateCache abStateCache

	// scanRunMu exposes the scan run mutex for test cleanup synchronization.
	scanRunMu *sync.Mutex

	// daemonPID for lease_owner format "daemon:{pid}" per spec §5.8.1.
	daemonPID int

	// initMu protects Set* initialization methods independently from scan execution.
	// All Set* methods in handler_registry.go acquire this mutex instead of scanRunMu.
	initMu sync.Mutex

	// Shutdown guard: wired via SetShutdownGuard after construction.
	shutdownCtx  context.Context
	shuttingDown *atomic.Bool

	// sessionLost is set when the tmux session disappears. When true,
	// dispatch of new tasks/commands is paused.
	sessionLost *atomic.Bool

	// undecidedTracker tracks consecutive undecided busy-probe results per agent.
	undecidedTracker *undecidedTracker

	// paneActivity tracks per-agent pane content snapshots across scans so
	// the lease-expiry path can extend leases for visibly-active agents
	// without falling back to the operator-tuned dispatch_lease_sec timer.
	// Wired to the worker pane capture function (paneCapture) so tests can
	// stub the tmux call.
	paneActivity *paneactivity.Tracker
	// paneCapture returns the joined capture-pane content for paneTarget.
	// Defaults to internal/tmux.CapturePaneJoined; tests inject a stub.
	paneCapture func(paneTarget string) (string, error)
	// paneFinder resolves an agentID to a tmux pane target. Defaults to
	// tmux.FindPaneByAgentID; tests inject a stub so the activity check
	// can run without a real tmux session.
	paneFinder func(agentID string) (string, error)

	// timeCache caches time.Parse(time.RFC3339, ...) results within a scan
	// cycle to avoid repeated parsing of identical timestamp strings.
	timeCache *timeParseCache

	// phaseC exposes Phase C components (complexity scoring, feature gating,
	// bandit, fingerprint DB, etc.) to the dispatch pipeline. Wired via
	// SetPhaseCManager after daemon startup; nil-safe at all call sites.
	phaseC *PhaseCManager

	// consecutiveCascadeBreakScans tracks how many recent scan ticks ended
	// with the signal cascade-break tripped. Persists across scans (the
	// per-tick signalCascadeTracker is local to stepDeliverSignals) so the
	// daemon can surface the meta-circuit "tmux delivery has been degraded
	// for N consecutive ticks" instead of only the per-tick message. This
	// counter is the primitive an operator-facing meta-circuit reads.
	consecutiveCascadeBreakScans atomic.Int32

	// phaseMergeDeferStart records the first time isPhaseMergeRecorded
	// observed a per-phase deferral so we can escalate when the merge gate
	// is stuck for too long (the orchestration must never wedge on a
	// transient or silently failed merge — a force-mark unblocks Phase
	// Completed and lets the Planner proceed). Keyed by
	// "<commandID>::<phaseID>". Cleared when the phase merge is recorded.
	phaseMergeDeferStart sync.Map

	// awaitingFillStallNotifiedClock records the last time the watchdog
	// fired an awaiting_fill_stall signal so subsequent scan cycles can
	// re-fire after a configurable interval. Keyed by
	// "<commandID>::<phaseID>". Cleared on phase exit (best-effort —
	// stale entries simply skip until the phase re-enters awaiting_fill).
	awaitingFillStallLastFire sync.Map

	// awaitingFillStallFireCount tracks how many times the watchdog has
	// re-fired an awaiting_fill_stall signal for a given (command, phase)
	// without progress. After awaitingFillStallEscalateAfter fires the
	// scanner escalates: marks plan_status=failed and emits an
	// Orchestrator command_failed notification so a Planner that has
	// genuinely wedged (input collision, queued messages stuck, claude
	// loop) does not hold the iteration open indefinitely. Keyed by
	// "<commandID>::<phaseID>"; reset to 0 on phase exit.
	awaitingFillStallFireCount sync.Map

	// publishQuarantineLogged tracks (command, reason) pairs already
	// surfaced as a worktree_publish_quarantined WARN. Quarantined
	// integration state is durable until an operator unquarantines
	// manually, so re-WARNing every scan tick is pure noise. Keyed by
	// "<commandID>::<reason>"; the value is an empty struct used as a
	// set marker. Cleared implicitly when the daemon restarts (we want
	// the WARN to fire again after a daemon bounce so an operator
	// inspecting a fresh log sees the situation).
	publishQuarantineLogged sync.Map
}

// NewQueueHandler creates a new QueueHandler with all sub-modules.
// A single shared ExecutorProvider is created and injected into all handlers
// that need executor access (Dispatcher, ResultHandler, CancelHandler).
func NewQueueHandler(maestroDir string, cfg model.Config, lockMap *lock.MutexMap, logger *log.Logger, logLevel LogLevel, opts ...QueueHandlerOption) *QueueHandler {
	components := newQueueComponents(maestroDir, cfg, lockMap, logger, logLevel)
	qh := &QueueHandler{
		maestroDir:          maestroDir,
		config:              cfg,
		dl:                  components.dl,
		logger:              logger,
		logLevel:            logLevel,
		clock:               components.clock,
		execProvider:        components.execProvider,
		queueStore:          components.queueStore,
		leaseManager:        components.leaseManager,
		dispatcher:          components.dispatcher,
		dependencyResolver:  components.dependencyResolver,
		cancelHandler:       components.cancelHandler,
		resultHandler:       components.resultHandler,
		reconciler:          components.reconciler,
		deadLetterProcessor: components.deadLetterProcessor,
		metricsHandler:      components.metricsHandler,
		lockMap:             lockMap,
		daemonPID:           os.Getpid(),
	}
	qh.undecidedTracker = newUndecidedTracker()
	qh.timeCache = newTimeParseCache()
	qh.paneActivity = paneactivity.New(compileBusyPattern(cfg.Watcher.BusyPatterns))
	qh.paneCapture = capturePaneJoinedFromTmux
	qh.paneFinder = tmux.FindPaneByAgentID
	se := newScanPhaseExecutor(qh)
	se.debounce = NewDebounceController(cfg.Watcher.DebounceSec, components.dl, qh.PeriodicScanWithContext)
	qh.scanExecutor = se
	qh.scanRunMu = &se.scanRunMu
	// Inline-retry abort hook: lets the dispatcher's per-task retry loop
	// short-circuit when the queue entry is no longer in_progress at the
	// expected lease epoch. Wired here (rather than via a SetXxx setter)
	// because qh and dispatcher are both visible to NewQueueHandler and
	// the relationship is intrinsic — the dispatcher belongs to qh.
	if qh.dispatcher != nil {
		qh.dispatcher.SetTaskAliveChecker(newQueueTaskAliveChecker(qh))
	}
	for _, opt := range opts {
		opt(qh)
	}
	return qh
}

// leaseOwnerID returns the lease owner identifier in "daemon:{pid}" format per spec §5.8.1.
func (qh *QueueHandler) leaseOwnerID() string {
	return fmt.Sprintf("daemon:%d", qh.daemonPID)
}

// compileBusyPattern returns a compiled regex matching any of the
// pipe-separated tokens in cfg.Watcher.BusyPatterns, or nil when the
// pattern is empty/invalid. nil is treated by the activity tracker as
// "pattern matching disabled — fall back to hash deltas only".
func compileBusyPattern(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return re
}

// capturePaneJoinedFromTmux is the production paneCapture implementation.
// Indirected through a function so tests can stub the tmux dependency.
//
// Captures both the main screen (with scrollback) and the alternate
// screen, then returns the concatenation. The scrollback range matters:
// claude-code's Bash tool renders the approval prompt as ordinary
// terminal output that scrolls past the visible viewport within a few
// rows of subsequent output. Reports of 2026-05-04 pinned the resulting
// blind spot — the daemon's previous capture (visible-only) returned a
// ~500-byte snippet of just the TUI status bar while the
// `Do you want to proceed?` / `Bash command (unsandboxed)` lines lived
// in scrollback. The user manually confirmed `tmux capture-pane -S -120`
// surfaces the prompt, while plain capture-pane and `-a` both miss it.
//
// We pull the last `paneCaptureScrollbackLines` rows of history together
// with the current visible content. The detector still runs `MatchString`
// over the concatenation, so any prompt rendered within that window is
// observable regardless of whether it has scrolled past the viewport.
//
// Best-effort: scrollback / alternate failures are silently swallowed;
// returning whatever portion we managed to capture keeps the detector
// alive even on tmux misbehaviour.
func capturePaneJoinedFromTmux(paneTarget string) (string, error) {
	main, err := tmux.CapturePaneJoined(paneTarget, paneCaptureScrollbackLines)
	if err != nil {
		return "", err
	}
	alt, _ := tmux.CapturePaneAlternateJoined(paneTarget, paneCaptureScrollbackLines)
	if alt == "" {
		return main, nil
	}
	if main == "" {
		return alt, nil
	}
	// Concatenate with a newline separator so line-anchored regexes can
	// match either buffer's content.
	return alt + "\n" + main, nil
}

// paneCaptureScrollbackLines is the number of scrollback rows the daemon
// pulls alongside the visible pane content. 200 rows comfortably covers
// the typical blocked-prompt window (claude-code emits ~20 rows of
// chrome around an approval banner) plus a margin for follow-up output
// that scrolls the prompt past the visible viewport. The trade-off is
// log bloat when MAESTRO_LOG_PANE_TAIL=1 is set: tail snippets stay
// truncated to 512 bytes so the scrollback inclusion does not balloon
// daemon.log size.
const paneCaptureScrollbackLines = 200

// findPaneTarget routes paneFinder lookups through the QueueHandler so
// tests can swap the function out without reaching into the tmux
// package globals.
func (qh *QueueHandler) findPaneTarget(agentID string) (string, error) {
	if qh.paneFinder == nil {
		return tmux.FindPaneByAgentID(agentID)
	}
	return qh.paneFinder(agentID)
}
