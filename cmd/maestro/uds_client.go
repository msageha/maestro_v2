package main

import (
	"context"
	"time"

	"github.com/msageha/maestro_v2/internal/uds"
)

// udsClientIface abstracts UDS client methods for testability.
type udsClientIface interface {
	SendCommand(command string, params any) (*uds.Response, error)
	SendCommandContext(ctx context.Context, command string, params any) (*uds.Response, error)
	SetTimeout(d time.Duration)
}

// clientFactory creates UDS clients for the given socket path.
type clientFactory func(socketPath string) udsClientIface

// cliApp holds injectable dependencies for CLI commands.
// Tests inject mock factories via newTestApp; production code uses newCLIApp.
type cliApp struct {
	createClient clientFactory
}

// newCLIApp creates a cliApp with production defaults.
func newCLIApp() *cliApp {
	return &cliApp{
		createClient: func(socketPath string) udsClientIface {
			return uds.NewClient(socketPath)
		},
	}
}

// newDaemonClient creates a UDS client connected to the daemon socket in the given .maestro directory.
func (a *cliApp) newDaemonClient(maestroDir string) udsClientIface {
	socketPath, err := uds.SocketPath(maestroDir)
	if err != nil {
		return errorUDSClient{err: err}
	}
	return a.createClient(socketPath)
}

type errorUDSClient struct {
	err error
}

func (c errorUDSClient) SendCommand(string, any) (*uds.Response, error) {
	return nil, c.err
}

func (c errorUDSClient) SendCommandContext(context.Context, string, any) (*uds.Response, error) {
	return nil, c.err
}

func (c errorUDSClient) SetTimeout(time.Duration) {}
