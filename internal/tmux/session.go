// Package tmux provides helpers for managing tmux sessions and panes.
package tmux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// debugLogger is an optional logger for tracing all tmux operations.
// Set via SetDebugLogger to enable debug logging.
var debugLogger atomic.Value // *log.Logger or nil

// SetDebugLogger enables debug logging for all tmux operations.
// Pass nil to disable. The logger output should go to a file for post-mortem analysis.
func SetDebugLogger(l *log.Logger) {
	if l == nil {
		debugLogger.Store((*log.Logger)(nil))
	} else {
		debugLogger.Store(l)
	}
}

func debugLog(format string, args ...any) {
	v := debugLogger.Load()
	if v == nil {
		return
	}
	l, ok := v.(*log.Logger)
	if !ok || l == nil {
		return
	}
	l.Printf("[tmux] "+format, args...)
}

// callerInfo returns "file:line function" for the caller at the given skip depth.
func callerInfo(skip int) string {
	pc, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	fn := runtime.FuncForPC(pc)
	funcName := "unknown"
	if fn != nil {
		funcName = fn.Name()
		// Shorten to last component
		if idx := strings.LastIndex(funcName, "/"); idx >= 0 {
			funcName = funcName[idx+1:]
		}
	}
	// Shorten file path to last 2 components
	parts := strings.Split(file, "/")
	if len(parts) > 2 {
		file = strings.Join(parts[len(parts)-2:], "/")
	}
	return fmt.Sprintf("%s:%d %s", file, line, funcName)
}

// defaultCmdTimeout is the timeout for individual tmux commands.
// tmux IPC is normally sub-millisecond; 5s catches hung servers
// while avoiding false positives on slower systems.
const defaultCmdTimeout = 5 * time.Second

// maxMessageSize is the maximum allowed size (in bytes) for text sent via
// SendTextAndSubmit or SendCommand. This prevents accidental resource
// exhaustion from extremely large payloads being loaded into tmux buffers.
const maxMessageSize = 1 << 20 // 1 MB

// bracketedPasteDelay is the pause between pasting content into a pane and
// sending Enter to submit it. Claude Code's Ink-based TUI needs time to
// process the bracketed paste into its input field; 100ms was empirically
// too short under load, causing intermittent delivery failures. 500ms was
// chosen as a safe margin through production testing.
const bracketedPasteDelay = 500 * time.Millisecond

// validUserVarName matches safe tmux user variable names (alphanumeric + underscore).
// This prevents tmux format injection via names containing #( or #[.
var validUserVarName = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// ErrorKind categorizes tmux command failures.
type ErrorKind int

// ErrorKind constants enumerate the categories of tmux command failures.
const (
	ErrKindServer   ErrorKind = iota + 1 // tmux server unreachable
	ErrKindSession                       // session not found
	ErrKindPane                          // pane or window not found
	ErrKindTimeout                       // command timed out
	ErrKindCommand                       // other command error
	ErrKindCanceled                      // context was canceled (not timeout)
)

func (k ErrorKind) String() string {
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
	case ErrKindCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// Error is a classified tmux command error.
type Error struct {
	Kind   ErrorKind
	Op     string // tmux subcommand (e.g., "send-keys")
	Stderr string // raw stderr from tmux
	Err    error  // underlying error
}

func (e *Error) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("tmux %s (%s): %v: %s", e.Op, e.Kind, e.Err, e.Stderr)
	}
	return fmt.Sprintf("tmux %s (%s): %v", e.Op, e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Is supports errors.Is() matching by Kind.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Kind == t.Kind
}

// Sentinel errors for use with errors.Is().
var (
	ErrTmuxServer  = &Error{Kind: ErrKindServer}
	ErrTmuxSession = &Error{Kind: ErrKindSession}
)

