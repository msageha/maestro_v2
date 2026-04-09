package daemon

// maxDebounceSec is the upper bound on how long triggers can be coalesced
// before forcing a scan, preventing indefinite starvation under sustained bursts.
// Why: 5 seconds is long enough to coalesce typical filesystem event storms
// from multi-file atomic writes, but short enough to keep scan latency
// bounded for interactive use cases.
const maxDebounceSec = 5.0
