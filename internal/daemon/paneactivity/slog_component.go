package paneactivity

import "log/slog"

// slogc returns the process default slog logger labeled with this package's
// component. Resolved at call time — the daemon wires slog.Default() to
// daemon.log only after startup (cmd_daemon), so a package-level var would
// capture the pre-SetDefault stderr logger.
func slogc() *slog.Logger {
	return slog.Default().With("component", "paneactivity")
}
