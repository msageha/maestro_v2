package daemon

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// fixTestDirPerms walks root and ensures every directory has mode 0755.
// This guards against the process-wide syscall.Umask(0177) in
// server.Start() (called by daemon_startup_test.go) which can cause
// t.TempDir() / os.MkdirAll to create directories with 0600 (no
// execute bit), resulting in sporadic "mkdir: permission denied".
func fixTestDirPerms(t *testing.T, root string) {
	t.Helper()
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0755)
		}
		return nil
	})
}
