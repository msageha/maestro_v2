package uds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
)

// handlerFunc is the function signature for command handlers registered on a Server.
type handlerFunc func(req *Request) *Response

// defaultMaxConcurrentConns is the default maximum number of concurrent connections.
const defaultMaxConcurrentConns = 64

// maxCommandLength is the maximum allowed length for a command name in bytes.
const maxCommandLength = 256

// backpressureWriteDeadline is the write deadline applied when sending a
// backpressure rejection response to a client.
const backpressureWriteDeadline = 1 * time.Second

// maxUnixSocketPathLen is the maximum length of a Unix domain socket path.
// POSIX defines struct sockaddr_un.sun_path as 108 bytes on Linux and most
// Unix-like systems. macOS uses 104 bytes; we use the more conservative 104
// to be safe across platforms.
const maxUnixSocketPathLen = 104

// ErrUnixSocketUnavailable marks environments where AF_UNIX sockets cannot be
// created, usually because a sandbox or container blocks socket bind.
var ErrUnixSocketUnavailable = errors.New("unix domain sockets unavailable")

// UnixSocketUnavailableError preserves the original listen error while letting
// callers use errors.Is(err, ErrUnixSocketUnavailable).
type UnixSocketUnavailableError struct {
	Path string
	Err  error
}

func (e *UnixSocketUnavailableError) Error() string {
	return fmt.Sprintf(
		"listen on %s: %v (unix domain socket creation is blocked by OS permissions; run Maestro in an environment that permits AF_UNIX sockets)",
		e.Path,
		e.Err,
	)
}

func (e *UnixSocketUnavailableError) Unwrap() error { return e.Err }

// Is implements errors.Is so callers can match against the
// ErrUnixSocketUnavailable sentinel without unwrapping the concrete
// type by hand.
func (e *UnixSocketUnavailableError) Is(target error) bool {
	return target == ErrUnixSocketUnavailable
}

// IsUnixSocketUnavailable reports whether err indicates the process cannot
// create Unix domain sockets in the current environment.
func IsUnixSocketUnavailable(err error) bool {
	return errors.Is(err, ErrUnixSocketUnavailable)
}

// Server is a Unix Domain Socket server that dispatches incoming requests to registered handlers.
type Server struct {
	socketPath  string
	listener    net.Listener
	handlers    map[string]handlerFunc
	mu          sync.RWMutex
	connTimeout time.Duration
	maxConns    int
	connSem     chan struct{}
	wg          sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
}

// NewServer creates a new Server that will listen on the given Unix socket path.
func NewServer(socketPath string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		socketPath:  socketPath,
		handlers:    make(map[string]handlerFunc),
		connTimeout: 30 * time.Second,
		maxConns:    defaultMaxConcurrentConns,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Handle registers a handlerFunc for the given command name.
func (s *Server) Handle(command string, handler handlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[command] = handler
}

// Start begins listening for connections on the configured Unix socket path.
func (s *Server) Start() error {
	if err := validateSocketPath(s.socketPath); err != nil {
		return err
	}

	// Remove stale socket file
	_ = os.Remove(s.socketPath)

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return wrapListenError(s.socketPath, err)
	}

	// Set socket permissions to 0600 after creation.
	// This avoids using syscall.Umask which is process-global and affects
	// concurrent goroutines.
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(s.socketPath)
		return fmt.Errorf("chmod socket %s: %w", s.socketPath, err)
	}

	s.listener = listener
	s.connSem = make(chan struct{}, s.maxConns)

	s.wg.Add(1)
	go s.acceptLoop()

	return nil
}

// ProbeUnixSocket verifies that the current process can bind a Unix socket at
// socketPath, then removes the probe socket. It is intended for preflight checks
// and tests that need to distinguish code failures from sandbox restrictions.
func ProbeUnixSocket(socketPath string) error {
	if err := validateSocketPath(socketPath); err != nil {
		return err
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return wrapListenError(socketPath, err)
	}
	if err := listener.Close(); err != nil {
		_ = os.Remove(socketPath)
		return fmt.Errorf("close probe socket %s: %w", socketPath, err)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove probe socket %s: %w", socketPath, err)
	}
	return nil
}

func validateSocketPath(socketPath string) error {
	if len(socketPath) > maxUnixSocketPathLen {
		return fmt.Errorf("socket path too long: %d bytes exceeds %d byte limit (path: %s)", len(socketPath), maxUnixSocketPathLen, socketPath)
	}
	return nil
}

