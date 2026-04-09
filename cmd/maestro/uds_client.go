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

// newUDSClient creates a UDS client. Replaced in tests to inject mocks.
var newUDSClient = func(socketPath string) udsClientIface {
	return uds.NewClient(socketPath)
}
