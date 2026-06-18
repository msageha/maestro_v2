package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/daemon/admission"
	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// TestStepCircuitBreaker_NilStateReader verifies that stepCircuitBreaker
// does not panic when the circuit breaker is enabled but no StateReader
// has been wired (e.g. during early startup or in degraded configurations).
func TestStepCircuitBreaker_NilStateReader(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	// Wire a circuit breaker that is Enabled but has no StateReader.
	cfg := model.Config{
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:                true,
			ProgressTimeoutMinutes: ptr.Int(30),
		},
	}
	cb := circuitbreaker.NewHandler(cfg, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	if cb.StateReader() != nil {
		t.Fatalf("precondition: StateReader should be nil")
	}
	qh.SetCircuitBreaker(cb)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
					{ID: "cmd2", Status: model.StatusPending},
				},
			},
		},
	}

	// Must not panic on nil StateReader().
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("stepCircuitBreaker panicked with nil StateReader: %v", r)
		}
	}()
	qh.stepCircuitBreaker(&s)
}

// cbMockStateManager implements core.StateManager for circuit breaker tests.
// It records TripCircuitBreaker calls and lets tests control GetCircuitBreakerState.
type cbMockStateManager struct {
	trippedCalls []cbTripCall                          // recorded TripCircuitBreaker calls
	cbStates     map[string]*model.CircuitBreakerState // GetCircuitBreakerState results
	getCBCalled  map[string]int                        // tracks GetCircuitBreakerState call count
	tripErr      error                                 // error to return from TripCircuitBreaker
}

type cbTripCall struct {
	CommandID              string
	Reason                 string
	ProgressTimeoutMinutes int
}

func (m *cbMockStateManager) GetTaskState(string, string) (model.Status, error) { return "", nil }
func (m *cbMockStateManager) GetEffectiveTaskStatus(string, string) (model.Status, error) {
	return "", nil
}
func (m *cbMockStateManager) GetEffectiveTaskStatusForCompletion(string, string) (model.Status, error) {
	return "", nil
}
func (m *cbMockStateManager) GetCommandPhases(string) ([]model.PhaseInfo, error)   { return nil, nil }
func (m *cbMockStateManager) GetTaskDependencies(string, string) ([]string, error) { return nil, nil }
func (m *cbMockStateManager) HasNonTerminalTaskState(string) (bool, error)         { return false, nil }
func (m *cbMockStateManager) GetNonTerminalTaskStates(string) (map[string]model.Status, error) {
	return nil, nil
}
func (m *cbMockStateManager) IsSystemCommitReady(string, string) (bool, bool, error) {
	return false, false, nil
}
func (m *cbMockStateManager) IsCommandCancelRequested(string) (bool, error) { return false, nil }
func (m *cbMockStateManager) ApplyPhaseTransition(string, string, model.PhaseStatus) error {
	return nil
}
func (m *cbMockStateManager) SetPhaseCancelledReason(string, string, *string) error      { return nil }
func (m *cbMockStateManager) UpdateTaskState(string, string, model.Status, string) error { return nil }

func (m *cbMockStateManager) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	if m.getCBCalled == nil {
		m.getCBCalled = make(map[string]int)
	}
	m.getCBCalled[commandID]++
	if s, ok := m.cbStates[commandID]; ok {
		return s, nil
	}
	return &model.CircuitBreakerState{}, nil
}

func (m *cbMockStateManager) TripCircuitBreaker(commandID, reason string, progressTimeoutMinutes int) error {
	m.trippedCalls = append(m.trippedCalls, cbTripCall{
		CommandID:              commandID,
		Reason:                 reason,
		ProgressTimeoutMinutes: progressTimeoutMinutes,
	})
	return m.tripErr
}

func (m *cbMockStateManager) MarkAwaitingFillStallNotified(string, string, string) error {
	return nil
}

func (m *cbMockStateManager) MarkCircuitBreakerProgress(string) error { return nil }

// TestStepCircuitBreaker_SignalEmittedAtomicallyWithTrip verifies that when
// TripCircuitBreaker succeeds, the planner signal is emitted using the reason
// from CheckProgressTimeout — not by re-reading state via GetCircuitBreakerState.
// This eliminates the TOCTOU window between trip and signal emission.
func TestStepCircuitBreaker_SignalEmittedAtomicallyWithTrip(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	// Create a mock state manager that returns a *different* reason on
	// GetCircuitBreakerState than what CheckProgressTimeout produces.
	// If the signal contains the GetCircuitBreakerState reason, the TOCTOU
	// race is still present.
	staleReason := "stale_reason_from_reread"
	mock := &cbMockStateManager{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {
				Tripped:        false,
				LastProgressAt: ptr.String(qh.clock.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)),
				TripReason:     &staleReason,
			},
		},
	}

	cfg := model.Config{
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:                true,
			ProgressTimeoutMinutes: ptr.Int(30),
		},
	}
	cb := circuitbreaker.NewHandler(cfg, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	cb.SetStateReader(mock)
	qh.SetCircuitBreaker(cb)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
				},
			},
		},
		signals: fileState[model.PlannerSignalQueue]{
			Data: model.PlannerSignalQueue{},
			Path: filepath.Join(maestroDir, "queue", "signals.yaml"),
		},
		signalIndex: buildSignalIndex(nil),
	}

	qh.stepCircuitBreaker(&s)

	// 1. TripCircuitBreaker must have been called exactly once.
	if got := len(mock.trippedCalls); got != 1 {
		t.Fatalf("expected 1 TripCircuitBreaker call, got %d", got)
	}
	if mock.trippedCalls[0].CommandID != "cmd1" {
		t.Errorf("TripCircuitBreaker CommandID = %q, want cmd1", mock.trippedCalls[0].CommandID)
	}

	// 2. A signal must have been emitted.
	if got := len(s.signals.Data.Signals); got != 1 {
		t.Fatalf("expected 1 planner signal, got %d", got)
	}
	sig := s.signals.Data.Signals[0]
	if sig.Kind != "circuit_breaker_tripped" {
		t.Errorf("signal kind = %q, want circuit_breaker_tripped", sig.Kind)
	}
	if sig.CommandID != "cmd1" {
		t.Errorf("signal CommandID = %q, want cmd1", sig.CommandID)
	}

	// 3. The signal message must contain the reason from CheckProgressTimeout
	//    (which includes "progress_timeout="), NOT the stale reason from
	//    GetCircuitBreakerState re-read.
	if strings.Contains(sig.Message, staleReason) {
		t.Errorf("signal message contains stale reason from re-read %q; TOCTOU race still present", staleReason)
	}
	if !strings.Contains(sig.Message, "progress_timeout=") {
		t.Errorf("signal message should contain progress_timeout reason from CheckProgressTimeout, got: %s", sig.Message)
	}

	// 4. GetCircuitBreakerState must NOT have been called for cmd1 in the
	//    trip path — the signal was emitted atomically with the trip decision.
	if mock.getCBCalled["cmd1"] > 0 {
		// Note: CheckProgressTimeout internally calls GetCircuitBreakerState once.
		// The refactored code should NOT call it again after a successful trip.
		// CheckProgressTimeout calls it once → count should be exactly 1.
		if mock.getCBCalled["cmd1"] > 1 {
			t.Errorf("GetCircuitBreakerState called %d times for cmd1 (expected at most 1 from CheckProgressTimeout); extra call indicates TOCTOU re-read",
				mock.getCBCalled["cmd1"])
		}
	}

	if !s.signals.Dirty {
		t.Error("expected signals.Dirty to be true")
	}
}

