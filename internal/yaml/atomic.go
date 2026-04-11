// Package yaml provides atomic YAML file I/O and quarantine utilities.
package yaml

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"
)

// AtomicWrite marshals data as YAML and atomically writes it to path.
func AtomicWrite(path string, data any) error {
	content, err := yamlv3.Marshal(data)
	if err != nil {
		return fmt.Errorf("yaml marshal: %w", err)
	}
	return AtomicWriteRaw(path, content)
}

// AtomicWriteRaw atomically writes raw bytes to path using a temp file and rename.
func AtomicWriteRaw(path string, content []byte) error {
	// Step 1: Create temp file and write content
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".maestro-tmp-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	var tmpClosed bool
	defer func() {
		// Clean up temp file on any failure
		if !tmpClosed {
			if err := tmp.Close(); err != nil {
				log.Printf("WARN: failed to close temp file %s: %v", tmpName, err)
			}
		}
		if err := os.Remove(tmpName); err != nil && !os.IsNotExist(err) {
			log.Printf("WARN: failed to remove temp file %s: %v", tmpName, err)
		}
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
	tmpClosed = true

	// Step 2: Validate written content by re-reading temp file
	written, err := os.ReadFile(tmpName) //nolint:gosec // tmpName is an internally generated temp file path
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

	// SRE-004: Fsync parent directory to ensure rename metadata is durable
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync parent dir: %w", err)
	}

	return nil
}

// syncDir fsyncs a directory to ensure rename metadata durability.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is the parent of the atomic write target; caller controls path
	if err != nil {
		return err
	}
	defer func() {
		if err := d.Close(); err != nil {
			log.Printf("WARN: failed to close directory %s: %v", dir, err)
		}
	}()
	return d.Sync()
}

func validateYAML(content []byte) error {
	var v any
	return yamlv3.Unmarshal(content, &v)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is an internally managed backup path; caller controls path
	if err != nil {
		return err
	}
	defer func() {
		if err := in.Close(); err != nil {
			log.Printf("WARN: failed to close source file %s: %v", src, err)
		}
	}()

	// Stat the source to preserve its permissions on the backup.
	srcInfo, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat source for permissions: %w", err)
	}
	srcMode := srcInfo.Mode().Perm()

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".maestro-bak-tmp-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	var tmpClosed bool
	defer func() {
		if !tmpClosed {
			if err := tmp.Close(); err != nil {
				log.Printf("WARN: failed to close backup temp file %s: %v", tmpName, err)
			}
		}
		if err := os.Remove(tmpName); err != nil && !os.IsNotExist(err) {
			log.Printf("WARN: failed to remove backup temp file %s: %v", tmpName, err)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	// Match the backup file's permissions to the source file (e.g., 0600
	// for state files) before rename so the .bak is never world-readable.
	if err := tmp.Chmod(srcMode); err != nil {
		return fmt.Errorf("chmod backup temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	tmpClosed = true

	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}
