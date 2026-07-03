package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/daemon/lease"
	"github.com/msageha/maestro_v2/internal/daemon/paneactivity"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// TestObservePaneActivityForAgent_ReturnsActiveOnCrossScanHashDelta pins
// the pane-activity fast path: a worker whose pane content changes
// between two scans is treated as alive without going through the
// 5-second busy-check probe.
func TestObservePaneActivityForAgent_ReturnsActiveOnCrossScanHashDelta(t *testing.T) {
	tracker := paneactivity.New(nil)
	calls := 0
	contents := []string{"first scan output", "second scan output (changed)"}
	qh := &QueueHandler{
		paneActivity: tracker,
		paneFinder:   func(agentID string) (string, error) { return "session:0.1", nil },
		paneCapture: func(_ string) (string, error) {
			c := contents[calls]
			calls++
			return c, nil
		},
	}
	qh.config.Watcher.ScanIntervalSec = 60
	clock := &fixedClock{now: time.Unix(1_700_000_000, 0).UTC()}
	qh.clock = clock

	// First scan: baseline only — must be inactive.
	if active := qh.observePaneActivityForAgent("worker1"); active {
		t.Fatalf("first observation must be inactive (no baseline)")
	}

	// Advance clock past the scan-interval gating window.
	clock.now = time.Unix(1_700_000_120, 0).UTC()

	// Second scan with different content → active.
	if active := qh.observePaneActivityForAgent("worker1"); !active {
		t.Fatalf("second observation with hash delta must be active")
	}
}

// TestObservePaneActivityForAgent_ReturnsActiveOnBusyPattern guards the
// pattern-match branch: a single observation showing a known busy
// marker (e.g., "Thinking…") is enough to declare alive even without a
// previous snapshot.
func TestObservePaneActivityForAgent_ReturnsActiveOnBusyPattern(t *testing.T) {
	tracker := paneactivity.New(regexp.MustCompile(`Thinking|Working`))
	qh := &QueueHandler{
		paneActivity: tracker,
		paneFinder:   func(string) (string, error) { return "session:0.1", nil },
		paneCapture:  func(string) (string, error) { return "✻ Thinking…", nil },
	}
	qh.config.Watcher.ScanIntervalSec = 60
	qh.clock = &fixedClock{now: time.Unix(1_700_000_000, 0).UTC()}

	if active := qh.observePaneActivityForAgent("worker1"); !active {
		t.Fatalf("busy_pattern match must yield active without prior baseline")
	}
}

// TestObservePaneActivityForAgent_FailsClosedOnCaptureError ensures any
// tmux failure (pane gone, capture error) returns false so the caller
// falls back to the conservative busy-check path.
func TestObservePaneActivityForAgent_FailsClosedOnCaptureError(t *testing.T) {
	tracker := paneactivity.New(nil)
	qh := &QueueHandler{
		paneActivity: tracker,
		paneFinder:   func(string) (string, error) { return "session:0.1", nil },
		paneCapture: func(string) (string, error) {
			return "", errCaptureUnavailable
		},
	}
	qh.config.Watcher.ScanIntervalSec = 60
	qh.clock = &fixedClock{now: time.Unix(1_700_000_000, 0).UTC()}
	if active := qh.observePaneActivityForAgent("worker1"); active {
		t.Fatalf("capture error must yield inactive (busy-check fallback)")
	}
}

// TestObservePaneActivityForAgent_NoTrackerReturnsFalse pins the
// nil-tracker safety: a QueueHandler without paneActivity wired must
// not panic — it just returns inactive so the legacy code path runs.
func TestObservePaneActivityForAgent_NoTrackerReturnsFalse(t *testing.T) {
	qh := &QueueHandler{}
	if active := qh.observePaneActivityForAgent("worker1"); active {
		t.Fatalf("nil tracker must yield inactive")
	}
}

