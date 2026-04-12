package daemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func newTestContinuousHandler(maestroDir string, cfg model.Config) *ContinuousHandler {
	ch := NewContinuousHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	return ch
}

func writeContinuousState(t *testing.T, maestroDir string, state *model.Continuous) {
	t.Helper()
	stateDir := filepath.Join(maestroDir, "state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Ensure execute bit is set regardless of process-wide umask.
	os.Chmod(stateDir, 0755)
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "continuous.yaml"), state); err != nil {
		t.Fatal(err)
	}
}

func readContinuousState(t *testing.T, maestroDir string) *model.Continuous {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "continuous.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var state model.Continuous
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return &state
}

func TestContinuous_Disabled(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{Enabled: false},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	// Should be no-op when disabled
	err := ch.CheckAndAdvance("cmd_001", model.StatusCompleted)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No state file should be created
	if _, err := os.Stat(filepath.Join(maestroDir, "state", "continuous.yaml")); !os.IsNotExist(err) {
		t.Error("continuous.yaml should not be created when disabled")
	}
}

func TestContinuous_NotRunning(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{Enabled: true, MaxIterations: 10},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	// Write stopped state
	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 5,
		MaxIterations:    10,
		Status:           model.ContinuousStatusStopped,
		UpdatedAt:        now,
	})

	err := ch.CheckAndAdvance("cmd_001", model.StatusCompleted)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Iteration should NOT be incremented
	state := readContinuousState(t, maestroDir)
	if state.CurrentIteration != 5 {
		t.Errorf("iteration should remain 5, got %d", state.CurrentIteration)
	}
}

func TestContinuous_Increment(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{Enabled: true, MaxIterations: 10},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 3,
		MaxIterations:    10,
		Status:           model.ContinuousStatusRunning,
		UpdatedAt:        now,
	})

	err := ch.CheckAndAdvance("cmd_001", model.StatusCompleted)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	state := readContinuousState(t, maestroDir)
	if state.CurrentIteration != 4 {
		t.Errorf("iteration: got %d, want 4", state.CurrentIteration)
	}
	if state.LastCommandID == nil || *state.LastCommandID != "cmd_001" {
		t.Error("last_command_id should be cmd_001")
	}
	if state.Status != model.ContinuousStatusRunning {
		t.Errorf("status should remain running, got %s", state.Status)
	}
}

func TestContinuous_Idempotency(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{Enabled: true, MaxIterations: 10},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	lastCmd := "cmd_001"
	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 5,
		MaxIterations:    10,
		Status:           model.ContinuousStatusRunning,
		LastCommandID:    &lastCmd,
		UpdatedAt:        now,
	})

	// Same command ID → should be skipped
	err := ch.CheckAndAdvance("cmd_001", model.StatusCompleted)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	state := readContinuousState(t, maestroDir)
	if state.CurrentIteration != 5 {
		t.Errorf("iteration should remain 5 (idempotency), got %d", state.CurrentIteration)
	}

	// Different command ID → should increment
	err = ch.CheckAndAdvance("cmd_002", model.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	state = readContinuousState(t, maestroDir)
	if state.CurrentIteration != 6 {
		t.Errorf("iteration: got %d, want 6", state.CurrentIteration)
	}
}

func TestContinuous_MaxIterationsStop(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{Enabled: true, MaxIterations: 5},
	}

	ch := newTestContinuousHandler(maestroDir, cfg)

	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 4,
		MaxIterations:    5,
		Status:           model.ContinuousStatusRunning,
		UpdatedAt:        now,
	})

	err := ch.CheckAndAdvance("cmd_005", model.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}

	state := readContinuousState(t, maestroDir)
	if state.CurrentIteration != 5 {
		t.Errorf("iteration: got %d, want 5", state.CurrentIteration)
	}
	if state.Status != model.ContinuousStatusStopped {
		t.Errorf("status: got %s, want stopped", state.Status)
	}
	if state.PausedReason == nil || *state.PausedReason != "max_iterations_reached" {
		t.Error("paused_reason should be max_iterations_reached")
	}
}

func TestContinuous_PauseOnFailure(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{Enabled: true, MaxIterations: 10, PauseOnFailure: true},
	}

	ch := newTestContinuousHandler(maestroDir, cfg)

	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 2,
		MaxIterations:    10,
		Status:           model.ContinuousStatusRunning,
		UpdatedAt:        now,
	})

	err := ch.CheckAndAdvance("cmd_003", model.StatusFailed)
	if err != nil {
		t.Fatal(err)
	}

	state := readContinuousState(t, maestroDir)
	if state.CurrentIteration != 3 {
		t.Errorf("iteration: got %d, want 3", state.CurrentIteration)
	}
	if state.Status != model.ContinuousStatusPaused {
		t.Errorf("status: got %s, want paused", state.Status)
	}
	if state.PausedReason == nil || *state.PausedReason != "task_failure" {
		t.Error("paused_reason should be task_failure")
	}
}

func TestContinuous_PauseOnFailure_Disabled(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{Enabled: true, MaxIterations: 10, PauseOnFailure: false},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 2,
		MaxIterations:    10,
		Status:           model.ContinuousStatusRunning,
		UpdatedAt:        now,
	})

	// Failed command but pause_on_failure=false → should remain running
	err := ch.CheckAndAdvance("cmd_003", model.StatusFailed)
	if err != nil {
		t.Fatal(err)
	}

	state := readContinuousState(t, maestroDir)
	if state.Status != model.ContinuousStatusRunning {
		t.Errorf("status should remain running, got %s", state.Status)
	}
}

