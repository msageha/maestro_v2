package yaml

import (
	"crypto/sha256"
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

func TestAtomicWrite_BackupPreservesPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")

	// Write initial content
	if err := AtomicWrite(path, map[string]string{"version": "1"}); err != nil {
		t.Fatalf("first write failed: %v", err)
	}

	// Set restrictive permissions (0600) on the original file
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}

	// Write updated content — this triggers .bak creation
	if err := AtomicWrite(path, map[string]string{"version": "2"}); err != nil {
		t.Fatalf("second write failed: %v", err)
	}

	// Verify .bak file has the same permissions as the original
	bakInfo, err := os.Stat(path + ".bak")
	if err != nil {
		t.Fatalf("stat .bak failed: %v", err)
	}

	bakPerm := bakInfo.Mode().Perm()
	if bakPerm != 0600 {
		t.Errorf("backup permissions: got %04o, want 0600", bakPerm)
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

func TestValidationCache_SkipsOnIdenticalContent(t *testing.T) {
	c := &validationCache{entries: make(map[string]*validationEntry)}
	content := []byte("key: value\n")

	// First call: no cache entry, should not skip
	if c.shouldSkipValidation("/tmp/test.yaml", content) {
		t.Error("expected first call to not skip validation")
	}

	// Record successful validation
	c.recordValidation("/tmp/test.yaml", content)

	// Second call: same content, should skip
	if !c.shouldSkipValidation("/tmp/test.yaml", content) {
		t.Error("expected second call with same content to skip validation")
	}
}

func TestValidationCache_ValidatesOnDifferentContent(t *testing.T) {
	c := &validationCache{entries: make(map[string]*validationEntry)}
	content1 := []byte("key: value1\n")
	content2 := []byte("key: value2\n")

	c.recordValidation("/tmp/test.yaml", content1)

	// Different content should not skip validation
	if c.shouldSkipValidation("/tmp/test.yaml", content2) {
		t.Error("expected different content to trigger validation")
	}
}

func TestValidationCache_FallbackInterval(t *testing.T) {
	c := &validationCache{entries: make(map[string]*validationEntry)}
	content := []byte("key: value\n")

	c.recordValidation("/tmp/test.yaml", content)

	// Cache hits should skip validation up to fallback interval
	for i := uint32(1); i < validationFallbackInterval; i++ {
		if !c.shouldSkipValidation("/tmp/test.yaml", content) {
			t.Errorf("expected skip at hit %d", i)
		}
	}

	// At the fallback interval, should force validation
	if c.shouldSkipValidation("/tmp/test.yaml", content) {
		t.Error("expected fallback interval to force validation")
	}

	// After fallback, re-record and cache should work again
	c.recordValidation("/tmp/test.yaml", content)
	if !c.shouldSkipValidation("/tmp/test.yaml", content) {
		t.Error("expected cache to work again after re-recording")
	}
}

func TestValidationCache_ForceValidationFlag(t *testing.T) {
	c := &validationCache{entries: make(map[string]*validationEntry)}
	content := []byte("key: value\n")

	c.recordValidation("/tmp/test.yaml", content)

	// Enable ForceValidation
	oldVal := ForceValidation
	ForceValidation = true
	defer func() { ForceValidation = oldVal }()

	// Should not skip even with cached entry
	if c.shouldSkipValidation("/tmp/test.yaml", content) {
		t.Error("expected ForceValidation=true to prevent skipping")
	}
}

func TestValidationCache_IndependentPaths(t *testing.T) {
	c := &validationCache{entries: make(map[string]*validationEntry)}
	content := []byte("key: value\n")

	c.recordValidation("/tmp/a.yaml", content)

	// Different path with same content should not skip (no cache entry for this path)
	if c.shouldSkipValidation("/tmp/b.yaml", content) {
		t.Error("expected different path to not skip validation")
	}
}

func TestValidationCache_RecordUpdatesChecksum(t *testing.T) {
	c := &validationCache{entries: make(map[string]*validationEntry)}
	content1 := []byte("key: value1\n")
	content2 := []byte("key: value2\n")

	c.recordValidation("/tmp/test.yaml", content1)

	// Update with new content
	c.recordValidation("/tmp/test.yaml", content2)

	// Old content should not skip
	if c.shouldSkipValidation("/tmp/test.yaml", content1) {
		t.Error("expected old content to trigger validation after update")
	}

	// New content should skip
	if !c.shouldSkipValidation("/tmp/test.yaml", content2) {
		t.Error("expected new content to skip validation after update")
	}
}

func TestAtomicWrite_DataIntegrityWithCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	data := map[string]string{"key": "value"}

	// Write same data multiple times (exercises cache skip path)
	for i := 0; i < 5; i++ {
		if err := AtomicWrite(path, data); err != nil {
			t.Fatalf("AtomicWrite iteration %d failed: %v", i, err)
		}

		// Verify data integrity on every write
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile iteration %d failed: %v", i, err)
		}

		var result map[string]string
		if err := yamlv3.Unmarshal(content, &result); err != nil {
			t.Fatalf("Unmarshal iteration %d failed: %v", i, err)
		}
		if result["key"] != "value" {
			t.Errorf("iteration %d: got %q, want %q", i, result["key"], "value")
		}
	}
}

func TestValidationCache_RecordResetsHitCount(t *testing.T) {
	c := &validationCache{entries: make(map[string]*validationEntry)}
	content := []byte("key: value\n")
	path := "/tmp/test.yaml"
	sum := sha256.Sum256(content)

	c.recordValidation(path, content)

	// Accumulate some hits
	for i := 0; i < 50; i++ {
		c.shouldSkipValidation(path, content)
	}

	// Re-record should reset hitCount to 0
	c.recordValidation(path, content)

	c.mu.Lock()
	e := c.entries[path]
	if e.hitCount != 0 {
		t.Errorf("expected hitCount=0 after re-record, got %d", e.hitCount)
	}
	if e.sum != sum {
		t.Error("checksum mismatch after re-record")
	}
	c.mu.Unlock()
}
