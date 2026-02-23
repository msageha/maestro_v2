package yaml

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

func TestQuarantine(t *testing.T) {
	maestroDir := t.TempDir()
	filePath := filepath.Join(maestroDir, "corrupted.yaml")

	// Create a corrupted file
	os.WriteFile(filePath, []byte("corrupted: [\n"), 0644)

	if err := Quarantine(maestroDir, filePath); err != nil {
		t.Fatalf("Quarantine failed: %v", err)
	}

	// Original file should be gone
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Error("original file should be removed after quarantine")
	}

	// Quarantine dir should have the file
	quarantineDir := filepath.Join(maestroDir, "quarantine")
	entries, err := os.ReadDir(quarantineDir)
	if err != nil {
		t.Fatalf("ReadDir quarantine failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 quarantined file, got %d", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), "corrupted.yaml.") || !strings.HasSuffix(entries[0].Name(), ".corrupt") {
		t.Errorf("unexpected quarantine filename: %s", entries[0].Name())
	}
}

func TestRestoreFromBackup(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.yaml")
	bakPath := filePath + ".bak"

	// Create a valid backup
	validContent := []byte("schema_version: 1\nfile_type: queue_command\ncommands: []\n")
	os.WriteFile(bakPath, validContent, 0644)

	if err := RestoreFromBackup(filePath); err != nil {
		t.Fatalf("RestoreFromBackup failed: %v", err)
	}

	// File should be restored
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var header SchemaHeader
	if err := yamlv3.Unmarshal(content, &header); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if header.FileType != "queue_command" {
		t.Errorf("file_type: got %q", header.FileType)
	}
}

func TestRestoreFromBackup_NoBackup(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.yaml")

	err := RestoreFromBackup(filePath)
	if err == nil {
		t.Error("expected error when no backup exists")
	}
}

func TestRestoreFromBackup_CorruptBackup(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.yaml")
	bakPath := filePath + ".bak"

	os.WriteFile(bakPath, []byte(":\n  broken: [\n"), 0644)

	err := RestoreFromBackup(filePath)
	if err == nil {
		t.Error("expected error when backup is also corrupted")
	}
}

func TestGenerateSkeleton(t *testing.T) {
	tests := []struct {
		fileType    string
		expectField string
	}{
		{"queue_command", "commands"},
		{"queue_task", "tasks"},
		{"queue_notification", "notifications"},
		{"result_task", "results"},
		{"result_command", "results"},
		{"state_command", "command_id"},
		{"state_metrics", "queue_depth"},
		{"state_continuous", "current_iteration"},
	}

	for _, tt := range tests {
		t.Run(tt.fileType, func(t *testing.T) {
			dir := t.TempDir()
			filePath := filepath.Join(dir, "test.yaml")

			if err := GenerateSkeleton(filePath, tt.fileType); err != nil {
				t.Fatalf("GenerateSkeleton failed: %v", err)
			}

			content, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatalf("ReadFile failed: %v", err)
			}

			// Validate it's valid YAML with schema header
			var data map[string]any
			if err := yamlv3.Unmarshal(content, &data); err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}

			if data["schema_version"] != CurrentSchemaVersion {
				t.Errorf("schema_version: got %v", data["schema_version"])
			}
			if data["file_type"] != tt.fileType {
				t.Errorf("file_type: got %v", data["file_type"])
			}
			if _, ok := data[tt.expectField]; !ok {
				t.Errorf("missing expected field: %s", tt.expectField)
			}
		})
	}
}

func TestRecoverCorruptedFile_WithBackup(t *testing.T) {
	maestroDir := t.TempDir()
	filePath := filepath.Join(maestroDir, "test.yaml")
	bakPath := filePath + ".bak"

	// Create corrupted file and valid backup
	os.WriteFile(filePath, []byte("corrupted: [\n"), 0644)
	os.WriteFile(bakPath, []byte("schema_version: 1\nfile_type: queue_command\ncommands: []\n"), 0644)

	if err := RecoverCorruptedFile(maestroDir, filePath, "queue_command"); err != nil {
		t.Fatalf("RecoverCorruptedFile failed: %v", err)
	}

	// File should be restored from backup
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var header SchemaHeader
	yamlv3.Unmarshal(content, &header)
	if header.FileType != "queue_command" {
		t.Errorf("expected queue_command, got %q", header.FileType)
	}

	// Quarantine should have the corrupted file
	quarantineDir := filepath.Join(maestroDir, "quarantine")
	entries, _ := os.ReadDir(quarantineDir)
	if len(entries) != 1 {
		t.Errorf("expected 1 quarantined file, got %d", len(entries))
	}
}

func TestRecoverCorruptedFile_WithoutBackup(t *testing.T) {
	maestroDir := t.TempDir()
	filePath := filepath.Join(maestroDir, "test.yaml")

	// Create corrupted file, no backup
	os.WriteFile(filePath, []byte("corrupted: [\n"), 0644)

	if err := RecoverCorruptedFile(maestroDir, filePath, "queue_task"); err != nil {
		t.Fatalf("RecoverCorruptedFile failed: %v", err)
	}

	// File should be a skeleton
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var data map[string]any
	yamlv3.Unmarshal(content, &data)
	if data["file_type"] != "queue_task" {
		t.Errorf("expected queue_task, got %v", data["file_type"])
	}
	if _, ok := data["tasks"]; !ok {
		t.Error("expected tasks field in skeleton")
	}
}
