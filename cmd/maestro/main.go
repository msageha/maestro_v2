package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/model"
)

const version = "2.0.0"
const maestroDirEnv = "MAESTRO_DIR"

// ExitCodeRetryable is the exit code used when the CLI operation can be retried.
// This includes daemon-side rejections such as FENCING_REJECT, BACKPRESSURE,
// MAX_RUNTIME_EXCEEDED, and retryable agent exec errors.
const ExitCodeRetryable = 2

// ExitCodeSubmitUncertain is returned by `maestro agent exec` when the
// underlying tmux submit-confirmation probe exhausts its budget without
// observing either an activity marker or a content-growth signal. The
// message has already been pasted + Enter-submitted to the pane; the
// daemon-side dispatcher treats this same condition as non-retryable
// (queue_dispatch.go) to avoid the double-submit risk that re-delivering
// the envelope would create. Surfacing it as a distinct CLI exit code
// (instead of the generic `1`) lets scripts and humans tell "delivery
// likely succeeded but unverified" apart from "delivery actually failed",
// which is the UX problem reported when the orchestrator processed an
// `--with-clear` exec that looked failed at the shell level.
//
// Re-running the same `agent exec` after a `3` is the wrong recovery —
// it can put two copies of the prompt in the agent. Inspect the agent
// pane (or the daemon log if dispatched via queue) before deciding.
const ExitCodeSubmitUncertain = 3

// Fencing exit codes (F-019 step 2).
//
// These let the Worker shell wrapper (templates/instructions/worker.md) and
// any other downstream consumer branch on `$?` instead of grepping the
// stderr Message string. The codes are stable across heartbeat / result
// write / future fencing-aware subcommands so a single dispatch table at
// the call site covers every entry point.
//
// Range 10–19 was chosen to avoid collisions with:
//   - 0   success
//   - 1   generic CLI error
//   - 2   ExitCodeRetryable (legacy alias kept for backwards compat)
//   - 3   ExitCodeSubmitUncertain (agent exec submit-confirm exhausted)
//   - 64-78  sysexits.h convention
//   - 126/127 shell "command invoked / not found" semantics
//   - 128+N  signal-terminated processes
//
// Worker shell wrappers MUST read `$?` directly after the maestro call —
// piping into another command (e.g. `maestro task heartbeat … | tee log`)
// reports the pipe's exit code, which would clobber these.
const (
	// ExitCodeFencingEpoch (10) — heartbeat / result_write rejected because
	// the request's lease_epoch differs from the queue's. The task has
	// been reassigned to a newer lease and the calling Worker must end its
	// current turn without retrying.
	ExitCodeFencingEpoch = 10
	// ExitCodeMaxRuntimeExceeded (11) — heartbeat rejected because the
	// task has been in_progress longer than the configured cap. The task
	// is being torn down server-side; the Worker should also end its turn.
	ExitCodeMaxRuntimeExceeded = 11
	// ExitCodeFencingStatus (12) — fencing rejected because the task is
	// no longer in_progress (already completed / cancelled / dead-letter
	// from another path). Same shape as fencing_epoch from the Worker's
	// perspective: stop the turn.
	ExitCodeFencingStatus = 12
)

// CLIError represents an error with a specific exit code.
type CLIError struct {
	Code   int
	Msg    string
	Silent bool // true suppresses stderr output in the main handler
}

func (e *CLIError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return "cli error"
}

// ExitCode returns the process exit code for this error.
// A CLIError with Code==0 is treated as a programming error (errors should
// not be created for success cases), so it falls back to exit code 1.
func (e *CLIError) ExitCode() int {
	if e.Code == 0 {
		return 1
	}
	return e.Code
}

// stringSliceFlag is a package-level alias for [model.StringSlice],
// kept for brevity in flag declarations within the CLI commands.
type stringSliceFlag = model.StringSlice

// modeSetter implements flag.Value as a boolean flag that sets a shared string variable.
// Used for shorthand mode flags (e.g., --interrupt sets mode to "interrupt").
// Since flag.FlagSet processes args left to right, argv-order precedence is preserved.
type modeSetter struct {
	target *string
	val    string
}

func (m *modeSetter) String() string   { return "" }
func (m *modeSetter) Set(string) error { *m.target = m.val; return nil }
func (m *modeSetter) IsBoolFlag() bool { return true }

func main() {
	normalizeProcessEnvironment()
	os.Exit(newCLIApp().run(os.Args[1:]))
}

// normalizeProcessEnvironment promotes a missing or `dumb` TERM to a real
// terminfo entry so subshells that maestro (CLI or daemon) spawns do not
// inherit a degraded prompt environment from the parent. The 2026-04-28
// retest4 reported `[ERROR] - (starship::print): Under a 'dumb' terminal
// (TERM=dumb).` mixing into CLI output every invocation — a noise source
// the operator could not silence from outside maestro because
// claude-code propagates TERM=dumb to the maestro process. Setting the
// var here is process-local: the operator's interactive shell is
// untouched, and any explicit TERM the operator exports still wins.
//
// Mirrors the agent-pane normalisation in
// internal/agent/launcher.go:buildLaunchEnvForAgent so the rule is
// consistent across the CLI, daemon, and tmux-launched agent panes.
func normalizeProcessEnvironment() {
	if t := os.Getenv("TERM"); t == "" || t == "dumb" {
		_ = os.Setenv("TERM", "xterm-256color")
	}
}

