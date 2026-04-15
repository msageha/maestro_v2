package daemon

import (
	"bytes"
	"log"
	"path/filepath"
	"testing"
	"time"

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
//   Given:  worktree-enabled command with all tasks terminal, one phase still
//           pending, and cmd.UpdatedAt older than worktree.stall_cleanup_after
//   When:   stepWorktreeFastTrackCleanup runs
//   Then:   a worktreeCleanupItem with reason "fast_track_stall" is appended
//           (the merge step is skipped — no publish item is generated either).
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
	if s.work.worktreeCleanups[0].Reason != "orphan_terminal" {
		t.Errorf("reason = %q, want orphan_terminal", s.work.worktreeCleanups[0].Reason)
	}
}

// TestStepWorktreeOrphanCleanup_BeforeThresholdSkipped verifies negative case
// (elapsed < stall_cleanup_after).
func TestStepWorktreeOrphanCleanup_BeforeThresholdSkipped(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, fastTrackCleanupConfig("10m"))

	writeWorktreeState(t, maestroDir, "cmd1", model.IntegrationStatusCreated)

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
		t.Errorf("expected no cleanup before threshold, got %d", len(s.work.worktreeCleanups))
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
			commands: fileState[model.CommandQueue]{Data: model.CommandQueue{}},
			tasks:    map[string]*taskQueueEntry{},
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
			commands: fileState[model.CommandQueue]{Data: model.CommandQueue{}},
			tasks:    map[string]*taskQueueEntry{},
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
