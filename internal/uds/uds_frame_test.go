package uds

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
)

// pipeConn creates an in-memory net.Conn pair for frame-level testing.
func pipeConn(t *testing.T) (client, server net.Conn) {
	t.Helper()
	client, server = net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return client, server
}

// --- ReadFrame malformed input tests ---

func TestReadFrame_MalformedLength_TooShort(t *testing.T) {
	client, server := pipeConn(t)

	// Write only 2 bytes instead of the required 4-byte length prefix
	go func() {
		client.Write([]byte{0x00, 0x01})
		client.Close()
	}()

	var req Request
	err := ReadFrame(server, &req)
	if err == nil {
		t.Fatal("expected error for truncated length field")
	}
	if !strings.Contains(err.Error(), "read frame length") {
		t.Errorf("expected 'read frame length' error, got: %v", err)
	}
}

func TestReadFrame_MalformedLength_Empty(t *testing.T) {
	client, server := pipeConn(t)

	// Write zero bytes then close
	go func() {
		client.Close()
	}()

	var req Request
	err := ReadFrame(server, &req)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestReadFrame_OversizeFrame(t *testing.T) {
	client, server := pipeConn(t)

	// Write a length that exceeds maxFrameSize (10MB)
	go func() {
		oversize := uint32(maxFrameSize + 1)
		binary.Write(client, binary.BigEndian, oversize)
		client.Close()
	}()

	var req Request
	err := ReadFrame(server, &req)
	if err == nil {
		t.Fatal("expected error for oversize frame")
	}
	if !strings.Contains(err.Error(), "frame too large") {
		t.Errorf("expected 'frame too large' error, got: %v", err)
	}
}

func TestReadFrame_OversizeFrame_MaxUint32(t *testing.T) {
	client, server := pipeConn(t)

	// Write maximum uint32 length value
	go func() {
		binary.Write(client, binary.BigEndian, uint32(0xFFFFFFFF))
		client.Close()
	}()

	var req Request
	err := ReadFrame(server, &req)
	if err == nil {
		t.Fatal("expected error for max uint32 frame size")
	}
	if !strings.Contains(err.Error(), "frame too large") {
		t.Errorf("expected 'frame too large' error, got: %v", err)
	}
}

func TestReadFrame_PartialPayload(t *testing.T) {
	client, server := pipeConn(t)

	// Advertise 100 bytes but only send 10, then close
	go func() {
		binary.Write(client, binary.BigEndian, uint32(100))
		client.Write([]byte("short data"))
		client.Close()
	}()

	var req Request
	err := ReadFrame(server, &req)
	if err == nil {
		t.Fatal("expected error for partial payload")
	}
	if !strings.Contains(err.Error(), "read frame payload") {
		t.Errorf("expected 'read frame payload' error, got: %v", err)
	}
}

func TestReadFrame_ZeroLengthFrame(t *testing.T) {
	client, server := pipeConn(t)

	// Send length=0, which means an empty JSON payload
	go func() {
		binary.Write(client, binary.BigEndian, uint32(0))
		client.Close()
	}()

	var req Request
	err := ReadFrame(server, &req)
	if err == nil {
		t.Fatal("expected error for zero-length frame (empty JSON)")
	}
	if !strings.Contains(err.Error(), "unmarshal frame") {
		t.Errorf("expected 'unmarshal frame' error, got: %v", err)
	}
}

func TestReadFrame_InvalidJSON(t *testing.T) {
	client, server := pipeConn(t)

	// Send valid length but invalid JSON payload
	payload := []byte("{not valid json!!!")
	go func() {
		binary.Write(client, binary.BigEndian, uint32(len(payload)))
		client.Write(payload)
		client.Close()
	}()

	var req Request
	err := ReadFrame(server, &req)
	if err == nil {
		t.Fatal("expected error for invalid JSON payload")
	}
	if !strings.Contains(err.Error(), "unmarshal frame") {
		t.Errorf("expected 'unmarshal frame' error, got: %v", err)
	}
}

// --- WriteFrame edge case tests ---

func TestWriteFrame_EmptyPayload(t *testing.T) {
	client, server := pipeConn(t)

	type empty struct{}

	go func() {
		if err := WriteFrame(client, &empty{}); err != nil {
			t.Errorf("WriteFrame empty struct: %v", err)
		}
		client.Close()
	}()

	var result map[string]any
	err := ReadFrame(server, &result)
	if err != nil {
		t.Fatalf("ReadFrame of empty payload: %v", err)
	}
}

func TestWriteFrame_NilPayload(t *testing.T) {
	client, server := pipeConn(t)

	// json.Marshal(nil) produces "null"
	go func() {
		if err := WriteFrame(client, nil); err != nil {
			t.Errorf("WriteFrame nil: %v", err)
		}
		client.Close()
	}()

	// "null" is valid JSON; ReadFrame into interface{} should succeed
	var result any
	err := ReadFrame(server, &result)
	if err != nil {
		t.Fatalf("ReadFrame of nil payload: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got: %v", result)
	}
}

func TestWriteFrame_MaxSizeBoundary(t *testing.T) {
	client, server := pipeConn(t)

	// Create a JSON string payload just under the limit.
	// JSON encoding of a string adds quotes and escaping, so we need to account for that.
	// A string of N bytes marshals to N+2 bytes (with quotes), assuming no escaping needed.
	contentSize := maxFrameSize - 2 // minus 2 for JSON quotes
	largeStr := strings.Repeat("a", contentSize)

	errCh := make(chan error, 1)
	go func() {
		errCh <- WriteFrame(client, largeStr)
		client.Close()
	}()

	var result string
	err := ReadFrame(server, &result)
	if err != nil {
		t.Fatalf("ReadFrame at max boundary: %v", err)
	}
	if len(result) != contentSize {
		t.Errorf("expected length %d, got %d", contentSize, len(result))
	}

	if writeErr := <-errCh; writeErr != nil {
		t.Fatalf("WriteFrame at max boundary: %v", writeErr)
	}
}

func TestWriteFrame_ExceedsMaxSize(t *testing.T) {
	client, _ := pipeConn(t)

	// Create a payload that exceeds maxFrameSize after marshalling
	contentSize := maxFrameSize // plus quotes = maxFrameSize + 2, which exceeds limit
	largeStr := strings.Repeat("a", contentSize)

	err := WriteFrame(client, largeStr)
	if err == nil {
		t.Fatal("expected error for oversize write")
	}
	if !strings.Contains(err.Error(), "frame too large") {
		t.Errorf("expected 'frame too large' error, got: %v", err)
	}
}