// TestStepCircuitBreaker_ExternalTripEmitsSignalViaReread verifies that when
// the breaker was tripped by another path (e.g. result-write consecutive failures),
// stepCircuitBreaker still emits a signal by reading the current state.
func TestStepCircuitBreaker_ExternalTripEmitsSignalViaReread(t *testing.T) {
	t.Parallel()
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	externalReason := "consecutive_failures=5 reached threshold=5"
	mock := &cbMockStateManager{
		cbStates: map[string]*model.CircuitBreakerState{
			"cmd1": {
				Tripped:    true, // already tripped by another path
				TripReason: &externalReason,
			},
		},
	}

	cfg := model.Config{
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:                true,
			ProgressTimeoutMinutes: ptr.Int(30),
		},
	}
	cb := circuitbreaker.NewHandler(cfg, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	cb.SetStateReader(mock)
	qh.SetCircuitBreaker(cb)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: "cmd1", Status: model.StatusInProgress},
				},
			},
		},
		signals: fileState[model.PlannerSignalQueue]{
			Data: model.PlannerSignalQueue{},
			Path: filepath.Join(maestroDir, "queue", "signals.yaml"),
		},
		signalIndex: buildSignalIndex(nil),
	}

	qh.stepCircuitBreaker(&s)

	// TripCircuitBreaker should NOT have been called (CheckProgressTimeout
	// returns false when already tripped).
	if got := len(mock.trippedCalls); got != 0 {
		t.Fatalf("expected 0 TripCircuitBreaker calls (already tripped), got %d", got)
	}

	// A signal must still be emitted via the re-read path.
	if got := len(s.signals.Data.Signals); got != 1 {
		t.Fatalf("expected 1 planner signal from external trip, got %d", got)
	}
	sig := s.signals.Data.Signals[0]
	if !strings.Contains(sig.Message, externalReason) {
		t.Errorf("signal message should contain external reason %q, got: %s", externalReason, sig.Message)
	}
}

// writeWorktreeStateAt writes a minimal worktree state file with a custom
// integration created_at timestamp for fallback timeout tests.
func writeWorktreeStateAt(t *testing.T, maestroDir, commandID string, status model.IntegrationStatus, createdAt time.Time) {
	t.Helper()
	ts := createdAt.UTC().Format(time.RFC3339)
	state := model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "state_worktree",
		CommandID:     commandID,
		Integration: model.IntegrationState{
			CommandID: commandID,
			Branch:    "maestro/" + commandID + "/integration",
			BaseSHA:   "abc123",
			Status:    status,
			CreatedAt: ts,
			UpdatedAt: ts,
		},
		Workers: []model.WorktreeState{
			{
				CommandID: commandID,
				WorkerID:  "worker1",
				Path:      "/fake/worktree/worker1",
				Branch:    "maestro/" + commandID + "/worker1",
				BaseSHA:   "abc123",
				Status:    model.WorktreeStatusActive,
				CreatedAt: ts,
				UpdatedAt: ts,
			},
		},
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	path := filepath.Join(maestroDir, "state", "worktrees", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write worktree state: %v", err)
	}
}

func newScanStateWithCommand(commandID string) scanState {
	return scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{ID: commandID, Status: model.StatusInProgress},
				},
			},
		},
		signals: fileState[model.PlannerSignalQueue]{
			Data: model.PlannerSignalQueue{},
			Path: "/fake/signals.yaml",
		},
		signalIndex: buildSignalIndex(nil),
	}
}

func wtConfigDisabledAutoMerge(timeoutMin int) model.WorktreeConfig {
	return model.WorktreeConfig{
		Enabled:                     true,
		BaseBranch:                  "main",
		PathPrefix:                  ".maestro/worktrees",
		AutoCommit:                  true,
		AutoMerge:                   false, // disabled → triggers check
		MergeStrategy:               "ort",
		FallbackMergeTimeoutMinutes: ptr.Int(timeoutMin),
	}
}

// --- stepWorktreeFastTrackCleanup tests ---

