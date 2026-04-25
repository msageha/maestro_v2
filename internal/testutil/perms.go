package testutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// FixDirPerms walks root and ensures every directory has mode 0755.
// This guards against process-wide umask changes (e.g., syscall.Umask(0177))
// that can cause directories to be created without execute bits.
//
// Errors from WalkDir / Chmod are ignored intentionally: this is a best-effort
// test-fixture repair. If a directory cannot be chmodded, the subsequent
// test will fail with a clear message anyway.
func FixDirPerms(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0755) //nolint:gosec // 0755 is intentional for test-fixture dir perms
		}
		return nil
	})
}

// SetupDirFixPerms creates a temporary .maestro directory structure
// and fixes permissions to guard against umask issues in parallel tests.
// It returns the path to the .maestro directory.
func SetupDirFixPerms(t *testing.T) string {
	t.Helper()
	maestroDir := SetupDir(t)
	FixDirPerms(t, filepath.Dir(maestroDir))
	return maestroDir
}