// classifyError parses tmux stderr to determine the error category.
func classifyError(op, stderr string, err error) *Error {
	lower := strings.ToLower(stderr)

	var kind ErrorKind
	switch {
	case strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "server exited") ||
		strings.Contains(lower, "error connecting") ||
		strings.Contains(lower, "connect failed") ||
		strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "permission denied"):
		kind = ErrKindServer
	case strings.Contains(lower, "session not found") ||
		strings.Contains(lower, "can't find session") ||
		strings.Contains(lower, "no such session") ||
		strings.Contains(lower, "no sessions") ||
		// "no current target" / "no current client" / "no current session"
		// are emitted by tmux when a target-bearing command (kill-session,
		// list-windows, etc.) is asked to operate on an implicit current
		// target while no client is attached and no exact-match candidate
		// exists. Treat them as "session-equivalent missing" so idempotent
		// callers (KillSession, cleanup paths) succeed instead of bubbling
		// up a misleading ErrKindCommand. See issue: tmux cleanup not
		// idempotent when session is already gone.
		strings.Contains(lower, "no current target") ||
		strings.Contains(lower, "no current client") ||
		strings.Contains(lower, "no current session"):
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

	return &Error{
		Kind:   kind,
		Op:     op,
		Stderr: stderr,
		Err:    err,
	}
}

// contextErrorKind returns ErrKindTimeout for deadline exceeded and
// ErrKindCanceled for all other context errors.
func contextErrorKind(err error) ErrorKind {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrKindTimeout
	}
	return ErrKindCanceled
}

// bufSeq generates unique buffer names to prevent race conditions when
// multiple goroutines call SendTextAndSubmit concurrently within a single
// process.
var bufSeq atomic.Int64

// bufNamePID is captured once at process start so every buffer name baked
// in this process carries the PID of the daemon that owns the paste. tmux
// buffers are scoped to the tmux server (NOT to the maestro process), so
// two daemons sharing a tmux server would otherwise collide on
// `maestro-msg-1`, `maestro-msg-2`, … and silently overwrite each other's
// load-buffer payload before paste-buffer fires. The 2026-04 audit
// reproduced this end-to-end: a gemini-formation Planner pasted the
// codex-formation's command because the codex daemon's load-buffer landed
// in the same tmux buffer slot just before gemini's paste-buffer ran.
// PIDs are unique across all running processes on the same host, so the
// per-PID prefix makes `maestro-msg-<pid>-<seq>` globally unique on the
// shared tmux server.
var bufNamePID = os.Getpid()

