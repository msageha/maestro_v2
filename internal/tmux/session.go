// Package tmux provides helpers for managing tmux sessions and panes.
package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// defaultCmdTimeout is the timeout for individual tmux commands.
// tmux IPC is normally sub-millisecond; 5s catches hung servers
// while avoiding false positives on slower systems.
const defaultCmdTimeout = 5 * time.Second

// TmuxErrorKind categorizes tmux command failures.
type TmuxErrorKind int

const (
	ErrKindServer  TmuxErrorKind = iota + 1 // tmux server unreachable
	ErrKindSession                           // session not found
	ErrKindPane                              // pane or window not found
	ErrKindTimeout                           // command timed out
	ErrKindCommand                           // other command error
)

func (k TmuxErrorKind) String() string {
	switch k {
	case ErrKindServer:
		return "server"
	case ErrKindSession:
		return "session"
	case ErrKindPane:
		return "pane"
	case ErrKindTimeout:
		return "timeout"
	case ErrKindCommand:
		return "command"
	default:
		return "unknown"
	}
}

// TmuxError is a classified tmux command error.
type TmuxError struct {
	Kind   TmuxErrorKind
	Op     string // tmux subcommand (e.g., "send-keys")
	Stderr string // raw stderr from tmux
	Err    error  // underlying error
}

func (e *TmuxError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("tmux %s (%s): %v: %s", e.Op, e.Kind, e.Err, e.Stderr)
	}
	return fmt.Sprintf("tmux %s (%s): %v", e.Op, e.Kind, e.Err)
}

func (e *TmuxError) Unwrap() error { return e.Err }

// Is supports errors.Is() matching by Kind.
func (e *TmuxError) Is(target error) bool {
	t, ok := target.(*TmuxError)
	if !ok {
		return false
	}
	return e.Kind == t.Kind
}

// Sentinel errors for use with errors.Is().
var (
	ErrTmuxServer  = &TmuxError{Kind: ErrKindServer}
	ErrTmuxSession = &TmuxError{Kind: ErrKindSession}
	ErrTmuxPane    = &TmuxError{Kind: ErrKindPane}
	ErrTmuxTimeout = &TmuxError{Kind: ErrKindTimeout}
	ErrTmuxCommand = &TmuxError{Kind: ErrKindCommand}
)

// classifyTmuxError parses tmux stderr to determine the error category.
func classifyTmuxError(op, stderr string, err error) *TmuxError {
	lower := strings.ToLower(stderr)

	var kind TmuxErrorKind
	switch {
	case strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "server exited") ||
		strings.Contains(lower, "error connecting") ||
		strings.Contains(lower, "connect failed"):
		kind = ErrKindServer
	case strings.Contains(lower, "session not found") ||
		strings.Contains(lower, "can't find session") ||
		strings.Contains(lower, "no such session") ||
		strings.Contains(lower, "no sessions"):
		kind = ErrKindSession
	case strings.Contains(lower, "can't find pane") ||
		strings.Contains(lower, "no such pane") ||
		strings.Contains(lower, "can't find window") ||
		strings.Contains(lower, "no such window") ||
		strings.Contains(lower, "pane has exited"):
		kind = ErrKindPane
	default:
		kind = ErrKindCommand
	}

	return &TmuxError{
		Kind:   kind,
		Op:     op,
		Stderr: stderr,
		Err:    err,
	}
}

// bufSeq generates unique buffer names to prevent race conditions when
// multiple goroutines call SendTextAndSubmit concurrently.
var bufSeq atomic.Int64

