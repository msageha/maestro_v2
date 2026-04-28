package uds

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// socketFallbackDirName names the per-user fallback directory under the
// canonical /tmp base. The UID suffix is filled in by socketFallbackDirName(),
// so two users on the same host do not contend for the same dir.
const socketFallbackDirBaseName = "maestro-uds"

// socketFallbackDirName returns the per-user fallback directory name. Encoding
// the UID keeps the directory ownership and 0700 permissions stable across
// users on a shared host (e.g. CI runners) without coupling the dir name to
// TMPDIR, which is the variable that varies between sandboxed processes.
func socketFallbackDirName() string {
	uid := os.Getuid()
	if uid < 0 {
		// Non-Unix platforms report -1; fall back to the unsuffixed name so the
		// existing behaviour is preserved on platforms without proper uids.
		return socketFallbackDirBaseName
	}
	return fmt.Sprintf("%s-%d", socketFallbackDirBaseName, uid)
}

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
	dirName := socketFallbackDirName()

	for _, base := range socketFallbackBases() {
		dir := filepath.Join(base, dirName)
		path := filepath.Join(dir, name)
		if len(path) > maxUnixSocketPathLen {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create socket fallback dir %s: %w", dir, err)
		}
		// Owner-only directory mode is required for the UDS fallback path:
		// any peer that can traverse the directory could connect to the
		// socket. gosec G302 wants ≤0o600, which would block traversal
		// entirely; the fix here is to switch to octal-literal syntax so
		// gosec recognises the deliberate 0o700.
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directory needs +x for traversal; owner-only is the strictest viable mode
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

// socketFallbackBases lists candidate parent directories for the fallback
// socket path, in priority order.
//
// /tmp is intentionally first: it is the only path reachable by every Unix
// process under any sandbox, so daemon and clients agree on it regardless of
// the per-process TMPDIR. Sandboxed clients (e.g. Claude Code's Bash hooks)
// inject TMPDIR=/tmp/claude-<uid>/ which would otherwise diverge from the
// daemon's macOS default of /var/folders/.../T/ and silently break IPC. We
// keep os.TempDir() as a secondary base for environments where /tmp is not
// writable (some Linux containers), but the daemon and every CLI client must
// resolve to the same primary base by default.
func socketFallbackBases() []string {
	bases := []string{"/tmp"}
	td := os.TempDir()
	if td != "" && td != "/tmp" {
		bases = append(bases, td)
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