// TestCollectExpiredTaskBusyChecks_ExtendsLeaseWhenPaneActive asserts
// that when a worker's task lease has expired but the worker pane shows
// cross-scan activity, collectExpiredTaskBusyChecks extends the lease
// in place and emits no busy-check item. Eliminates the
// dispatch_lease_sec → re-dispatch loop on long-running tasks.
func TestCollectExpiredTaskBusyChecks_ExtendsLeaseWhenPaneActive(t *testing.T) {
	tracker := paneactivity.New(regexp.MustCompile(`Working|Thinking`))

	clock := &fixedClock{now: time.Unix(1_700_000_000, 0).UTC()}
	qh := newQueueHandlerForPaneActivityTest(t, tracker, clock)
	// Pane shows a clear busy marker so the activity verdict is
	// active without needing a baseline.
	qh.paneCapture = func(string) (string, error) { return "✻ Working...", nil }
	qh.paneFinder = func(string) (string, error) { return "session:0.1", nil }

	// Build a queue containing a single in-progress task whose lease has
	// already expired (lease_expires_at in the past).
	expiredAt := clock.now.Add(-1 * time.Minute).Format(time.RFC3339)
	owner := "daemon:1"
	tq := &taskQueueEntry{
		Queue: model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks: []model.Task{{
				ID:             "task_1",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiredAt,
				LeaseEpoch:     2,
				UpdatedAt:      clock.now.Add(-2 * time.Minute).Format(time.RFC3339),
				CommandID:      "cmd_1",
			}},
		},
		Path: "/tmp/worker1.yaml",
	}
	dirty := false

	items := qh.collectExpiredTaskBusyChecks(tq, "worker1", "/tmp/worker1.yaml", &dirty, nil)

	if len(items) != 0 {
		t.Fatalf("expected zero busy-check items when pane is active, got %d (%+v)", len(items), items)
	}
	if !dirty {
		t.Fatalf("dirty must flip true after lease extension")
	}
	got := tq.Queue.Tasks[0]
	if got.Status != model.StatusInProgress {
		t.Errorf("task status = %s, want in_progress (lease extended)", got.Status)
	}
	if got.LeaseExpiresAt == nil {
		t.Fatalf("LeaseExpiresAt must be populated after extension")
	}
	newExpiry, err := time.Parse(time.RFC3339, *got.LeaseExpiresAt)
	if err != nil {
		t.Fatalf("LeaseExpiresAt unparseable: %v", err)
	}
	if !newExpiry.After(clock.now) {
		t.Errorf("new lease expiry %s must be in the future relative to clock %s", newExpiry, clock.now)
	}
}

// TestCollectExpiredTaskBusyChecks_GraceExtendsWhenNoBaseline asserts
// that when the pane has no previous snapshot to compare against
// (worker just admitted, daemon just started, ForgetAgent was called),
// the verdict is VerdictUncertain and the function grace-extends the
// lease for one cycle so the next scan has a real baseline to judge.
// Releasing the lease on this verdict would re-dispatch workers mid-task.
func TestCollectExpiredTaskBusyChecks_GraceExtendsWhenNoBaseline(t *testing.T) {
	tracker := paneactivity.New(nil)
	clock := &fixedClock{now: time.Unix(1_700_000_000, 0).UTC()}
	qh := newQueueHandlerForPaneActivityTest(t, tracker, clock)
	qh.paneCapture = func(string) (string, error) { return "(no change since last scan)", nil }
	qh.paneFinder = func(string) (string, error) { return "session:0.1", nil }

	expiredAt := clock.now.Add(-1 * time.Minute).Format(time.RFC3339)
	owner := "daemon:1"
	tq := &taskQueueEntry{
		Queue: model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks: []model.Task{{
				ID:             "task_1",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiredAt,
				LeaseEpoch:     2,
				UpdatedAt:      clock.now.Add(-2 * time.Minute).Format(time.RFC3339),
				CommandID:      "cmd_1",
			}},
		},
		Path: "/tmp/worker1.yaml",
	}
	dirty := false

	items := qh.collectExpiredTaskBusyChecks(tq, "worker1", "/tmp/worker1.yaml", &dirty, nil)

	if len(items) != 0 {
		t.Fatalf("expected zero busy-check items when verdict is uncertain (grace extension), got %d", len(items))
	}
	if !dirty {
		t.Fatalf("dirty must flip true after grace extension")
	}
	got := tq.Queue.Tasks[0]
	if got.Status != model.StatusInProgress {
		t.Errorf("task status = %s, want in_progress (lease grace-extended)", got.Status)
	}
	if got.LeaseExpiresAt == nil {
		t.Fatalf("LeaseExpiresAt must be populated after grace extension")
	}
	newExpiry, err := time.Parse(time.RFC3339, *got.LeaseExpiresAt)
	if err != nil {
		t.Fatalf("LeaseExpiresAt unparseable: %v", err)
	}
	if !newExpiry.After(clock.now) {
		t.Errorf("new lease expiry %s must be in the future relative to clock %s", newExpiry, clock.now)
	}
}

