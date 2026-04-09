// Package uds implements Unix Domain Socket based IPC between the maestro CLI
// and daemon process. It provides a length-prefixed, JSON-encoded request/response
// protocol for command dispatch.
//
// # Main components
//
//   - Client: Connects to the daemon socket, sends a [Request], and reads the
//     [Response]. Supports configurable timeouts and automatic retry with
//     exponential backoff for transient dial errors (ECONNREFUSED, EAGAIN,
//     ENOENT).
//   - Server: Listens on a Unix socket and dispatches incoming requests to
//     registered [handlerFunc] callbacks by command name. Enforces a
//     configurable concurrency limit via semaphore, returning a backpressure
//     error when at capacity. Supports graceful shutdown with in-flight
//     connection draining.
//   - Protocol: Defines the wire format — 4-byte big-endian length prefix
//     followed by JSON-encoded [Request] or [Response]. Includes protocol
//     version negotiation and structured error codes (PROTOCOL_MISMATCH,
//     UNKNOWN_COMMAND, INTERNAL_ERROR, BACKPRESSURE, FENCING_REJECT).
//   - CallerRole: Role-based trust boundary support via the MAESTRO_AGENT_ROLE
//     environment variable, allowing the daemon to enforce access control on
//     sensitive commands.
package uds