// SendTextAndSubmit sends multi-line text to a pane using paste-buffer for
// reliable delivery, then sends Enter to submit. This avoids character-by-character
// key sending issues with newlines in the message.
func SendTextAndSubmit(ctx context.Context, paneTarget, text string) error {
	if len(text) > maxMessageSize {
		return fmt.Errorf("message size %d exceeds maximum %d bytes", len(text), maxMessageSize)
	}

	// Buffer names MUST be globally unique on the tmux server. See
	// bufNamePID for the PID-prefix rationale.
	bufName := fmt.Sprintf("maestro-msg-%d-%d", bufNamePID, bufSeq.Add(1))

	// Load text into tmux buffer via stdin (handles arbitrary content safely)
	loadCtx, loadCancel := context.WithTimeout(ctx, defaultCmdTimeout)
	defer loadCancel()
	cmd := exec.CommandContext(loadCtx, "tmux", "load-buffer", "-b", bufName, "-") //nolint:gosec // "tmux" is a fixed command; bufName is an internal counter-based name
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		stderr := strings.TrimSpace(string(out))
		if loadCtx.Err() != nil {
			return &Error{Kind: contextErrorKind(loadCtx.Err()), Op: "load-buffer", Stderr: stderr, Err: loadCtx.Err()}
		}
		return classifyError("load-buffer", stderr, err)
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
	// paste before we send Enter to submit. Uses context-aware sleep so
	// cancellation is respected. See bracketedPasteDelay for rationale.
	if err := sleepCtx(ctx, bracketedPasteDelay); err != nil {
		return &Error{Kind: contextErrorKind(err), Op: "send-text-submit-sleep", Err: err}
	}

	return sendKeysCtx(ctx, paneTarget, "Enter")
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

// sessionName stores the tmux session name. Access via GetSessionName()/SetSessionName().
// Protected by atomic.Value to prevent data races across goroutines.
//
// This is a package-level variable rather than a struct field because the session
// name is process-global state shared by all tmux operations. A SessionManager
// struct would improve testability but require threading it through every caller;
// the current design is intentional given the single-session-per-process model.
var sessionName atomic.Value

func init() {
	sessionName.Store("maestro")
}

// unsafeSessionChars matches characters that are unsafe in tmux session names.
// tmux uses `:` and `.` for target resolution, so these must be sanitized.
var unsafeSessionChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// GetSessionName returns the current tmux session name (goroutine-safe).
func GetSessionName() string {
	v, ok := sessionName.Load().(string)
	if !ok {
		return "maestro"
	}
	return v
}

// exactSessionTarget returns the session name prefixed with "=" to force
// tmux exact name matching, preventing prefix/glob resolution.
// Without this, `-t maestro-` would match `maestro-myproject` via prefix matching.
func exactSessionTarget() string {
	return "=" + GetSessionName()
}

// SetSessionName updates the tmux session name, sanitizing unsafe characters.
func SetSessionName(name string) {
	sanitized := unsafeSessionChars.ReplaceAllString(name, "_")
	if sanitized == "" {
		sanitized = "maestro"
	}
	sessionName.Store(sanitized)
}

// BuildMaestroSessionName returns a stable, collision-resistant tmux session
// name for a maestro project. The name combines the human-readable project
// name with an 8-hex-char hash of the absolute maestroDir, so two checkouts
// of the same project (or two repos that share a project name) get distinct
// sessions instead of colliding on a single global "maestro-<name>" slot.
//
// The hash is derived from the canonical absolute path of maestroDir
// (filepath.Abs + filepath.Clean + filepath.EvalSymlinks when available).
// Resolving symlinks keeps /tmp/... and /private/tmp/... on macOS pointed at
// the same tmux session. If absolute or symlink resolution fails, the best
// available cleaned path is hashed — the goal is stable per-checkout
// differentiation, not security.
//
// If maestroDir is empty, the legacy "maestro-<projectName>" form is returned
// for backward compatibility (e.g., test code that does not have a maestro
// directory). Callers in production paths should always supply maestroDir.
func BuildMaestroSessionName(projectName, maestroDir string) string {
	base := "maestro-" + projectName
	if maestroDir == "" {
		return base
	}
	canonical := maestroDir
	if abs, err := filepath.Abs(maestroDir); err == nil {
		canonical = filepath.Clean(abs)
		if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
			canonical = filepath.Clean(resolved)
		}
	}
	sum := sha256.Sum256([]byte(canonical))
	return base + "-" + hex.EncodeToString(sum[:])[:8]
}

// SessionExists checks whether the maestro tmux session exists.
func SessionExists() bool {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCmdTimeout)
	defer cancel()
	err := exec.CommandContext(ctx, "tmux", "has-session", "-t", exactSessionTarget()).Run() //nolint:gosec // "tmux" is a fixed command; target is derived from validated session name
	exists := err == nil
	debugLog("SessionExists session=%s exists=%v", GetSessionName(), exists)
	return exists
}

// CreateSession creates a new maestro tmux session.
// The first window is named windowName. Returns an error if the session already exists.
func CreateSession(windowName string) error {
	debugLog("CreateSession session=%s window=%s", GetSessionName(), windowName)
	err := run("new-session", "-d", "-s", GetSessionName(), "-n", windowName)
	if err != nil {
		debugLog("CreateSession FAILED session=%s error=%v", GetSessionName(), err)
	} else {
		debugLog("CreateSession OK session=%s", GetSessionName())
	}
	return err
}