// TestCollectExpiredTaskBusyChecks_FallsBackToBusyCheckWhenPaneIdle pins
// the negative direction: when there IS a baseline AND the pane content
// has not changed across scans (VerdictIdle), the conservative
// busy-check probe path must run so genuinely dead workers can be
// caught. Distinguishing this from the no-baseline grace case (above)
// is the whole point of the trichotomous verdict.
func TestCollectExpiredTaskBusyChecks_FallsBackToBusyCheckWhenPaneIdle(t *testing.T) {
	tracker := paneactivity.New(nil)
	clock := &fixedClock{now: time.Unix(1_700_000_000, 0).UTC()}
	qh := newQueueHandlerForPaneActivityTest(t, tracker, clock)
	qh.paneCapture = func(string) (string, error) { return "(no change since last scan)", nil }
	qh.paneFinder = func(string) (string, error) { return "session:0.1", nil }

	// Pre-seed a baseline so the verdict reaches Idle (not Uncertain).
	// Recording the snapshot directly mirrors what an earlier scan
	// would have done before this lease ever expired.
	tracker.RecordObservation("worker1", "(no change since last scan)",
		clock.now.Add(-2*time.Minute).UTC())

	expiredAt := clock.now.Add(-1 * time.Minute).Format(time.RFC3339)
	owner := "daemon:1"
	tq := &taskQueueEntry{
		Queue: model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks: []model.Task{{
				ID:             "task_1",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiredAt,
				LeaseEpoch:     2,
				UpdatedAt:      clock.now.Add(-2 * time.Minute).Format(time.RFC3339),
				CommandID:      "cmd_1",
			}},
		},
	}
	dirty := false

	items := qh.collectExpiredTaskBusyChecks(tq, "worker1", "/tmp/worker1.yaml", &dirty, nil)

	if len(items) != 1 {
		t.Fatalf("expected one busy-check item when verdict is idle, got %d", len(items))
	}
	if items[0].EntryID != "task_1" {
		t.Errorf("busy-check EntryID = %q, want task_1", items[0].EntryID)
	}
}

// TestStepBlockedPaneTimeout_FailsTaskAtScanTickGranularity pins that
// the new scan-tick blocked-pane fail path (Report 2026-05-05 P0-B)
// fires within one scan after the threshold elapses, instead of waiting
// for lease expiry. blockedSince is seeded directly via Tracker
// observation so the test does not depend on lease expiry timing.
func TestStepBlockedPaneTimeout_FailsTaskAtScanTickGranularity(t *testing.T) {
	t.Setenv("MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC", "60")
	tracker := paneactivity.New(nil)
	clock := &fixedClock{now: time.Unix(1_700_000_000, 0).UTC()}
	qh := newQueueHandlerForPaneActivityTest(t, tracker, clock)
	// failTaskBlockedPane writes a synthetic failed result via
	// updateYAMLFile under "result:<worker>" lockMap, so we need a real
	// directory + lock map to keep the path under test exercising the
	// production lock ordering rather than skipping it.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "results"), 0o750); err != nil {
		t.Fatalf("mkdir results: %v", err)
	}
	qh.maestroDir = dir
	qh.lockMap = lock.NewMutexMap()
	// Capture returns a Claude Code box-wrapped approval prompt every
	// time the scanner asks; this drives ObserveVerdict → VerdictBlocked.
	qh.paneCapture = func(string) (string, error) {
		return "│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No\n", nil
	}
	qh.paneFinder = func(string) (string, error) { return "session:0.1", nil }
	// Stage a prior blocked observation 90s ago — past the 60s threshold.
	tracker.RecordObservation("worker1", "│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No\n",
		clock.now.Add(-90*time.Second).UTC())
	// Force blockedSince to that earlier timestamp by calling
	// ObserveVerdict at the historical time.
	_ = tracker.ObserveVerdict("worker1",
		"│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No\n",
		time.Minute, clock.now.Add(-90*time.Second).UTC())

	owner := "daemon:1"
	expiredAt := clock.now.Add(time.Hour).Format(time.RFC3339) // lease NOT expired
	tq := &taskQueueEntry{
		Queue: model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks: []model.Task{{
				ID:             "task_blocked",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiredAt,
				LeaseEpoch:     1,
				UpdatedAt:      clock.now.Add(-2 * time.Minute).Format(time.RFC3339),
				CommandID:      "cmd_blocked",
			}},
		},
	}
	s := &scanState{
		tasks:     map[string]*taskQueueEntry{"/tmp/worker1.yaml": tq},
		taskDirty: map[string]bool{},
	}

	qh.stepBlockedPaneTimeout(s)

	if got := tq.Queue.Tasks[0].Status; got != model.StatusFailed {
		t.Fatalf("task status = %s, want failed (scan-tick timeout did not fire)", got)
	}
	if !s.taskDirty["/tmp/worker1.yaml"] {
		t.Errorf("taskDirty was not flagged after fail")
	}
}