// SendTextAndSubmit sends multi-line text to a pane using paste-buffer for
// reliable delivery, then sends Enter to submit. This avoids character-by-character
// key sending issues with newlines in the message.
func SendTextAndSubmit(ctx context.Context, paneTarget, text string) error {
	bufName := fmt.Sprintf("maestro-msg-%d", bufSeq.Add(1))

	// Load text into tmux buffer via stdin (handles arbitrary content safely)
	loadCtx, loadCancel := context.WithTimeout(ctx, defaultCmdTimeout)
	defer loadCancel()
	cmd := exec.CommandContext(loadCtx, "tmux", "load-buffer", "-b", bufName, "-")
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		stderr := strings.TrimSpace(string(out))
		if loadCtx.Err() != nil {
			return &TmuxError{Kind: ErrKindTimeout, Op: "load-buffer", Stderr: stderr, Err: loadCtx.Err()}
		}
		return classifyTmuxError("load-buffer", stderr, err)
	}

	// Guard: ensure the buffer is deleted even if paste-buffer fails.
	// On success, paste-buffer -d deletes it atomically; this defer
	// only fires on the error path to prevent buffer leaks.
	needCleanup := true
	defer func() {
		if needCleanup {
			_ = run("delete-buffer", "-b", bufName)
		}
	}()

	// Paste buffer content into the pane via bracketed paste.
	// -p forces bracketed paste so the app receives the entire text as a single paste unit.
	// -r prevents tmux from converting LF to CR inside the paste (avoids spurious submits).
	// -d deletes the buffer after pasting to avoid leaking tmux buffers.
	if err := runCtx(ctx, "paste-buffer", "-pr", "-b", bufName, "-d", "-t", paneTarget); err != nil {
		return err
	}
	needCleanup = false

	// Delay to let the target application finish processing the bracketed
	// paste before we send Enter to submit. Claude Code's Ink-based TUI
	// needs sufficient time to render the pasted content into its input field;
	// 100ms is too short under load and causes intermittent delivery failures.
	// Uses context-aware sleep so cancellation is respected.
	if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
		return &TmuxError{Kind: ErrKindTimeout, Op: "send-text-submit-sleep", Err: err}
	}

	return SendKeys(paneTarget, "Enter")
}

// sleepCtx sleeps for d or returns early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SessionName is the tmux session name. Set via SetSessionName before use.
var SessionName = "maestro"

// unsafeSessionChars matches characters that are unsafe in tmux session names.
// tmux uses `:` and `.` for target resolution, so these must be sanitized.
var unsafeSessionChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// SetSessionName updates the tmux session name, sanitizing unsafe characters.
func SetSessionName(name string) {
	sanitized := unsafeSessionChars.ReplaceAllString(name, "_")
	if sanitized == "" {
		sanitized = "maestro"
	}
	SessionName = sanitized
}

// SessionExists checks whether the maestro tmux session exists.
func SessionExists() bool {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCmdTimeout)
	defer cancel()
	err := exec.CommandContext(ctx, "tmux", "has-session", "-t", SessionName).Run()
	return err == nil
}

// CreateSession creates a new maestro tmux session.
// The first window is named windowName. Returns an error if the session already exists.
func CreateSession(windowName string) error {
	return run("new-session", "-d", "-s", SessionName, "-n", windowName)
}

// KillSession destroys the maestro tmux session.
func KillSession() error {
	return run("kill-session", "-t", SessionName)
}

// CreateWindow creates a new window in the maestro session.
func CreateWindow(name string) error {
	return run("new-window", "-t", SessionName, "-n", name)
}

// SplitPane splits the current pane in the given window horizontally or vertically.
// horizontal=true splits left-right (-h), false splits top-bottom (-v).
func SplitPane(windowTarget string, horizontal bool) error {
	flag := "-v"
	if horizontal {
		flag = "-h"
	}
	return run("split-window", flag, "-t", windowTarget)
}

// SelectLayout applies a tmux layout to a window.
func SelectLayout(windowTarget, layout string) error {
	return run("select-layout", "-t", windowTarget, layout)
}

// SetUserVar sets a tmux user variable on a pane (pane-scoped via -p).
// Format: tmux set-option -p -t <pane> @<name> <value>
func SetUserVar(paneTarget, name, value string) error {
	return run("set-option", "-p", "-t", paneTarget, "@"+name, value)
}

