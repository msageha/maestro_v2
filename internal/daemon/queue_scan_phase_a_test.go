package daemon

import (
	"bytes"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/circuitbreaker"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// TestStepCircuitBreaker_NilStateReader verifies that stepCircuitBreaker
// does not panic when the circuit breaker is enabled but no StateReader
// has been wired (e.g. during early startup or in degraded configurations).
func TestStepCircuitBreaker_NilStateReader(t *testing.T) {
	maestroDir := setupPhaseIntegrationDir(t)
	exec := newRecordingExecutor(nil)
	qh := newPhaseIntegrationQH(t, maestroDir, exec)

	// Wire a circuit breaker that is Enabled but has no StateReader.
	cfg := model.Config{
		CircuitBreaker: model.CircuitBreakerConfig{
			Enabled:                true,
			ProgressTimeoutMinutes: model.IntPtr(30),
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
		FallbackMergeTimeoutMinutes: model.IntPtr(timeoutMin),
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
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       commandID,
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: requiredIDs,
		TaskStates:      taskStates,
		Phases:          phases,
		CreatedAt:       ts,
		UpdatedAt:       ts,
	}
	path := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(path, state); err != nil {
		t.Fatalf("write command state: %v", err)
	}
}

// TestStepWorktreeFastTrackCleanup_TriggersWhenPhaseStallsPastThreshold
// verifies the fast-track cleanup pathway:
//   Given:  worktree-enabled command with all tasks terminal, one phase still
//           pending, and cmd.UpdatedAt older than worktree.stall_cleanup_after
//   When:   stepWorktreeFastTrackCleanup runs
//   Then:   a worktreeCleanupItem with reason "fast_track_stall" is appended
//           (the merge step is skipped — no publish item is generated either).
func TestStepWorktreeFastTrackCleanup_TriggersWhenPhaseStallsPastThreshold(t *testing.T) {
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
	maestroDir := setupScanPhaseTestDir(t)
	wtCfg := model.WorktreeConfig{
		Enabled:                     true,
		BaseBranch:                  "main",
		PathPrefix:                  ".maestro/worktrees",
		AutoCommit:                  true,
		AutoMerge:                   true,
		MergeStrategy:               "ort",
		FallbackMergeTimeoutMinutes: model.IntPtr(60),
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
