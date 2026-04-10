package main

import (
	"context"

	"github.com/msageha/maestro_v2/internal/uds"
)

// udsClientIface abstracts UDS client methods for testability.
type udsClientIface interface {
	SendCommand(command string, params any) (*uds.Response, error)
	SendCommandContext(ctx context.Context, command string, params any) (*uds.Response, error)
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

// newUDSClient creates a UDS client using the production default factory.
// Used by functions not yet migrated to cliApp method receivers.
var newUDSClient clientFactory = func(socketPath string) udsClientIface {
	return uds.NewClient(socketPath)
}
