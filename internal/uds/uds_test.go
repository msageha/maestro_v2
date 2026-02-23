package uds

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupTestServer(t *testing.T) (*Server, *Client, string) {
	t.Helper()
	// Use /tmp directly to avoid macOS Unix socket path length limit (104 bytes)
	dir, err := os.MkdirTemp("/tmp", "maestro-uds-test-*")
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
	dir, err := os.MkdirTemp("/tmp", "m-uds-*")
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
	if !strings.Contains(err.Error(), "maestro up") {
		t.Errorf("expected hint about 'maestro up', got: %v", err)
	}
}

func TestServer_ConnectionTimeout(t *testing.T) {
	server, _, sockPath := setupTestServer(t)
	server.SetConnTimeout(500 * time.Millisecond)

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
