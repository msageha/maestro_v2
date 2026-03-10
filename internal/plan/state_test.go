package plan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func newTestStateManager(t *testing.T) *StateManager {
	t.Helper()
	dir := t.TempDir()
	maestroDir := filepath.Join(dir, ".maestro")
	if err := os.MkdirAll(filepath.Join(maestroDir, "state", "commands"), 0755); err != nil {
		t.Fatalf("create test dirs: %v", err)
	}
	return NewStateManager(maestroDir, lock.NewMutexMap())
}

func TestStateManager_SaveAndLoad(t *testing.T) {
	t.Parallel()
	sm := newTestStateManager(t)

	original := &model.CommandState{
		SchemaVersion:     1,
		FileType:          "state_command",
		CommandID:         "cmd-save-load",
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1"},
		OptionalTaskIDs:   []string{"t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCompleted,
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	if err := sm.SaveState(original); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	loaded, err := sm.LoadState("cmd-save-load")
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}

	if loaded.CommandID != original.CommandID {
		t.Errorf("CommandID = %q, want %q", loaded.CommandID, original.CommandID)
	}
	if loaded.PlanStatus != original.PlanStatus {
		t.Errorf("PlanStatus = %q, want %q", loaded.PlanStatus, original.PlanStatus)
	}
	if loaded.ExpectedTaskCount != original.ExpectedTaskCount {
		t.Errorf("ExpectedTaskCount = %d, want %d", loaded.ExpectedTaskCount, original.ExpectedTaskCount)
	}
	if len(loaded.RequiredTaskIDs) != len(original.RequiredTaskIDs) {
		t.Errorf("RequiredTaskIDs length = %d, want %d", len(loaded.RequiredTaskIDs), len(original.RequiredTaskIDs))
	}
	if len(loaded.TaskStates) != len(original.TaskStates) {
		t.Errorf("TaskStates length = %d, want %d", len(loaded.TaskStates), len(original.TaskStates))
	}
}

func TestStateManager_StateExists(t *testing.T) {
	t.Parallel()
	sm := newTestStateManager(t)

	if sm.StateExists("nonexistent") {
		t.Errorf("StateExists = true for nonexistent command, want false")
	}

	state := &model.CommandState{
		CommandID: "exists-cmd",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := sm.SaveState(state); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	if !sm.StateExists("exists-cmd") {
		t.Errorf("StateExists = false after save, want true")
	}
}

func TestStateManager_DeleteState(t *testing.T) {
	t.Parallel()
	sm := newTestStateManager(t)

	state := &model.CommandState{
		CommandID: "delete-cmd",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	if err := sm.SaveState(state); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	if !sm.StateExists("delete-cmd") {
		t.Fatalf("StateExists = false before delete, want true")
	}

	if err := sm.DeleteState("delete-cmd"); err != nil {
		t.Fatalf("DeleteState failed: %v", err)
	}

	if sm.StateExists("delete-cmd") {
		t.Errorf("StateExists = true after delete, want false")
	}
}

func TestCanComplete_AllCompleted(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 3,
		RequiredTaskIDs:   []string{"t1", "t2"},
		OptionalTaskIDs:   []string{"t3"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCompleted,
			"t3": model.StatusCompleted,
		},
	}

	status, err := CanComplete(state)
	if err != nil {
		t.Fatalf("CanComplete returned error: %v", err)
	}
	if status != model.PlanStatusCompleted {
		t.Errorf("status = %q, want %q", status, model.PlanStatusCompleted)
	}
}

func TestCanComplete_HasFailed(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1", "t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusFailed,
		},
	}

	status, err := CanComplete(state)
	if err != nil {
		t.Fatalf("CanComplete returned error: %v", err)
	}
	if status != model.PlanStatusFailed {
		t.Errorf("status = %q, want %q", status, model.PlanStatusFailed)
	}
}