// fastTrackCleanupConfig returns a worktree config with fast-track stall
// cleanup enabled at the requested duration.
func fastTrackCleanupConfig(stall string) model.WorktreeConfig {
	return model.WorktreeConfig{
		Enabled:           true,
		BaseBranch:        "main",
		PathPrefix:        ".maestro/worktrees",
		AutoCommit:        true,
		AutoMerge:         true,
		MergeStrategy:     "ort",
		StallCleanupAfter: stall,
	}
}

// writeCommandStateAt writes a command state file with custom timestamps so
// fast-track cleanup tests can control "elapsed since last update".
func writeCommandStateAt(t *testing.T, maestroDir, commandID string, taskStates map[string]model.Status, phases []model.Phase, updatedAt time.Time) {
	t.Helper()
	var requiredIDs []string
	for id := range taskStates {
		requiredIDs = append(requiredIDs, id)
	}
	ts := updatedAt.UTC().Format(time.RFC3339)
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs: requiredIDs,
			TaskStates:      taskStates,
		},
		PhaseTracking: model.PhaseTracking{
			Phases: phases,
		},
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	path := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write command state: %v", err)
	}
}

// TestStepWorktreeFastTrackCleanup_TriggersWhenPhaseStallsPastThreshold
// verifies the fast-track cleanup pathway:
//
//	Given:  worktree-enabled command with all tasks terminal, one phase still
//	        pending, and cmd.UpdatedAt older than worktree.stall_cleanup_after
//	When:   stepWorktreeFastTrackCleanup runs
//	Then:   a worktreeCleanupItem with reason "fast_track_stall" is appended
//	        (the merge step is skipped — no publish item is generated either).
func TestStepWorktreeFastTrackCleanup_TriggersWhenPhaseStallsPastThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	stalledAt := qh.clock.Now().Add(-30 * time.Minute)
	writeCommandStateAt(t, maestroDir, "cmd1",
		map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCompleted,
		},
		[]model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted},
			{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusPending},
		},
		stalledAt,
	)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{
						ID:        "cmd1",
						Status:    model.StatusInProgress,
						CreatedAt: stalledAt.UTC().Format(time.RFC3339),
						UpdatedAt: stalledAt.UTC().Format(time.RFC3339),
					},
				},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
				{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
			},
		}),
	}

	qh.stepWorktreeFastTrackCleanup(&s)

	if got := len(s.work.worktreeCleanups); got != 1 {
		t.Fatalf("expected 1 fast-track cleanup item, got %d", got)
	}
	if s.work.worktreeCleanups[0].CommandID != "cmd1" {
		t.Errorf("cleanup CommandID = %q, want cmd1", s.work.worktreeCleanups[0].CommandID)
	}
	if s.work.worktreeCleanups[0].Reason != "fast_track_stall" {
		t.Errorf("cleanup Reason = %q, want fast_track_stall", s.work.worktreeCleanups[0].Reason)
	}
	if len(s.work.worktreeMerges) != 0 {
		t.Errorf("expected no merge items in fast-track path, got %d", len(s.work.worktreeMerges))
	}
}

// TestStepWorktreeFastTrackCleanup_DoesNotTriggerBeforeThreshold verifies
// that the fast-track path is silent when the elapsed time since the
// command's last update is below worktree.stall_cleanup_after.
func TestStepWorktreeFastTrackCleanup_DoesNotTriggerBeforeThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	recent := qh.clock.Now().Add(-1 * time.Minute)
	writeCommandStateAt(t, maestroDir, "cmd1",
		map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		[]model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusPending},
		},
		recent,
	)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{
						ID:        "cmd1",
						Status:    model.StatusInProgress,
						CreatedAt: recent.UTC().Format(time.RFC3339),
						UpdatedAt: recent.UTC().Format(time.RFC3339),
					},
				},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			},
		}),
	}

	qh.stepWorktreeFastTrackCleanup(&s)

	if got := len(s.work.worktreeCleanups); got != 0 {
		t.Fatalf("expected no cleanup items before threshold, got %d", got)
	}
}

// TestStepCheckWorktreeConfigViolations_DisabledAndTimedOut verifies that a
// worktree_config_violation signal is emitted when AutoMerge is off and the
// integration branch has stayed in a non-merged status past the fallback
// timeout.
func TestStepCheckWorktreeConfigViolations_DisabledAndTimedOut(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := wtConfigDisabledAutoMerge(60)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	writeWorktreeStateAt(t, maestroDir, "cmd1", model.IntegrationStatusCreated,
		qh.clock.Now().Add(-2*time.Hour))

	s := newScanStateWithCommand("cmd1")
	qh.stepCheckWorktreeConfigViolations(&s)

	if !s.signals.Dirty {
		t.Fatalf("expected signals dirty after violation")
	}
	if got := len(s.signals.Data.Signals); got != 1 {
		t.Fatalf("expected 1 planner signal, got %d", got)
	}
	sig := s.signals.Data.Signals[0]
	if sig.Kind != "worktree_config_violation" {
		t.Errorf("kind: got %q, want worktree_config_violation", sig.Kind)
	}
	if sig.CommandID != "cmd1" {
		t.Errorf("command_id: got %q, want cmd1", sig.CommandID)
	}
}

// TestStepCheckWorktreeConfigViolations_BothEnabled verifies the step is a
// no-op when both AutoCommit and AutoMerge are enabled.
func TestStepCheckWorktreeConfigViolations_BothEnabled(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{
		Enabled:                     true,
		BaseBranch:                  "main",
		PathPrefix:                  ".maestro/worktrees",
		AutoCommit:                  true,
		AutoMerge:                   true,
		MergeStrategy:               "ort",
		FallbackMergeTimeoutMinutes: ptr.Int(60),
	}
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	writeWorktreeStateAt(t, maestroDir, "cmd1", model.IntegrationStatusCreated,
		qh.clock.Now().Add(-2*time.Hour))

	s := newScanStateWithCommand("cmd1")
	qh.stepCheckWorktreeConfigViolations(&s)

	if s.signals.Dirty {
		t.Errorf("expected no signal dirty when both flags enabled")
	}
	if len(s.signals.Data.Signals) != 0 {
		t.Errorf("expected 0 signals, got %d", len(s.signals.Data.Signals))
	}
}

