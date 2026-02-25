// Package tmux provides helpers for managing tmux sessions and panes.
package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// bufSeq generates unique buffer names to prevent race conditions when
// multiple goroutines call SendTextAndSubmit concurrently.
var bufSeq atomic.Int64

// SendTextAndSubmit sends multi-line text to a pane using paste-buffer for
// reliable delivery, then sends Enter to submit. This avoids character-by-character
// key sending issues with newlines in the message.
func SendTextAndSubmit(paneTarget, text string) error {
	bufName := fmt.Sprintf("maestro-msg-%d", bufSeq.Add(1))

	// Load text into tmux buffer via stdin (handles arbitrary content safely)
	cmd := exec.Command("tmux", "load-buffer", "-b", bufName, "-")
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux load-buffer: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// Paste buffer content into the pane via bracketed paste.
	// -p forces bracketed paste so the app receives the entire text as a single paste unit.
	// -r prevents tmux from converting LF to CR inside the paste (avoids spurious submits).
	// -d deletes the buffer after pasting to avoid leaking tmux buffers.
	if err := run("paste-buffer", "-pr", "-b", bufName, "-d", "-t", paneTarget); err != nil {
		return err
	}

	// Delay to let the target application finish processing the bracketed
	// paste before we send Enter to submit. Claude Code's Ink-based TUI
	// needs sufficient time to render the pasted content into its input field;
	// 100ms is too short under load and causes intermittent delivery failures.
	time.Sleep(500 * time.Millisecond)

	return SendKeys(paneTarget, "Enter")
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
	err := exec.Command("tmux", "has-session", "-t", SessionName).Run()
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

// CapturePane captures pane content with the -J flag, which joins wrapped
// lines to produce stable output regardless of terminal width.
// lastN specifies how many lines from the bottom to capture (0 = entire visible pane).
func CapturePane(paneTarget string, lastN int) (string, error) {
	args := []string{"capture-pane", "-p", "-J", "-t", paneTarget}
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

func run(args ...string) error {
	cmd := exec.Command("tmux", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func output(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
