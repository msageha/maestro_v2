package daemon

import (
	"io"
	"log"
	"regexp"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/daemon/lease"
	"github.com/msageha/maestro_v2/internal/daemon/paneactivity"
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

	items := qh.collectExpiredTaskBusyChecks(tq, "worker1", "/tmp/worker1.yaml", &dirty)

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

	items := qh.collectExpiredTaskBusyChecks(tq, "worker1", "/tmp/worker1.yaml", &dirty)

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

	items := qh.collectExpiredTaskBusyChecks(tq, "worker1", "/tmp/worker1.yaml", &dirty)

	if len(items) != 1 {
		t.Fatalf("expected one busy-check item when verdict is idle, got %d", len(items))
	}
	if items[0].EntryID != "task_1" {
		t.Errorf("busy-check EntryID = %q, want task_1", items[0].EntryID)
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