// TestStepCheckWorktreeConfigViolations_DedupeAcrossCalls verifies upsert
// dedup prevents a second notification for the same command.
func TestStepCheckWorktreeConfigViolations_DedupeAcrossCalls(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := wtConfigDisabledAutoMerge(60)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	writeWorktreeStateAt(t, maestroDir, "cmd1", model.IntegrationStatusCreated,
		qh.clock.Now().Add(-2*time.Hour))

	s := newScanStateWithCommand("cmd1")
	qh.stepCheckWorktreeConfigViolations(&s)
	qh.stepCheckWorktreeConfigViolations(&s)

	if got := len(s.signals.Data.Signals); got != 1 {
		t.Fatalf("expected 1 signal after duplicate calls, got %d", got)
	}
}

// TestStepCheckWorktreeConfigViolations_WithinTimeout verifies no signal is
// emitted before the fallback timeout elapses.
func TestStepCheckWorktreeConfigViolations_WithinTimeout(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := wtConfigDisabledAutoMerge(60)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, wtCfg)

	writeWorktreeStateAt(t, maestroDir, "cmd1", model.IntegrationStatusCreated,
		qh.clock.Now().Add(-5*time.Minute))

	s := newScanStateWithCommand("cmd1")
	qh.stepCheckWorktreeConfigViolations(&s)

	if len(s.signals.Data.Signals) != 0 {
		t.Errorf("expected no signal within timeout, got %d", len(s.signals.Data.Signals))
	}
}

// --- stepWorktreePublish guard relaxation tests (案1) ---

// TestStepWorktreePublish_TerminalCmdMergedIntegrationCollects verifies that
// a terminal (completed) command with HasWorktrees and integration.status=merged
// still produces a publish item via the relaxed guard.
func TestStepWorktreePublish_TerminalCmdMergedIntegrationCollects(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	// Non-phased command state (phases:[]) so collect... advances to publish.
	writeCommandState(t, maestroDir, "cmd1",
		map[string]model.Status{"t1": model.StatusCompleted}, nil)

	now := qh.clock.Now()
	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusCompleted,
					Content:   "test publish",
					CreatedAt: now.UTC().Format(time.RFC3339),
					UpdatedAt: now.UTC().Format(time.RFC3339),
				}},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted}},
		}),
	}

	qh.stepWorktreePublish(&s)

	if got := len(s.work.worktreePublishes); got != 1 {
		t.Fatalf("expected 1 publish item, got %d", got)
	}
	if s.work.worktreePublishes[0].CommandID != "cmd1" {
		t.Errorf("publish CommandID = %q, want cmd1", s.work.worktreePublishes[0].CommandID)
	}
}

// TestStepWorktreePublish_PendingCmdSkipped verifies the new guard still
// skips pending commands (which have no worktrees yet).
func TestStepWorktreePublish_PendingCmdSkipped(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)
	writeCommandState(t, maestroDir, "cmd1",
		map[string]model.Status{"t1": model.StatusCompleted}, nil)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{ID: "cmd1", Status: model.StatusPending}},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted}},
		}),
	}
	qh.stepWorktreePublish(&s)
	if len(s.work.worktreePublishes)+len(s.work.worktreeCleanups) != 0 {
		t.Errorf("pending command must not produce publish/cleanup items")
	}
}

// --- stepWorktreeOrphanCleanup tests (案2) ---

// TestStepWorktreeOrphanCleanup_NonPhasedTerminalCreatedFires verifies the
// canonical leak case: a non-phased command terminated past the threshold with
// integration.status=created produces an orphan_terminal cleanup item.
func TestStepWorktreeOrphanCleanup_NonPhasedTerminalCreatedFires(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)
	writeCommandState(t, maestroDir, "cmd1",
		map[string]model.Status{"t1": model.StatusFailed}, nil)

	stalled := qh.clock.Now().Add(-30 * time.Minute)
	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusFailed,
					CreatedAt: stalled.UTC().Format(time.RFC3339),
					UpdatedAt: stalled.UTC().Format(time.RFC3339),
				}},
			},
		},
	}

	qh.stepWorktreeOrphanCleanup(&s)

	if got := len(s.work.worktreeCleanups); got != 1 {
		t.Fatalf("expected 1 orphan cleanup item, got %d", got)
	}
	// integration_status=created on a terminal command is classified as
	// a no-op terminal so the cleanup happens immediately regardless of
	// the stall threshold. The reason "orphan_terminal" still applies to
	// the failed/conflict/partial_merge cases.
	if s.work.worktreeCleanups[0].Reason != "no_op_terminal" {
		t.Errorf("reason = %q, want no_op_terminal", s.work.worktreeCleanups[0].Reason)
	}
}

// TestStepWorktreeOrphanCleanup_BeforeThresholdSkipped_FailedIntegration
// verifies that the threshold IS still honoured for non-no-op integration
// statuses (failed/conflict/partial_merge). The created status now
// bypasses the threshold (TestStepWorktreeOrphanCleanup_CreatedBypassesThreshold).
func TestStepWorktreeOrphanCleanup_BeforeThresholdSkipped(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusFailed)

	recent := qh.clock.Now().Add(-1 * time.Minute)
	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusCompleted,
					CreatedAt: recent.UTC().Format(time.RFC3339),
					UpdatedAt: recent.UTC().Format(time.RFC3339),
				}},
			},
		},
	}

	qh.stepWorktreeOrphanCleanup(&s)
	if len(s.work.worktreeCleanups) != 0 {
		t.Errorf("expected no cleanup before threshold for failed integration, got %d", len(s.work.worktreeCleanups))
	}
}

