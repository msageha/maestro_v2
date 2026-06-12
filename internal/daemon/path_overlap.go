package daemon

import (
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/pathutil"
)

// SentinelUnknownPaths is the placeholder used when a task's expected_paths is
// missing or empty after schema validation should have rejected it. Treating
// such a task as occupying an "unknown" path forces the path-overlap check to
// flag a conflict with every other task, which conservatively blocks dispatch
// rather than silently letting the task race against in-flight work.
//
// REQUIREMENTS.md §S1-1 mandates non-empty expected_paths; this sentinel is a
// defensive guard for legacy state files or future code paths that bypass
// validation.
const SentinelUnknownPaths = "<unknown-paths>"

// HasPathOverlap reports whether two expected_paths entries overlap.
// Overlap means one path equals or contains the other at a directory boundary.
// Empty paths never overlap with anything. The SentinelUnknownPaths token
// always overlaps to enforce the conservative-block behaviour for tasks with
// missing expected_paths.
func HasPathOverlap(a, b string) bool {
	if a == SentinelUnknownPaths || b == SentinelUnknownPaths {
		return true
	}
	if a == "" || b == "" {
		return false
	}
	ca := pathutil.NormalizePath(a)
	cb := pathutil.NormalizePath(b)
	if ca == "" || cb == "" {
		return false
	}
	if ca == cb {
		return true
	}
	// Check containment at directory boundary.
	return pathutil.IsDescendant(cb, ca) || pathutil.IsDescendant(ca, cb)
}

// inFlightPathEntry pairs a task ID with its expected_paths for overlap logging.
type inFlightPathEntry struct {
	TaskID        string
	ExpectedPaths []string
	// ABGroupID exempts candidates of the SAME A/B race from mutual overlap
	// blocking: they intentionally share expected_paths (the shadow is a
	// copy) and are structurally isolated in per-candidate worktrees, so
	// serializing them would turn the race into a walkover.
	ABGroupID string
}

// collectInFlightPaths gathers expected_paths from all in-progress tasks
// across all task queues with valid (non-expired) leases.
func collectInFlightPaths(taskQueues map[string]*taskQueueEntry, isExpired func(*string) bool) []inFlightPathEntry {
	var entries []inFlightPathEntry
	for _, tq := range taskQueues {
		for i := range tq.Queue.Tasks {
			task := &tq.Queue.Tasks[i]
			if task.Status != model.StatusInProgress {
				continue
			}
			if isExpired(task.LeaseExpiresAt) {
				continue
			}
			if len(task.ExpectedPaths) == 0 {
				// STRICT (REQUIREMENTS.md §S1-1): a validated task always has
				// non-empty expected_paths. Reaching here indicates corrupted
				// or legacy state. Treat the task as occupying an "unknown"
				// path so any candidate is conservatively blocked, rather than
				// silently allowing concurrent work against unbounded paths.
				entries = append(entries, inFlightPathEntry{
					TaskID:        task.ID,
					ExpectedPaths: []string{SentinelUnknownPaths},
					ABGroupID:     task.ABGroupID,
				})
				continue
			}
			entries = append(entries, inFlightPathEntry{
				TaskID:        task.ID,
				ExpectedPaths: task.ExpectedPaths,
				ABGroupID:     task.ABGroupID,
			})
		}
	}
	return entries
}

// findOverlappingTask returns the first in-flight entry whose paths overlap
// with the candidate's paths, plus the specific conflicting path pair.
// Returns ("", "", "") if no overlap is found.
//
// STRICT (REQUIREMENTS.md §S1-1): a candidate with empty expected_paths is
// conservatively flagged as conflicting with the first in-flight task. This is
// a defensive guard — schema validation should have rejected such a task
// before it reached the dispatch path.
func findOverlappingTask(candidate *model.Task, inFlight []inFlightPathEntry) (conflictTaskID, candidatePath, inFlightPath string) {
	if len(candidate.ExpectedPaths) == 0 {
		if len(inFlight) > 0 {
			entry := inFlight[0]
			ip := ""
			if len(entry.ExpectedPaths) > 0 {
				ip = entry.ExpectedPaths[0]
			}
			return entry.TaskID, SentinelUnknownPaths, ip
		}
		return "", "", ""
	}
	for _, entry := range inFlight {
		if candidate.ABGroupID != "" && entry.ABGroupID == candidate.ABGroupID {
			// Same A/B race: candidates intentionally share paths and are
			// isolated in per-candidate worktrees — never serialize them.
			continue
		}
		for _, cp := range candidate.ExpectedPaths {
			for _, ip := range entry.ExpectedPaths {
				if HasPathOverlap(cp, ip) {
					return entry.TaskID, cp, ip
				}
			}
		}
	}
	return "", "", ""
}
