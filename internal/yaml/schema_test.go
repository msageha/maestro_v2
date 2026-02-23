package yaml

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateSchemaHeader_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	content := []byte("schema_version: 1\nfile_type: queue_command\ncommands: []\n")
	os.WriteFile(path, content, 0644)

	if err := ValidateSchemaHeader(path, "queue_command"); err != nil {
		t.Errorf("expected valid, got error: %v", err)
	}
}

func TestValidateSchemaHeader_AllFileTypes(t *testing.T) {
	fileTypes := []string{
		"queue_command", "queue_task", "queue_notification",
		"result_task", "result_command",
		"state_command", "state_metrics", "state_continuous",
	}

	for _, ft := range fileTypes {
		t.Run(ft, func(t *testing.T) {
			content := []byte("schema_version: 1\nfile_type: " + ft + "\n")
			if err := ValidateSchemaHeaderFromBytes(content, ft); err != nil {
				t.Errorf("expected valid for %q, got error: %v", ft, err)
			}
		})
	}
}

func TestValidateSchemaHeader_UnsupportedVersion(t *testing.T) {
	content := []byte("schema_version: 99\nfile_type: queue_command\n")
	err := ValidateSchemaHeaderFromBytes(content, "queue_command")
	if err == nil {
		t.Error("expected error for unsupported version")
	}
}

func TestValidateSchemaHeader_NegativeVersion(t *testing.T) {
	content := []byte("schema_version: -1\nfile_type: queue_command\n")
	err := ValidateSchemaHeaderFromBytes(content, "queue_command")
	if err == nil {
		t.Error("expected error for negative schema_version")
	}
}

func TestValidateSchemaHeader_MissingVersion(t *testing.T) {
	content := []byte("file_type: queue_command\n")
	err := ValidateSchemaHeaderFromBytes(content, "queue_command")
	if err == nil {
		t.Error("expected error for missing schema_version")
	}
}

func TestValidateSchemaHeader_MissingFileType(t *testing.T) {
	content := []byte("schema_version: 1\n")
	err := ValidateSchemaHeaderFromBytes(content, "queue_command")
	if err == nil {
		t.Error("expected error for missing file_type")
	}
}

func TestValidateSchemaHeader_UnknownFileType(t *testing.T) {
	content := []byte("schema_version: 1\nfile_type: unknown_type\n")
	err := ValidateSchemaHeaderFromBytes(content, "unknown_type")
	if err == nil {
		t.Error("expected error for unknown file_type")
	}
}

func TestValidateSchemaHeader_FileTypeMismatch(t *testing.T) {
	content := []byte("schema_version: 1\nfile_type: queue_task\n")
	err := ValidateSchemaHeaderFromBytes(content, "queue_command")
	if err == nil {
		t.Error("expected error for file_type mismatch")
	}
}

func TestValidateSchemaHeader_EmptyExpectedType(t *testing.T) {
	content := []byte("schema_version: 1\nfile_type: queue_command\n")
	if err := ValidateSchemaHeaderFromBytes(content, ""); err != nil {
		t.Errorf("expected valid when no expected type specified, got: %v", err)
	}
}

func TestNeedsMigration(t *testing.T) {
	if NeedsMigration(CurrentSchemaVersion) {
		t.Error("current version should not need migration")
	}
	if !NeedsMigration(0) {
		t.Error("version 0 should need migration")
	}
}
