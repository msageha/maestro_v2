package plan

import "log/slog"

// slogc returns the process default slog logger labeled with this package's
// component. The plan package runs inside the daemon (bridge PlanExecutor),
// where cmd_daemon routes slog.Default() to daemon.log; resolving the default
// AT CALL TIME is essential — a package-level var would capture the
// pre-SetDefault stderr logger and the component label would never reach
// daemon.log.
func slogc() *slog.Logger {
	return slog.Default().With("component", "plan")
}
