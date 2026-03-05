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
	CapturePane(paneTarget string, lastN int) (string, error)
	CapturePaneJoined(paneTarget string, lastN int) (string, error)
	IsShellCommand(cmd string) bool
}

// TmuxPaneIO implements PaneIO using the real tmux package.
type TmuxPaneIO struct{}

// NewTmuxPaneIO creates a new TmuxPaneIO instance.
func NewTmuxPaneIO() *TmuxPaneIO {
	return &TmuxPaneIO{}
}

func (t *TmuxPaneIO) FindPaneByAgentID(agentID string) (string, error) {
	return tmux.FindPaneByAgentID(agentID)
}

func (t *TmuxPaneIO) SendCtrlC(paneTarget string) error {
	return tmux.SendCtrlC(paneTarget)
}

func (t *TmuxPaneIO) SendKeys(paneTarget string, keys ...string) error {
	return tmux.SendKeys(paneTarget, keys...)
}

func (t *TmuxPaneIO) SendCommand(paneTarget, command string) error {
	return tmux.SendCommand(paneTarget, command)
}

func (t *TmuxPaneIO) SendTextAndSubmit(ctx context.Context, paneTarget, text string) error {
	return tmux.SendTextAndSubmit(ctx, paneTarget, text)
}

func (t *TmuxPaneIO) SetUserVar(paneTarget, name, value string) error {
	return tmux.SetUserVar(paneTarget, name, value)
}

func (t *TmuxPaneIO) GetUserVar(paneTarget, name string) (string, error) {
	return tmux.GetUserVar(paneTarget, name)
}

func (t *TmuxPaneIO) GetPanePID(paneTarget string) (string, error) {
	return tmux.GetPanePID(paneTarget)
}

func (t *TmuxPaneIO) GetPaneCurrentCommand(paneTarget string) (string, error) {
	return tmux.GetPaneCurrentCommand(paneTarget)
}

func (t *TmuxPaneIO) CapturePane(paneTarget string, lastN int) (string, error) {
	return tmux.CapturePane(paneTarget, lastN)
}

func (t *TmuxPaneIO) CapturePaneJoined(paneTarget string, lastN int) (string, error) {
	return tmux.CapturePaneJoined(paneTarget, lastN)
}

func (t *TmuxPaneIO) IsShellCommand(cmd string) bool {
	return tmux.IsShellCommand(cmd)
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
// Fallback check: the line starts with '>' (ASCII 0x3E). This covers older Claude Code
// versions or terminal environments where the Unicode character is not rendered.
//
// The fallback is intentionally broad; false positives (e.g. markdown blockquotes) are
// mitigated by callers performing stability checks before invoking this function.
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
	// Fallback: check only the last non-blank line for '>'.
	// Limiting to the last line avoids false positives from markdown
	// blockquotes or shell output on earlier lines.
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		return strings.HasPrefix(trimmed, ">")
	}
	return false
}

// clearTextVisible checks whether "/clear" text is visible near the bottom of the pane content.
// This is the primary signal for detecting that /clear was NOT processed as a command
// and instead remains as literal text in the input field.
func clearTextVisible(content string) bool {
	lines := strings.Split(content, "\n")
	// Check the last 6 non-blank lines (covers input line + a few status lines)
	checked := 0
	for i := len(lines) - 1; i >= 0 && checked < 6; i-- {
		trimmed := strings.TrimSpace(stripANSI(lines[i]))
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "/clear") {
			return true
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
// (0x00-0x1F, 0x7F) except tab. This prevents injection via tmux SendCommand.
func containsControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\t' {
			return true
		}
		if r == 0x7F {
			return true
		}
	}
	return false
}

// shellQuote wraps a path in single quotes for safe shell expansion.
// Single quotes inside the path are escaped.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
