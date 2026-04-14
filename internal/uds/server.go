package uds

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

// handlerFunc is the function signature for command handlers registered on a Server.
type handlerFunc func(req *Request) *Response

// defaultMaxConcurrentConns is the default maximum number of concurrent connections.
const defaultMaxConcurrentConns = 64

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
	// Remove stale socket file
	_ = os.Remove(s.socketPath)

	// Set umask before creating the socket so the file is created with 0600
	// permissions atomically, eliminating the TOCTOU window between
	// Listen and Chmod.
	oldUmask := syscall.Umask(0177)
	listener, err := net.Listen("unix", s.socketPath)
	syscall.Umask(oldUmask)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.socketPath, err)
	}

	s.listener = listener
	s.connSem = make(chan struct{}, s.maxConns)

	s.wg.Add(1)
	go s.acceptLoop()

	return nil
}

// Stop gracefully shuts down the server, closing the listener and waiting for active connections to finish.
func (s *Server) Stop() error {
	s.cancel()
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			log.Printf("DEBUG: failed to close listener during stop: %v", err)
		}
	}
	s.wg.Wait()
	if err := os.Remove(s.socketPath); err != nil && !os.IsNotExist(err) {
		log.Printf("DEBUG: failed to remove socket file during stop: %v", err)
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
				log.Printf("listener closed, stopping accept loop: %v", err)
				return
			}
			// Temporary errors: log and continue
			log.Printf("accept error: %v", err)
			continue
		}

		// Semaphore acquire with context awareness: reject connection if at capacity
		select {
		case s.connSem <- struct{}{}:
			s.wg.Add(1)
			go s.handleConn(conn)
		case <-s.ctx.Done():
			conn.Close()
			return
		default:
			log.Printf("connection rejected: max concurrent connections (%d) reached", s.maxConns)
			// Set tight write deadline to prevent slow-write attacks on the rejection path
			_ = conn.SetWriteDeadline(time.Now().Add(1 * time.Second))
			resp := ErrorResponse(ErrCodeBackpressure, "server at capacity, try again later")
			if err := writeFrame(conn, resp); err != nil {
				log.Printf("DEBUG: failed to write backpressure response: %v", err)
			}
			if err := conn.Close(); err != nil {
				log.Printf("DEBUG: failed to close rejected connection: %v", err)
			}
		}
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { <-s.connSem }()
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("DEBUG: failed to close handled connection: %v", err)
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in handleConn: %v\n%s", r, debug.Stack())
			// Send an error response so the client does not wait indefinitely.
			resp := ErrorResponse(ErrCodeInternal, fmt.Sprintf("internal server error: panic: %v", r))
			_ = writeFrame(conn, resp)
		}
	}()

	_ = conn.SetDeadline(time.Now().Add(s.connTimeout))

	var req Request
	if err := readFrame(conn, &req); err != nil {
		log.Printf("read request error: %v", err)
		return
	}

	resp := s.processRequest(&req)

	if err := writeFrame(conn, resp); err != nil {
		log.Printf("write response error: %v", err)
	}
}

func (s *Server) processRequest(req *Request) *Response {
	if req.ProtocolVersion != ProtocolVersion {
		return ErrorResponse(
			ErrCodeProtocolMismatch,
			fmt.Sprintf("protocol version mismatch: got %d, expected %d", req.ProtocolVersion, ProtocolVersion),
		)
	}

	// Validate and normalize CallerRole before dispatching to handlers.
	// Empty CallerRole (direct CLI invocation) is normalized to "cli".
	if err := ValidateCallerRole(req.CallerRole); err != nil {
		return ErrorResponse(ErrCodeValidation, err.Error())
	}
	req.CallerRole = NormalizeCallerRole(req.CallerRole)

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
