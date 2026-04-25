package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// RealVerifyRunner executes verification commands defined in `.maestro/
// verify.yaml`. When the file is absent, it falls back to model
// .DefaultVerifyConfigForProject so Go projects retain the historical
// `go vet ./...` baseline while non-Go repositories receive an empty config
// (forcing the operator to author verify.yaml rather than silently inheriting
// a guaranteed-failing Go command).
//
// Categories run in deterministic order — Build → Lint → Typecheck → Test →
// Security → Performance — and each command runs sequentially. The first
// failure returns Passed=false with the failing command's tail output as the
// reason; remaining commands are not executed (fail-fast). This mirrors the
// expected behaviour of CI tooling and keeps daemon CPU bounded.
type RealVerifyRunner struct {
	maestroDir     string
	projectDir     string
	logger         *slog.Logger
	commandTimeout time.Duration
	maxOutputBytes int
	// runner is the function used to execute a shell command. Tests
	// override this; production uses execShellCommand which spawns
	// `bash -c`.
	runner func(ctx context.Context, dir, cmd string) (output string, exitCode int, err error)
}

// defaultVerifyCommandTimeout is the per-command timeout. Most build/lint/test
// invocations finish far below this; the cap prevents a runaway invocation
// from blocking task progression indefinitely.
const defaultVerifyCommandTimeout = 5 * time.Minute

// defaultVerifyMaxOutputBytes caps how much stdout/stderr is retained for the
// failure reason. Reporting more would balloon the audit log without aiding
// triage; the operator can re-run the command locally for full output.
const defaultVerifyMaxOutputBytes = 4 * 1024

// NewRealVerifyRunner constructs a RealVerifyRunner anchored at maestroDir
// (where verify.yaml is expected) and projectDir (the working directory used
// for command execution; usually the repository root).
func NewRealVerifyRunner(maestroDir, projectDir string, logger *slog.Logger) *RealVerifyRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &RealVerifyRunner{
		maestroDir:     maestroDir,
		projectDir:     projectDir,
		logger:         logger,
		commandTimeout: defaultVerifyCommandTimeout,
		maxOutputBytes: defaultVerifyMaxOutputBytes,
		runner:         execShellCommand,
	}
}

// Run loads verify.yaml (or DefaultVerifyConfigForProject as fallback) and
// executes each command sequentially under cmd.Dir = workingDir (or
// r.projectDir when workingDir is empty). The first failure short-circuits
// the run; verifyOutcome reports the failing category and command in the
// reason for downstream audit.
//
// workingDir is non-empty when the caller has resolved a task-specific path
// (worker worktree, integration worktree, project root for RunOnMain). The
// fallback to r.projectDir exists for legacy test paths that do not pass a
// per-task directory; production callers always pass a resolved value so
// that worker-uncommitted changes are visible to verify commands.
func (r *RealVerifyRunner) Run(ctx context.Context, taskID, commandID, workingDir string, expectedPaths []string) (VerifyOutcome, error) {
	cfg, err := model.LoadOrDefaultVerifyConfigForProject(r.projectDir, filepath.Join(r.maestroDir, "verify.yaml"))
	if err != nil {
		return VerifyOutcome{Passed: false, Reason: fmt.Sprintf("verify_config_invalid: %v", err)}, nil
	}

	runDir := workingDir
	if runDir == "" {
		runDir = r.projectDir
	}

	r.logger.Info("verify_runner_start",
		"task_id", taskID, "command_id", commandID,
		"working_dir", runDir, "fallback", cfg.IsEmpty())

	categories := []struct {
		name string
		cmds []string
	}{
		{"build", cfg.Build},
		{"lint", cfg.Lint},
		{"typecheck", cfg.Typecheck},
		{"test", cfg.Test},
		{"security", cfg.Security},
		{"performance", cfg.Performance},
	}

	for _, cat := range categories {
		for _, cmd := range cat.cmds {
			cmd = strings.TrimSpace(cmd)
			if cmd == "" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return VerifyOutcome{Passed: false, Reason: fmt.Sprintf("verify_aborted: %v", err)}, err
			}

			cmdCtx, cancel := context.WithTimeout(ctx, r.commandTimeout)
			output, exitCode, runErr := r.runner(cmdCtx, runDir, cmd)
			cancel()

			if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
				reason := fmt.Sprintf("verify_timeout category=%s command=%q timeout=%s", cat.name, cmd, r.commandTimeout)
				r.logger.Warn("verify_runner_timeout",
					"task_id", taskID, "command_id", commandID,
					"category", cat.name, "command", cmd)
				return VerifyOutcome{Passed: false, Reason: reason}, nil
			}

			if runErr != nil || exitCode != 0 {
				tail := tailBytes(output, r.maxOutputBytes)
				reason := fmt.Sprintf("verify_failed category=%s command=%q exit=%d output_tail=%q",
					cat.name, cmd, exitCode, tail)
				if runErr != nil {
					reason = fmt.Sprintf("%s err=%v", reason, runErr)
				}
				r.logger.Warn("verify_runner_command_failed",
					"task_id", taskID, "command_id", commandID,
					"category", cat.name, "command", cmd, "exit", exitCode)
				return VerifyOutcome{Passed: false, Reason: reason}, nil
			}

			r.logger.Debug("verify_runner_command_passed",
				"task_id", taskID, "command_id", commandID,
				"category", cat.name, "command", cmd)
		}
	}

	r.logger.Info("verify_runner_passed",
		"task_id", taskID, "command_id", commandID, "expected_paths", len(expectedPaths))
	return VerifyOutcome{Passed: true}, nil
}

// execShellCommand runs cmd via `bash -c` with cmd.Dir = dir, capturing the
// combined output. Returns (output, exitCode, error). exitCode is 0 on
// success, non-zero on command failure, and -1 when the process could not
// be started.
func execShellCommand(ctx context.Context, dir, cmd string) (string, int, error) {
	c := exec.CommandContext(ctx, "bash", "-c", cmd) //nolint:gosec // cmd is loaded from verify.yaml whose Validate() rejects shell meta-characters; dir is a controlled application directory
	c.Dir = dir
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	output := buf.String()
	if err == nil {
		return output, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return output, exitErr.ExitCode(), nil
	}
	return output, -1, err
}

// tailBytes returns the last n bytes of s. When s is shorter than n the full
// string is returned. The function never returns mid-rune-broken output.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	// Trim partial UTF-8 prefix so log readers do not see a replacement char
	// at the head of the tail. RuneStart bytes have the form 0xxxxxxx or
	// 11xxxxxx; we advance past any 10xxxxxx continuation bytes.
	for i := 0; i < 4 && i < len(cut); i++ {
		if cut[0]&0xC0 != 0x80 {
			break
		}
		cut = cut[1:]
	}
	return cut
}