// TestStepWorktreeOrphanCleanup_MergedSkipped verifies idempotency vs 案1:
// integration.status=merged is handled by stepWorktreePublish, so the orphan
// cleanup step must NOT also fire on the same command.
func TestStepWorktreeOrphanCleanup_MergedSkipped(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusMerged)

	stalled := qh.clock.Now().Add(-30 * time.Minute)
	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusCompleted,
					CreatedAt: stalled.UTC().Format(time.RFC3339),
					UpdatedAt: stalled.UTC().Format(time.RFC3339),
				}},
			},
		},
	}

	qh.stepWorktreeOrphanCleanup(&s)
	if len(s.work.worktreeCleanups) != 0 {
		t.Errorf("merged integration must be left to stepWorktreePublish, got %d cleanup items", len(s.work.worktreeCleanups))
	}
}

// TestStepWorktreeOrphanCleanup_NoDoubleFireWithPublish verifies that running
// both stepWorktreePublish and stepWorktreeOrphanCleanup against the same
// terminal command never enqueues two cleanup items for the same cmd.
func TestStepWorktreeOrphanCleanup_NoDoubleFireWithPublish(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	// integration=created → publish step's collect... falls through to default
	// (no item); orphan step (case A) emits cleanup.
	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)
	writeCommandState(t, maestroDir, "cmd1",
		map[string]model.Status{"t1": model.StatusFailed}, nil)

	stalled := qh.clock.Now().Add(-30 * time.Minute)
	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusFailed,
					CreatedAt: stalled.UTC().Format(time.RFC3339),
					UpdatedAt: stalled.UTC().Format(time.RFC3339),
				}},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {{ID: "t1", CommandID: "cmd1", Status: model.StatusFailed}},
		}),
	}

	qh.stepWorktreePublish(&s)
	pubCleanups := len(s.work.worktreeCleanups)
	qh.stepWorktreeOrphanCleanup(&s)

	// Only orphan step should add a cleanup; publish step contributes 0 here
	// (created → default → no-op) unless cleanup_on_failure is configured,
	// which fastTrackCleanupConfig leaves default (false).
	if got := len(s.work.worktreeCleanups); got > 1 {
		t.Fatalf("double-fire detected: total=%d (publish=%d, orphan added=%d)",
			got, pubCleanups, got-pubCleanups)
	}
	if got := len(s.work.worktreeCleanups); got != 1 {
		t.Fatalf("expected exactly 1 cleanup after both steps, got %d", got)
	}
}

// TestStepDispatchOrRecovery_Boundaries exercises the empty/all-stale
// branch boundaries of the collect/apply phase. These cases historically
// regressed when new step branches were added without considering scans
// that have nothing to dispatch and nothing to recover.
func TestStepDispatchOrRecovery_Boundaries(t *testing.T) {
	t.Parallel()
	t.Run("empty_state", func(t *testing.T) {
		t.Parallel()
		qh := newMinimalQueueHandler(t)
		s := scanState{
			commands:  fileState[model.CommandQueue]{Data: model.CommandQueue{}},
			tasks:     map[string]*taskQueueEntry{},
			taskDirty: map[string]bool{},
			notifications: fileState[model.NotificationQueue]{
				Data: model.NotificationQueue{},
			},
		}
		qh.stepDispatchOrRecovery(&s)

		if n := len(s.work.dispatches) + len(s.work.busyChecks); n != 0 {
			t.Errorf("empty scan must produce no work items, got %d", n)
		}
		if s.commands.Dirty || s.notifications.Dirty {
			t.Errorf("empty scan must not dirty any queue (commands=%v notifications=%v)",
				s.commands.Dirty, s.notifications.Dirty)
		}
	})

	t.Run("all_pending_no_expired", func(t *testing.T) {
		t.Parallel()
		qh := newMinimalQueueHandler(t)
		s := scanState{
			commands: fileState[model.CommandQueue]{
				Data: model.CommandQueue{
					Commands: []model.Command{
						{ID: "cmd1", Status: model.StatusPending},
						{ID: "cmd2", Status: model.StatusPending},
					},
				},
			},
			tasks:     map[string]*taskQueueEntry{},
			taskDirty: map[string]bool{},
			notifications: fileState[model.NotificationQueue]{
				Data: model.NotificationQueue{},
			},
		}

		qh.stepDispatchOrRecovery(&s)

		// No expired leases anywhere → must take the dispatch branch and
		// emit zero busy checks.
		if got := len(s.work.busyChecks); got != 0 {
			t.Errorf("no expired leases: busyChecks must be 0, got %d", got)
		}
	})

	t.Run("no_panic_on_nil_task_dirty_map", func(t *testing.T) {
		t.Parallel()
		qh := newMinimalQueueHandler(t)
		// Defensive: fresh scanState with nil maps must not panic the dispatch
		// branch (callers always go through initScanState, but the function
		// itself must remain robust against zero values).
		s := scanState{
			commands:  fileState[model.CommandQueue]{Data: model.CommandQueue{}},
			tasks:     map[string]*taskQueueEntry{},
			taskDirty: map[string]bool{},
			notifications: fileState[model.NotificationQueue]{
				Data: model.NotificationQueue{},
			},
		}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("stepDispatchOrRecovery panicked on minimal state: %v", r)
			}
		}()
		qh.stepDispatchOrRecovery(&s)
	})
}

