package uds

import (
	"fmt"
	"net"
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

// Send dials the daemon, writes the given Request, and returns the Response.
func (c *Client) Send(req *Request) (*Response, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, c.timeout)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to connect to daemon at %s: %w\n"+
				"Is the daemon running? Start it with: maestro daemon",
			c.socketPath, err,
		)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(c.timeout))

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