func TestStepBlockedPaneTimeout_UnrecoverableUsesShorterThreshold(t *testing.T) {
	t.Setenv("MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC", "60")
	t.Setenv("MAESTRO_BLOCKED_PANE_UNRECOVERABLE_FAIL_AFTER_SEC", "30")
	tracker := paneactivity.New(nil)
	clock := &fixedClock{now: time.Unix(1_700_000_000, 0).UTC()}
	qh := newQueueHandlerForPaneActivityTest(t, tracker, clock)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "results"), 0o750); err != nil {
		t.Fatalf("mkdir results: %v", err)
	}
	qh.maestroDir = dir
	qh.lockMap = lock.NewMutexMap()

	ordinaryPrompt := "│ Do you want to proceed?\n│ ❯ 1. Yes\n│   2. No\n"
	unrecoverablePrompt := "Bash command (unsandboxed)\nDo you want to proceed?\n❯ 1. Yes\n  2. No\n"
	qh.paneFinder = func(agentID string) (string, error) { return agentID, nil }
	qh.paneCapture = func(paneTarget string) (string, error) {
		switch paneTarget {
		case "worker1":
			return ordinaryPrompt, nil
		case "worker2":
			return unrecoverablePrompt, nil
		default:
			return "", nil
		}
	}

	since := clock.now.Add(-45 * time.Second).UTC()
	_ = tracker.ObserveVerdict("worker1", ordinaryPrompt, time.Minute, since)
	_ = tracker.ObserveVerdict("worker2", unrecoverablePrompt, time.Minute, since)

	owner := "daemon:1"
	leaseFuture := clock.now.Add(time.Hour).Format(time.RFC3339)
	ordinaryQueue := &taskQueueEntry{
		Queue: model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks: []model.Task{{
				ID:             "task_ordinary",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &leaseFuture,
				LeaseEpoch:     1,
				UpdatedAt:      clock.now.Add(-time.Minute).Format(time.RFC3339),
				CommandID:      "cmd_ordinary",
			}},
		},
	}
	unrecoverableQueue := &taskQueueEntry{
		Queue: model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks: []model.Task{{
				ID:             "task_unrecoverable",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &leaseFuture,
				LeaseEpoch:     1,
				UpdatedAt:      clock.now.Add(-time.Minute).Format(time.RFC3339),
				CommandID:      "cmd_unrecoverable",
			}},
		},
	}
	s := &scanState{
		tasks: map[string]*taskQueueEntry{
			"/tmp/worker1.yaml": ordinaryQueue,
			"/tmp/worker2.yaml": unrecoverableQueue,
		},
		taskDirty: map[string]bool{},
	}

	qh.stepBlockedPaneTimeout(s)

	if got := ordinaryQueue.Queue.Tasks[0].Status; got != model.StatusInProgress {
		t.Fatalf("ordinary blocked task status = %s, want in_progress before normal threshold", got)
	}
	if got := unrecoverableQueue.Queue.Tasks[0].Status; got != model.StatusFailed {
		t.Fatalf("unrecoverable blocked task status = %s, want failed at short threshold", got)
	}
	if s.taskDirty["/tmp/worker1.yaml"] {
		t.Errorf("ordinary blocked task must not be marked dirty before normal threshold")
	}
	if !s.taskDirty["/tmp/worker2.yaml"] {
		t.Errorf("unrecoverable blocked task must be marked dirty after fast fail")
	}
}