func TestContinuous_NoStateFile(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{Enabled: true, MaxIterations: 10},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	// No state file exists — should create default (stopped) and return early
	err := ch.CheckAndAdvance("cmd_001", model.StatusCompleted)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Default state is stopped, so no iteration should happen.
	// Verify no state file was created since we returned early.
	statePath := filepath.Join(maestroDir, "state", "continuous.yaml")
	if _, err := os.Stat(statePath); err == nil {
		t.Error("expected no state file to be created when default status is stopped")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking state file: %v", err)
	}
}

func TestContinuous_PauseTakesPrecedenceOverStop(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{Enabled: true, MaxIterations: 3, PauseOnFailure: true},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 2,
		MaxIterations:    3,
		Status:           model.ContinuousStatusRunning,
		UpdatedAt:        now,
	})

	// Both pause_on_failure and max_iterations would trigger at iteration 3 with failure
	err := ch.CheckAndAdvance("cmd_003", model.StatusFailed)
	if err != nil {
		t.Fatal(err)
	}

	state := readContinuousState(t, maestroDir)
	// Pause should take precedence (checked first)
	if state.Status != model.ContinuousStatusPaused {
		t.Errorf("status: got %s, want paused (pause takes precedence over stop)", state.Status)
	}
	if state.PausedReason == nil || *state.PausedReason != "task_failure" {
		t.Error("paused_reason should be task_failure")
	}
}

func TestContinuous_MaxConsecutiveFailures_Gate(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{
			Enabled:                true,
			MaxIterations:          100,
			PauseOnFailure:         false, // gate must fire even when pause_on_failure is off
			MaxConsecutiveFailures: 3,
		},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion: 1,
		FileType:      "state_continuous",
		Status:        model.ContinuousStatusRunning,
		UpdatedAt:     now,
	})

	// Two failures: still running, counter accumulates.
	if err := ch.CheckAndAdvance("cmd_a", model.StatusFailed); err != nil {
		t.Fatal(err)
	}
	if err := ch.CheckAndAdvance("cmd_b", model.StatusFailed); err != nil {
		t.Fatal(err)
	}
	state := readContinuousState(t, maestroDir)
	if state.Status != model.ContinuousStatusRunning {
		t.Fatalf("status after 2 failures: got %s, want running", state.Status)
	}
	if state.ConsecutiveFailures != 2 {
		t.Errorf("consecutive_failures: got %d, want 2", state.ConsecutiveFailures)
	}

	// Third failure trips the gate.
	if err := ch.CheckAndAdvance("cmd_c", model.StatusFailed); err != nil {
		t.Fatal(err)
	}
	state = readContinuousState(t, maestroDir)
	if state.Status != model.ContinuousStatusStopped {
		t.Errorf("status: got %s, want stopped", state.Status)
	}
	if state.PausedReason == nil || *state.PausedReason != "max_consecutive_failures_reached" {
		t.Errorf("paused_reason: got %v, want max_consecutive_failures_reached", state.PausedReason)
	}
	if state.ConsecutiveFailures != 3 {
		t.Errorf("consecutive_failures: got %d, want 3", state.ConsecutiveFailures)
	}
}

func TestContinuous_ConsecutiveFailures_ResetOnSuccess(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{
			Enabled:                true,
			MaxIterations:          100,
			MaxConsecutiveFailures: 3,
		},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:       1,
		FileType:            "state_continuous",
		Status:              model.ContinuousStatusRunning,
		ConsecutiveFailures: 2,
		UpdatedAt:           now,
	})

	if err := ch.CheckAndAdvance("cmd_ok", model.StatusCompleted); err != nil {
		t.Fatal(err)
	}
	state := readContinuousState(t, maestroDir)
	if state.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures should reset to 0 on success, got %d", state.ConsecutiveFailures)
	}
	if state.Status != model.ContinuousStatusRunning {
		t.Errorf("status should remain running, got %s", state.Status)
	}
}

func TestContinuous_MaxConsecutiveFailures_Disabled(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{
			Enabled:                true,
			MaxIterations:          100,
			MaxConsecutiveFailures: 0, // disabled
		},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion: 1,
		FileType:      "state_continuous",
		Status:        model.ContinuousStatusRunning,
		UpdatedAt:     now,
	})

	for i := 0; i < 10; i++ {
		if err := ch.CheckAndAdvance(fmt.Sprintf("cmd_%d", i), model.StatusFailed); err != nil {
			t.Fatal(err)
		}
	}
	state := readContinuousState(t, maestroDir)
	if state.Status != model.ContinuousStatusRunning {
		t.Errorf("status should remain running when gate is disabled, got %s", state.Status)
	}
	if state.ConsecutiveFailures != 10 {
		t.Errorf("consecutive_failures: got %d, want 10", state.ConsecutiveFailures)
	}
}

func TestContinuous_MaxIterationsZero_Unlimited(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Continuous: model.ContinuousConfig{Enabled: true, MaxIterations: 0},
	}
	ch := newTestContinuousHandler(maestroDir, cfg)

	now := time.Now().UTC().Format(time.RFC3339)
	writeContinuousState(t, maestroDir, &model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 999,
		MaxIterations:    0,
		Status:           model.ContinuousStatusRunning,
		UpdatedAt:        now,
	})

	err := ch.CheckAndAdvance("cmd_1000", model.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}

	state := readContinuousState(t, maestroDir)
	if state.CurrentIteration != 1000 {
		t.Errorf("iteration: got %d, want 1000", state.CurrentIteration)
	}
	if state.Status != model.ContinuousStatusRunning {
		t.Errorf("status should remain running (unlimited), got %s", state.Status)
	}
}