func wrapListenError(socketPath string, err error) error {
	if isUnixSocketPermissionError(err) {
		return &UnixSocketUnavailableError{Path: socketPath, Err: err}
	}
	return fmt.Errorf("listen on %s: %w", socketPath, err)
}

func isUnixSocketPermissionError(err error) bool {
	if errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EACCES) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "permission denied")
}

// Stop gracefully shuts down the server, closing the listener and waiting for active connections to finish.
func (s *Server) Stop() error {
	s.cancel()
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			slog.Debug("failed to close listener during stop", "error", err)
		}
	}
	s.wg.Wait()
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		slog.Debug("failed to remove socket file during stop", "error", err)
	}
	return nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// Exit on shutdown context cancellation
			select {
			case <-s.ctx.Done():
				return
			default:
			}
			// Exit on permanent listener errors (e.g. listener closed)
			if errors.Is(err, net.ErrClosed) {
				slog.Info("listener closed, stopping accept loop", "error", err)
				return
			}
			// Temporary errors: log and continue
			slog.Error("accept error", "error", err)
			continue
		}

		// Semaphore acquire with context awareness: reject connection if at capacity
		select {
		case s.connSem <- struct{}{}:
			s.wg.Add(1)
			go s.handleConn(conn)
		case <-s.ctx.Done():
			_ = conn.Close() // shutdown path: nothing useful to log on close error
			return
		default:
			s.rejectConn(conn)
		}
	}
}

// rejectConn sends a backpressure error response and closes the connection.
// Uses defer to guarantee conn.Close() even if writeFrame panics.
func (s *Server) rejectConn(conn net.Conn) {
	defer func() {
		if err := conn.Close(); err != nil {
			slog.Debug("failed to close rejected connection", "error", err)
		}
	}()
	slog.Warn("connection rejected: max concurrent connections reached", "max_conns", s.maxConns)
	_ = conn.SetWriteDeadline(time.Now().Add(backpressureWriteDeadline))
	resp := ErrorResponse(ErrCodeBackpressure, "server at capacity, try again later")
	if err := writeFrame(conn, resp); err != nil {
		slog.Debug("failed to write backpressure response", "error", err)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { <-s.connSem }()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in handleConn", "panic", r, "stack", string(debug.Stack()))
			// Write error response before closing so the client does not wait indefinitely.
			resp := ErrorResponse(ErrCodeInternal, fmt.Sprintf("internal server error: panic: %v", r))
			_ = writeFrame(conn, resp)
		}
		if err := conn.Close(); err != nil {
			slog.Debug("failed to close handled connection", "error", err)
		}
	}()

	_ = conn.SetDeadline(time.Now().Add(s.connTimeout))

	var req Request
	wireVersion, err := readVersionedRequest(conn, &req)
	if err != nil {
		slog.Error("read request error", "error", err)
		return
	}

	// Reject wire versions newer than what we support.
	if wireVersion > WireVersion {
		resp := ErrorResponse(
			ErrCodeProtocolMismatch,
			fmt.Sprintf("unsupported wire version: got %d, max supported %d", wireVersion, WireVersion),
		)
		_ = writeFrame(conn, resp)
		return
	}

	resp := s.processRequest(&req)

	if err := writeFrame(conn, resp); err != nil {
		slog.Error("write response error", "error", err)
	}
}

func (s *Server) processRequest(req *Request) *Response {
	if req.ProtocolVersion != ProtocolVersion {
		return ErrorResponse(
			ErrCodeProtocolMismatch,
			fmt.Sprintf("protocol version mismatch: got %d, expected %d", req.ProtocolVersion, ProtocolVersion),
		)
	}

	if req.Command == "" {
		return ErrorResponse(ErrCodeValidation, "empty command")
	}
	if len(req.Command) > maxCommandLength {
		return ErrorResponse(
			ErrCodeValidation,
			fmt.Sprintf("command too long: %d bytes exceeds %d byte limit", len(req.Command), maxCommandLength),
		)
	}

	// Validate CallerRole before dispatching to handlers. Current CLI clients
	// always populate it via ResolveCallerRole; a missing value means the
	// request bypassed the client-side role resolution path.
	if err := ValidateCallerRole(req.CallerRole); err != nil {
		return ErrorResponse(ErrCodeValidation, err.Error())
	}
	if req.CallerRole == "" {
		return ErrorResponse(ErrCodeValidation, "caller_role is required")
	}

	s.mu.RLock()
	handler, ok := s.handlers[req.Command]
	s.mu.RUnlock()

	if !ok {
		return ErrorResponse(
			ErrCodeUnknownCommand,
			fmt.Sprintf("unknown command: %q", req.Command),
		)
	}

	return handler(req)
}