// KillSession destroys the maestro tmux session.
// It is idempotent: if the session (or tmux server) does not exist, it returns nil.
func KillSession() error {
	caller := callerInfo(2)
	debugLog("KillSession called session=%s caller=%s", GetSessionName(), caller)
	err := run("kill-session", "-t", exactSessionTarget())
	if err != nil {
		// Idempotent: session or server already gone is not an error.
		if errors.Is(err, ErrTmuxSession) || errors.Is(err, ErrTmuxServer) {
			debugLog("KillSession OK (already gone) session=%s caller=%s", GetSessionName(), caller)
			return nil
		}
		debugLog("KillSession FAILED session=%s error=%v caller=%s", GetSessionName(), err, caller)
		return err
	}
	debugLog("KillSession OK session=%s caller=%s", GetSessionName(), caller)
	return nil
}

// SessionHealthResult contains diagnostic information from a session health check.
type SessionHealthResult struct {
	Alive          bool
	ServerRunning  bool
	OtherSessions  string // other sessions on the server (if server is running)
	Stderr         string // stderr from has-session (if session is dead)
	WindowInfo     string // window/pane info (if session is alive)
	ServerOptions  string // server options (exit-empty, exit-unattached)
	SessionOptions string // session options (destroy-unattached)
}

// SessionHealthCheck performs a detailed session health check and logs the result.
// Returns true if the session is alive, false otherwise.
// When the session is missing, it also checks if the tmux server is running.
func SessionHealthCheck() bool {
	result := SessionHealthCheckDetailed()
	return result.Alive
}

// SessionHealthCheckDetailed performs a detailed session health check and returns
// diagnostic information useful for debugging session loss events.
func SessionHealthCheckDetailed() SessionHealthResult {
	name := GetSessionName()
	ctx, cancel := context.WithTimeout(context.Background(), defaultCmdTimeout)
	defer cancel()

	result := SessionHealthResult{}

	// Check session (use "=" prefix for exact name matching)
	exactName := "=" + name
	cmd := exec.CommandContext(ctx, "tmux", "has-session", "-t", exactName) //nolint:gosec // "tmux" is a fixed command; exactName is derived from validated session name
	out, err := cmd.CombinedOutput()
	if err == nil {
		result.Alive = true
		result.ServerRunning = true

		// Session alive — gather window/pane info for diagnostics
		winOut, winErr := output("list-windows", "-t", exactName, "-F", "#{window_index}:#{window_name}:#{window_panes}")
		if winErr == nil {
			result.WindowInfo = strings.TrimSpace(winOut)
			debugLog("SessionHealthCheck OK session=%s windows=[%s]", name, result.WindowInfo)
		} else {
			debugLog("SessionHealthCheck OK session=%s (list-windows failed: %v)", name, winErr)
		}
		return result
	}

	result.Stderr = strings.TrimSpace(string(out))
	debugLog("SessionHealthCheck DEAD session=%s stderr=%q", name, result.Stderr)

	// Check if tmux server itself is running
	serverCmd := exec.CommandContext(ctx, "tmux", "list-sessions")
	serverOut, serverErr := serverCmd.CombinedOutput()
	if serverErr != nil {
		debugLog("SessionHealthCheck SERVER_DOWN stderr=%q", strings.TrimSpace(string(serverOut)))
		result.ServerRunning = false
	} else {
		result.ServerRunning = true
		result.OtherSessions = strings.TrimSpace(string(serverOut))
		debugLog("SessionHealthCheck SERVER_OK other_sessions=%q", result.OtherSessions)
	}

	// Gather server options for diagnostics (only if server is running)
	if result.ServerRunning {
		if optOut, optErr := output("show-options", "-s", "-v", "exit-empty"); optErr == nil {
			result.ServerOptions = "exit-empty=" + strings.TrimSpace(optOut)
		}
		if optOut, optErr := output("show-options", "-s", "-v", "exit-unattached"); optErr == nil {
			result.ServerOptions += " exit-unattached=" + strings.TrimSpace(optOut)
		}
	}

	return result
}

