// Package yaml provides atomic YAML file I/O and quarantine utilities.
package yaml

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"
)

func AtomicWrite(path string, data any) error {
	content, err := yamlv3.Marshal(data)
	if err != nil {
		return fmt.Errorf("yaml marshal: %w", err)
	}
	return AtomicWriteRaw(path, content)
}

func AtomicWriteRaw(path string, content []byte) error {
	// Step 1: Create temp file and write content
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".maestro-tmp-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	defer func() {
		// Clean up temp file on any failure
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Step 2: Validate written content by re-reading temp file
	written, err := os.ReadFile(tmpName)
	if err != nil {
		return fmt.Errorf("read temp file for validation: %w", err)
	}
	if err := validateYAML(written); err != nil {
		return fmt.Errorf("yaml validation failed: %w", err)
	}

	// Step 3: Create .bak if original exists
	if _, err := os.Stat(path); err == nil {
		bakPath := path + ".bak"
		if err := copyFile(path, bakPath); err != nil {
			return fmt.Errorf("create backup: %w", err)
		}
	}

	// Step 4: Atomic rename (same-volume, APFS atomic)
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomic rename: %w", err)
	}

	return nil
}

func validateYAML(content []byte) error {
	var v any
	return yamlv3.Unmarshal(content, &v)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
