package tmux

import (
	"fmt"
	"os/exec"
	"strings"
)

const SessionName = "maestro"

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

// CapturePane captures pane content. lastN specifies how many lines from the
// bottom to capture (0 = entire visible pane).
func CapturePane(paneTarget string, lastN int) (string, error) {
	args := []string{"capture-pane", "-t", paneTarget, "-p"}
	if lastN > 0 {
		args = append(args, "-S", fmt.Sprintf("-%d", lastN))
	}
	return output(args...)
}

// SendKeys sends keystrokes to a pane.
func SendKeys(paneTarget string, keys ...string) error {
	args := []string{"send-keys", "-t", paneTarget}
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
func SetupWorkerGrid(windowTarget string, workerCount int) ([]string, error) {
	if workerCount < 1 || workerCount > 8 {
		return nil, fmt.Errorf("worker count must be 1-8, got %d", workerCount)
	}

	const paneFormat = "#{session_name}:#{window_index}.#{pane_index}"

	if workerCount == 1 {
		return ListPanes(windowTarget, paneFormat)
	}

	// Calculate 2-column grid: left column gets ceil(count/2), right gets floor(count/2).
	leftRows := (workerCount + 1) / 2
	rightRows := workerCount / 2

	// Step 1: Horizontal split → 2 columns
	if err := SplitPane(windowTarget, true); err != nil {
		return nil, fmt.Errorf("horizontal split: %w", err)
	}

	// Use pane IDs (%N) for stable targeting during subsequent splits.
	ids, err := ListPanes(windowTarget, "#{pane_id}")
	if err != nil {
		return nil, fmt.Errorf("list pane ids: %w", err)
	}
	leftID := strings.TrimSpace(ids[0])
	rightID := strings.TrimSpace(ids[1])

	// Step 2: Split left column vertically into rows
	for i := 1; i < leftRows; i++ {
		if err := SplitPane(leftID, false); err != nil {
			return nil, fmt.Errorf("split left row %d: %w", i, err)
		}
	}

	// Step 3: Split right column vertically into rows
	for i := 1; i < rightRows; i++ {
		if err := SplitPane(rightID, false); err != nil {
			return nil, fmt.Errorf("split right row %d: %w", i, err)
		}
	}

	return ListPanes(windowTarget, paneFormat)
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
