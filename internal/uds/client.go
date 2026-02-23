package uds

import (
	"fmt"
	"net"
	"time"
)

type Client struct {
	socketPath string
	timeout    time.Duration
}

func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		timeout:    30 * time.Second,
	}
}

func (c *Client) SetTimeout(d time.Duration) {
	c.timeout = d
}

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

func (c *Client) SendCommand(command string, params any) (*Response, error) {
	req, err := NewRequest(command, params)
	if err != nil {
		return nil, err
	}
	return c.Send(req)
}
