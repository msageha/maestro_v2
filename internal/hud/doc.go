// Package hud implements the read-only terminal observation HUD
// (`maestro hud`, GitHub issue #29).
//
// Design contract:
//
//   - Read-only: the HUD reads `.maestro/` (state/, queue/, results/,
//     dead_letters/, quarantine/) and never writes to any daemon-owned
//     file. It carries no control-plane operation whatsoever (no queue
//     write, no UDS mutation) — operator actions stay with the `maestro`
//     CLI. This is the structural answer to the #20/#23 web-dashboard
//     threat surface: no HTTP server, no auth, no pty writes.
//   - Daemon-independent and opt-in: the HUD starts and keeps polling even
//     when the daemon is down; every section that cannot be read renders
//     as "unavailable" instead of failing. The daemon never depends on the
//     HUD.
//   - Shared derivation, no re-implementation: queue depths come from
//     internal/status (same code path as `maestro status`), all YAML
//     shapes come from internal/model, and reads go through the
//     billion-laughs-safe internal/yaml.SafeUnmarshal. Patterns /
//     corrections style sections (learnings, skill candidates) only
//     display signals produced elsewhere (C-5 / skill-factory); the HUD
//     performs no detection of its own.
//   - The only file the HUD writes is its own snapshot history
//     (state/hud_history.jsonl, JSONL, append-only with age pruning),
//     which no daemon component reads or writes.
//
// The renderer is a pure function from a collected Snapshot (plus history
// diffs) to a string, so frames are unit-testable without a terminal. The
// interactive loop only adds ANSI alternate-screen management, periodic
// polling, and signal-driven teardown on top — all via stdlib.
package hud
