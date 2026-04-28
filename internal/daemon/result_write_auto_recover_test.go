package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/daemon/worktree"
	"github.com/msageha/maestro_v2/internal/model"
)

// stubAutoRecoverWorktreeManager is a minimal autoRecoverWorktreeManager
// stand-in for unit-testing maybeAutoRecoverAfterResolution. Each method
// returns the canned (action, err) pair the test installed for that call.
type stubAutoRecoverWorktreeManager struct {
	autoRecoverAction worktree.AutoRecoverAction
	autoRecoverErr    error
	resetErr          error

	// Counters so the test can assert which path was hit.
	autoRecoverCalls int
	resetCalls       int
}

func (s *stubAutoRecoverWorktreeManager) AutoRecoverAfterResolution(_ context.Context, _, _ string, _ bool) (worktree.AutoRecoverAction, error) {
	s.autoRecoverCalls++
	return s.autoRecoverAction, s.autoRecoverErr
}

func (s *stubAutoRecoverWorktreeManager) ResetResolvingWorkerToConflict(_, _ string) error {
	s.resetCalls++
	return s.resetErr
}

// captureLog returns a logFunc that records every (level, msg) pair into the
// supplied slice for downstream assertions.
type capturedLog struct {
	level LogLevel
	msg   string
}

func captureLog(out *[]capturedLog) logFunc {
	return func(level LogLevel, format string, args ...any) {
		// Build the rendered message lazily — tests that assert on substrings
		// pay the format cost; tests that only count entries ignore msg.
		*out = append(*out, capturedLog{level: level, msg: format})
		_ = args
	}
}

// newAutoRecoverTestAPI assembles the smallest ResultWriteAPI shape that
// maybeAutoRecoverAfterResolution depends on (worktreeManager + ctx +
// logFn). Other ResultWriteAPI dependencies are deliberately left zero —
// the helper only exercises the post-completion AutoRecover hook.
func newAutoRecoverTestAPI(t *testing.T, mgr autoRecoverWorktreeManager, logs *[]capturedLog) *ResultWriteAPI {
	t.Helper()
	return &ResultWriteAPI{
		apiContext: &apiContext{
			logFn: captureLog(logs),
		},
		ctx:             func() context.Context { return context.Background() },
		worktreeManager: mgr,
	}
}

func hasLogAt(logs []capturedLog, level LogLevel, contains string) bool {
	for _, e := range logs {
		if e.level == level && strings.Contains(e.msg, contains) {
			return true
		}
	}
	return false
}

// TestMaybeAutoRecoverAfterResolution_NoWorktreeStateIsSwallowed pins the
// post-publish run_on_main verification regression: once the worktree
// pipeline has cleaned up the command's state file, AutoRecoverAfterResolution
// returns ErrNoWorktreeState. That is the *expected* shape for a successful
// publish (there is nothing left to recover), so it must be logged at debug
// rather than warn — emitting "auto_recover_after_resolution_failed" for
// every successful publish drowned operators in false-positive noise during
// the 2026-04 audit.
func TestMaybeAutoRecoverAfterResolution_NoWorktreeStateIsSwallowed(t *testing.T) {
	t.Parallel()
	stub := &stubAutoRecoverWorktreeManager{
		autoRecoverErr: worktree.ErrNoWorktreeState,
	}
	var logs []capturedLog
	api := newAutoRecoverTestAPI(t, stub, &logs)

	api.maybeAutoRecoverAfterResolution(
		ResultWriteParams{CommandID: "cmd1", Reporter: "worker1", TaskID: "t1"},
		model.StatusCompleted,
		model.StatusCompleted,
		false,
	)

	if stub.autoRecoverCalls != 1 {
		t.Fatalf("autoRecoverCalls = %d, want 1", stub.autoRecoverCalls)
	}
	if hasLogAt(logs, LogLevelWarn, "auto_recover_after_resolution_failed") {
		t.Errorf("ErrNoWorktreeState must not surface as warn-level failure log: %+v", logs)
	}
	if !hasLogAt(logs, LogLevelDebug, "auto_recover_after_resolution_no_state") {
		t.Errorf("expected debug-level no-state log; got %+v", logs)
	}
}

// TestMaybeAutoRecoverAfterResolution_GenericErrorStillWarns guards against
// the no-state swallow accidentally widening: any non-ErrNoWorktreeState
// failure (e.g. ResumeMerge git op error) must still surface as warn so
// operators see real recovery breakage.
func TestMaybeAutoRecoverAfterResolution_GenericErrorStillWarns(t *testing.T) {
	t.Parallel()
	stub := &stubAutoRecoverWorktreeManager{
		autoRecoverErr: errors.New("git fetch failed"),
	}
	var logs []capturedLog
	api := newAutoRecoverTestAPI(t, stub, &logs)

	api.maybeAutoRecoverAfterResolution(
		ResultWriteParams{CommandID: "cmd1", Reporter: "worker1", TaskID: "t1"},
		model.StatusCompleted,
		model.StatusCompleted,
		false,
	)

	if !hasLogAt(logs, LogLevelWarn, "auto_recover_after_resolution_failed") {
		t.Errorf("real recovery failure must still log at warn: %+v", logs)
	}
}

// TestMaybeAutoRecoverAfterResolution_ResetSwallowsNoState mirrors the same
// gate on the StatusFailed branch: a failed task whose worktree state has
// already been torn down has no resolving worker to reset, and that path
// also went through the warn-level "reset_resolving_worker_failed" line
// before the swallow was added.
func TestMaybeAutoRecoverAfterResolution_ResetSwallowsNoState(t *testing.T) {
	t.Parallel()
	stub := &stubAutoRecoverWorktreeManager{
		resetErr: worktree.ErrNoWorktreeState,
	}
	var logs []capturedLog
	api := newAutoRecoverTestAPI(t, stub, &logs)

	api.maybeAutoRecoverAfterResolution(
		ResultWriteParams{CommandID: "cmd1", Reporter: "worker1", TaskID: "t1"},
		model.StatusFailed,
		model.StatusFailed,
		false,
	)

	if stub.resetCalls != 1 {
		t.Fatalf("resetCalls = %d, want 1", stub.resetCalls)
	}
	if hasLogAt(logs, LogLevelWarn, "reset_resolving_worker_failed") {
		t.Errorf("ErrNoWorktreeState must not surface as warn on the failed branch: %+v", logs)
	}
	if !hasLogAt(logs, LogLevelDebug, "reset_resolving_worker_no_state") {
		t.Errorf("expected debug-level no-state log; got %+v", logs)
	}
}
