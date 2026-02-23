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

const ProtocolVersion = 1

type Request struct {
	ProtocolVersion int             `json:"protocol_version"`
	Command         string          `json:"command"`
	Params          json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   *ErrorDetail    `json:"error,omitempty"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	ErrCodeProtocolMismatch = "PROTOCOL_MISMATCH"
	ErrCodeUnknownCommand   = "UNKNOWN_COMMAND"
	ErrCodeInternal         = "INTERNAL_ERROR"
	ErrCodeBackpressure     = "BACKPRESSURE"
	ErrCodeValidation       = "VALIDATION_ERROR"
	ErrCodeNotFound         = "NOT_FOUND"
	ErrCodeFencingReject    = "FENCING_REJECT"
	ErrCodeDuplicate        = "DUPLICATE"
	ErrCodeCancelled        = "CANCELLED"
)

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

func SuccessResponse(data any) *Response {
	resp := &Response{Success: true}
	if data != nil {
		raw, _ := json.Marshal(data)
		resp.Data = raw
	}
	return resp
}

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

// WriteFrame writes a length-prefixed JSON frame to the connection.
// Format: [4-byte BigEndian length][JSON payload]
func WriteFrame(conn net.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
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

	if length > 10*1024*1024 { // 10MB safety limit
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
