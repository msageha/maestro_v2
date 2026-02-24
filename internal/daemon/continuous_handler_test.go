package daemon

import (
	"bytes"
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
		UpdatedAt:        &now,
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
		UpdatedAt:        &now,
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
		UpdatedAt:        &now,
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
		UpdatedAt:        &now,
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
		UpdatedAt:        &now,
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
		UpdatedAt:        &now,
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

	// Default state is stopped, so no iteration should happen
	// No state file should have been created since we returned early
}

func TestContinuous_PauseTakesPrecedenceOverStop(t *testing.T) {
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
		UpdatedAt:        &now,
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

func TestContinuous_MaxIterationsZero_Unlimited(t *testing.T) {
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
		UpdatedAt:        &now,
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
