package uds

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupTestServer(t *testing.T) (*Server, *Client, string) {
	t.Helper()
	// Use os.MkdirTemp with empty dir to respect $TMPDIR (sandbox-friendly).
	// Keep prefix short to stay within Unix socket 108-byte path limit.
	dir, err := os.MkdirTemp("", "m-uds-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "t.sock")

	server := NewServer(sockPath)
	client := NewClient(sockPath)
	client.SetTimeout(5 * time.Second)

	return server, client, sockPath
}

func shortTempSockPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "m-uds-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

func TestFraming_RoundTrip(t *testing.T) {
	sockPath := shortTempSockPath(t, "f.sock")

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		var req Request
		if err := ReadFrame(conn, &req); err != nil {
			t.Errorf("server ReadFrame: %v", err)
			return
		}

		if req.Command != "test" {
			t.Errorf("expected command %q, got %q", "test", req.Command)
		}
		if req.ProtocolVersion != ProtocolVersion {
			t.Errorf("expected protocol_version %d, got %d", ProtocolVersion, req.ProtocolVersion)
		}

		resp := SuccessResponse(map[string]string{"result": "ok"})
		if err := WriteFrame(conn, resp); err != nil {
			t.Errorf("server WriteFrame: %v", err)
		}
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req, _ := NewRequest("test", nil)
	if err := WriteFrame(conn, req); err != nil {
		t.Fatalf("client WriteFrame: %v", err)
	}

	var resp Response
	if err := ReadFrame(conn, &resp); err != nil {
		t.Fatalf("client ReadFrame: %v", err)
	}

	if !resp.Success {
		t.Error("expected success response")
	}

	<-done
}

func TestFraming_LargePayload(t *testing.T) {
	sockPath := shortTempSockPath(t, "l.sock")

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// Create a large payload (~1MB)
	largeContent := strings.Repeat("x", 1024*1024)

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		var req Request
		if err := ReadFrame(conn, &req); err != nil {
			t.Errorf("server ReadFrame: %v", err)
			return
		}

		var params map[string]string
		json.Unmarshal(req.Params, &params)
		if len(params["content"]) != 1024*1024 {
			t.Errorf("expected content length %d, got %d", 1024*1024, len(params["content"]))
		}

		resp := SuccessResponse(map[string]int{"length": len(params["content"])})
		WriteFrame(conn, resp)
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req, _ := NewRequest("large_test", map[string]string{"content": largeContent})
	if err := WriteFrame(conn, req); err != nil {
		t.Fatalf("client WriteFrame: %v", err)
	}

	var resp Response
	if err := ReadFrame(conn, &resp); err != nil {
		t.Fatalf("client ReadFrame: %v", err)
	}

	if !resp.Success {
		t.Error("expected success")
	}

	<-done
}

func TestServer_ProtocolVersionMismatch(t *testing.T) {
	server, client, _ := setupTestServer(t)

	server.Handle("ping", func(req *Request) *Response {
		return SuccessResponse(map[string]string{"status": "pong"})
	})

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	// Send request with wrong protocol version
	req := &Request{
		ProtocolVersion: 999,
		Command:         "ping",
	}

	resp, err := client.Send(req)
	if err != nil {
		t.Fatalf("client send: %v", err)
	}

	if resp.Success {
		t.Error("expected failure for version mismatch")
	}
	if resp.Error == nil {
		t.Fatal("expected error detail")
	}
	if resp.Error.Code != ErrCodeProtocolMismatch {
		t.Errorf("expected code %q, got %q", ErrCodeProtocolMismatch, resp.Error.Code)
	}
}

func TestServer_UnknownCommand(t *testing.T) {
	server, client, _ := setupTestServer(t)

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	resp, err := client.SendCommand("nonexistent", nil)
	if err != nil {
		t.Fatalf("client send: %v", err)
	}

	if resp.Success {
		t.Error("expected failure for unknown command")
	}
	if resp.Error.Code != ErrCodeUnknownCommand {
		t.Errorf("expected code %q, got %q", ErrCodeUnknownCommand, resp.Error.Code)
	}
}

func TestServer_HandlerExecution(t *testing.T) {
	server, client, _ := setupTestServer(t)

	server.Handle("ping", func(req *Request) *Response {
		return SuccessResponse(map[string]string{"status": "pong"})
	})

	server.Handle("echo", func(req *Request) *Response {
		var params map[string]string
		json.Unmarshal(req.Params, &params)
		return SuccessResponse(params)
	})

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	// Test ping
	resp, err := client.SendCommand("ping", nil)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !resp.Success {
		t.Error("ping: expected success")
	}

	var pingData map[string]string
	json.Unmarshal(resp.Data, &pingData)
	if pingData["status"] != "pong" {
		t.Errorf("ping: got %q", pingData["status"])
	}

	// Test echo
	resp, err = client.SendCommand("echo", map[string]string{"msg": "hello"})
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if !resp.Success {
		t.Error("echo: expected success")
	}

	var echoData map[string]string
	json.Unmarshal(resp.Data, &echoData)
	if echoData["msg"] != "hello" {
		t.Errorf("echo: got %q", echoData["msg"])
	}
}

func TestServer_MultipleClients(t *testing.T) {
	server, _, sockPath := setupTestServer(t)

	server.Handle("ping", func(req *Request) *Response {
		return SuccessResponse(map[string]string{"status": "pong"})
	})

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	// Send from multiple clients concurrently
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			c := NewClient(sockPath)
			c.SetTimeout(5 * time.Second)
			resp, err := c.SendCommand("ping", nil)
			if err != nil {
				errs <- err
				return
			}
			if !resp.Success {
				errs <- nil // report but don't fail
				return
			}
			errs <- nil
		}()
	}

	for i := 0; i < 10; i++ {
		if err := <-errs; err != nil {
			t.Errorf("client %d: %v", i, err)
		}
	}
}

