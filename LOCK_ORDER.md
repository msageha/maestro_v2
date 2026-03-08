# Lock Ordering Policy

## Canonical Lock Order

All keyed locks acquired via `lock.MutexMap` must follow this total order:

```
queue:* (level 1) → state:* (level 2) → result:* (level 3)
```

When multiple locks are held simultaneously, lower-level locks must be acquired before higher-level locks. Acquiring a level-N lock while holding a level-M lock where M > N is a **violation** and may cause deadlock.

Same-level locks (e.g., two `queue:` keys) may be acquired in any order.

## Lock Classes

| Prefix | Level | Protects | Example Keys |
|--------|-------|----------|-------------|
| `queue:` | 1 | Queue YAML files (`queue/*.yaml`) | `queue:worker1`, `queue:planner`, `queue:orchestrator` |
| `state:` | 2 | Command state files (`state/commands/*.yaml`) and other state | `state:{commandID}`, `state:continuous`, `state:learnings` |
| `result:` | 3 | Result files (`results/*.yaml`) | `result:worker1`, `result:planner` |

Keys without a known prefix (e.g., `unknown:key`) are not tracked and excluded from order checking.

## Programmatic Enforcement

Lock order is enforced at runtime when built with `-tags lockorder`:

```bash
MAESTRO_LOCKORDER=panic go test -tags lockorder ./internal/lock/...
```

The `MAESTRO_LOCKORDER` environment variable controls behavior:

| Value | Behavior |
|-------|----------|
| `off` (default) | No tracking |
| `warn` / `1` / `true` | Log violations |
| `panic` / `strict` / `2` | Panic on violation (for tests) |

Implementation: `internal/lock/lock_order_enabled.go`

## Nesting Rules

### Allowed

- Acquire locks in ascending level order: `queue:X` then `state:Y` then `result:Z`
- Skip levels: `queue:X` then `result:Z` (levels 1 then 3, no state lock needed)
- Acquire a single lock without nesting
- Release a higher-level lock, then acquire a lower-level lock (sequential, not nested)

### Forbidden

- `state:X` then `queue:Y` (level 2 then 1 - backward)
- `result:X` then `state:Y` (level 3 then 2 - backward)
- `result:X` then `queue:Y` (level 3 then 1 - backward)

## Additional Synchronization Primitives

Beyond the keyed `lockMap`, the codebase uses other synchronization mechanisms:

| Primitive | Scope | Purpose |
|-----------|-------|---------|
| `scanMu` (`sync.RWMutex`) | `QueueHandler` | Serializes periodic scan with file-level operations. API handlers acquire `RLock`; scan acquires `Lock`. |
| `execMu` (`sync.Mutex`) | Various handlers | Prevents concurrent execution of the same handler (e.g., `ResultHandler`, `CancelHandler`). |
| `debounceMu` (`sync.Mutex`) | `QueueHandler` | Protects debounce timer state. |
| `FileLock` | Process-level | Ensures single daemon instance via `flock(2)`. |

These primitives operate independently of the keyed lock order and do not participate in the level-based enforcement. However, `scanMu.RLock` is typically acquired **before** any `lockMap` key to prevent TOCTOU races with the periodic scan.

## Violation Response

If a lock order violation is detected (via `-tags lockorder`):
1. Fix the acquisition order to follow `queue → state → result`
2. If atomicity across multiple resources is needed, acquire all required locks in the canonical order before performing any operations
3. If holding a higher-level lock while needing a lower-level lock, release the higher-level lock first, acquire the lower-level lock, then re-acquire the higher-level lock (be aware of TOCTOU implications)