// CreateWindow creates a new window in the maestro session.
func CreateWindow(name string) error {
	return run("new-window", "-t", exactSessionTarget(), "-n", name)
}

// splitPane splits the current pane in the given window horizontally or vertically.
// horizontal=true splits left-right (-h), false splits top-bottom (-v).
func splitPane(windowTarget string, horizontal bool) error {
	flag := "-v"
	if horizontal {
		flag = "-h"
	}
	return run("split-window", flag, "-t", windowTarget)
}

// SetUserVar sets a tmux user variable on a pane (pane-scoped via -p).
// Format: tmux set-option -p -t <pane> @<name> <value>
// Both name and value are validated to prevent format injection.
func SetUserVar(paneTarget, name, value string) error {
	if !validUserVarName.MatchString(name) {
		return fmt.Errorf("invalid user variable name %q: must match [a-zA-Z0-9_]+", name)
	}
	if err := validateUserVarValue(value); err != nil {
		return fmt.Errorf("invalid user variable value for %q: %w", name, err)
	}
	return run("set-option", "-p", "-t", paneTarget, "@"+name, value)
}

// validateUserVarValue checks that a user variable value does not contain
// control characters (null bytes, newlines) that could interfere with tmux
// command-line parsing.
func validateUserVarValue(value string) error {
	for i, ch := range value {
		if ch == 0 {
			return fmt.Errorf("null byte at position %d", i)
		}
		if ch == '\n' || ch == '\r' {
			return fmt.Errorf("newline at position %d", i)
		}
	}
	return nil
}

// GetUserVar reads a tmux user variable from a pane.
// The name parameter is validated to contain only alphanumeric characters and
// underscores to prevent tmux format injection (e.g., #(command) execution).
func GetUserVar(paneTarget, name string) (string, error) {
	if !validUserVarName.MatchString(name) {
		return "", fmt.Errorf("invalid user variable name %q: must match [a-zA-Z0-9_]+", name)
	}
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

// CapturePaneAlternateJoined captures pane content from the alternate screen
// with the -J flag. If no alternate screen exists, tmux returns an empty result
// rather than an error because -q is used.
func CapturePaneAlternateJoined(paneTarget string, lastN int) (string, error) {
	args := []string{"capture-pane", "-a", "-q", "-t", paneTarget, "-pJ"}
	if lastN > 0 {
		args = append(args, "-S", fmt.Sprintf("-%d", lastN))
	}
	return output(args...)
}

// sendKeysCtx sends keystrokes to a pane with context support.
func sendKeysCtx(ctx context.Context, paneTarget string, keys ...string) error {
	args := make([]string, 0, 3+len(keys))
	args = append(args, "send-keys", "-t", paneTarget)
	args = append(args, keys...)
	return runCtx(ctx, args...)
}

// SendKeys sends keystrokes to a pane.
func SendKeys(paneTarget string, keys ...string) error {
	return sendKeysCtx(context.Background(), paneTarget, keys...)
}

// ListPanes returns pane IDs for a window, formatted by the given format string.
func ListPanes(windowTarget, format string) ([]string, error) {
	out, err := output("list-panes", "-t", windowTarget, "-F", format)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return []string{}, nil
	}
	return strings.Split(out, "\n"), nil
}

