package agent

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/msageha/maestro_v2/internal/tmux"
)

// PaneIO abstracts tmux pane operations for testability.
type PaneIO interface {
	FindPaneByAgentID(agentID string) (string, error)
	SendCtrlC(paneTarget string) error
	SendKeys(paneTarget string, keys ...string) error
	SendCommand(paneTarget, command string) error
	SendTextAndSubmit(ctx context.Context, paneTarget, text string) error
	SetUserVar(paneTarget, name, value string) error
	GetUserVar(paneTarget, name string) (string, error)
	GetPanePID(paneTarget string) (string, error)
	GetPaneCurrentCommand(paneTarget string) (string, error)
	GetPaneCurrentPath(paneTarget string) (string, error)
	CapturePane(paneTarget string, lastN int) (string, error)
	CapturePaneJoined(paneTarget string, lastN int) (string, error)
	IsShellCommand(cmd string) bool
	RespawnPane(paneTarget, startDir string) error
}

// TmuxPaneIO implements PaneIO using the real tmux package.
type TmuxPaneIO struct{}

// NewTmuxPaneIO creates a new TmuxPaneIO instance.
func NewTmuxPaneIO() *TmuxPaneIO {
	return &TmuxPaneIO{}
}

// FindPaneByAgentID delegates to tmux.FindPaneByAgentID.
func (t *TmuxPaneIO) FindPaneByAgentID(agentID string) (string, error) {
	return tmux.FindPaneByAgentID(agentID)
}

// SendCtrlC delegates to tmux.SendCtrlC.
func (t *TmuxPaneIO) SendCtrlC(paneTarget string) error {
	return tmux.SendCtrlC(paneTarget)
}

// SendKeys delegates to tmux.SendKeys.
func (t *TmuxPaneIO) SendKeys(paneTarget string, keys ...string) error {
	return tmux.SendKeys(paneTarget, keys...)
}

// SendCommand delegates to tmux.SendCommand.
func (t *TmuxPaneIO) SendCommand(paneTarget, command string) error {
	return tmux.SendCommand(paneTarget, command)
}

// SendTextAndSubmit delegates to tmux.SendTextAndSubmit.
func (t *TmuxPaneIO) SendTextAndSubmit(ctx context.Context, paneTarget, text string) error {
	return tmux.SendTextAndSubmit(ctx, paneTarget, text)
}

// SetUserVar delegates to tmux.SetUserVar.
func (t *TmuxPaneIO) SetUserVar(paneTarget, name, value string) error {
	return tmux.SetUserVar(paneTarget, name, value)
}

// GetUserVar delegates to tmux.GetUserVar.
func (t *TmuxPaneIO) GetUserVar(paneTarget, name string) (string, error) {
	return tmux.GetUserVar(paneTarget, name)
}

// GetPanePID delegates to tmux.GetPanePID.
func (t *TmuxPaneIO) GetPanePID(paneTarget string) (string, error) {
	return tmux.GetPanePID(paneTarget)
}

// GetPaneCurrentCommand delegates to tmux.GetPaneCurrentCommand.
func (t *TmuxPaneIO) GetPaneCurrentCommand(paneTarget string) (string, error) {
	return tmux.GetPaneCurrentCommand(paneTarget)
}

// GetPaneCurrentPath delegates to tmux.GetPaneCurrentPath.
func (t *TmuxPaneIO) GetPaneCurrentPath(paneTarget string) (string, error) {
	return tmux.GetPaneCurrentPath(paneTarget)
}

// CapturePane delegates to tmux.CapturePane.
func (t *TmuxPaneIO) CapturePane(paneTarget string, lastN int) (string, error) {
	return tmux.CapturePane(paneTarget, lastN)
}

// CapturePaneJoined delegates to tmux.CapturePaneJoined.
func (t *TmuxPaneIO) CapturePaneJoined(paneTarget string, lastN int) (string, error) {
	return tmux.CapturePaneJoined(paneTarget, lastN)
}

// IsShellCommand delegates to tmux.IsShellCommand.
func (t *TmuxPaneIO) IsShellCommand(cmd string) bool {
	return tmux.IsShellCommand(cmd)
}

// RespawnPane delegates to tmux.RespawnPane.
func (t *TmuxPaneIO) RespawnPane(paneTarget, startDir string) error {
	return tmux.RespawnPane(paneTarget, startDir)
}

// --- Pane Content Analysis Helpers ---

// maxPromptSearchLines limits how many non-blank lines (from the bottom)
// are checked for the ❯ prompt character.
const maxPromptSearchLines = 6