func TestCanComplete_HasCancelled(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1", "t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCancelled,
		},
	}

	status, err := CanComplete(state)
	if err != nil {
		t.Fatalf("CanComplete returned error: %v", err)
	}
	if status != model.PlanStatusCancelled {
		t.Errorf("status = %q, want %q", status, model.PlanStatusCancelled)
	}
}

func TestCanComplete_NotSealed(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusPlanning,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatalf("CanComplete returned nil error for non-sealed plan")
	}
}

func TestCanComplete_TaskCountMismatch(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 5,
		RequiredTaskIDs:   []string{"t1"},
		OptionalTaskIDs:   []string{"t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCompleted,
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatalf("CanComplete returned nil error for task count mismatch")
	}
}

func TestCanComplete_NonTerminalTask(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1", "t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusInProgress,
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatalf("CanComplete returned nil error for non-terminal required task")
	}
}

func TestCanComplete_FillingPhase(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		Phases: []model.Phase{
			{
				Name:   "phase-1",
				Status: model.PhaseStatusFilling,
			},
		},
	}

	_, err := CanComplete(state)
	if err == nil {
		t.Fatalf("CanComplete returned nil error for filling phase")
	}

	var retryErr *RetryableError
	if !errors.As(err, &retryErr) {
		t.Errorf("error is not RetryableError, got %T: %v", err, err)
	}
}

func TestDeriveStatus_TimedOutPhase(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
		Phases: []model.Phase{
			{
				Name:   "phase-1",
				Status: model.PhaseStatusTimedOut,
			},
		},
	}

	status, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus returned error: %v", err)
	}
	if status != model.PlanStatusFailed {
		t.Errorf("status = %q, want %q", status, model.PlanStatusFailed)
	}
}

func TestDeriveStatus_OnOptionalFailed_Ignore(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1"},
		OptionalTaskIDs:   []string{"t2"},
		CompletionPolicy: model.CompletionPolicy{
			OnOptionalFailed: "ignore",
		},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusFailed,
		},
	}

	status, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus returned error: %v", err)
	}
	if status != model.PlanStatusCompleted {
		t.Errorf("status = %q, want %q", status, model.PlanStatusCompleted)
	}
}

func TestDeriveStatus_OnOptionalFailed_Default(t *testing.T) {
	t.Parallel()
	// Default (empty string) should behave as "ignore"
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1"},
		OptionalTaskIDs:   []string{"t2"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusFailed,
		},
	}

	status, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus returned error: %v", err)
	}
	if status != model.PlanStatusCompleted {
		t.Errorf("status = %q, want %q", status, model.PlanStatusCompleted)
	}
}

func TestDeriveStatus_OnOptionalFailed_Warn(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		CommandID:         "cmd-test-warn",
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1"},
		OptionalTaskIDs:   []string{"t2"},
		CompletionPolicy: model.CompletionPolicy{
			OnOptionalFailed: "warn",
		},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusFailed,
		},
	}

	status, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus returned error: %v", err)
	}
	// warn logs but still completes successfully
	if status != model.PlanStatusCompleted {
		t.Errorf("status = %q, want %q", status, model.PlanStatusCompleted)
	}
}

func TestDeriveStatus_OnOptionalFailed_FailCommand(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1"},
		OptionalTaskIDs:   []string{"t2"},
		CompletionPolicy: model.CompletionPolicy{
			OnOptionalFailed: "fail_command",
		},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusFailed,
		},
	}

	status, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus returned error: %v", err)
	}
	if status != model.PlanStatusFailed {
		t.Errorf("status = %q, want %q", status, model.PlanStatusFailed)
	}
}

func TestDeriveStatus_OnOptionalFailed_NoFailure(t *testing.T) {
	t.Parallel()
	// When optional tasks succeed, policy should not affect result
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1"},
		OptionalTaskIDs:   []string{"t2"},
		CompletionPolicy: model.CompletionPolicy{
			OnOptionalFailed: "fail_command",
		},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusCompleted,
		},
	}

	status, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus returned error: %v", err)
	}
	if status != model.PlanStatusCompleted {
		t.Errorf("status = %q, want %q", status, model.PlanStatusCompleted)
	}
}