// ListAllPanes returns pane info across all windows in the session.
func ListAllPanes(format string) ([]string, error) {
	out, err := output("list-panes", "-s", "-t", exactSessionTarget(), "-F", format)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return []string{}, nil
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
// The command text is sent in literal mode (-l) to prevent tmux from
// interpreting special key sequences (e.g., C-a, M-x). Enter is sent
// separately as a key press to submit the command.
//
// Each invocation emits a debug-log entry to tmux_debug.log including the
// command and pane target. This is the canonical wire-level counter for
// "how many times did the daemon push a command into a pane" — independent
// of how the runtime renders the result in its transcript. The 2026-04-27
// E2E run reported "/clear" appearing twice in Claude Code's pane
// transcript per task transition; cross-checking SendCommand entries
// against transcript occurrences is what distinguishes a true double-send
// (one tmux call per visible "/clear" → bug) from a Claude Code rendering
// quirk (one tmux call per task transition, two visible "/clear" lines
// → cosmetic).
func SendCommand(paneTarget, command string) error {
	if len(command) > maxMessageSize {
		return fmt.Errorf("command size %d exceeds maximum %d bytes", len(command), maxMessageSize)
	}
	debugLog("SendCommand pane=%s command=%q", paneTarget, command)
	// Send command text literally (no special key interpretation).
	if err := SendKeys(paneTarget, "-l", command); err != nil {
		return err
	}
	// Send Enter as a key press to submit.
	return SendKeys(paneTarget, "Enter")
}

// SendCtrlC sends Ctrl+C to a pane.
func SendCtrlC(paneTarget string) error {
	return SendKeys(paneTarget, "C-c")
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
		if err := splitPane(firstPaneID, false); err != nil {
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
		if err := splitPane(strings.TrimSpace(rowIDs[i]), true); err != nil {
			return nil, fmt.Errorf("split row %d cols: %w", i, err)
		}
	}

	// The layout tree is now row-major: V(H(TL,TR), H(BL,BR), ...).
	// tmux assigns pane indices by depth-first traversal, giving row-major order
	// automatically. No reordering needed.
	return ListPanes(windowTarget, paneFormat)
}

// CreateSessionWithServerOptions creates a new maestro tmux session and applies
// the given server-level options atomically in a single tmux invocation.
//
// This avoids two competing races:
//   - Starting an empty server first and then setting options: with the default
//     exit-empty=on, the empty server exits before the follow-up set-option
//     commands can reach it ("no server running on ..." error).
//   - Creating a detached session first and then setting options: if the user's
//     tmux.conf sets exit-unattached=on, the server may exit between new-session
//     and the first set-option.
//
// Chaining `new-session` and `set-option` commands via tmux's `;` separator
// runs them within a single client invocation, so the server stays up for the
// entire sequence regardless of exit-empty / exit-unattached defaults.
//
// The iteration order of serverOptions does not affect correctness because
// tmux applies each option synchronously within the chain.
func CreateSessionWithServerOptions(windowName string, serverOptions map[string]string) error {
	debugLog("CreateSessionWithServerOptions session=%s window=%s options=%v",
		GetSessionName(), windowName, serverOptions)

	args := make([]string, 0, 6+5*len(serverOptions))
	args = append(args, "new-session", "-d", "-s", GetSessionName(), "-n", windowName)
	for name, value := range serverOptions {
		args = append(args, ";", "set-option", "-s", name, value)
	}

	err := run(args...)
	if err != nil {
		debugLog("CreateSessionWithServerOptions FAILED session=%s error=%v", GetSessionName(), err)
	} else {
		debugLog("CreateSessionWithServerOptions OK session=%s", GetSessionName())
	}
	return err
}

// SetServerOption sets a server-level tmux option.
// Server options (e.g., exit-empty, exit-unattached) affect the entire tmux server,
// not just a single session. Use this for options that must survive session destruction.
func SetServerOption(name, value string) error {
	return run("set-option", "-s", name, value)
}

// SetSessionOption sets a session-level tmux option on the maestro session.
// Verifies session existence via exact match before applying the option.
// Note: tmux set-option does not support the "=" exact-match prefix (unlike
// has-session, kill-session, etc.), so we use the plain session name for the
// set-option call but guard it with an exact-match existence check first.
func SetSessionOption(name, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "tmux", "has-session", "-t", exactSessionTarget()).Run(); err != nil { //nolint:gosec // "tmux" is a fixed command; target is derived from validated session name
		return fmt.Errorf("set-option (session): session %q not found: %w", GetSessionName(), err)
	}
	return run("set-option", "-t", GetSessionName(), name, value)
}

