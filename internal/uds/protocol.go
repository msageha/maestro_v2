package uds

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// CallerRoleEnv is the primary environment variable from which CLI clients
// resolve Request.CallerRole. The agent launcher sets this when spawning
// role-specific claude processes. If it is absent inside a managed tmux pane,
// ResolveCallerRole falls back to the pane-local @role value.
//
// SECURITY NOTE: This is an *advisory hint*, NOT an authenticated credential.
// The value is read from a process-environment variable that any local
// process running as the same user can set or override before invoking the
// maestro CLI. There is no cryptographic binding between the role string and
// the calling process — the daemon trusts the value because the UDS socket
// is mode 0600 and so only processes running as the same UNIX user can
// connect at all. The trust boundary is therefore "same UNIX user" — within
// that boundary, role separation is best-effort and prevents *honest*
// mistakes (e.g. a Worker accidentally running an Orchestrator-only command),
// but it does not defend against a same-user adversary.
//
// Do not document this field as "authenticated" anywhere in the code base.
const CallerRoleEnv = "MAESTRO_AGENT_ROLE"

// CallerRole constants define the valid values for the MAESTRO_AGENT_ROLE
// environment variable and the Request.CallerRole field.
const (
	RoleOrchestrator = "orchestrator"
	RolePlanner      = "planner"
	RoleWorker       = "worker"
	RoleCLI          = "cli"
)

// ValidCallerRoles is the authoritative whitelist of caller roles accepted by
// the system. Roles are case-sensitive lowercase strings. Any CallerRole value
// not in this set is rejected at the protocol level.
var ValidCallerRoles = map[string]bool{
	RoleOrchestrator: true,
	RolePlanner:      true,
	RoleWorker:       true,
	RoleCLI:          true,
}

// ValidateCallerRole checks that role is either empty or a known role in
// ValidCallerRoles. Empty is accepted for direct CLI invocations and is
// resolved by ResolveCallerRole.
func ValidateCallerRole(role string) error {
	if role == "" {
		return nil
	}
	if !ValidCallerRoles[role] {
		return fmt.Errorf("invalid caller role %q: must be one of orchestrator, planner, worker, cli", role)
	}
	return nil
}

// NormalizeCallerRole returns RoleCLI for an empty string, otherwise returns
// the role unchanged. Call ValidateCallerRole first to reject unknown roles.
func NormalizeCallerRole(role string) string {
	if role == "" {
		return RoleCLI
	}
	return role
}

// ResolveCallerRole returns the most reliable caller role available to a CLI
// process. MAESTRO_AGENT_ROLE is the primary signal set by the launcher. When
// it is absent but the process is still inside a Maestro-managed tmux pane,
// the pane-local @role option is used so `env -u MAESTRO_AGENT_ROLE maestro ...`
// does not accidentally regain operator/CLI privileges. Claude Code's Bash
// sandbox can strip both variables from tool subprocesses, so as a final
// best-effort fallback we walk the parent process chain and read the launcher-set
// role from the ancestor Claude process environment.
func ResolveCallerRole() (string, error) {
	role := os.Getenv(CallerRoleEnv)
	if role == "" {
		role = inferCallerRoleFromTmuxPane()
	}
	if role == "" {
		role = inferCallerRoleFromProcessAncestors()
	}
	if err := ValidateCallerRole(role); err != nil {
		return "", err
	}
	return NormalizeCallerRole(role), nil
}

func inferCallerRoleFromTmuxPane() string {
	paneID := os.Getenv("TMUX_PANE")
	if paneID == "" {
		return ""
	}
	if !validTmuxPaneID(paneID) {
		return ""
	}
	// Per-instance socket isolation (Report 2026-05-06 round-4 LOW): each project's
	// tmux server runs on `tmux -L <socket>`, so a query against the default
	// socket cannot see the pane. We extract the absolute socket path from
	// the TMUX env (`<socket_path>,<pid>,<session_id>` — set by the tmux
	// client itself, so it is trustworthy) and pass it as `-S <socket_path>`.
	args := []string{"display-message", "-t", paneID, "-p", "#{@role}"}
	if socketPath := socketPathFromTmuxEnv(os.Getenv("TMUX")); socketPath != "" {
		args = append([]string{"-S", socketPath}, args...)
	}
	out, err := exec.Command("tmux", args...).Output() //nolint:gosec // paneID is constrained to tmux's %<digits> form; socketPath comes from TMUX env set by tmux itself.
	if err != nil {
		return ""
	}
	role := strings.TrimSpace(string(out))
	if !ValidCallerRoles[role] {
		return ""
	}
	return role
}