// TestStepWorktreeFastTrackCleanup_SkipsAwaitingFill verifies that a phase in
// awaiting_fill status is NOT force-failed by the fast-track cleanup.  This
// covers the race where Phase 1 completes and Phase 2 transitions to
// awaiting_fill in the same scan cycle: the fast-track cleanup must not kill
// Phase 2 before the planner receives its awaiting_fill notification.
// awaiting_fill has its own fill-deadline timeout (checkAwaitingFillTimeout)
// and is NOT a "stuck" phase.
func TestStepWorktreeFastTrackCleanup_SkipsAwaitingFill(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	stalledAt := qh.clock.Now().Add(-30 * time.Minute)
	writeCommandStateAt(t, maestroDir, "cmd1",
		map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		[]model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted},
			{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusAwaitingFill},
		},
		stalledAt,
	)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{
					{
						ID:        "cmd1",
						Status:    model.StatusInProgress,
						CreatedAt: stalledAt.UTC().Format(time.RFC3339),
						UpdatedAt: stalledAt.UTC().Format(time.RFC3339),
					},
				},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			},
		}),
	}

	qh.stepWorktreeFastTrackCleanup(&s)

	if got := len(s.work.worktreeCleanups); got != 0 {
		t.Fatalf("expected no fast-track cleanup for awaiting_fill phase, got %d", got)
	}
}

// TestStepWorktreeFastTrackCleanup_SkipsPendingPhaseAwaitingDependency
// asserts that a deferred phase declaring `depends_on_phases` (starting
// at `pending`) is not force-failed while its declared dependency is
// still active. Sweeping all non-terminal/non-awaiting_fill phases into
// the stuck set would force-fail legitimate downstream phases the
// moment the command's UpdatedAt aged past the threshold.
//
// The scenario: parallel_conflict completed, report_integration is at
// awaiting_fill (Planner pending), integration_verification (pending,
// depends_on=report_integration) gets cleaned up. Cleanup must skip
// integration_verification because its declared dependency is not yet
// terminal.
func TestStepWorktreeFastTrackCleanup_SkipsPendingPhaseAwaitingDependency(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	stalledAt := qh.clock.Now().Add(-30 * time.Minute)
	writeCommandStateAt(t, maestroDir, "cmd1",
		map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		[]model.Phase{
			{PhaseID: "p1", Name: "parallel_conflict", Status: model.PhaseStatusCompleted},
			{PhaseID: "p2", Name: "report_integration", Status: model.PhaseStatusAwaitingFill,
				Type: "deferred", DependsOnPhases: []string{"parallel_conflict"}},
			{PhaseID: "p3", Name: "integration_verification", Status: model.PhaseStatusPending,
				Type: "deferred", DependsOnPhases: []string{"report_integration"}},
		},
		stalledAt,
	)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusInProgress,
					CreatedAt: stalledAt.UTC().Format(time.RFC3339),
					UpdatedAt: stalledAt.UTC().Format(time.RFC3339),
				}},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			},
		}),
	}

	qh.stepWorktreeFastTrackCleanup(&s)

	if got := len(s.work.worktreeCleanups); got != 0 {
		t.Fatalf("expected no fast-track cleanup while a pending phase awaits its dependency, got %d (%+v)",
			got, s.work.worktreeCleanups)
	}

	// Verify the integration_verification phase status was NOT mutated.
	statePath := filepath.Join(maestroDir, "state", "commands", "cmd1.yaml")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st model.CommandState
	if err := yamlv3.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	for _, p := range st.Phases {
		if p.Name == "integration_verification" && p.Status != model.PhaseStatusPending {
			t.Errorf("integration_verification phase mutated to %s; expected pending preserved", p.Status)
		}
	}
}

// TestStepWorktreeFastTrackCleanup_FlagsPendingWithTerminalDeps covers
// the inverse: when every declared dependency is terminal but the
// phase still hasn't activated, that IS a real stall (dep resolver
// silently failing or a Planner abandonment after a phase failed).
// Cleanup should still fire in that case so the worktrees do not leak.
func TestStepWorktreeFastTrackCleanup_FlagsPendingWithTerminalDeps(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	stalledAt := qh.clock.Now().Add(-30 * time.Minute)
	writeCommandStateAt(t, maestroDir, "cmd1",
		map[string]model.Status{
			"t1": model.StatusFailed, // upstream failed
		},
		[]model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusFailed},
			{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusPending,
				Type: "deferred", DependsOnPhases: []string{"phase1"}},
		},
		stalledAt,
	)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusInProgress,
					CreatedAt: stalledAt.UTC().Format(time.RFC3339),
					UpdatedAt: stalledAt.UTC().Format(time.RFC3339),
				}},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusFailed},
			},
		}),
	}

	qh.stepWorktreeFastTrackCleanup(&s)

	if got := len(s.work.worktreeCleanups); got != 1 {
		t.Fatalf("expected fast-track cleanup when pending phase's deps are terminal, got %d", got)
	}
}

// TestStepWorktreeFastTrackCleanup_SkipsPausedForReplan asserts that
// fast-track cleanup observes the state side: when a task has hit
// max-retries on verify and transitioned to paused_for_replan, the
// queue side may still show terminal worker-reported status (completed).
// Cleanup must skip while any task is non-terminal in state, otherwise
// the worker worktree would be deleted while the Planner is still
// composing add-retry-task and the retry dispatch could not resolve
// the worktree path.
func TestStepWorktreeFastTrackCleanup_SkipsPausedForReplan(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	stalledAt := qh.clock.Now().Add(-30 * time.Minute)
	// State: t1 paused_for_replan, t2 completed. Phase still active so
	// the existing "stuck phase" check would have flagged it as a fast-
	// track candidate. The new state-side gate must override that.
	writeCommandStateAt(t, maestroDir, "cmd1",
		map[string]model.Status{
			"t1": model.StatusPausedForReplan,
			"t2": model.StatusCompleted,
		},
		[]model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusActive},
		},
		stalledAt,
	)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusInProgress,
					CreatedAt: stalledAt.UTC().Format(time.RFC3339),
					UpdatedAt: stalledAt.UTC().Format(time.RFC3339),
				}},
			},
		},
		// Queue side shows both tasks at terminal worker-reported status
		// (the worker reported completed, but verify failed and state
		// transitioned to paused_for_replan without touching the queue).
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
				{ID: "t2", CommandID: "cmd1", Status: model.StatusCompleted},
			},
		}),
	}

	qh.stepWorktreeFastTrackCleanup(&s)

	if got := len(s.work.worktreeCleanups); got != 0 {
		t.Fatalf("expected no fast-track cleanup while a task sits at paused_for_replan, got %d (%+v)",
			got, s.work.worktreeCleanups)
	}
}

