package uds

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
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

// dialWithRetry dials the daemon with exponential backoff retry for transient errors.
func (c *Client) dialWithRetry(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: c.timeout}
	var dialErr error
	backoff := 100 * time.Millisecond

	for attempt := 0; attempt < maxDialRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
		if err == nil {
			return conn, nil
		}
		dialErr = err
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
	return nil, fmt.Errorf(
		"failed to connect to daemon at %s: %w\n"+
			"Is the daemon running? Start it with: maestro daemon",
		c.socketPath, dialErr,
	)
}

// send dials the daemon, writes the given Request, and returns the Response.
// Transient dial errors (ECONNREFUSED, EAGAIN, ENOENT) are retried up to 3 times
// with exponential backoff.
func (c *Client) send(req *Request) (*Response, error) {
	return c.sendContext(context.Background(), req)
}

// SendCommand is a convenience method that creates a Request from the command and params, then sends it.
func (c *Client) SendCommand(command string, params any) (*Response, error) {
	req, err := newRequest(command, params)
	if err != nil {
		return nil, err
	}
	return c.send(req)
}

// sendContext behaves like send but is cancelable via ctx. Cancellation
// (including SIGINT propagated through a context) closes the underlying
// connection so the in-flight dial / read / write returns promptly.
func (c *Client) sendContext(ctx context.Context, req *Request) (*Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	conn, err := c.dialWithRetry(ctx)
	if err != nil {
		return nil, err
	}

	// Use sync.Once to serialize conn.Close() between the defer and the
	// context-cancellation goroutine, preventing a double-close race.
	var closeOnce sync.Once
	closeConn := func() {
		closeOnce.Do(func() {
			if err := conn.Close(); err != nil {
				log.Printf("WARN: failed to close client connection: %v", err)
			}
		})
	}
	defer closeConn()

	// Propagate ctx cancellation by closing the connection, which unblocks
	// any in-flight writeFrame/readFrame with a use-of-closed-network-connection
	// error.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			closeConn()
		case <-stop:
		}
	}()

	if err := conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, fmt.Errorf("set connection deadline: %w", err)
	}

	if err := writeFrame(conn, req); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("send request: %w", err)
	}

	var resp Response
	if err := readFrame(conn, &resp); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("read response: %w", err)
	}

	return &resp, nil
}

// SendCommandContext is a convenience method like SendCommand but cancelable via ctx.
func (c *Client) SendCommandContext(ctx context.Context, command string, params any) (*Response, error) {
	req, err := newRequest(command, params)
	if err != nil {
		return nil, err
	}
	return c.sendContext(ctx, req)
}