func TestBlockedPaneUnrecoverableFailAfterEnvAndClamp(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv("MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC", "")
		t.Setenv("MAESTRO_BLOCKED_PANE_UNRECOVERABLE_FAIL_AFTER_SEC", "")
		if got := blockedPaneUnrecoverableFailAfter(); got != 30*time.Second {
			t.Fatalf("blockedPaneUnrecoverableFailAfter = %s, want 30s", got)
		}
	})
	t.Run("env_override", func(t *testing.T) {
		t.Setenv("MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC", "60")
		t.Setenv("MAESTRO_BLOCKED_PANE_UNRECOVERABLE_FAIL_AFTER_SEC", "15")
		if got := blockedPaneUnrecoverableFailAfter(); got != 15*time.Second {
			t.Fatalf("blockedPaneUnrecoverableFailAfter = %s, want 15s", got)
		}
	})
	t.Run("invalid_falls_back_to_default", func(t *testing.T) {
		t.Setenv("MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC", "60")
		t.Setenv("MAESTRO_BLOCKED_PANE_UNRECOVERABLE_FAIL_AFTER_SEC", "not-a-number")
		if got := blockedPaneUnrecoverableFailAfter(); got != 30*time.Second {
			t.Fatalf("blockedPaneUnrecoverableFailAfter = %s, want 30s", got)
		}
	})
	t.Run("non_positive_falls_back_to_default", func(t *testing.T) {
		t.Setenv("MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC", "60")
		t.Setenv("MAESTRO_BLOCKED_PANE_UNRECOVERABLE_FAIL_AFTER_SEC", "0")
		if got := blockedPaneUnrecoverableFailAfter(); got != 30*time.Second {
			t.Fatalf("blockedPaneUnrecoverableFailAfter = %s, want 30s", got)
		}
	})
	t.Run("clamped_to_normal_threshold", func(t *testing.T) {
		t.Setenv("MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC", "20")
		t.Setenv("MAESTRO_BLOCKED_PANE_UNRECOVERABLE_FAIL_AFTER_SEC", "120")
		if got := blockedPaneUnrecoverableFailAfter(); got != 20*time.Second {
			t.Fatalf("blockedPaneUnrecoverableFailAfter = %s, want 20s clamp", got)
		}
	})
	t.Run("normal_kill_switch_clamps_to_zero", func(t *testing.T) {
		t.Setenv("MAESTRO_BLOCKED_PANE_FAIL_AFTER_SEC", "0")
		t.Setenv("MAESTRO_BLOCKED_PANE_UNRECOVERABLE_FAIL_AFTER_SEC", "15")
		if got := blockedPaneUnrecoverableFailAfter(); got != 0 {
			t.Fatalf("blockedPaneUnrecoverableFailAfter = %s, want 0s clamp", got)
		}
	})
}

// TestStepBlockedPaneTimeout_FailsImmediatelyOnTerminalError pins the
// post-2026-05-06 P0 fix: a worker pane showing a runtime terminal-error
// frame (Claude API 4xx, content filter, …) must be failed at scan-tick
// granularity, NOT at lease expiry. Previously the fast-fail logic was
// only wired to the lease-expiry path, so a terminal-error frame on a
// task whose lease was still valid sat there for max_in_progress_min.
func TestStepBlockedPaneTimeout_FailsImmediatelyOnTerminalError(t *testing.T) {
	tracker := paneactivity.New(nil)
	clock := &fixedClock{now: time.Unix(1_700_000_000, 0).UTC()}
	qh := newQueueHandlerForPaneActivityTest(t, tracker, clock)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "results"), 0o750); err != nil {
		t.Fatalf("mkdir results: %v", err)
	}
	qh.maestroDir = dir
	qh.lockMap = lock.NewMutexMap()
	// Capture returns a Claude API content-filter terminal-error frame.
	// blockedHintRegex / activeHintRegex would otherwise classify this
	// pane as blocked / active; terminalErrorRegex must take precedence.
	terminalErrorContent := `Cogitating for 12s
API Error: 400 Output blocked by content filtering policy`
	qh.paneCapture = func(string) (string, error) {
		return terminalErrorContent, nil
	}
	qh.paneFinder = func(string) (string, error) { return "session:0.1", nil }

	owner := "daemon:1"
	leaseFuture := clock.now.Add(time.Hour).Format(time.RFC3339) // lease NOT expired
	tq := &taskQueueEntry{
		Queue: model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks: []model.Task{{
				ID:             "task_terminal",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &leaseFuture,
				LeaseEpoch:     1,
				UpdatedAt:      clock.now.Add(-2 * time.Minute).Format(time.RFC3339),
				CommandID:      "cmd_terminal",
			}},
		},
	}
	s := &scanState{
		tasks:     map[string]*taskQueueEntry{"/tmp/worker1.yaml": tq},
		taskDirty: map[string]bool{},
	}

	qh.stepBlockedPaneTimeout(s)

	if got := tq.Queue.Tasks[0].Status; got != model.StatusFailed {
		t.Fatalf("task status = %s, want failed (terminal error fast-fail did not fire at scan-tick)", got)
	}
	if !s.taskDirty["/tmp/worker1.yaml"] {
		t.Errorf("taskDirty was not flagged after terminal-error fail")
	}
}

