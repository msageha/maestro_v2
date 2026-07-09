package main

import (
	"context"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/uds"
)

// maxInlineUDSPayloadBytes caps payloads the CLI sends inline inside a UDS
// request (stdin-sourced tasks YAML, verify configs). The UDS transport
// rejects frames larger than 2 MB (internal/uds/protocol.go maxFrameSize);
// JSON string escaping plus the request envelope inflate the payload on the
// wire, so the CLI stops at 1 MB and fails fast with an actionable error
// instead of surfacing an opaque "frame too large" after the fact.
const maxInlineUDSPayloadBytes = 1 * 1024 * 1024

// udsDaemonStartHint is the recovery hint embedded by internal/uds when the
// daemon socket cannot be dialed. It instructs the reader to run
// `maestro daemon`, which is a foreground process: an LLM agent following
// it verbatim blocks its own turn. The CLI rewrites the hint to an
// agent-safe one (internal/uds is shared with the daemon and is not
// modified for a presentation concern).
const udsDaemonStartHint = "Is the daemon running? Start it with: maestro daemon"

// agentSafeDaemonHint replaces udsDaemonStartHint in CLI-facing errors.
const agentSafeDaemonHint = "Is the daemon running? Start the formation with `maestro up -d`. " +
	"Do NOT run `maestro daemon` directly — it stays in the foreground and will hang this session. " +
	"If `maestro up -d` fails, report the error to the operator instead of retrying."

// rewrittenError swaps the display message while preserving the original
// error chain for errors.Is/As.
type rewrittenError struct {
	msg   string
	cause error
}

func (e *rewrittenError) Error() string { return e.msg }
func (e *rewrittenError) Unwrap() error { return e.cause }

// rewriteDaemonStartHint replaces the foreground-daemon recovery hint from
// internal/uds with agentSafeDaemonHint. Errors without the hint pass
// through unchanged.
func rewriteDaemonStartHint(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !strings.Contains(msg, udsDaemonStartHint) {
		return err
	}
	return &rewrittenError{
		msg:   strings.Replace(msg, udsDaemonStartHint, agentSafeDaemonHint, 1),
		cause: err,
	}
}

// hintRewritingClient decorates a UDS client so every transport error passes
// through rewriteDaemonStartHint before reaching command handlers. Applied
// centrally in newDaemonClient so all daemon-RPC subcommands share the
// agent-safe hint without per-call-site wrapping.
type hintRewritingClient struct {
	inner udsClientIface
}

func (c hintRewritingClient) SendCommand(command string, params any) (*uds.Response, error) {
	resp, err := c.inner.SendCommand(command, params)
	return resp, rewriteDaemonStartHint(err)
}

func (c hintRewritingClient) SendCommandContext(ctx context.Context, command string, params any) (*uds.Response, error) {
	resp, err := c.inner.SendCommandContext(ctx, command, params)
	return resp, rewriteDaemonStartHint(err)
}

func (c hintRewritingClient) SetTimeout(d time.Duration) { c.inner.SetTimeout(d) }

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
	return hintRewritingClient{inner: a.createClient(socketPath)}
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
