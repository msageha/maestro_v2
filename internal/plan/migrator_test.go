package plan

import (
	"fmt"
	"testing"
)

func TestMigrator_NoMigrationNeeded(t *testing.T) {
	m := NewMigrator(1)
	data := map[string]interface{}{"schema_version": 1}

	if err := m.Migrate(data, 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMigrator_NeedsMigration(t *testing.T) {
	m := NewMigrator(3)

	if m.NeedsMigration(3) {
		t.Error("NeedsMigration(3) = true, want false for current version")
	}
	if !m.NeedsMigration(1) {
		t.Error("NeedsMigration(1) = false, want true for older version")
	}
	if !m.NeedsMigration(2) {
		t.Error("NeedsMigration(2) = false, want true for older version")
	}
	if m.NeedsMigration(0) {
		t.Error("NeedsMigration(0) = true, want false for invalid version")
	}
}

func TestMigrator_SingleStep(t *testing.T) {
	m := NewMigrator(2)
	if err := m.Register(1, func(data map[string]interface{}) error {
		data["new_field"] = "added_by_migration"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	data := map[string]interface{}{"schema_version": 1}
	if err := m.Migrate(data, 1); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if data["schema_version"] != 2 {
		t.Errorf("schema_version = %v, want 2", data["schema_version"])
	}
	if data["new_field"] != "added_by_migration" {
		t.Errorf("new_field = %v, want 'added_by_migration'", data["new_field"])
	}
}

func TestMigrator_MultiStep(t *testing.T) {
	m := NewMigrator(4)
	if err := m.Register(1, func(data map[string]interface{}) error {
		data["step1"] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(2, func(data map[string]interface{}) error {
		data["step2"] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(3, func(data map[string]interface{}) error {
		data["step3"] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	data := map[string]interface{}{"schema_version": 1}
	if err := m.Migrate(data, 1); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if data["schema_version"] != 4 {
		t.Errorf("schema_version = %v, want 4", data["schema_version"])
	}
	for _, key := range []string{"step1", "step2", "step3"} {
		if data[key] != true {
			t.Errorf("%s = %v, want true", key, data[key])
		}
	}
}

func TestMigrator_PartialMigration(t *testing.T) {
	m := NewMigrator(4)
	if err := m.Register(1, func(data map[string]interface{}) error {
		data["step1"] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(2, func(data map[string]interface{}) error {
		data["step2"] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(3, func(data map[string]interface{}) error {
		data["step3"] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Start from version 2 (skip step 1)
	data := map[string]interface{}{"schema_version": 2}
	if err := m.Migrate(data, 2); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if data["schema_version"] != 4 {
		t.Errorf("schema_version = %v, want 4", data["schema_version"])
	}
	if _, ok := data["step1"]; ok {
		t.Error("step1 should not have been applied")
	}
	if data["step2"] != true {
		t.Errorf("step2 = %v, want true", data["step2"])
	}
	if data["step3"] != true {
		t.Errorf("step3 = %v, want true", data["step3"])
	}
}

func TestMigrator_MissingStep(t *testing.T) {
	m := NewMigrator(3)
	if err := m.Register(1, func(data map[string]interface{}) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Missing step 2→3

	data := map[string]interface{}{"schema_version": 1}
	err := m.Migrate(data, 1)
	if err == nil {
		t.Fatal("expected error for missing migration step")
	}
}

func TestMigrator_StepFailure(t *testing.T) {
	m := NewMigrator(3)
	if err := m.Register(1, func(data map[string]interface{}) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(2, func(data map[string]interface{}) error {
		return fmt.Errorf("intentional failure")
	}); err != nil {
		t.Fatal(err)
	}

	data := map[string]interface{}{"schema_version": 1}
	err := m.Migrate(data, 1)
	if err == nil {
		t.Fatal("expected error for failed migration step")
	}
}

func TestMigrator_InvalidVersion(t *testing.T) {
	m := NewMigrator(1)

	data := map[string]interface{}{}
	if err := m.Migrate(data, 0); err == nil {
		t.Error("expected error for version 0")
	}
	if err := m.Migrate(data, -1); err == nil {
		t.Error("expected error for negative version")
	}
}

func TestMigrator_FutureVersion(t *testing.T) {
	m := NewMigrator(1)

	data := map[string]interface{}{"schema_version": 5}
	if err := m.Migrate(data, 5); err == nil {
		t.Error("expected error for future version")
	}
}

func TestMigrator_RegisterDuplicate(t *testing.T) {
	m := NewMigrator(3)
	if err := m.Register(1, func(data map[string]interface{}) error { return nil }); err != nil {
		t.Fatal(err)
	}

	if err := m.Register(1, func(data map[string]interface{}) error { return nil }); err == nil {
		t.Error("expected error for duplicate registration")
	}
}

func TestDefaultMigrator(t *testing.T) {
	if DefaultMigrator == nil {
		t.Fatal("DefaultMigrator is nil")
	}
	if DefaultMigrator.current != CurrentSchemaVersion {
		t.Errorf("DefaultMigrator.current = %d, want %d", DefaultMigrator.current, CurrentSchemaVersion)
	}
}