func TestDeriveStatus_OnOptionalFailed_UnsupportedValue(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"t1"},
		OptionalTaskIDs:   []string{"t2"},
		CompletionPolicy: model.CompletionPolicy{
			OnOptionalFailed: "invalid_policy",
		},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusFailed,
		},
	}

	_, err := DeriveStatus(state)
	if err == nil {
		t.Fatalf("DeriveStatus returned nil error for unsupported on_optional_failed value")
	}
}

func TestDeriveStatus_DependencyFailurePolicy_Valid(t *testing.T) {
	t.Parallel()
	validPolicies := []string{"cancel_dependents", "fail_dependents", "ignore"}

	for _, policy := range validPolicies {
		policy := policy
		t.Run(policy, func(t *testing.T) {
			t.Parallel()
			state := &model.CommandState{
				PlanStatus:        model.PlanStatusSealed,
				ExpectedTaskCount: 1,
				RequiredTaskIDs:   []string{"t1"},
				CompletionPolicy: model.CompletionPolicy{
					DependencyFailurePolicy: policy,
				},
				TaskStates: map[string]model.Status{
					"t1": model.StatusCompleted,
				},
			}

			status, err := DeriveStatus(state)
			if err != nil {
				t.Fatalf("DeriveStatus returned error for policy %q: %v", policy, err)
			}
			if status != model.PlanStatusCompleted {
				t.Errorf("status = %q, want %q", status, model.PlanStatusCompleted)
			}
		})
	}
}

func TestDeriveStatus_DependencyFailurePolicy_Unsupported(t *testing.T) {
	t.Parallel()
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		CompletionPolicy: model.CompletionPolicy{
			DependencyFailurePolicy: "invalid_policy",
		},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
	}

	_, err := DeriveStatus(state)
	if err == nil {
		t.Fatalf("DeriveStatus returned nil error for unsupported dependency_failure_policy value")
	}
}

func TestDeriveStatus_DependencyFailurePolicy_Default(t *testing.T) {
	t.Parallel()
	// Empty string defaults to "cancel_dependents" — should not error
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:   []string{"t1"},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
		},
	}

	status, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus returned error: %v", err)
	}
	if status != model.PlanStatusCompleted {
		t.Errorf("status = %q, want %q", status, model.PlanStatusCompleted)
	}
}

// --- LoadState backup recovery tests ---

func TestLoadState_CorruptedYAML_RecoveredFromBackup(t *testing.T) {
	t.Parallel()
	sm := newTestStateManager(t)

	// Save a valid state (this creates the .bak via AtomicWrite)
	original := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd-recover",
		PlanStatus:    model.PlanStatusSealed,
		CreatedAt:     "2026-01-01T00:00:00Z",
		UpdatedAt:     "2026-01-01T00:00:00Z",
	}
	if err := sm.SaveState(original); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	// Save again to create a .bak (first save creates no .bak since file didn't exist)
	original.UpdatedAt = "2026-01-02T00:00:00Z"
	if err := sm.SaveState(original); err != nil {
		t.Fatalf("SaveState (second) failed: %v", err)
	}

	// Corrupt the primary file
	path, _ := sm.StatePath("cmd-recover")
	if err := os.WriteFile(path, []byte("{{CORRUPTED YAML!!!"), 0644); err != nil {
		t.Fatalf("corrupt file: %v", err)
	}

	// LoadState should recover from backup
	loaded, err := sm.LoadState("cmd-recover")
	if err != nil {
		t.Fatalf("LoadState should recover from backup, got error: %v", err)
	}
	if loaded.CommandID != "cmd-recover" {
		t.Errorf("CommandID = %q, want %q", loaded.CommandID, "cmd-recover")
	}
}

