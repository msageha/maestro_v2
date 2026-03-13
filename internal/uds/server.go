package uds

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

// HandlerFunc is the function signature for command handlers registered on a Server.
type HandlerFunc func(req *Request) *Response

// DefaultMaxConcurrentConns is the default maximum number of concurrent connections.
const DefaultMaxConcurrentConns = 64

// Server is a Unix Domain Socket server that dispatches incoming requests to registered handlers.
type Server struct {
	socketPath  string
	listener    net.Listener
	handlers    map[string]HandlerFunc
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
		handlers:    make(map[string]HandlerFunc),
		connTimeout: 30 * time.Second,
		maxConns:    DefaultMaxConcurrentConns,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// SetConnTimeout sets the per-connection read/write deadline for the server.
func (s *Server) SetConnTimeout(d time.Duration) {
	s.connTimeout = d
}

// SetMaxConcurrentConns sets the maximum number of concurrent connections.
// Must be called before Start().
func (s *Server) SetMaxConcurrentConns(n int) {
	if n > 0 {
		s.maxConns = n
	}
}

// Handle registers a HandlerFunc for the given command name.
func (s *Server) Handle(command string, handler HandlerFunc) {
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
		_ = s.listener.Close()
	}
	s.wg.Wait()
	_ = os.Remove(s.socketPath)
	return nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return
			default:
				log.Printf("accept error: %v", err)
				continue
			}
		}

		// Non-blocking semaphore acquire: reject connection if at capacity
		select {
		case s.connSem <- struct{}{}:
			s.wg.Add(1)
			go s.handleConn(conn)
		default:
			log.Printf("connection rejected: max concurrent connections (%d) reached", s.maxConns)
			// Best-effort error response before closing
			resp := ErrorResponse(ErrCodeBackpressure, "server at capacity, try again later")
			_ = WriteFrame(conn, resp)
			_ = conn.Close()
		}
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { <-s.connSem }()
	defer func() { _ = conn.Close() }()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in handleConn: %v\n%s", r, debug.Stack())
			// Send an error response so the client does not wait indefinitely.
			resp := ErrorResponse(ErrCodeInternal, fmt.Sprintf("internal server error: panic: %v", r))
			_ = WriteFrame(conn, resp)
		}
	}()

	_ = conn.SetDeadline(time.Now().Add(s.connTimeout))

	var req Request
	if err := ReadFrame(conn, &req); err != nil {
		log.Printf("read request error: %v", err)
		return
	}

	resp := s.processRequest(&req)

	if err := WriteFrame(conn, resp); err != nil {
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