// socketPathFromTmuxEnv parses the TMUX env variable (set by tmux for
// processes running inside a pane) to extract the absolute socket path
// the parent tmux server is listening on. Format is
// `<socket_path>,<server_pid>,<session_id>`. Returns "" when TMUX is
// unset or malformed (caller falls back to default socket).
func socketPathFromTmuxEnv(env string) string {
	if env == "" {
		return ""
	}
	idx := strings.IndexByte(env, ',')
	if idx <= 0 {
		return ""
	}
	return env[:idx]
}

func validTmuxPaneID(s string) bool {
	if len(s) < 2 || s[0] != '%' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func inferCallerRoleFromProcessAncestors() string {
	pid := os.Getppid()
	for depth := 0; depth < 12 && pid > 1; depth++ {
		if role := callerRoleFromProcessEnv(pid); role != "" {
			return role
		}
		next, ok := parentPID(pid)
		if !ok || next == pid {
			return ""
		}
		pid = next
	}
	return ""
}

func callerRoleFromProcessEnv(pid int) string {
	out, err := exec.Command("ps", "eww", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // pid comes from os.Getppid/ps and is rendered as a decimal.
	if err != nil {
		return ""
	}
	return extractCallerRoleFromProcessListing(string(out))
}

func parentPID(pid int) (int, bool) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // pid comes from os.Getppid/ps and is rendered as a decimal.
	if err != nil {
		return 0, false
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || ppid <= 0 {
		return 0, false
	}
	return ppid, true
}

func extractCallerRoleFromProcessListing(listing string) string {
	marker := CallerRoleEnv + "="
	for _, field := range strings.Fields(listing) {
		if !strings.HasPrefix(field, marker) {
			continue
		}
		role := strings.TrimPrefix(field, marker)
		if ValidCallerRoles[role] {
			return role
		}
	}
	return ""
}

// ProtocolVersion is the current version of the UDS wire protocol.
// This value is carried both as a wire-level version byte prefix and inside
// the JSON Request.ProtocolVersion field. The wire-level byte enables early
// rejection before JSON parsing; the JSON field remains for backward
// compatibility with legacy (v0) clients that omit the version prefix.
const ProtocolVersion = 1

// WireVersion is the version byte written at the start of each client request.
// Legacy clients (v0) omit this byte; the server detects legacy frames by
// checking whether the first byte is 0x00 (which is always the high byte of
// the 4-byte big-endian length for any payload ≤ 16 MB).
const WireVersion byte = 1

// Request represents an IPC request sent from a CLI client to the daemon.
type Request struct {
	ProtocolVersion int             `json:"protocol_version"`
	Command         string          `json:"command"`
	Params          json.RawMessage `json:"params,omitempty"`
	// CallerRole identifies the role of the caller (orchestrator, planner,
	// worker, cli, etc.). Populated by the CLI client from the
	// MAESTRO_AGENT_ROLE environment variable, which the agent launcher sets
	// per-pane. Empty when invoked from a plain shell. Used by the daemon to
	// enforce trust boundaries on operator-recovery commands so that worker
	// agents cannot invoke them even if they bypass the launcher/policy hook
	// layers.
	CallerRole string `json:"caller_role,omitempty"`
}

// Response represents an IPC response returned from the daemon to a CLI client.
type Response struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   *errorDetail    `json:"error,omitempty"`
}

// errorDetail contains a machine-readable error code and a human-readable message.
//
// Details: an optional structured payload that lets the daemon surface
// fencing context (e.g. current_epoch, current_status) without the CLI
// having to parse it back out of Message. Older daemons leave this nil and
// existing CLI consumers ignore it, so the addition is backwards
// compatible.
type errorDetail struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details,omitempty"`
}

