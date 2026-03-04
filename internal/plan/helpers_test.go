package plan

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileIfExists_FileExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	want := []byte("hello: world\n")
	if err := os.WriteFile(path, want, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := readFileIfExists(path)
	if err != nil {
		t.Fatalf("readFileIfExists(%q) error: %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("readFileIfExists(%q) = %q, want %q", path, got, want)
	}
}

func TestReadFileIfExists_FileDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.yaml")

	got, err := readFileIfExists(path)
	if err != nil {
		t.Fatalf("readFileIfExists should return nil error for missing file, got: %v", err)
	}
	if got != nil {
		t.Errorf("readFileIfExists should return nil data for missing file, got: %q", got)
	}
}

func TestReadFileIfExists_PermissionError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "noperm.yaml")
	if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	// Make unreadable
	if err := os.Chmod(path, 0000); err != nil {
		t.Skip("cannot change permissions on this platform")
	}
	defer os.Chmod(path, 0644)

	_, err := readFileIfExists(path)
	if err == nil {
		t.Fatal("readFileIfExists should return error for permission-denied file")
	}
}

func TestUnmarshalYAML_ValidData(t *testing.T) {
	data := []byte("name: test\ncount: 42\n")
	var result struct {
		Name  string `yaml:"name"`
		Count int    `yaml:"count"`
	}
	if err := unmarshalYAML(data, &result); err != nil {
		t.Fatalf("unmarshalYAML error: %v", err)
	}
	if result.Name != "test" || result.Count != 42 {
		t.Errorf("unmarshalYAML = %+v, want {Name:test Count:42}", result)
	}
}

func TestUnmarshalYAML_InvalidData(t *testing.T) {
	data := []byte(":\n  :\n  bad yaml [[")
	var result map[string]string
	if err := unmarshalYAML(data, &result); err == nil {
		t.Fatal("unmarshalYAML should return error for invalid YAML")
	}
}

func TestUnmarshalYAML_EmptyData(t *testing.T) {
	var result map[string]string
	if err := unmarshalYAML([]byte{}, &result); err != nil {
		t.Fatalf("unmarshalYAML on empty data should not error, got: %v", err)
	}
}

func TestWriteYAMLAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.yaml")

	data := map[string]string{"key": "value"}
	if err := writeYAMLAtomic(path, data); err != nil {
		t.Fatalf("writeYAMLAtomic error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}

	var result map[string]string
	if err := unmarshalYAML(got, &result); err != nil {
		t.Fatalf("unmarshal written file: %v", err)
	}
	if result["key"] != "value" {
		t.Errorf("got key=%q, want %q", result["key"], "value")
	}
}

func TestWriteYAMLAtomic_InvalidPath(t *testing.T) {
	err := writeYAMLAtomic("/nonexistent/dir/file.yaml", map[string]string{"a": "b"})
	if err == nil {
		t.Fatal("writeYAMLAtomic should return error for invalid path")
	}
}
