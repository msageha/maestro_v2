package uds

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const socketFallbackDirName = "maestro-uds"

// SocketPath returns the daemon socket path for a .maestro directory.
//
// Unix domain sockets have a small platform-dependent path length limit. The
// conventional .maestro/daemon.sock path is kept when it fits; otherwise a
// stable short path under the system temp directory is derived from the absolute
// .maestro path. Projects under /tmp and /private/tmp also use the stable temp
// path because some sandboxes permit normal files there but reject socket bind.
// Both daemon and clients must use this resolver.
func SocketPath(maestroDir string) (string, error) {
	preferred := filepath.Join(maestroDir, DefaultSocketName)
	canonical, err := canonicalSocketBase(maestroDir)
	if err != nil {
		return "", err
	}
	if len(preferred) <= maxUnixSocketPathLen && !needsFallbackSocketPath(canonical) {
		return preferred, nil
	}

	sum := sha256.Sum256([]byte(canonical))
	name := hex.EncodeToString(sum[:8]) + ".sock"

	for _, base := range socketFallbackBases() {
		dir := filepath.Join(base, socketFallbackDirName)
		path := filepath.Join(dir, name)
		if len(path) > maxUnixSocketPathLen {
			continue
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", fmt.Errorf("create socket fallback dir %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return "", fmt.Errorf("chmod socket fallback dir %s: %w", dir, err)
		}
		return path, nil
	}

	return "", fmt.Errorf("socket path too long: preferred path is %d bytes and no fallback path fits within %d bytes", len(preferred), maxUnixSocketPathLen)
}

// SocketCleanupPaths returns all socket paths that may need stale-file cleanup.
// The first path is the active path when it can be resolved. The conventional
// .maestro/daemon.sock path is included as well so upgrades from older builds do
// not leave stale sockets behind when the active path falls back to /tmp.
func SocketCleanupPaths(maestroDir string) []string {
	preferred := filepath.Join(maestroDir, DefaultSocketName)
	active, err := SocketPath(maestroDir)
	if err != nil {
		return []string{preferred}
	}
	if active == preferred {
		return []string{active}
	}
	return []string{active, preferred}
}

func socketFallbackBases() []string {
	bases := []string{os.TempDir()}
	if os.TempDir() != "/tmp" {
		bases = append(bases, "/tmp")
	}
	return bases
}

func canonicalSocketBase(maestroDir string) (string, error) {
	abs, err := filepath.Abs(maestroDir)
	if err != nil {
		return "", fmt.Errorf("resolve maestro dir: %w", err)
	}
	canonical := filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = filepath.Clean(resolved)
	}
	return canonical, nil
}

func needsFallbackSocketPath(canonicalMaestroDir string) bool {
	clean := filepath.Clean(canonicalMaestroDir)
	return clean == "/tmp" ||
		clean == "/private/tmp" ||
		strings.HasPrefix(clean, "/tmp/") ||
		strings.HasPrefix(clean, "/private/tmp/")
}