// SetWindowOption sets a window-level tmux option for a specific window.
// windowTarget should be in the form "session:window" (e.g., "maestro:0").
// This only affects the specified window, avoiding side effects on other sessions or windows.
func SetWindowOption(windowTarget, name, value string) error {
	return run("set-option", "-w", "-t", windowTarget, name, value)
}

// ListSessions returns the names of all sessions on the current tmux server.
func ListSessions() ([]string, error) {
	out, err := output("list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// SelectWindow selects (focuses) a window in the maestro session.
func SelectWindow(windowTarget string) error {
	return run("select-window", "-t", windowTarget)
}

// AttachSession attaches the current terminal to the maestro tmux session.
// This replaces the current process with tmux attach-session.
func AttachSession() error {
	cmd := exec.Command("tmux", "attach-session", "-t", exactSessionTarget()) //nolint:gosec // "tmux" is a fixed command; target is derived from validated session name
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

// RespawnPane kills the current process in the pane and respawns it as a fresh
// shell in the given start directory. This is more reliable than sending
// Ctrl+C/Ctrl+D to exit a running process because it does not depend on the
// process handling those signals gracefully.
func RespawnPane(paneTarget, startDir string) error {
	return run("respawn-pane", "-k", "-t", paneTarget, "-c", startDir)
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
	// Fast path: if context is already done, don't spawn a process.
	if err := ctx.Err(); err != nil {
		debugLog("runCtx SKIP (ctx done) args=%v err=%v", args, err)
		return &Error{Kind: contextErrorKind(err), Op: args[0], Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, defaultCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...) //nolint:gosec // "tmux" is a fixed command; args are controlled internally
	out, err := cmd.CombinedOutput()
	if err != nil {
		stderr := strings.TrimSpace(string(out))
		if ctx.Err() != nil {
			debugLog("runCtx TIMEOUT args=%v stderr=%q", args, stderr)
			return &Error{Kind: contextErrorKind(ctx.Err()), Op: args[0], Stderr: stderr, Err: ctx.Err()}
		}
		classified := classifyError(args[0], stderr, err)
		debugLog("runCtx %s args=%v kind=%s stderr=%q", debugLevelForErrorKind(classified.Kind), args, classified.Kind, stderr)
		return classified
	}
	return nil
}

// debugLevelForErrorKind selects the prefix used by debugLog for a
// classified tmux error. Idempotent-cleanup failures (no session, no
// server) are the load-bearing case for `maestro up`'s pre-cleanup and
// `maestro down`'s post-kill restore: callers explicitly handle these
// via errors.Is(err, ErrTmuxSession/ErrTmuxServer) and continue. The
// 2026-04-28 retest4 reader saw these in tmux_debug.log as "ERROR" and
// questioned whether the cleanup succeeded; demoting to "DEBUG" matches
// the actual severity. Genuine failures (timeout, IPC, command missing,
// classified as Generic / Timeout / Cancelled / Conflict) still log as
// ERROR so post-mortem analysis surfaces them.
func debugLevelForErrorKind(kind ErrorKind) string {
	switch kind {
	case ErrKindSession, ErrKindServer:
		return "DEBUG"
	default:
		return "ERROR"
	}
}

// outputCtx executes a tmux command that returns output, with context support and error classification.
func outputCtx(ctx context.Context, args ...string) (string, error) {
	// Fast path: if context is already done, don't spawn a process.
	if err := ctx.Err(); err != nil {
		return "", &Error{Kind: contextErrorKind(err), Op: args[0], Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, defaultCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", args...) //nolint:gosec // "tmux" is a fixed command; args are controlled internally
	out, err := cmd.CombinedOutput()
	if err != nil {
		stderr := strings.TrimSpace(string(out))
		if ctx.Err() != nil {
			return "", &Error{Kind: contextErrorKind(ctx.Err()), Op: args[0], Stderr: stderr, Err: ctx.Err()}
		}
		return "", classifyError(args[0], stderr, err)
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
