package uds

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
)

// CallerRoleEnv is the environment variable from which CLI clients populate
// Request.CallerRole. The agent launcher sets this when spawning role-specific
// claude processes so that downstream maestro CLI invocations carry an
// authenticated role hint to the daemon.
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

// ValidateCallerRole checks that role is either empty (treated as RoleCLI for
// direct CLI invocations where MAESTRO_AGENT_ROLE is unset) or a known role in
// ValidCallerRoles. Returns an error for unknown or improperly-cased roles.
func ValidateCallerRole(role string) error {
	if role == "" {
		return nil // empty is allowed; normalized to RoleCLI by NormalizeCallerRole
	}
	if !ValidCallerRoles[role] {
		return fmt.Errorf("invalid caller role %q: must be one of orchestrator, planner, worker, cli", role)
	}
	return nil
}

// NormalizeCallerRole returns RoleCLI for an empty string (direct CLI
// invocation without MAESTRO_AGENT_ROLE set), otherwise returns the role
// unchanged. Call ValidateCallerRole first to reject unknown roles.
func NormalizeCallerRole(role string) string {
	if role == "" {
		return RoleCLI
	}
	return role
}

// ProtocolVersion is the current version of the UDS wire protocol.
const ProtocolVersion = 1

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
type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
// The CallerRole is read from the MAESTRO_AGENT_ROLE environment variable,
// validated against ValidCallerRoles, and normalized (empty → "cli").
func newRequest(command string, params any) (*Request, error) {
	role := os.Getenv(CallerRoleEnv)
	if err := ValidateCallerRole(role); err != nil {
		return nil, err
	}
	req := &Request{
		ProtocolVersion: ProtocolVersion,
		Command:         command,
		CallerRole:      NormalizeCallerRole(role),
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