// GetUserVar reads a tmux user variable from a pane.
func GetUserVar(paneTarget, name string) (string, error) {
	out, err := output("display-message", "-t", paneTarget, "-p", "#{@"+name+"}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// CapturePane captures pane content without the -J flag, preserving the
// visual line structure. Each wrapped line appears as a separate line in the
// output. Use this for prompt detection and pattern matching where line
// boundaries matter. For hash-based comparisons, use CapturePaneJoined instead.
// lastN specifies how many lines from the bottom to capture (0 = entire visible pane).
func CapturePane(paneTarget string, lastN int) (string, error) {
	args := []string{"capture-pane", "-p", "-t", paneTarget}
	if lastN > 0 {
		args = append(args, "-S", fmt.Sprintf("-%d", lastN))
	}
	return output(args...)
}

// CapturePaneJoined captures pane content with the -J flag, which joins
// wrapped lines and preserves trailing spaces. This produces stable output
// regardless of terminal width, making it suitable for hash-based comparisons.
// lastN specifies how many lines from the bottom to capture (0 = entire visible pane).
func CapturePaneJoined(paneTarget string, lastN int) (string, error) {
	args := []string{"capture-pane", "-t", paneTarget, "-pJ"}
	if lastN > 0 {
		args = append(args, "-S", fmt.Sprintf("-%d", lastN))
	}
	return output(args...)
}

// SendKeys sends keystrokes to a pane.
func SendKeys(paneTarget string, keys ...string) error {
	args := make([]string, 0, 3+len(keys))
	args = append(args, "send-keys", "-t", paneTarget)
	args = append(args, keys...)
	return run(args...)
}

// ListPanes returns pane IDs for a window, formatted by the given format string.
func ListPanes(windowTarget, format string) ([]string, error) {
	out, err := output("list-panes", "-t", windowTarget, "-F", format)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// ListAllPanes returns pane info across all windows in the session.
func ListAllPanes(format string) ([]string, error) {
	out, err := output("list-panes", "-s", "-t", SessionName, "-F", format)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// FindPaneByAgentID finds the pane target (session:window.pane) for a given agent_id.
func FindPaneByAgentID(agentID string) (string, error) {
	lines, err := ListAllPanes("#{session_name}:#{window_index}.#{pane_index}\t#{@agent_id}")
	if err != nil {
		return "", fmt.Errorf("list panes: %w", err)
	}
	for _, line := range lines {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 && parts[1] == agentID {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("agent %q not found in tmux session", agentID)
}

// SendCommand sends a command string to a pane (text + Enter).
func SendCommand(paneTarget, command string) error {
	return SendKeys(paneTarget, command, "Enter")
}

// SendCtrlC sends Ctrl+C to a pane.
func SendCtrlC(paneTarget string) error {
	return SendKeys(paneTarget, "", "C-c")
}

// SetupWorkerGrid creates worker panes in a 2-column × N-row grid layout.
// The window must already exist with a single pane.
//
// Panes are created rows-first so that tmux assigns pane indices in row-major
// order (left-to-right, top-to-bottom): worker1=top-left, worker2=top-right,
// worker3=bottom-left, worker4=bottom-right, etc.
func SetupWorkerGrid(windowTarget string, workerCount int) ([]string, error) {
	if workerCount < 1 || workerCount > 8 {
		return nil, fmt.Errorf("worker count must be 1-8, got %d", workerCount)
	}

	const paneFormat = "#{session_name}:#{window_index}.#{pane_index}"

	if workerCount == 1 {
		return ListPanes(windowTarget, paneFormat)
	}

	totalRows := (workerCount + 1) / 2
	fullRows := workerCount / 2 // rows that get 2 columns

	// Step 1: Create rows by vertical-splitting the first pane.
	// Always split the first pane (top) so rows appear in top-to-bottom
	// order in the layout tree.
	ids, err := ListPanes(windowTarget, "#{pane_id}")
	if err != nil {
		return nil, fmt.Errorf("list pane ids: %w", err)
	}
	firstPaneID := strings.TrimSpace(ids[0])

	for i := 1; i < totalRows; i++ {
		if err := SplitPane(firstPaneID, false); err != nil {
			return nil, fmt.Errorf("split row %d: %w", i, err)
		}
	}

	// Get pane IDs for each row (layout tree order: top to bottom).
	rowIDs, err := ListPanes(windowTarget, "#{pane_id}")
	if err != nil {
		return nil, fmt.Errorf("list row pane ids: %w", err)
	}

	// Step 2: Split each full row horizontally to create 2 columns.
	// The last row is left unsplit when workerCount is odd.
	for i := 0; i < fullRows; i++ {
		if err := SplitPane(strings.TrimSpace(rowIDs[i]), true); err != nil {
			return nil, fmt.Errorf("split row %d cols: %w", i, err)
		}
	}

	// The layout tree is now row-major: V(H(TL,TR), H(BL,BR), ...).
	// tmux assigns pane indices by depth-first traversal, giving row-major order
	// automatically. No reordering needed.
	return ListPanes(windowTarget, paneFormat)
}

// SetSessionOption sets a session-level tmux option on the maestro session.
func SetSessionOption(name, value string) error {
	return run("set-option", "-t", SessionName, name, value)
}

// SetWindowOption sets a window-level tmux option for a specific window.
// windowTarget should be in the form "session:window" (e.g., "maestro:0").
// This only affects the specified window, avoiding side effects on other sessions or windows.
func SetWindowOption(windowTarget, name, value string) error {
	return run("set-option", "-w", "-t", windowTarget, name, value)
}

// SelectWindow selects (focuses) a window in the maestro session.
func SelectWindow(windowTarget string) error {
	return run("select-window", "-t", windowTarget)
}

// AttachSession attaches the current terminal to the maestro tmux session.
// This replaces the current process with tmux attach-session.
func AttachSession() error {
	cmd := exec.Command("tmux", "attach-session", "-t", SessionName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ShellCommands is the canonical set of known shell command names.
// Used to detect whether a tmux pane is running a plain shell (idle)
// rather than an application like the agent CLI.
var ShellCommands = map[string]bool{
	"bash": true, "zsh": true, "fish": true,
	"sh": true, "dash": true, "tcsh": true, "csh": true,
}

// IsShellCommand reports whether cmd is a known shell command name.
func IsShellCommand(cmd string) bool {
	return ShellCommands[cmd]
}

// GetPaneCurrentCommand returns the currently running command in a pane.
func GetPaneCurrentCommand(paneTarget string) (string, error) {
	out, err := output("display-message", "-t", paneTarget, "-p", "#{pane_current_command}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// GetPanePID returns the process ID of the pane's shell process.
func GetPanePID(paneTarget string) (string, error) {
	out, err := output("display-message", "-t", paneTarget, "-p", "#{pane_pid}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// runCtx executes a tmux command with context support and error classification.
func runCtx(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, defaultCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		stderr := strings.TrimSpace(string(out))
		if ctx.Err() != nil {
			return &TmuxError{Kind: ErrKindTimeout, Op: args[0], Stderr: stderr, Err: ctx.Err()}
		}
		return classifyTmuxError(args[0], stderr, err)
	}
	return nil
}

// outputCtx executes a tmux command that returns output, with context support and error classification.
func outputCtx(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		stderr := strings.TrimSpace(string(out))
		if ctx.Err() != nil {
			return "", &TmuxError{Kind: ErrKindTimeout, Op: args[0], Stderr: stderr, Err: ctx.Err()}
		}
		return "", classifyTmuxError(args[0], stderr, err)
	}
	return string(out), nil
}

// run executes a tmux command with the default timeout.
func run(args ...string) error {
	return runCtx(context.Background(), args...)
}

// output executes a tmux command that returns output, with the default timeout.
func output(args ...string) (string, error) {
	return outputCtx(context.Background(), args...)
}
