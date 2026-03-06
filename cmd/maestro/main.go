package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/model"
)

const version = "2.0.0"

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

func (e *CLIError) ExitCode() int {
	if e.Code == 0 {
		return 1
	}
	return e.Code
}

// stringSliceFlag is a package-level alias for [model.StringSlice],
// kept for brevity in flag declarations within the CLI commands.
type stringSliceFlag = model.StringSlice

// newFlagSet creates a flag.FlagSet that suppresses default output (errors are handled by callers).
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// modeSetter implements flag.Value as a boolean flag that sets a shared string variable.
// Used for shorthand mode flags (e.g., --interrupt sets mode to "interrupt").
// Since flag.FlagSet processes args left to right, argv-order precedence is preserved.
type modeSetter struct {
	target *string
	val    string
}

func (m *modeSetter) String() string  { return "" }
func (m *modeSetter) Set(string) error { *m.target = m.val; return nil }
func (m *modeSetter) IsBoolFlag() bool { return true }

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 2 {
		printUsage()
		return 1
	}

	var err error
	switch os.Args[1] {
	case "daemon":
		err = runDaemon(os.Args[2:])
	case "setup":
		err = runSetup(os.Args[2:])
	case "up":
		err = runUp(os.Args[2:])
	case "down":
		err = runDown(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "queue":
		err = runQueue(os.Args[2:])
	case "result":
		err = runResult(os.Args[2:])
	case "task":
		err = runTask(os.Args[2:])
	case "plan":
		err = runPlan(os.Args[2:])
	case "agent":
		err = runAgent(os.Args[2:])
	case "worker":
		err = runWorker(os.Args[2:])
	case "dashboard":
		err = runDashboard(os.Args[2:])
	case "version":
		fmt.Printf("maestro %s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "maestro: unknown command: %s\n\n", os.Args[1])
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
  plan complete [options]          Report command completion
  plan add-retry-task [options]    Replace failed task
  plan request-cancel [options]    Request cancellation
  plan rebuild [options]           Rebuild state from results

Internal:
  daemon            Run daemon process
  agent launch      Launch agent in tmux pane
  agent exec        Send message to agent
  task heartbeat    Send heartbeat for an active task

Utilities:
  worker standby    Show idle workers
  dashboard         Regenerate dashboard.md
  version           Show version
  help              Show this help

`, version)
}