func (a *cliApp) run(args []string) int {
	if len(args) < 1 {
		printUsage()
		return 1
	}

	var err error
	switch args[0] {
	case "daemon":
		err = runDaemon(args[1:])
	case "setup":
		err = runSetup(args[1:])
	case "up":
		err = runUp(args[1:])
	case "down":
		err = runDown(args[1:])
	case "status":
		err = runStatus(args[1:])
	case "queue":
		err = a.runQueue(args[1:])
	case "result":
		err = a.runResult(args[1:])
	case "task":
		err = a.runTask(args[1:])
	case "plan":
		err = a.runPlan(args[1:])
	case "agent":
		err = runAgent(args[1:])
	case "worker":
		err = runWorker(args[1:])
	case "skill":
		err = a.runSkill(args[1:])
	case "verify":
		err = a.runVerify(args[1:])
	case "dashboard":
		err = a.runDashboard(args[1:])
	case "version":
		fmt.Printf("maestro %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "maestro: unknown command: %s\n\n", args[0])
		printUsage()
		return 1
	}

	if err != nil {
		var ce *CLIError
		if errors.As(err, &ce) {
			if !ce.Silent {
				fmt.Fprintln(os.Stderr, ce.Msg)
			}
			return ce.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// findMaestroDir searches for .maestro/ in the current directory and ancestors.
// Returns ("", nil) if not found, or ("", error) if the working directory cannot be determined.
func findMaestroDir() (string, error) {
	if envDir := os.Getenv(maestroDirEnv); envDir != "" {
		dir, err := filepath.Abs(envDir)
		if err != nil {
			return "", fmt.Errorf("%s: resolve %q: %w", maestroDirEnv, envDir, err)
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		// dir originates from MAESTRO_DIR env var, abs-resolved and
		// symlink-resolved above. The Stat is pure metadata access and
		// cannot itself traverse outside the resolved path. gosec G703
		// is a false positive here.
		info, err := os.Stat(dir) //nolint:gosec // dir already resolved
		if err != nil {
			return "", fmt.Errorf("%s: stat %q: %w", maestroDirEnv, dir, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s: %q is not a directory", maestroDirEnv, dir)
		}
		return dir, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		candidate := filepath.Join(dir, ".maestro")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// requireMaestroDir finds the .maestro directory or returns a user-facing error
// prefixed with the command name.
func requireMaestroDir(cmd string) (string, error) {
	dir, err := findMaestroDir()
	if err != nil {
		return "", fmt.Errorf("maestro %s: %w", cmd, err)
	}
	if dir == "" {
		return "", &CLIError{Code: 1, Msg: fmt.Sprintf("maestro %s: .maestro/ directory not found. Run 'maestro setup <dir>' first.", cmd)}
	}
	return dir, nil
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `maestro %s — Multi-agent orchestration system

Usage: maestro <command> [options]

Formation:
  setup <dir> [name]  Initialize .maestro/ directory
  up [flags]        Start formation and attach (--detach|-d to skip)
  down              Graceful shutdown
  status [--json]   Show formation status

Agent Commands (CLI → Daemon):
  queue write <target> [options]   Write to queue
  result write <reporter> [options] Write result
  plan submit [options]            Submit task plan
  plan complete [options]          Report command completion (--summary <text> | --summary-file <path>)
  plan add-retry-task [options]    Replace failed task
  plan request-cancel [options]    Request cancellation
  plan rebuild [options]           Rebuild state from results
  plan recover [options]           Auto-select worktree recovery action
  plan resolve-conflict [options]  Clear a publish-blocking commit_failed worker (NOT for phase merge_conflict — for those, use plan add-task)
  verify write [options]           Write command-scoped verify config

Internal:
  daemon            Run daemon process
  agent launch      Launch agent in tmux pane
  agent exec        Send message to agent
  task heartbeat    Send heartbeat for an active task

Skill Management:
  skill list                  List registered skills
  skill candidates [--status] List skill candidates
  skill approve <id> [--name] Approve a skill candidate
  skill reject <id>           Reject a skill candidate

Utilities:
  worker standby     Show idle workers
  dashboard          Regenerate dashboard.md
  version            Show version
  help               Show this help

Examples:
  # Clear a worker from commit_failed_workers after manually fixing publish-blocking state.
  # NOT the right command for an in-phase merge_conflict — use 'plan add-task --worker-id <id>'
  # to dispatch a conflict resolution task; the daemon then auto-runs resume_merge.
  maestro plan resolve-conflict --command-id cmd_42 --phase-id ph_3 \
      --worker-id worker2 --conflicting-files internal/a.go,internal/b.go

`, version)
}
