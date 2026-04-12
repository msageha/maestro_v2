package daemon

import (
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

type testYAMLStruct struct {
	Name    string `yaml:"name"`
	Version int    `yaml:"version"`
}

func TestLoadYAMLFile_Success(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	data := testYAMLStruct{Name: "hello", Version: 42}
	raw, err := yamlv3.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}

	result, rawBytes, err := loadYAMLFile[testYAMLStruct](path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Name != "hello" || result.Version != 42 {
		t.Errorf("got %+v, want {Name:hello Version:42}", result)
	}
	if rawBytes == nil {
		t.Error("expected raw bytes, got nil")
	}
}

func TestLoadYAMLFile_MissingAllowed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nonexistent.yaml")

	result, rawBytes, err := loadYAMLFile[testYAMLStruct](path, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Name != "" || result.Version != 0 {
		t.Errorf("expected zero value, got %+v", result)
	}
	if rawBytes != nil {
		t.Error("expected nil bytes for missing file")
	}
}

func TestLoadYAMLFile_MissingNotAllowed(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nonexistent.yaml")

	_, _, err := loadYAMLFile[testYAMLStruct](path, false)
	if err == nil {
		t.Fatal("expected error for missing file with allowMissing=false")
	}
	// Read errors are returned unwrapped so os.IsNotExist works directly.
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist to return true, got: %v", err)
	}
}

func TestLoadYAMLFile_InvalidYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")

	if err := os.WriteFile(path, []byte(":::invalid"), 0600); err != nil {
		t.Fatal(err)
	}

	_, rawBytes, err := loadYAMLFile[testYAMLStruct](path, false)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	if rawBytes == nil {
		t.Error("expected raw bytes even on parse failure")
	}
}

func TestLoadQueueFile_WithDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.yaml")

	type simpleQueue struct {
		Version int    `yaml:"version"`
		Type    string `yaml:"type"`
	}

	// File does not exist — defaults should be applied
	result, rawBytes, err := loadQueueFile(path, func(q *simpleQueue) {
		if q.Version == 0 {
			q.Version = 1
			q.Type = "test_queue"
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rawBytes != nil {
		t.Error("expected nil bytes for missing file")
	}
	if result.Version != 1 || result.Type != "test_queue" {
		t.Errorf("defaults not applied: got %+v", result)
	}

	// Write file and re-read
	raw, _ := yamlv3.Marshal(simpleQueue{Version: 2, Type: "existing"})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}

	result2, _, err := loadQueueFile(path, func(q *simpleQueue) {
		if q.Version == 0 {
			q.Version = 1
			q.Type = "test_queue"
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result2.Version != 2 || result2.Type != "existing" {
		t.Errorf("expected existing values, got %+v", result2)
	}
}
