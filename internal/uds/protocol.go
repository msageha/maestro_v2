// Package uds implements Unix Domain Socket based IPC between the CLI and daemon.
package uds

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// ProtocolVersion is the current version of the UDS wire protocol.
const ProtocolVersion = 1

// Request represents an IPC request sent from a CLI client to the daemon.
type Request struct {
	ProtocolVersion int             `json:"protocol_version"`
	Command         string          `json:"command"`
	Params          json.RawMessage `json:"params,omitempty"`
}

// Response represents an IPC response returned from the daemon to a CLI client.
type Response struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   *ErrorDetail    `json:"error,omitempty"`
}

// ErrorDetail contains a machine-readable error code and a human-readable message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error code constants used in ErrorDetail.Code to classify failures.
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
	// ErrCodeDuplicate indicates a duplicate resource or operation was detected.
	ErrCodeDuplicate = "DUPLICATE"
	// ErrCodeCancelled indicates the operation was cancelled.
	ErrCodeCancelled = "CANCELLED"
	// ErrCodeActionRequired indicates the caller must take an action before retrying.
	ErrCodeActionRequired = "ACTION_REQUIRED"
	// ErrCodeMaxRuntimeExceeded indicates the operation exceeded its maximum allowed runtime.
	ErrCodeMaxRuntimeExceeded = "MAX_RUNTIME_EXCEEDED"
)

// NewRequest creates a new Request with the given command and optional params marshalled to JSON.
func NewRequest(command string, params any) (*Request, error) {
	req := &Request{
		ProtocolVersion: ProtocolVersion,
		Command:         command,
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
		Error: &ErrorDetail{
			Code:    code,
			Message: message,
		},
	}
}

// DefaultSocketName is the conventional socket filename inside .maestro/.
const DefaultSocketName = "daemon.sock"

// maxFrameSize is the safety limit for frame payloads (10 MB).
const maxFrameSize = 10 * 1024 * 1024

// WriteFrame writes a length-prefixed JSON frame to the connection.
// Format: [4-byte BigEndian length][JSON payload]
func WriteFrame(conn net.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}

	if len(data) > maxFrameSize {
		return fmt.Errorf("frame too large: %d bytes exceeds %d byte limit", len(data), maxFrameSize)
	}

	length := uint32(len(data))
	if err := binary.Write(conn, binary.BigEndian, length); err != nil {
		return fmt.Errorf("write frame length: %w", err)
	}
	// Use io.Copy to guarantee all bytes are written (handles short writes)
	if _, err := io.Copy(conn, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}
	return nil
}

// ReadFrame reads a length-prefixed JSON frame from the connection.
func ReadFrame(conn net.Conn, v any) error {
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
