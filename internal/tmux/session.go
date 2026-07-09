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

// bracketedPasteDelay is the base pause between pasting content into a pane
// and sending Enter to submit it. Claude Code's Ink-based TUI needs time to
// process the bracketed paste into its input field; 100ms was empirically
// too short under load, causing intermittent delivery failures. 500ms was
// chosen as a safe margin through production testing for typical message
// sizes. Large pastes scale beyond this base — see pasteDelayFor.
const bracketedPasteDelay = 500 * time.Millisecond

// pasteDelayExtraPerKB and pasteDelayMax tune pasteDelayFor: each KB of
// payload adds processing headroom on top of bracketedPasteDelay, capped so
// a worst-case envelope (128 KB task content) cannot stall delivery for an
// unbounded time.
const (
	pasteDelayExtraPerKB = 15 * time.Millisecond
	pasteDelayMax        = 3 * time.Second
)

// pasteDelayFor returns the pre-Enter pause for a paste of size n bytes:
// the empirically-validated base for typical messages, plus size-scaled
// headroom for large envelopes. The TUI ingests a 128 KB bracketed paste
// noticeably slower than a one-liner; a fixed base risks sending Enter
// mid-ingestion, which the downstream submit-confirmation probe then has to
// repair (assumed-running with lease-TTL fallback). Scaling the delay keeps
// the probe a safety net instead of a routine path.
func pasteDelayFor(n int) time.Duration {
	d := bracketedPasteDelay + time.Duration(n/1024)*pasteDelayExtraPerKB
	if d > pasteDelayMax {
		return pasteDelayMax
	}
	return d
}

// validUserVarName matches safe tmux user variable names (alphanumeric + underscore).
// This prevents tmux format injection via names containing #( or #[.
var validUserVarName = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// ErrorKind categorizes tmux command failures.
type ErrorKind int

// ErrorKind constants enumerate the categories of tmux command failures.
const (
	ErrKindServer     ErrorKind = iota + 1 // tmux server unreachable
	ErrKindSession                         // session not found
	ErrKindPane                            // pane or window not found
	ErrKindTimeout                         // command timed out
	ErrKindCommand                         // other command error
	ErrKindCanceled                        // context was canceled (not timeout)
	ErrKindPermission                      // socket access denied (sandbox, file permissions)
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
	case ErrKindPermission:
		return "permission"
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
	ErrTmuxServer     = &Error{Kind: ErrKindServer}
	ErrTmuxSession    = &Error{Kind: ErrKindSession}
	ErrTmuxPermission = &Error{Kind: ErrKindPermission}
)

