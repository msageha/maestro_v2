package formation

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
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

// preflightRuntimes verifies every runtime referenced by the agents config
// (claude-code / codex / gemini, resolved via agent.ConfiguredRuntimes)
// BEFORE any tmux/daemon resource is created:
//
//   - binary resolution on PATH is a hard check: a missing CLI can never
//     launch, so failing fast here beats the late failure at pane launch
//     (launcher.go exec.LookPath) that otherwise surfaces only after the
//     first dispatch.
//   - credential presence is best-effort and warning-only: auth stores are
//     not reliably inspectable (keychain-backed logins in particular), so
//     an unknown auth state must not block startup.
//
// No subprocess is spawned — version probes are `maestro doctor` territory
// (they add per-runtime latency and belong in the explicit diagnostic
// command, not on every `maestro up`). The launch-time LookPath and the
// pane terminal-error fast-fail remain the authoritative backstops.
func preflightRuntimes(cfg model.Config, rp *agent.RuntimePreflight, warn io.Writer) error {
	var missing []string
	for _, rt := range agent.ConfiguredRuntimes(cfg) {
		cmdName, _, err := rp.CheckBinary(rt.Name)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s (command %q, agents: %s)",
				rt.Name, cmdName, strings.Join(rt.Agents, ", ")))
			continue
		}
		if status, detail := rp.CheckAuth(rt.Name); status != agent.RuntimeAuthOK {
			_, _ = fmt.Fprintf(warn, "Warning: runtime %s (agents: %s) auth status unknown: %s\n",
				rt.Name, strings.Join(rt.Agents, ", "), detail)
		}
	}
	if len(missing) > 0 {
		return markPreflightError(fmt.Errorf(
			"agent runtime binaries not found on PATH: %s; install the missing CLI or fix the agents.* model settings in .maestro/config.yaml, then run `maestro doctor` to re-check",
			strings.Join(missing, "; ")))
	}
	return nil
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
