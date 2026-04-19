// Package yaml provides atomic YAML file I/O and quarantine utilities.
package yaml

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	yamlv3 "gopkg.in/yaml.v3"
)

// ForceValidation forces full YAML Unmarshal validation on every write when true.
// Useful for debugging and development. Default is false (checksum-based skip enabled).
var ForceValidation bool

// validationFallbackInterval forces full validation every N consecutive cache hits
// for the same path, as a safety net against undetected data corruption.
const validationFallbackInterval uint32 = 100

type validationEntry struct {
	sum      [32]byte
	hitCount uint32
}

// validationCache caches successful YAML validation results keyed by file path.
// Uses SHA-256 checksums to skip redundant full Unmarshal when content is unchanged.
type validationCache struct {
	mu      sync.Mutex
	entries map[string]*validationEntry
}

var globalValidationCache = &validationCache{
	entries: make(map[string]*validationEntry),
}

// shouldSkipValidation returns true if full YAML validation can be safely skipped.
// Validation is skipped when the content checksum matches a previously validated value,
// except every validationFallbackInterval hits which force revalidation.
func (c *validationCache) shouldSkipValidation(path string, content []byte) bool {
	if ForceValidation {
		return false
	}

	sum := sha256.Sum256(content)

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[path]
	if !ok || e.sum != sum {
		return false
	}

	// Fallback: force full validation periodically to detect silent corruption.
	e.hitCount++
	if e.hitCount >= validationFallbackInterval {
		e.hitCount = 0
		return false
	}
	return true
}

// recordValidation stores the checksum of successfully validated content.
func (c *validationCache) recordValidation(path string, content []byte) {
	sum := sha256.Sum256(content)
	c.mu.Lock()
	c.entries[path] = &validationEntry{sum: sum}
	c.mu.Unlock()
}

// AtomicWrite marshals data as YAML and atomically writes it to path.
func AtomicWrite(path string, data any) error {
	content, err := yamlv3.Marshal(data)
	if err != nil {
		return fmt.Errorf("yaml marshal: %w", err)
	}
	return AtomicWriteRaw(path, content)
}

// AtomicWriteRaw atomically writes raw bytes to path using a temp file and rename.
func AtomicWriteRaw(path string, content []byte) (retErr error) {
	// Step 1: Create temp file and write content
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".maestro-tmp-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	var tmpClosed bool
	defer func() {
		if !tmpClosed {
			if closeErr := tmp.Close(); closeErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close temp file: %w", closeErr))
			}
		}
		if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("remove temp file: %w", removeErr))
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
	if !bytes.Equal(written, content) {
		return fmt.Errorf("write verification failed: content mismatch (wrote %d bytes, read back %d bytes)", len(content), len(written))
	}
	// Checksum-based conditional validation: skip full Unmarshal when content
	// matches a previously validated checksum for this path. Periodic fallback
	// (every validationFallbackInterval hits) ensures corruption is eventually detected.
	if !globalValidationCache.shouldSkipValidation(path, written) {
		if err := validateYAML(written); err != nil {
			return fmt.Errorf("yaml validation failed: %w", err)
		}
		globalValidationCache.recordValidation(path, written)
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
			slog.Warn("failed to close directory", "dir", dir, "error", err)
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
			slog.Warn("failed to close source file", "path", src, "error", err)
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
				slog.Warn("failed to close backup temp file", "path", tmpName, "error", err)
			}
		}
		if err := os.Remove(tmpName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("failed to remove backup temp file", "path", tmpName, "error", err)
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
