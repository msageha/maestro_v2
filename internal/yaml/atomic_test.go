package yaml

import (
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

func TestAtomicWrite_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	data := map[string]any{"key": "value", "count": 42}
	if err := AtomicWrite(path, data); err != nil {
		t.Fatalf("AtomicWrite failed: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var result map[string]any
	if err := yamlv3.Unmarshal(content, &result); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if result["key"] != "value" {
		t.Errorf("key: got %v, want %q", result["key"], "value")
	}
}

func TestAtomicWrite_CreatesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	// Write initial content
	if err := AtomicWrite(path, map[string]string{"version": "1"}); err != nil {
		t.Fatalf("first write failed: %v", err)
	}

	// Write updated content
	if err := AtomicWrite(path, map[string]string{"version": "2"}); err != nil {
		t.Fatalf("second write failed: %v", err)
	}

	// Verify .bak contains original content
	bakContent, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("ReadFile .bak failed: %v", err)
	}

	var bakData map[string]string
	if err := yamlv3.Unmarshal(bakContent, &bakData); err != nil {
		t.Fatalf("Unmarshal .bak failed: %v", err)
	}

	if bakData["version"] != "1" {
		t.Errorf("backup version: got %q, want %q", bakData["version"], "1")
	}

	// Verify current file has new content
	curContent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile current failed: %v", err)
	}

	var curData map[string]string
	if err := yamlv3.Unmarshal(curContent, &curData); err != nil {
		t.Fatalf("Unmarshal current failed: %v", err)
	}

	if curData["version"] != "2" {
		t.Errorf("current version: got %q, want %q", curData["version"], "2")
	}
}

func TestAtomicWriteRaw_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	invalidYAML := []byte(":\n  invalid: [\n    broken")
	err := AtomicWriteRaw(path, invalidYAML)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}

	// Verify file was not created
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file should not exist after failed write")
	}
}

func TestAtomicWrite_NoTempFileLeftOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	invalidYAML := []byte(":\n  broken: [\n")
	_ = AtomicWriteRaw(path, invalidYAML)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".yaml" {
			t.Errorf("unexpected file remaining: %s", entry.Name())
		}
	}
}

func TestAtomicWrite_StructData(t *testing.T) {
	type testStruct struct {
		Name    string `yaml:"name"`
		Version int    `yaml:"version"`
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	if err := AtomicWrite(path, &testStruct{Name: "maestro", Version: 2}); err != nil {
		t.Fatalf("AtomicWrite failed: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	var result testStruct
	if err := yamlv3.Unmarshal(content, &result); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if result.Name != "maestro" || result.Version != 2 {
		t.Errorf("got %+v", result)
	}
}