func TestClient_DaemonNotRunning(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "nonexistent.sock")

	client := NewClient(sockPath)
	client.SetTimeout(1 * time.Second)

	_, err := client.SendCommand("ping", nil)
	if err == nil {
		t.Fatal("expected error when daemon not running")
	}
	if !strings.Contains(err.Error(), "failed to connect to daemon") {
		t.Errorf("expected daemon connection error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "maestro daemon") {
		t.Errorf("expected hint about 'maestro daemon', got: %v", err)
	}
}

func TestServer_ConnectionTimeout(t *testing.T) {
	server, _, sockPath := setupTestServer(t)
	// Server uses default connTimeout (30s). Override the field directly for test.
	server.connTimeout = 500 * time.Millisecond

	server.Handle("slow", func(req *Request) *Response {
		return SuccessResponse(nil)
	})

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	// Connect but don't send anything — server should timeout the connection
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Wait for server to timeout the connection
	time.Sleep(800 * time.Millisecond)

	// Verify the idle connection was closed by the server:
	// Attempting to read from a server-closed connection should fail
	buf := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, readErr := conn.Read(buf)
	if readErr == nil {
		t.Error("expected read error on timed-out connection, but read succeeded")
	}

	// Verify server is still responsive to new clients
	client := NewClient(sockPath)
	client.SetTimeout(2 * time.Second)
	resp, err := client.SendCommand("slow", nil)
	if err != nil {
		t.Fatalf("client after timeout: %v", err)
	}
	if !resp.Success {
		t.Error("expected success after timeout recovery")
	}
}

func TestServer_SocketPermissions(t *testing.T) {
	server, _, sockPath := setupTestServer(t)

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	info, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Check permissions (mask off socket type bits)
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("expected permissions 0600, got %04o", perm)
	}
}

func TestServer_StopCleansUpSocket(t *testing.T) {
	server, _, sockPath := setupTestServer(t)

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}

	// Verify socket exists
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket should exist: %v", err)
	}

	server.Stop()

	// Verify socket is removed
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Error("socket should be removed after stop")
	}
}

func TestErrorResponse(t *testing.T) {
	resp := ErrorResponse(ErrCodeInternal, "something failed")
	if resp.Success {
		t.Error("expected failure")
	}
	if resp.Error.Code != ErrCodeInternal {
		t.Errorf("code: got %q", resp.Error.Code)
	}
	if resp.Error.Message != "something failed" {
		t.Errorf("message: got %q", resp.Error.Message)
	}
}

func TestSuccessResponse_WithData(t *testing.T) {
	resp := SuccessResponse(map[string]int{"count": 42})
	if !resp.Success {
		t.Error("expected success")
	}

	var data map[string]int
	json.Unmarshal(resp.Data, &data)
	if data["count"] != 42 {
		t.Errorf("count: got %d", data["count"])
	}
}

func TestSuccessResponse_NilData(t *testing.T) {
	resp := SuccessResponse(nil)
	if !resp.Success {
		t.Error("expected success")
	}
	if resp.Data != nil {
		t.Errorf("expected nil data, got %s", string(resp.Data))
	}
}