// lastLineMaxDisplay is the maximum display length for lastNonBlankLine output.
const lastLineMaxDisplay = 80

// ansiEscape matches ANSI escape sequences including CSI (with private params),
// OSC, and charset designators.
var ansiEscape = regexp.MustCompile(`\x1b(?:\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]|\][^\x07]*(?:\x07|\x1b\\)|\([B0UK]|[>=])`)

// stripANSI removes ANSI escape sequences from s.
func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

// isPromptReady checks whether the pane content indicates Claude Code is at its input prompt.
// It inspects the last non-blank line of the captured pane output.
//
// Primary check: the line contains '❯' (U+276F HEAVY RIGHT-POINTING ANGLE QUOTATION MARK
// ORNAMENT), which is the character Claude Code uses in its input prompt.
// Fallback check: the line is exactly '>' (ASCII 0x3E, after trimming whitespace).
// This covers older Claude Code versions or terminal environments where the Unicode
// character is not rendered. The match is strict (bare '>' only) to avoid false
// positives from markdown blockquotes or log output.
func isPromptReady(content string) bool {
	lines := strings.Split(content, "\n")
	// Scan bottom-up, checking up to maxPromptSearchLines non-blank lines for ❯.
	// Claude Code's TUI may show a status bar below the prompt,
	// so the prompt line is not necessarily the last non-blank line.
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < maxPromptSearchLines; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "❯") {
			return true
		}
		checked++
	}
	// Fallback: check only the last non-blank line for a bare '>'.
	// Requires the trimmed line to be exactly ">" to avoid false positives
	// from markdown blockquotes ("> quoted text") or log output containing '>'.
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		return trimmed == ">"
	}
	return false
}

// promptInputText extracts the user-visible text that follows the Claude Code
// prompt glyph (❯) on the prompt line, scanning bottom-up like isPromptReady.
// Returns found=false when no prompt line is visible within the search window.
// Box-drawing border characters and surrounding whitespace are stripped, so an
// empty input box yields ("", true). Used by the orchestrator user-composing
// guard to tell "prompt idle and empty" apart from "prompt idle but a human is
// mid-draft".
func promptInputText(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < maxPromptSearchLines; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		if idx := strings.Index(trimmed, "❯"); idx >= 0 {
			text := trimmed[idx+len("❯"):]
			return strings.Trim(text, "│ \t"), true
		}
		checked++
	}
	return "", false
}

// clearTextVisible checks whether "/clear" text is visible near the bottom of the pane content
// as a command (not as part of agent output prose).
// This is the primary signal for detecting that /clear was NOT processed as a command
// and instead remains as literal text in the input field.
//
// Matches:
//   - Bare "/clear" on a line by itself (command typed without prompt marker captured)
//   - "/clear" on a prompt line (line containing ❯ or starting with ">")
//
// Does NOT match "/clear" embedded in longer text like "Running /clear command..."
func clearTextVisible(content string) bool {
	lines := strings.Split(content, "\n")
	// Check the last maxPromptSearchLines non-blank lines (covers input line + a few status lines)
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < maxPromptSearchLines; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		if trimmed == "/clear" {
			return true
		}
		// Check for /clear as a command on a prompt line:
		// Strip the prompt marker (❯ or >) and check if the remainder is exactly "/clear".
		if idx := strings.LastIndex(trimmed, "❯"); idx >= 0 {
			after := strings.TrimSpace(trimmed[idx+len("❯"):])
			if after == "/clear" {
				return true
			}
		}
		if strings.HasPrefix(trimmed, ">") {
			after := strings.TrimSpace(trimmed[1:])
			if after == "/clear" {
				return true
			}
		}
		checked++
	}
	return false
}

// contentHash returns a hex-encoded SHA-256 hash of the content.
func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// lastNonBlankLine returns the last non-blank line from content, truncated to lastLineMaxDisplay chars.
func lastNonBlankLine(content string) string {
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if len(trimmed) > lastLineMaxDisplay {
			return trimmed[:lastLineMaxDisplay] + "..."
		}
		return trimmed
	}
	return "<empty>"
}

// containsControlChars returns true if s contains any ASCII control characters
// (0x00-0x1F, 0x7F) except tab, or Unicode line/paragraph separators (U+2028,
// U+2029). The Unicode separators are included because they act as line breaks
// in some contexts and could enable injection via tmux SendCommand.
func containsControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\t' {
			return true
		}
		if r == 0x7F || r == 0x2028 || r == 0x2029 {
			return true
		}
	}
	return false
}
