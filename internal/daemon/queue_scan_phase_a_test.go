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