// classifyError parses tmux stderr to determine the error category.
func classifyError(op, stderr string, err error) *Error {
	lower := strings.ToLower(stderr)

	var kind ErrorKind
	switch {
	// Permission failures must be classified BEFORE the server-unreachable
	// case: a sandbox denying tmux socket access is NOT equivalent to "the
	// server/session is already gone". Idempotent callers (KillSession,
	// cleanup paths) treat ErrKindServer/ErrKindSession as success, and
	// folding permission errors into ErrKindServer made KillSession report
	// success while the session was still alive behind the denied socket.
	case strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "permission denied"):
		kind = ErrKindPermission
	case strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "server exited") ||
		strings.Contains(lower, "error connecting") ||
		strings.Contains(lower, "connect failed"):
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
	cmd := exec.CommandContext(loadCtx, "tmux", tmuxArgs([]string{"load-buffer", "-b", bufName, "-"})...) //nolint:gosec // "tmux" is a fixed command; bufName is an internal counter-based name
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
	// paste before we send Enter to submit. Size-scaled so large envelopes
	// get proportionally more ingestion time. Uses context-aware sleep so
	// cancellation is respected. See pasteDelayFor for rationale.
	if err := sleepCtx(ctx, pasteDelayFor(len(text))); err != nil {
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

// tmuxSocket holds the per-instance tmux socket name (passed as the
// argument to `tmux -L <socket>`). An empty string means use the default
// socket (kept for backward compatibility and tests).
//
// Sharing the default socket lets concurrent maestro instances from
// different projects pile sessions onto the same tmux server, which
// produced SESSION_LOST races, ID collisions, and stray
// autoAcceptTrustDialog keypresses (Report 2026-05-06: every time `tmux
// ls` showed coexisting instances, the later `up` reproducibly observed
// SESSION_LOST).
//
// One socket per instance gives each instance its own tmux server and
// removes the shared-server races structurally.
var tmuxSocket atomic.Value

func init() {
	sessionName.Store("maestro")
	tmuxSocket.Store("")
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

// GetTmuxSocket returns the current tmux socket name. An empty string
// means the default socket (no `-L` flag is added).
func GetTmuxSocket() string {
	v, ok := tmuxSocket.Load().(string)
	if !ok {
		return ""
	}
	return v
}

// SetTmuxSocket updates the tmux socket name used by `tmux -L <name>`.
// Unsafe characters are sanitised with the same rules as the session
// name. An empty string means the default socket (backward compat).
func SetTmuxSocket(name string) {
	if name == "" {
		tmuxSocket.Store("")
		return
	}
	sanitized := unsafeSessionChars.ReplaceAllString(name, "_")
	tmuxSocket.Store(sanitized)
}

// BuildMaestroSocketName returns a per-project tmux socket name. It is
// stabilised by the same rule as the session name (project name +
// maestroDir hash). Socket names are used as a filesystem path, so we
// keep them short (tmux itself accepts up to ~104 chars; we stay well
// below that).
func BuildMaestroSocketName(projectName, maestroDir string) string {
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

// tmuxArgs returns the tmux argv prefix with socket settings applied.
// When a socket is configured, `-L <socket>` is prepended to args.
func tmuxArgs(args []string) []string {
	socket := GetTmuxSocket()
	if socket == "" {
		return args
	}
	combined := make([]string, 0, len(args)+2)
	combined = append(combined, "-L", socket)
	combined = append(combined, args...)
	return combined
}

// BuildMaestroSessionName returns the human-readable tmux session name for a
// maestro project, in the form "maestro-<projectName>".
//
// Cross-instance collision (two checkouts of the same project, or two repos
// sharing a project name) is handled at the socket layer by
// BuildMaestroSocketName: each maestro instance gets its own tmux server via
// `tmux -L <socket>`, so within a given socket there is at most one session
// per project. That makes the session name itself a pure UX surface — short
// and predictable so operators can `tmux attach -t maestro-<projectName>`
// once the right socket is selected (see `maestro attach`).
func BuildMaestroSessionName(projectName string) string {
	return "maestro-" + projectName
}

// SessionExists checks whether the maestro tmux session exists.
func SessionExists() bool {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCmdTimeout)
	defer cancel()
	err := exec.CommandContext(ctx, "tmux", tmuxArgs([]string{"has-session", "-t", exactSessionTarget()})...).Run() //nolint:gosec // "tmux" is a fixed command; target is derived from validated session name
	exists := err == nil
	debugLog("SessionExists session=%s exists=%v", GetSessionName(), exists)
	return exists
}

// SessionExistsChecked reports whether the maestro session exists,
// distinguishing "definitely absent" (session/server not found — false, nil)
// from "could not determine" (permission denied, timeout, IPC failure —
// false, non-nil error). Guards that must not fail open on an unreadable
// tmux socket (e.g. RunUp's destroy-and-recreate protection) use this
// instead of the boolean-only SessionExists.
func SessionExistsChecked() (bool, error) {
	err := run("has-session", "-t", exactSessionTarget())
	if err == nil {
		debugLog("SessionExistsChecked session=%s exists=true", GetSessionName())
		return true, nil
	}
	if errors.Is(err, ErrTmuxSession) || errors.Is(err, ErrTmuxServer) {
		debugLog("SessionExistsChecked session=%s exists=false", GetSessionName())
		return false, nil
	}
	debugLog("SessionExistsChecked session=%s indeterminate error=%v", GetSessionName(), err)
	return false, err
}

// KillSession destroys the maestro tmux session.
// It is idempotent: if the session (or tmux server) does not exist, it returns nil.
// Permission failures (ErrKindPermission — e.g. a sandbox denying tmux socket
// access) are NOT treated as idempotent success: the session may still be
// alive behind the denied socket, so the error is surfaced to the caller.
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

// SessionHealthCheckDetailed performs a detailed session health check and returns
// diagnostic information useful for debugging session loss events.
func SessionHealthCheckDetailed() SessionHealthResult {
	name := GetSessionName()
	ctx, cancel := context.WithTimeout(context.Background(), defaultCmdTimeout)
	defer cancel()

	result := SessionHealthResult{}

	// Check session (use "=" prefix for exact name matching)
	exactName := "=" + name
	cmd := exec.CommandContext(ctx, "tmux", tmuxArgs([]string{"has-session", "-t", exactName})...) //nolint:gosec // "tmux" is a fixed command; exactName is derived from validated session name
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
	serverCmd := exec.CommandContext(ctx, "tmux", tmuxArgs([]string{"list-sessions"})...) //nolint:gosec // "tmux" is a fixed command; arguments are constants
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

// CreateWindow creates a new window in the maestro session and returns its
// immutable window ID (@N). Callers must target the window via this ID
// instead of assuming a numeric index: with `set -g base-index 1` in the
// user's tmux.conf (which per-instance sockets still read), window indices
// do not start at 0 and any hardcoded ":N" target breaks.
func CreateWindow(name string) (string, error) {
	out, err := output("new-window", "-t", exactSessionTarget(), "-P", "-F", "#{window_id}", "-n", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// WindowActivePane returns the immutable pane ID (%N) of the window's
// active pane. For a freshly created window this is its only pane.
func WindowActivePane(windowTarget string) (string, error) {
	out, err := output("display-message", "-t", windowTarget, "-p", "#{pane_id}")
	if err != nil {
		return "", err
	}
	pane := strings.TrimSpace(out)
	if pane == "" {
		return "", fmt.Errorf("window %s: empty pane id from display-message", windowTarget)
	}
	return pane, nil
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

// FindPaneByAgentID finds the pane target for a given agent_id. The target
// is the immutable pane ID (%N), NOT the session:window.pane_index form:
// pane indices are renumbered when an operator closes a pane mid-delivery,
// which would silently retarget the multi-second send sequence (clear
// confirmation, busy retries, paste, Enter) at a different agent's pane.
// Every tmux command accepts %N targets, and downstream consumers treat the
// target as an opaque key.
func FindPaneByAgentID(agentID string) (string, error) {
	lines, err := ListAllPanes("#{pane_id}\t#{@agent_id}")
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
	// Send command text literally (no special key interpretation). The "--"
	// terminator stops tmux flag parsing so a command starting with "-" is
	// delivered as text instead of being misread as a send-keys flag.
	if err := SendKeys(paneTarget, "-l", "--", command); err != nil {
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
	if len(ids) == 0 {
		return nil, fmt.Errorf("window %s has no panes", windowTarget)
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
	if len(rowIDs) < totalRows {
		return nil, fmt.Errorf("window %s: expected %d row panes after splits, got %d", windowTarget, totalRows, len(rowIDs))
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
//
// Returns the immutable window ID (@N) of the session's initial window
// (via new-session -P -F). Callers must use this ID instead of assuming
// the initial window is index 0: `set -g base-index 1` in the user's
// tmux.conf shifts the first index and breaks hardcoded ":0" targets.
func CreateSessionWithServerOptions(windowName string, serverOptions map[string]string) (string, error) {
	debugLog("CreateSessionWithServerOptions session=%s window=%s options=%v",
		GetSessionName(), windowName, serverOptions)

	args := make([]string, 0, 9+5*len(serverOptions))
	args = append(args, "new-session", "-d", "-P", "-F", "#{window_id}", "-s", GetSessionName(), "-n", windowName)
	for name, value := range serverOptions {
		args = append(args, ";", "set-option", "-s", name, value)
	}

	out, err := output(args...)
	if err != nil {
		debugLog("CreateSessionWithServerOptions FAILED session=%s error=%v", GetSessionName(), err)
		return "", err
	}
	windowID := strings.TrimSpace(out)
	if windowID == "" {
		debugLog("CreateSessionWithServerOptions FAILED session=%s error=empty window id", GetSessionName())
		return "", fmt.Errorf("new-session: empty window id for session %q", GetSessionName())
	}
	debugLog("CreateSessionWithServerOptions OK session=%s window_id=%s", GetSessionName(), windowID)
	return windowID, nil
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
	if err := exec.CommandContext(ctx, "tmux", tmuxArgs([]string{"has-session", "-t", exactSessionTarget()})...).Run(); err != nil { //nolint:gosec // "tmux" is a fixed command; target is derived from validated session name
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
	cmd := exec.Command("tmux", tmuxArgs([]string{"attach-session", "-t", exactSessionTarget()})...) //nolint:gosec // "tmux" is a fixed command; target is derived from validated session name
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SwitchClient switches the current tmux client (we must already be inside
// tmux) to targetSession. Used when `maestro attach` is invoked from inside
// an existing tmux client, where attach-session would be rejected.
func SwitchClient(targetSession string) error {
	return run("switch-client", "-t", "="+targetSession)
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

// GetPaneCurrentPath returns the current working directory of the pane's
// active process as reported by tmux.
func GetPaneCurrentPath(paneTarget string) (string, error) {
	out, err := output("display-message", "-t", paneTarget, "-p", "#{pane_current_path}")
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
	op := args[0]
	cmd := exec.CommandContext(ctx, "tmux", tmuxArgs(args)...) //nolint:gosec // "tmux" is a fixed command; args are controlled internally
	out, err := cmd.CombinedOutput()
	if err != nil {
		stderr := strings.TrimSpace(string(out))
		if ctx.Err() != nil {
			debugLog("runCtx TIMEOUT args=%v stderr=%q", args, stderr)
			return &Error{Kind: contextErrorKind(ctx.Err()), Op: op, Stderr: stderr, Err: ctx.Err()}
		}
		classified := classifyError(op, stderr, err)
		debugLog("runCtx %s args=%v kind=%s stderr=%q", debugLevelForErrorKind(classified.Kind), args, classified.Kind, stderr)
		return classified
	}
	return nil
}

// debugLevelForErrorKind selects the prefix used by debugLog for a
// classified tmux error. Idempotent-cleanup failures (no session, no
// server) are the load-bearing case for `maestro up`'s pre-cleanup and
// `maestro down`'s post-kill restore: callers explicitly handle these
// via errors.Is(err, ErrTmuxSession/ErrTmuxServer) and continue, so they
// log at DEBUG. Genuine failures (timeout, IPC, command missing —
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
//
// stdout and stderr are kept separate (cmd.Output + ExitError.Stderr) so
// tmux warnings emitted on stderr during an otherwise-successful command can
// never leak into the returned value. Callers parse the result (pane IDs,
// content hashes, user vars); mixing in stderr — the old CombinedOutput
// behavior — silently corrupted those parses.
func outputCtx(ctx context.Context, args ...string) (string, error) {
	// Fast path: if context is already done, don't spawn a process.
	if err := ctx.Err(); err != nil {
		return "", &Error{Kind: contextErrorKind(err), Op: args[0], Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, defaultCmdTimeout)
	defer cancel()
	op := args[0]
	cmd := exec.CommandContext(ctx, "tmux", tmuxArgs(args)...) //nolint:gosec // "tmux" is a fixed command; args are controlled internally
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		if ctx.Err() != nil {
			return "", &Error{Kind: contextErrorKind(ctx.Err()), Op: op, Stderr: stderr, Err: ctx.Err()}
		}
		return "", classifyError(op, stderr, err)
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