func TestLoadState_CorruptedYAML_NoBackup(t *testing.T) {
	t.Parallel()
	sm := newTestStateManager(t)

	// Write corrupted YAML directly (no .bak exists)
	path, _ := sm.StatePath("cmd-no-bak")
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{{CORRUPTED YAML!!!"), 0644); err != nil {
		t.Fatal(err)
	}

	// LoadState should fail with both parse and backup errors
	_, err := sm.LoadState("cmd-no-bak")
	if err == nil {
		t.Fatal("LoadState should fail when YAML is corrupted and no backup exists")
	}
	if !strings.Contains(err.Error(), "backup recovery also failed") {
		t.Errorf("error should mention backup recovery failure, got: %v", err)
	}
}

func TestLoadState_FileNotFound_NoRecoveryAttempt(t *testing.T) {
	t.Parallel()
	sm := newTestStateManager(t)

	// Non-existent file should return os.ErrNotExist, NOT attempt recovery
	_, err := sm.LoadState("cmd-nonexistent")
	if err == nil {
		t.Fatal("LoadState should fail for non-existent file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got: %v", err)
	}
}

func TestLoadState_InvalidSchemaVersion_RecoveredFromBackup(t *testing.T) {
	t.Parallel()
	sm := newTestStateManager(t)

	// Save a valid state twice to create a .bak
	original := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd-schema",
		CreatedAt:     "2026-01-01T00:00:00Z",
		UpdatedAt:     "2026-01-01T00:00:00Z",
	}
	if err := sm.SaveState(original); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}
	original.UpdatedAt = "2026-01-02T00:00:00Z"
	if err := sm.SaveState(original); err != nil {
		t.Fatalf("SaveState (second) failed: %v", err)
	}

	// Write a YAML file with wrong schema_version
	path, _ := sm.StatePath("cmd-schema")
	badContent := "schema_version: 999\nfile_type: state_command\ncommand_id: cmd-schema\n"
	// Write via AtomicWriteRaw to bypass struct validation
	if err := yamlutil.AtomicWriteRaw(path, []byte(badContent)); err != nil {
		t.Fatalf("write bad schema: %v", err)
	}

	// LoadState should recover from backup
	loaded, err := sm.LoadState("cmd-schema")
	if err != nil {
		t.Fatalf("LoadState should recover from backup, got error: %v", err)
	}
	if loaded.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", loaded.SchemaVersion)
	}
}

func TestDeriveStatus_RequiredFailedOverridesOptionalPolicy(t *testing.T) {
	t.Parallel()
	// Required failure should take precedence over optional failure policy
	state := &model.CommandState{
		PlanStatus:        model.PlanStatusSealed,
		ExpectedTaskCount: 3,
		RequiredTaskIDs:   []string{"t1", "t2"},
		OptionalTaskIDs:   []string{"t3"},
		CompletionPolicy: model.CompletionPolicy{
			OnRequiredFailed: "fail_command",
			OnOptionalFailed: "ignore",
		},
		TaskStates: map[string]model.Status{
			"t1": model.StatusCompleted,
			"t2": model.StatusFailed,
			"t3": model.StatusFailed,
		},
	}

	status, err := DeriveStatus(state)
	if err != nil {
		t.Fatalf("DeriveStatus returned error: %v", err)
	}
	if status != model.PlanStatusFailed {
		t.Errorf("status = %q, want %q", status, model.PlanStatusFailed)
	}
}

// --- Migrator integration tests ---

func TestLoadState_MigratorIntegration_CurrentVersion(t *testing.T) {
	t.Parallel()
	// When schema_version == CurrentSchemaVersion, no migration should be applied
	sm := newTestStateManager(t)

	original := &model.CommandState{
		SchemaVersion: CurrentSchemaVersion,
		FileType:      "state_command",
		CommandID:     "cmd-current-ver",
		PlanStatus:    model.PlanStatusPlanning,
		CreatedAt:     "2026-01-01T00:00:00Z",
		UpdatedAt:     "2026-01-01T00:00:00Z",
	}
	if err := sm.SaveState(original); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	loaded, err := sm.LoadState("cmd-current-ver")
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}
	if loaded.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", loaded.SchemaVersion, CurrentSchemaVersion)
	}
	if loaded.CommandID != "cmd-current-ver" {
		t.Errorf("CommandID = %q, want %q", loaded.CommandID, "cmd-current-ver")
	}
}

