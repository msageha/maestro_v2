package uds

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"syscall"
	"time"
)

// Client is a Unix Domain Socket client that sends requests to a running daemon.
type Client struct {
	socketPath string
	timeout    time.Duration
}

// NewClient creates a new Client that connects to the daemon at the given Unix socket path.
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		timeout:    30 * time.Second,
	}
}

// SetTimeout sets the dial and read/write deadline for client connections.
func (c *Client) SetTimeout(d time.Duration) {
	c.timeout = d
}

// maxDialRetries is the maximum number of dial attempts for transient errors.
const maxDialRetries = 3

// isTransientDialError returns true if the error is a transient connection error
// that is safe to retry (connection refused, resource temporarily unavailable,
// or socket not yet created).
func isTransientDialError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EAGAIN) ||
		errors.Is(err, syscall.ENOENT)
}

// Send dials the daemon, writes the given Request, and returns the Response.
// Transient dial errors (ECONNREFUSED, EAGAIN, ENOENT) are retried up to 3 times
// with exponential backoff.
func (c *Client) Send(req *Request) (*Response, error) {
	var conn net.Conn
	var dialErr error
	backoff := 100 * time.Millisecond

	for attempt := 0; attempt < maxDialRetries; attempt++ {
		conn, dialErr = net.DialTimeout("unix", c.socketPath, c.timeout)
		if dialErr == nil {
			break
		}
		if !isTransientDialError(dialErr) {
			break
		}
		if attempt < maxDialRetries-1 {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	if dialErr != nil {
		return nil, fmt.Errorf(
			"failed to connect to daemon at %s: %w\n"+
				"Is the daemon running? Start it with: maestro daemon",
			c.socketPath, dialErr,
		)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("WARN: failed to close client connection: %v", err)
		}
	}()

	if err := conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, fmt.Errorf("set connection deadline: %w", err)
	}

	if err := WriteFrame(conn, req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	var resp Response
	if err := ReadFrame(conn, &resp); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return &resp, nil
}

// SendCommand is a convenience method that creates a Request from the command and params, then sends it.
func (c *Client) SendCommand(command string, params any) (*Response, error) {
	req, err := NewRequest(command, params)
	if err != nil {
		return nil, err
	}
	return c.Send(req)
}

// SendContext behaves like Send but is cancelable via ctx. Cancellation
// (including SIGINT propagated through a context) closes the underlying
// connection so the in-flight dial / read / write returns promptly.
func (c *Client) SendContext(ctx context.Context, req *Request) (*Response, error) {
	if ctx == nil {
		return c.Send(req)
	}

	dialer := &net.Dialer{Timeout: c.timeout}
	var conn net.Conn
	var dialErr error
	backoff := 100 * time.Millisecond

	for attempt := 0; attempt < maxDialRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, dialErr = dialer.DialContext(ctx, "unix", c.socketPath)
		if dialErr == nil {
			break
		}
		if !isTransientDialError(dialErr) {
			break
		}
		if attempt < maxDialRetries-1 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			backoff *= 2
		}
	}
	if dialErr != nil {
		return nil, fmt.Errorf(
			"failed to connect to daemon at %s: %w\n"+
				"Is the daemon running? Start it with: maestro daemon",
			c.socketPath, dialErr,
		)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("WARN: failed to close client connection: %v", err)
		}
	}()

	// Propagate ctx cancellation by closing the connection, which unblocks
	// any in-flight WriteFrame/ReadFrame with a use-of-closed-network-connection
	// error.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	if err := conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, fmt.Errorf("set connection deadline: %w", err)
	}

	if err := WriteFrame(conn, req); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("send request: %w", err)
	}

	var resp Response
	if err := ReadFrame(conn, &resp); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("read response: %w", err)
	}

	return &resp, nil
}

// SendCommandContext is a convenience method like SendCommand but cancelable via ctx.
func (c *Client) SendCommandContext(ctx context.Context, command string, params any) (*Response, error) {
	req, err := NewRequest(command, params)
	if err != nil {
		return nil, err
	}
	return c.SendContext(ctx, req)
}
