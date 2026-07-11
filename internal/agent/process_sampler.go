package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// processSampler measures cumulative CPU time consumed by a pane's process
// tree. Abstracted behind an interface so busyDetector tests can inject a
// fake without shelling out to the real `ps`.
type processSampler interface {
	// descendantCPUSeconds returns the sum of cumulative CPU time (user+sys,
	// fractional seconds) across rootPID and every live descendant of it.
	// Returns an error if the process table cannot be read, or if it was
	// read but not one row could be parsed (a systemic format mismatch,
	// e.g. an unexpected `ps` output layout on some platform) — either case
	// must surface as an error so callers treat the signal as inconclusive
	// rather than silently reading "no CPU used" forever.
	descendantCPUSeconds(ctx context.Context, rootPID int) (float64, error)
}

// psProcessSampler is the production processSampler, backed by `ps`.
type psProcessSampler struct{}

func (psProcessSampler) descendantCPUSeconds(ctx context.Context, rootPID int) (float64, error) {
	procs, err := listAllProcesses(ctx)
	if err != nil {
		return 0, err
	}
	return sumDescendantCPUSeconds(rootPID, procs), nil
}

// processInfo is one row of `ps -A -o pid=,ppid=,cputime=`.
type processInfo struct {
	pid        int
	ppid       int
	cpuSeconds float64
}

// listAllProcesses snapshots pid/ppid/cumulative-CPU-time for every process
// on the system. `-o pid=,ppid=,cputime=` (no header) is supported by both
// BSD ps (macOS) and procps ps (Linux), keeping this portable across
// maestro's supported platforms without depending on Linux-only /proc.
func listAllProcesses(ctx context.Context) ([]processInfo, error) {
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,cputime=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps -A: %w", err)
	}

	var procs []processInfo
	var nonEmptyLines int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonEmptyLines++
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		cpuSec, err := parseCPUTime(fields[2])
		if err != nil {
			continue
		}
		procs = append(procs, processInfo{pid: pid, ppid: ppid, cpuSeconds: cpuSec})
	}

	// A systemic parse failure (every row rejected despite non-empty output,
	// e.g. an unanticipated `ps` output format on some platform/version)
	// must surface as an error. Silently returning an empty table here would
	// make descendantCPUSeconds report "0 CPU used" forever with no error —
	// indistinguishable from a genuinely idle pane — permanently and
	// silently disabling the CPU-based override.
	if nonEmptyLines > 0 && len(procs) == 0 {
		return nil, fmt.Errorf("ps -A: parsed 0 of %d non-empty lines (unexpected output format?)", nonEmptyLines)
	}
	return procs, nil
}

// parseCPUTime parses ps's cputime column into fractional seconds. Both BSD
// ps (macOS: "[[dd-]hh:]mm:ss.cc", hundredths of a second) and procps ps
// (Linux: "[dd-]hh:mm:ss", whole seconds) are accepted — the seconds field
// may or may not carry a fractional component.
func parseCPUTime(s string) (float64, error) {
	var days float64
	if idx := strings.Index(s, "-"); idx >= 0 {
		d, err := strconv.ParseFloat(s[:idx], 64)
		if err != nil {
			return 0, fmt.Errorf("parse days in cputime %q: %w", s, err)
		}
		days = d
		s = s[idx+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("unexpected cputime format %q", s)
	}
	var vals [3]float64
	offset := 3 - len(parts)
	for i, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0, fmt.Errorf("parse cputime field %q: %w", p, err)
		}
		vals[offset+i] = v
	}
	return days*86400 + vals[0]*3600 + vals[1]*60 + vals[2], nil
}

// sumDescendantCPUSeconds walks the process tree rooted at rootPID (BFS over
// the ppid adjacency built from procs) and sums cpuSeconds across rootPID and
// every live descendant. Cycle-safe via a visited set (adversarial or
// corrupt ppid data cannot cause an infinite loop).
//
// Known limitation: this compares point-in-time snapshots of *currently
// live* processes. A child that is spawned and fully exits between the two
// samples (e.g. a very fast individual compiler invocation) contributes no
// CPU time to either snapshot and is invisible to the delta. In practice a
// real build/fuzz workload keeps at least one descendant alive throughout
// most sampling windows, so this is an acceptable gap for a veto-only signal
// that always falls back to the existing text-based checks.
func sumDescendantCPUSeconds(rootPID int, procs []processInfo) float64 {
	childrenOf := make(map[int][]int, len(procs))
	byPID := make(map[int]processInfo, len(procs))
	for _, p := range procs {
		childrenOf[p.ppid] = append(childrenOf[p.ppid], p.pid)
		byPID[p.pid] = p
	}

	var total float64
	visited := make(map[int]bool)
	queue := []int{rootPID}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if visited[pid] {
			continue
		}
		visited[pid] = true
		if p, ok := byPID[pid]; ok {
			total += p.cpuSeconds
		}
		queue = append(queue, childrenOf[pid]...)
	}
	return total
}