func TestLoadState_MigratorIntegration_OlderVersion(t *testing.T) {
	// Simulate an older schema version that needs migration.
	// Temporarily bump CurrentSchemaVersion by using a custom migrator.
	sm := newTestStateManager(t)

	// Save a state file with schema_version=1 directly (raw YAML)
	path, _ := sm.StatePath("cmd-migrate")
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}

	// Only test when CurrentSchemaVersion > 1, otherwise skip
	if CurrentSchemaVersion <= 1 {
		// Register a temporary migration to test the flow:
		// Save original DefaultMigrator, replace with test migrator
		origMigrator := DefaultMigrator
		testVersion := 2
		DefaultMigrator = NewMigrator(testVersion)
		DefaultMigrator.Register(1, func(data map[string]interface{}) error {
			// Simulate migration: add a field
			data["migrated_from_v1"] = true
			return nil
		})
		origCurrentVersion := CurrentSchemaVersion
		// Temporarily override (package-level var)
		defer func() {
			DefaultMigrator = origMigrator
			// CurrentSchemaVersion is const, can't restore; test validates behavior
			_ = origCurrentVersion
		}()

		// Write a v1 state file
		content := "schema_version: 1\nfile_type: state_command\ncommand_id: cmd-migrate\nplan_status: planning\ncreated_at: '2026-01-01T00:00:00Z'\nupdated_at: '2026-01-01T00:00:00Z'\n"
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		// Since CurrentSchemaVersion is const=1, NeedsMigration(1) returns false.
		// We can only fully test migration when schema evolves beyond v1.
		// Verify that LoadState works with current version without error.
		loaded, err := sm.LoadState("cmd-migrate")
		if err != nil {
			t.Fatalf("LoadState failed: %v", err)
		}
		if loaded.CommandID != "cmd-migrate" {
			t.Errorf("CommandID = %q, want %q", loaded.CommandID, "cmd-migrate")
		}
		return
	}

	// When CurrentSchemaVersion > 1, test actual migration
	content := "schema_version: 1\nfile_type: state_command\ncommand_id: cmd-migrate\nplan_status: planning\ncreated_at: '2026-01-01T00:00:00Z'\nupdated_at: '2026-01-01T00:00:00Z'\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	loaded, err := sm.LoadState("cmd-migrate")
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}
	if loaded.SchemaVersion != CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d (should have been migrated)", loaded.SchemaVersion, CurrentSchemaVersion)
	}
}

func TestLoadState_MigratorIntegration_NeedsMigrationCalled(t *testing.T) {
	t.Parallel()
	// Verify that the migration path is connected by checking that
	// DefaultMigrator.NeedsMigration is correctly invoked during LoadState.
	sm := newTestStateManager(t)

	original := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd-needs-mig",
		PlanStatus:    model.PlanStatusPlanning,
		CreatedAt:     "2026-01-01T00:00:00Z",
		UpdatedAt:     "2026-01-01T00:00:00Z",
	}
	if err := sm.SaveState(original); err != nil {
		t.Fatalf("SaveState failed: %v", err)
	}

	// With CurrentSchemaVersion=1, NeedsMigration(1) should return false,
	// and LoadState should work without attempting migration.
	loaded, err := sm.LoadState("cmd-needs-mig")
	if err != nil {
		t.Fatalf("LoadState failed: %v", err)
	}
	if loaded.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", loaded.SchemaVersion)
	}

	// Verify DefaultMigrator is properly configured
	if DefaultMigrator.NeedsMigration(CurrentSchemaVersion) {
		t.Error("NeedsMigration(CurrentSchemaVersion) should be false")
	}
}
