package plan

import (
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

func TestDefaultMigrator(t *testing.T) {
	if DefaultMigrator == nil {
		t.Fatal("DefaultMigrator is nil")
	}
	if DefaultMigrator.current != CurrentSchemaVersion {
		t.Errorf("DefaultMigrator.current = %d, want %d", DefaultMigrator.current, CurrentSchemaVersion)
	}
}