// FencingDetails is the canonical schema for the `details` payload of
// fencing-related error responses. Embedding this lets CLI consumers
// branch on machine-readable fields instead of grepping the message string.
//
// Field semantics:
//   - Kind: short stable token; one of "fencing_epoch_mismatch",
//     "fencing_status_mismatch", "max_runtime_exceeded".
//   - CurrentEpoch / RequestEpoch: lease_epoch values from the queue and
//     from the rejected request respectively. Always populated for epoch
//     mismatches; omitted (zero) for non-epoch fencing kinds.
//   - CurrentStatus: the queue task's current status (e.g. "completed",
//     "cancelled", "in_progress"); populated for status mismatches.
//   - TaskID / WorkerID: helps the CLI / worker.md flow correlate the
//     reject with its own pending request without re-parsing the message.
type FencingDetails struct {
	Kind          string `json:"kind"`
	TaskID        string `json:"task_id,omitempty"`
	WorkerID      string `json:"worker_id,omitempty"`
	CurrentEpoch  int    `json:"current_epoch,omitempty"`
	RequestEpoch  int    `json:"request_epoch,omitempty"`
	CurrentStatus string `json:"current_status,omitempty"`
}

// Error code constants used in errorDetail.Code to classify failures.
const (
	// ErrCodeProtocolMismatch indicates a protocol version mismatch between client and server.
	ErrCodeProtocolMismatch = "PROTOCOL_MISMATCH"
	// ErrCodeUnknownCommand indicates the requested command is not registered on the server.
	ErrCodeUnknownCommand = "UNKNOWN_COMMAND"
	// ErrCodeInternal indicates an unexpected internal server error.
	ErrCodeInternal = "INTERNAL_ERROR"
	// ErrCodeBackpressure indicates the server is at capacity and cannot accept new connections.
	ErrCodeBackpressure = "BACKPRESSURE"
	// ErrCodeValidation indicates the request parameters failed validation.
	ErrCodeValidation = "VALIDATION_ERROR"
	// ErrCodeNotFound indicates the requested resource was not found.
	ErrCodeNotFound = "NOT_FOUND"
	// ErrCodeFencingReject indicates the request was rejected due to a fencing token conflict.
	ErrCodeFencingReject = "FENCING_REJECT"
	// ErrCodeFencingRejectStatus indicates the request was rejected because the task status is not in_progress.
	ErrCodeFencingRejectStatus = "FENCING_REJECT_STATUS"
	// ErrCodeFencingRejectEpoch indicates the request was rejected due to a lease epoch mismatch.
	ErrCodeFencingRejectEpoch = "FENCING_REJECT_EPOCH"
	// ErrCodeDuplicate indicates a duplicate resource or operation was detected.
	ErrCodeDuplicate = "DUPLICATE"
	// ErrCodeActionRequired indicates the caller must take an action before retrying.
	ErrCodeActionRequired = "ACTION_REQUIRED"
	// ErrCodeMaxRuntimeExceeded indicates the operation exceeded its maximum allowed runtime.
	ErrCodeMaxRuntimeExceeded = "MAX_RUNTIME_EXCEEDED"
)

// newRequest creates a new Request with the given command and optional params marshalled to JSON.
// The CallerRole is resolved from MAESTRO_AGENT_ROLE or, inside managed tmux
// panes, from the pane-local @role value. Plain shells resolve to "cli".
func newRequest(command string, params any) (*Request, error) {
	role, err := ResolveCallerRole()
	if err != nil {
		return nil, err
	}
	req := &Request{
		ProtocolVersion: ProtocolVersion,
		Command:         command,
		CallerRole:      role,
	}
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		req.Params = data
	}
	return req, nil
}

// SuccessResponse creates a Response with Success set to true and the given data marshalled to JSON.
func SuccessResponse(data any) *Response {
	resp := &Response{Success: true}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return ErrorResponse(ErrCodeInternal, fmt.Sprintf("marshal response data: %v", err))
		}
		resp.Data = raw
	}
	return resp
}

// ErrorResponse creates a Response with Success set to false and the given error code and message.
func ErrorResponse(code, message string) *Response {
	return &Response{
		Success: false,
		Error: &errorDetail{
			Code:    code,
			Message: message,
		},
	}
}

// ErrorResponseWithDetails is the structured-payload variant of ErrorResponse.
// `details` is marshalled as JSON and stored in errorDetail.Details. On
// marshal failure the function falls back to ErrorResponse so the caller
// always observes a well-formed error.
//
// Use this for fencing-related rejects (heartbeat / result_write epoch or
// status mismatch, max_runtime_exceeded) so the CLI / Worker can branch on
// machine-readable fields without grepping Message.
func ErrorResponseWithDetails(code, message string, details any) *Response {
	if details == nil {
		return ErrorResponse(code, message)
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return ErrorResponse(code, message)
	}
	return &Response{
		Success: false,
		Error: &errorDetail{
			Code:    code,
			Message: message,
			Details: raw,
		},
	}
}

