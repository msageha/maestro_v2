package uds

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"time"
)

type HandlerFunc func(req *Request) *Response

type Server struct {
	socketPath  string
	listener    net.Listener
	handlers    map[string]HandlerFunc
	mu          sync.RWMutex
	connTimeout time.Duration
	wg          sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
}

func NewServer(socketPath string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		socketPath:  socketPath,
		handlers:    make(map[string]HandlerFunc),
		connTimeout: 30 * time.Second,
		ctx:         ctx,
		cancel:      cancel,
	}
}

func (s *Server) SetConnTimeout(d time.Duration) {
	s.connTimeout = d
}

func (s *Server) Handle(command string, handler HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[command] = handler
}

func (s *Server) Start() error {
	// Remove stale socket file
	_ = os.Remove(s.socketPath)

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.socketPath, err)
	}

	// Set socket file permissions to 0600
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	s.listener = listener

	s.wg.Add(1)
	go s.acceptLoop()

	return nil
}

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

		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in handleConn: %v\n%s", r, debug.Stack())
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