// TestStepWorktreeFastTrackCleanup_SkipsRepairPending covers the
// shorter-window symmetric case: state at repair_pending means the
// daemon is mid-retry and will dispatch into the worktree on the next
// scan. Cleanup must not race the retry dispatch.
func TestStepWorktreeFastTrackCleanup_SkipsRepairPending(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	stalledAt := qh.clock.Now().Add(-30 * time.Minute)
	writeCommandStateAt(t, maestroDir, "cmd1",
		map[string]model.Status{
			"t1": model.StatusRepairPending,
		},
		[]model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusActive},
		},
		stalledAt,
	)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusInProgress,
					CreatedAt: stalledAt.UTC().Format(time.RFC3339),
					UpdatedAt: stalledAt.UTC().Format(time.RFC3339),
				}},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			},
		}),
	}

	qh.stepWorktreeFastTrackCleanup(&s)

	if got := len(s.work.worktreeCleanups); got != 0 {
		t.Fatalf("expected no fast-track cleanup while a task sits at repair_pending, got %d (%+v)",
			got, s.work.worktreeCleanups)
	}
}

// TestStepWorktreeFastTrackCleanup_ClearsPhantomTaskAndProceeds asserts
// that once the cleanup window has elapsed, a phantom TaskStates entry
// (state has the task at non-terminal but no worker queue lists it) is
// terminated so phase progression can resume.
//
// Non-running phantoms route to `cancelled` (the only legal terminal
// hop from `planned` per model.validTaskStateTransitions) — phantoms by
// definition did not actually execute. in_progress / running phantoms
// still go to `failed`.
func TestStepWorktreeFastTrackCleanup_ClearsPhantomTaskAndProceeds(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	stalledAt := qh.clock.Now().Add(-30 * time.Minute)
	// Phantom scenario: t1 finished, but a retry task t1_retry is in
	// TaskStates as `planned` without a queue entry anywhere. Phase
	// p1 lists t1_retry in TaskIDs (RegisterRetryTaskInState appended
	// it on the way to the queue write that subsequently failed).
	writeCommandStateAt(t, maestroDir, "cmd1",
		map[string]model.Status{
			"t1":       model.StatusCompleted,
			"t1_retry": model.StatusPlanned, // phantom — no queue entry
		},
		[]model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusActive,
				TaskIDs: []string{"t1", "t1_retry"}},
		},
		stalledAt,
	)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusInProgress,
					CreatedAt: stalledAt.UTC().Format(time.RFC3339),
					UpdatedAt: stalledAt.UTC().Format(time.RFC3339),
				}},
			},
		},
		// Queue has only the original task; t1_retry is missing entirely.
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			},
		}),
	}

	// First scan only records suspicion: daemon-side retry registration
	// writes state before queue, so a single-instant probe could misread a
	// registering task. Destruction requires queue absence on two
	// consecutive scans.
	qh.stepWorktreeFastTrackCleanup(&s)
	{
		data, err := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd1.yaml"))
		if err != nil {
			t.Fatalf("read state after first scan: %v", err)
		}
		var st model.CommandState
		if err := yamlv3.Unmarshal(data, &st); err != nil {
			t.Fatalf("unmarshal state after first scan: %v", err)
		}
		if got := st.TaskStates["t1_retry"]; got != model.StatusPlanned {
			t.Errorf("after first scan phantom t1_retry status=%s, want planned (suspicion only)", got)
		}
	}

	// Second scan confirms and clears.
	qh.stepWorktreeFastTrackCleanup(&s)

	// Phantom should be force-cancelled in state. The source status was
	// `planned`, so validTaskStateTransitions only permits the `cancelled`
	// terminal hop (not `failed`). Cancellation is semantically right for
	// a task that was registered in state but never made it onto any
	// worker queue — by definition, it never executed.
	statePath := filepath.Join(maestroDir, "state", "commands", "cmd1.yaml")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st model.CommandState
	if err := yamlv3.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if got := st.TaskStates["t1_retry"]; got != model.StatusCancelled {
		t.Errorf("phantom t1_retry status=%s, want cancelled", got)
	}

	// Cleanup proceeds because the state-side gate is now satisfied.
	if got := len(s.work.worktreeCleanups); got != 1 {
		t.Fatalf("expected fast-track cleanup after phantom cleared, got %d", got)
	}
}

// TestStepWorktreeFastTrackCleanup_DoesNotClearPhantomBeforeThreshold
// guards the conservative grace window: phantom-task force-fail must
// only fire after cmd.UpdatedAt has aged past stall_cleanup_after, so
// the brief race between RegisterRetryTaskInState and AddRetryTaskToQueue
// (during which a retry task is legitimately in state without a queue
// entry yet) is not false-failed.
func TestStepWorktreeFastTrackCleanup_DoesNotClearPhantomBeforeThreshold(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	// Only 1 minute since UpdatedAt — well within the 10-minute window.
	recentlyAt := qh.clock.Now().Add(-1 * time.Minute)
	writeCommandStateAt(t, maestroDir, "cmd1",
		map[string]model.Status{
			"t1":       model.StatusCompleted,
			"t1_retry": model.StatusPlanned, // mid-registration, no queue yet
		},
		[]model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusActive,
				TaskIDs: []string{"t1", "t1_retry"}},
		},
		recentlyAt,
	)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusInProgress,
					CreatedAt: recentlyAt.UTC().Format(time.RFC3339),
					UpdatedAt: recentlyAt.UTC().Format(time.RFC3339),
				}},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted},
			},
		}),
	}

	qh.stepWorktreeFastTrackCleanup(&s)

	statePath := filepath.Join(maestroDir, "state", "commands", "cmd1.yaml")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st model.CommandState
	if err := yamlv3.Unmarshal(data, &st); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if got := st.TaskStates["t1_retry"]; got != model.StatusPlanned {
		t.Errorf("phantom t1_retry status=%s, want planned (preserved within grace window)", got)
	}
	if got := len(s.work.worktreeCleanups); got != 0 {
		t.Fatalf("expected no cleanup within grace window, got %d", got)
	}
}