// ErrorDetails returns the marshalled details payload of an error response,
// or nil if no details were attached. Helper for CLI / test consumers that
// want to read the structured payload without exporting errorDetail.
func (r *Response) ErrorDetails() json.RawMessage {
	if r == nil || r.Error == nil {
		return nil
	}
	return r.Error.Details
}

// DefaultSocketName is the conventional socket filename inside .maestro/.
const DefaultSocketName = "daemon.sock"

// maxFrameSize is the safety limit for frame payloads (2 MB).
// Typical IPC messages (YAML task definitions, result reports) are well under 100 KB.
// 2 MB provides headroom for large task content while preventing runaway allocations.
const maxFrameSize = 2 * 1024 * 1024

// writeFrame writes a length-prefixed JSON frame to the connection.
// Format: [4-byte BigEndian length][JSON payload]
func writeFrame(conn net.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}

	if len(data) > maxFrameSize {
		return fmt.Errorf("frame too large: %d bytes exceeds %d byte limit", len(data), maxFrameSize)
	}

	length := uint32(len(data)) //nolint:gosec // len(data) is bounded by maxFrameSize check above
	if err := binary.Write(conn, binary.BigEndian, length); err != nil {
		return fmt.Errorf("write frame length: %w", err)
	}
	// Use io.Copy to guarantee all bytes are written (handles short writes)
	if _, err := io.Copy(conn, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}
	return nil
}

// readFrame reads a length-prefixed JSON frame from the connection.
func readFrame(conn net.Conn, v any) error {
	var length uint32
	if err := binary.Read(conn, binary.BigEndian, &length); err != nil {
		return fmt.Errorf("read frame length: %w", err)
	}

	if length > maxFrameSize {
		return fmt.Errorf("frame too large: %d bytes", length)
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fmt.Errorf("read frame payload: %w", err)
	}

	if err := json.Unmarshal(buf, v); err != nil {
		return fmt.Errorf("unmarshal frame: %w", err)
	}
	return nil
}

// writeVersionedFrame writes a version-byte-prefixed, length-prefixed JSON
// frame to the connection. The wire format is:
//
//	[1 byte: WireVersion][4 byte BE length][JSON payload]
func writeVersionedFrame(conn net.Conn, v any) error {
	if _, err := conn.Write([]byte{WireVersion}); err != nil {
		return fmt.Errorf("write version byte: %w", err)
	}
	return writeFrame(conn, v)
}

// readVersionedRequest reads a request that may or may not carry a version-byte
// prefix. It peeks at the first byte to distinguish:
//
//   - 0x00 → legacy (v0) client: the byte is the high byte of the 4-byte
//     big-endian length prefix (always 0x00 for payloads ≤ 16 MB).
//   - ≥ 1  → versioned client: the byte is the wire version number, followed
//     by a standard length-prefixed frame.
//
// Returns the detected wire version (0 for legacy) and any error.
func readVersionedRequest(conn net.Conn, v any) (byte, error) {
	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil {
		return 0, fmt.Errorf("read first byte: %w", err)
	}

	if first[0] == 0x00 {
		// Legacy v0: first byte is high byte of 4-byte BE length.
		// Read remaining 3 bytes to reconstruct the full length.
		var rest [3]byte
		if _, err := io.ReadFull(conn, rest[:]); err != nil {
			return 0, fmt.Errorf("read frame length: %w", err)
		}
		length := binary.BigEndian.Uint32([]byte{first[0], rest[0], rest[1], rest[2]})

		if length > maxFrameSize {
			return 0, fmt.Errorf("frame too large: %d bytes", length)
		}

		buf := make([]byte, length)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return 0, fmt.Errorf("read frame payload: %w", err)
		}

		if err := json.Unmarshal(buf, v); err != nil {
			return 0, fmt.Errorf("unmarshal frame: %w", err)
		}
		return 0, nil
	}

	// Versioned: first byte is the wire version number.
	wireVersion := first[0]
	if err := readFrame(conn, v); err != nil {
		return wireVersion, err
	}
	return wireVersion, nil
}
