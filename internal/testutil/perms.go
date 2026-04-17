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
func FixDirPerms(t *testing.T, root string) {
	t.Helper()
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0755)
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
