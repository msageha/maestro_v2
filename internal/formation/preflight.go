package formation

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"

	"github.com/msageha/maestro_v2/internal/uds"
)

// ErrPreflightFailed indicates maestro up failed before any formation resources
// were created. CLI callers must not run CleanupOnFailure for errors matching
// this sentinel.
var ErrPreflightFailed = errors.New("maestro up preflight failed before creating resources")

// ErrSandboxedLaunch indicates maestro up was invoked in an environment that
// blocks the OS primitives the formation needs (AF_UNIX sockets / tmux).
var ErrSandboxedLaunch = errors.New("maestro up launched in a restricted sandbox")

type preflightError struct {
	err error
}

func (e *preflightError) Error() string {
	return e.err.Error()
}

func (e *preflightError) Unwrap() []error {
	return []error{ErrPreflightFailed, e.err}
}

func markPreflightError(err error) error {
	if err == nil {
		return nil
	}
	return &preflightError{err: err}
}

// preflightEnvironment verifies the host can create the primitives a
// formation needs BEFORE any tmux/daemon resource is created:
//   - tmux binary is on PATH
//   - a Unix domain socket can be bound at a unique probe path next to the
//     daemon socket, falling back to os.TempDir when that probe path would
//     exceed the AF_UNIX path-length limit
//
// Returns ErrSandboxedLaunch (wrapped, with remediation) when a check fails
// in a way consistent with an OS sandbox. A clean environment (including
// dev machines whose sandbox allows Unix sockets) passes.
func preflightEnvironment(maestroDir string) error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return markPreflightError(fmt.Errorf("tmux binary not found on PATH: %w", err))
	}

	socketPath, err := uds.SocketPath(maestroDir)
	if err != nil {
		return markPreflightError(fmt.Errorf("resolve daemon socket path: %w", err))
	}
	probePath, err := preflightProbeSocketPath(socketPath)
	if err != nil {
		return markPreflightError(err)
	}

	return classifyPreflightProbeErr(uds.ProbeUnixSocket(probePath))
}

// preflightProbeSeq disambiguates concurrent probes within one process. The
// daemon socket's parent directory is the per-UID shared /tmp/maestro-uds-<uid>/
// and the probe name previously used the PID alone, so two concurrent
// preflightEnvironment calls in the same process (parallel `Up` invocations,
// parallel tests) raced on the same path — ProbeUnixSocket removes the path
// before binding, so the loser observed "bind: address already in use" (or
// had its live probe socket removed underneath it).
var preflightProbeSeq atomic.Uint64

func preflightProbeSocketPath(realSocketPath string) (string, error) {
	name := fmt.Sprintf(".preflight-probe-%d-%d.sock", os.Getpid(), preflightProbeSeq.Add(1))
	probePath := filepath.Join(filepath.Dir(realSocketPath), name)
	if len(probePath) <= uds.MaxUnixSocketPathLen() {
		return probePath, nil
	}
	probePath = filepath.Join(os.TempDir(), name)
	if len(probePath) <= uds.MaxUnixSocketPathLen() {
		return probePath, nil
	}
	return "", fmt.Errorf("preflight probe socket path too long: %d bytes exceeds %d byte limit (path: %s)", len(probePath), uds.MaxUnixSocketPathLen(), probePath)
}

func classifyPreflightProbeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, uds.ErrUnixSocketUnavailable) {
		return markPreflightError(fmt.Errorf(
			"%w: AF_UNIX socket needed by the maestro daemon cannot be created here; this usually means maestro up is running inside an OS sandbox, such as a sandboxed Claude Code Bash tool; run maestro up from a normal shell outside the sandbox, or grant the sandbox Unix-socket access: %w",
			ErrSandboxedLaunch,
			err,
		))
	}
	return markPreflightError(fmt.Errorf("probe daemon unix socket: %w", err))
}