// newQueueHandlerForPaneActivityTest assembles a QueueHandler skeleton
// adequate for collectExpiredTaskBusyChecks. The full daemon wiring
// (file store, metrics, executor) is out of scope here — only the
// fields the function under test reads need to be set.
func newQueueHandlerForPaneActivityTest(t *testing.T, tracker *paneactivity.Tracker, clock *fixedClock) *QueueHandler {
	t.Helper()
	qh := &QueueHandler{
		paneActivity: tracker,
		clock:        clock,
		timeCache:    newTimeParseCache(),
	}
	qh.config.Watcher.ScanIntervalSec = 60
	qh.config.Watcher.DispatchLeaseSec = 60
	logger := log.New(io.Discard, "", 0)
	qh.dl = NewDaemonLoggerFromLegacy("test", logger, LogLevelError)
	qh.logger = logger
	qh.logLevel = LogLevelError
	qh.leaseManager = lease.New(qh.config.Watcher, logger, core.LogLevelError, lease.WithClock(qh.clock))
	se := newScanPhaseExecutor(qh)
	qh.scanExecutor = se
	return qh
}

// errCaptureUnavailable is the sentinel returned by the capture stub in
// TestObservePaneActivityForAgent_FailsClosedOnCaptureError. Defined as
// a value rather than a fmt.Errorf to keep the test deterministic.
var errCaptureUnavailable = &captureError{msg: "stub: capture unavailable"}

type captureError struct{ msg string }

func (e *captureError) Error() string { return e.msg }

// Compile-time assertion that the test wiring still satisfies the model
// types it conjures so a refactor that breaks this file fails the build
// rather than the test run.
var _ = model.StatusInProgress

// TestCollectExpiredTaskBusyChecks_ReusesScanTickVerdict pins the E2E
// 2026-06-11 fix: stepBlockedPaneTimeout observes every in-progress
// worker's pane at the top of the scan tick, so the lease-expiry path
// must reuse that verdict instead of re-observing. A second observation
// in the same tick lands within minPrevAge and degrades to a same-scan
// VerdictUncertain, which the grace-extension path then extends on every
// expiry up to the 30-minute hard cap — a dead pane (claude process gone,
// shell prompt) was repeatedly "grace-extended one cycle" for 30 minutes.
func TestCollectExpiredTaskBusyChecks_ReusesScanTickVerdict(t *testing.T) {
	tracker := paneactivity.New(nil)
	clock := &fixedClock{now: time.Unix(1_700_000_000, 0).UTC()}
	qh := newQueueHandlerForPaneActivityTest(t, tracker, clock)
	captureCalls := 0
	qh.paneCapture = func(string) (string, error) {
		captureCalls++
		return "(idle shell prompt)", nil
	}
	qh.paneFinder = func(string) (string, error) { return "session:0.1", nil }

	expiredAt := clock.now.Add(-1 * time.Minute).Format(time.RFC3339)
	owner := "daemon:1"
	tq := &taskQueueEntry{
		Queue: model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks: []model.Task{{
				ID:             "task_1",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiredAt,
				LeaseEpoch:     2,
				UpdatedAt:      clock.now.Add(-2 * time.Minute).Format(time.RFC3339),
				CommandID:      "cmd_1",
			}},
		},
		Path: "/tmp/worker1.yaml",
	}
	dirty := false

	// Simulate stepBlockedPaneTimeout having already judged the pane Idle
	// earlier in this scan tick.
	scanVerdicts := map[string]paneactivity.Verdict{
		"worker1": paneactivity.VerdictIdle,
	}
	items := qh.collectExpiredTaskBusyChecks(tq, "worker1", "/tmp/worker1.yaml", &dirty, scanVerdicts)

	if captureCalls != 0 {
		t.Errorf("pane must NOT be re-observed when a scan-tick verdict is cached, captures=%d", captureCalls)
	}
	if len(items) != 1 {
		t.Fatalf("cached VerdictIdle must fall through to the busy-check probe, got %d items", len(items))
	}
}