// --- SendContext tests ---

func TestSendContext_NormalOperation(t *testing.T) {
	server, _, sockPath := setupTestServer(t)

	server.Handle("ping", func(req *Request) *Response {
		return SuccessResponse(map[string]string{"status": "pong"})
	})

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	client := NewClient(sockPath)
	client.SetTimeout(5 * time.Second)

	ctx := context.Background()
	req, err := NewRequest("ping", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := client.SendContext(ctx, req)
	if err != nil {
		t.Fatalf("send context: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}

	var data map[string]string
	json.Unmarshal(resp.Data, &data)
	if data["status"] != "pong" {
		t.Errorf("expected pong, got %q", data["status"])
	}
}

func TestSendContext_NilContext(t *testing.T) {
	server, _, sockPath := setupTestServer(t)

	server.Handle("ping", func(req *Request) *Response {
		return SuccessResponse(nil)
	})

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	client := NewClient(sockPath)
	client.SetTimeout(5 * time.Second)

	req, _ := NewRequest("ping", nil)
	//nolint:staticcheck // testing nil context path intentionally
	resp, err := client.SendContext(nil, req)
	if err != nil {
		t.Fatalf("send context nil: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}
}

func TestSendContext_CancelBeforeDial(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "no.sock")

	client := NewClient(sockPath)
	client.SetTimeout(5 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	req, _ := NewRequest("ping", nil)
	_, err := client.SendContext(ctx, req)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestSendContext_CancelDuringOperation(t *testing.T) {
	server, _, sockPath := setupTestServer(t)

	// Handler that blocks until context is done
	server.Handle("slow", func(req *Request) *Response {
		time.Sleep(5 * time.Second)
		return SuccessResponse(nil)
	})

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	client := NewClient(sockPath)
	client.SetTimeout(10 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req, _ := NewRequest("slow", nil)
	_, err := client.SendContext(ctx, req)
	if err == nil {
		t.Fatal("expected error for context timeout")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got: %v", err)
	}
}

func TestSendCommandContext_NormalOperation(t *testing.T) {
	server, _, sockPath := setupTestServer(t)

	server.Handle("echo", func(req *Request) *Response {
		var params map[string]string
		json.Unmarshal(req.Params, &params)
		return SuccessResponse(params)
	})

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	client := NewClient(sockPath)
	client.SetTimeout(5 * time.Second)

	resp, err := client.SendCommandContext(context.Background(), "echo", map[string]string{"msg": "hi"})
	if err != nil {
		t.Fatalf("send command context: %v", err)
	}
	if !resp.Success {
		t.Error("expected success")
	}

	var data map[string]string
	json.Unmarshal(resp.Data, &data)
	if data["msg"] != "hi" {
		t.Errorf("expected 'hi', got %q", data["msg"])
	}
}

func TestSendContext_ServerNotRunning(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "no.sock")

	client := NewClient(sockPath)
	client.SetTimeout(1 * time.Second)

	ctx := context.Background()
	req, _ := NewRequest("ping", nil)
	_, err := client.SendContext(ctx, req)
	if err == nil {
		t.Fatal("expected error when server not running")
	}
	if !strings.Contains(err.Error(), "failed to connect to daemon") {
		t.Errorf("expected daemon connection error, got: %v", err)
	}
}

// --- Server lifecycle tests ---

func TestServer_StartStopStart(t *testing.T) {
	dir, err := os.MkdirTemp("", "m-uds-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "t.sock")

	// First lifecycle
	server1 := NewServer(sockPath)
	server1.Handle("ping", func(req *Request) *Response {
		return SuccessResponse(map[string]string{"round": "1"})
	})
	if err := server1.Start(); err != nil {
		t.Fatalf("start 1: %v", err)
	}

	client := NewClient(sockPath)
	client.SetTimeout(2 * time.Second)

	resp, err := client.SendCommand("ping", nil)
	if err != nil {
		t.Fatalf("round 1 send: %v", err)
	}
	if !resp.Success {
		t.Error("round 1: expected success")
	}

	server1.Stop()

	// Second lifecycle on same socket path
	server2 := NewServer(sockPath)
	server2.Handle("ping", func(req *Request) *Response {
		return SuccessResponse(map[string]string{"round": "2"})
	})
	if err := server2.Start(); err != nil {
		t.Fatalf("start 2: %v", err)
	}
	defer server2.Stop()

	resp, err = client.SendCommand("ping", nil)
	if err != nil {
		t.Fatalf("round 2 send: %v", err)
	}
	if !resp.Success {
		t.Error("round 2: expected success")
	}

	var data map[string]string
	json.Unmarshal(resp.Data, &data)
	if data["round"] != "2" {
		t.Errorf("expected round 2, got %q", data["round"])
	}
}

func TestServer_HandlerPanicRecovery(t *testing.T) {
	server, client, _ := setupTestServer(t)

	server.Handle("panic", func(req *Request) *Response {
		panic("test panic")
	})

	server.Handle("ping", func(req *Request) *Response {
		return SuccessResponse(nil)
	})

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	// Trigger panic handler
	resp, err := client.SendCommand("panic", nil)
	if err != nil {
		t.Fatalf("panic send: %v", err)
	}
	if resp.Success {
		t.Error("expected failure for panicking handler")
	}
	if resp.Error == nil {
		t.Fatal("expected error detail")
	}
	if resp.Error.Code != ErrCodeInternal {
		t.Errorf("expected code %q, got %q", ErrCodeInternal, resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "panic") {
		t.Errorf("expected panic in error message, got: %q", resp.Error.Message)
	}

	// Server should still be responsive after panic
	resp, err = client.SendCommand("ping", nil)
	if err != nil {
		t.Fatalf("post-panic send: %v", err)
	}
	if !resp.Success {
		t.Error("expected success after panic recovery")
	}
}

func TestServer_Backpressure(t *testing.T) {
	dir, err := os.MkdirTemp("", "m-uds-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "t.sock")

	server := NewServer(sockPath)
	// Set maxConns to 1 so we can easily trigger backpressure
	server.maxConns = 1

	blocker := make(chan struct{})
	server.Handle("block", func(req *Request) *Response {
		<-blocker
		return SuccessResponse(nil)
	})

	if err := server.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	defer server.Stop()

	// First client occupies the single connection slot
	c1 := NewClient(sockPath)
	c1.SetTimeout(5 * time.Second)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c1.SendCommand("block", nil)
	}()

	// Give time for first connection to be accepted
	time.Sleep(100 * time.Millisecond)

	// Second client should get backpressure response
	c2 := NewClient(sockPath)
	c2.SetTimeout(2 * time.Second)
	resp, err := c2.SendCommand("block", nil)

	// Unblock first client
	close(blocker)
	wg.Wait()

	if err != nil {
		t.Fatalf("backpressure client: %v", err)
	}
	if resp.Success {
		t.Error("expected failure for backpressure")
	}
	if resp.Error == nil {
		t.Fatal("expected error detail")
	}
	if resp.Error.Code != ErrCodeBackpressure {
		t.Errorf("expected code %q, got %q", ErrCodeBackpressure, resp.Error.Code)
	}
}

func TestClient_SendRetryOnTransientError(t *testing.T) {
	dir, err := os.MkdirTemp("", "m-uds-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "t.sock")

	// Start server after a short delay to simulate transient ENOENT
	server := NewServer(sockPath)
	server.Handle("ping", func(req *Request) *Response {
		return SuccessResponse(map[string]string{"status": "pong"})
	})

	go func() {
		time.Sleep(250 * time.Millisecond)
		server.Start()
	}()
	defer server.Stop()

	client := NewClient(sockPath)
	client.SetTimeout(5 * time.Second)

	// Send will retry on ENOENT and eventually succeed once server starts
	resp, err := client.SendCommand("ping", nil)
	if err != nil {
		t.Fatalf("expected retry success, got: %v", err)
	}
	if !resp.Success {
		t.Error("expected success after retry")
	}
}

func TestNewRequest_MarshalError(t *testing.T) {
	// channels cannot be JSON marshalled
	_, err := NewRequest("test", make(chan int))
	if err == nil {
		t.Fatal("expected error for unmarshallable params")
	}
	if !strings.Contains(err.Error(), "marshal params") {
		t.Errorf("expected marshal error, got: %v", err)
	}
}

func TestSuccessResponse_MarshalError(t *testing.T) {
	// channels cannot be JSON marshalled
	resp := SuccessResponse(make(chan int))
	if resp.Success {
		t.Error("expected failure for unmarshallable data")
	}
	if resp.Error == nil {
		t.Fatal("expected error detail")
	}
	if resp.Error.Code != ErrCodeInternal {
		t.Errorf("expected code %q, got %q", ErrCodeInternal, resp.Error.Code)
	}
}