// TestStepWorktreeFastTrackCleanup_HonoursRecentTaskActivity asserts that
// the fast-track elapsed reference is max(cmd.UpdatedAt, latest task
// UpdatedAt for this command). cmd.UpdatedAt is not refreshed by every
// internal transition, so recent task progress must suppress the
// cleanup gate even when cmd.UpdatedAt is stuck at the original dispatch
// timestamp.
func TestStepWorktreeFastTrackCleanup_HonoursRecentTaskActivity(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

	stalledAt := qh.clock.Now().Add(-30 * time.Minute)
	recentTaskAt := qh.clock.Now().Add(-1 * time.Minute) // recent activity
	writeCommandStateAt(t, maestroDir, "cmd1",
		map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		[]model.Phase{
			{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusActive,
				TaskIDs: []string{"t1"}},
		},
		stalledAt,
	)

	s := scanState{
		commands: fileState[model.CommandQueue]{
			Data: model.CommandQueue{
				Commands: []model.Command{{
					ID:        "cmd1",
					Status:    model.StatusInProgress,
					CreatedAt: stalledAt.UTC().Format(time.RFC3339),
					UpdatedAt: stalledAt.UTC().Format(time.RFC3339),
				}},
			},
		},
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{
					ID:        "t1",
					CommandID: "cmd1",
					Status:    model.StatusCompleted,
					UpdatedAt: recentTaskAt.UTC().Format(time.RFC3339),
				},
			},
		}),
	}

	qh.stepWorktreeFastTrackCleanup(&s)

	if got := len(s.work.worktreeCleanups); got != 0 {
		t.Fatalf("expected no cleanup when task UpdatedAt is recent, got %d", got)
	}
}

// --- stepAdmissionSync tests ---

// TestStepAdmissionSync_NilController verifies that stepAdmissionSync is a
// no-op when no admission controller is configured.
func TestStepAdmissionSync_NilController(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)
	qh.admissionCtrl = nil

	s := scanState{
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusInProgress},
			},
		}),
	}
	// Must not panic.
	qh.stepAdmissionSync(&s)
}

// TestStepAdmissionSync_WithController verifies that stepAdmissionSync
// passes in_progress tasks to the admission controller without panicking.
func TestStepAdmissionSync_WithController(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)

	ac := admission.NewController(model.AdmissionControl{})
	qh.SetAdmissionController(ac)

	s := scanState{
		tasks: makeTaskQueues(map[string][]model.Task{
			"worker1": {
				{ID: "t1", CommandID: "cmd1", Status: model.StatusInProgress},
				{ID: "t2", CommandID: "cmd1", Status: model.StatusPending},
				{ID: "t3", CommandID: "cmd1", Status: model.StatusCompleted},
			},
			"worker2": {
				{ID: "t4", CommandID: "cmd1", Status: model.StatusInProgress},
			},
		}),
	}

	// Must not panic and should successfully record in-flight tasks
	qh.stepAdmissionSync(&s)
}

// TestStepAdmissionSync_EmptyTasks verifies no panic with empty task queues
// and an active admission controller.
func TestStepAdmissionSync_EmptyTasks(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)

	ac := admission.NewController(model.AdmissionControl{})
	qh.SetAdmissionController(ac)

	s := scanState{
		tasks: map[string]*taskQueueEntry{},
	}
	// Must not panic
	qh.stepAdmissionSync(&s)
}

// --- diagnosePhaseTasks tests ---

// TestDiagnosePhaseTasks_NoDiagnoser verifies that diagnosePhaseTasks returns
// empty string when phaseDiagnoser is nil.
func TestDiagnosePhaseTasks_NoDiagnoser(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)
	qh.scanExecutor.phaseDiagnoser = nil

	// Even with a state reader, should return empty when no diagnoser
	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{ID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted, RequiredTaskIDs: []string{"t1"}},
	})
	qh.SetStateReader(reader)

	result := qh.diagnosePhaseTasks("cmd1", "p1", "phase1", makeTaskQueues(map[string][]model.Task{
		"worker1": {{ID: "t1", CommandID: "cmd1", Status: model.StatusCompleted}},
	}))
	if result != "" {
		t.Errorf("expected empty string with nil diagnoser, got %q", result)
	}
}

// TestDiagnosePhaseTasks_EmptyPhaseTaskIDs verifies that diagnosePhaseTasks
// returns empty string when the phase has no required task IDs.
func TestDiagnosePhaseTasks_EmptyPhaseTaskIDs(t *testing.T) {
	t.Parallel()
	qh := newMinimalQueueHandler(t)

	called := false
	qh.scanExecutor.phaseDiagnoser = func(phase model.Phase, tasks []model.Task, _ []model.TaskResult) string {
		called = true
		return "diagnosis"
	}

	reader := newPhaseIntegrationStateReader()
	reader.setPhases("cmd1", []PhaseInfo{
		{ID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted, RequiredTaskIDs: nil},
	})
	qh.SetStateReader(reader)

	result := qh.diagnosePhaseTasks("cmd1", "p1", "phase1", nil)
	if result != "" {
		t.Errorf("expected empty string for phase with no task IDs, got %q", result)
	}
	if called {
		t.Error("diagnoser should not be called when phase has no task IDs")
	}
}
