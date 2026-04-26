// Package daemonapi contains UDS endpoint adapters for the daemon.
//
// The package owns request decoding, caller-role checks, shallow validation,
// and endpoint-level routing. It deliberately does not own queue/state/result
// state transitions. Those domain operations are injected as small functions
// or interfaces from package daemon, so endpoint growth does not pull lifecycle,
// scan, worktree, or result-write internals back into the transport layer.
package daemonapi
