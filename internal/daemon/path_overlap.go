package daemon

import (
	"path"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// HasPathOverlap reports whether two expected_paths entries overlap.
// Overlap means one path equals or contains the other at a directory boundary.
// Empty paths never overlap with anything.
func HasPathOverlap(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ca := normalizePath(a)
	cb := normalizePath(b)
	if ca == "" || cb == "" {
		return false
	}
	if ca == cb {
		return true
	}
	// Check containment at directory boundary.
	return isDescendant(cb, ca) || isDescendant(ca, cb)
}

// normalizePath strips trailing slashes and cleans the path.
func normalizePath(p string) string {
	cleaned := path.Clean(strings.TrimSuffix(p, "/"))
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// isDescendant reports whether child is a descendant of dir at a "/" boundary.
func isDescendant(child, dir string) bool {
	return strings.HasPrefix(child, dir+"/")
}

// HasTaskPathOverlap reports whether any expected_path in paths1 overlaps
// with any expected_path in paths2.
func HasTaskPathOverlap(paths1, paths2 []string) bool {
	if len(paths1) == 0 || len(paths2) == 0 {
		return false
	}
	for _, p1 := range paths1 {
		for _, p2 := range paths2 {
			if HasPathOverlap(p1, p2) {
				return true
			}
		}
	}
	return false
}

// inFlightPathEntry pairs a task ID with its expected_paths for overlap logging.
type inFlightPathEntry struct {
	TaskID        string
	ExpectedPaths []string
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
				continue
			}
			entries = append(entries, inFlightPathEntry{
				TaskID:        task.ID,
				ExpectedPaths: task.ExpectedPaths,
			})
		}
	}
	return entries
}

// findOverlappingTask returns the first in-flight entry whose paths overlap
// with the candidate's paths, plus the specific conflicting path pair.
// Returns ("", "", "") if no overlap is found.
func findOverlappingTask(candidate *model.Task, inFlight []inFlightPathEntry) (conflictTaskID, candidatePath, inFlightPath string) {
	if len(candidate.ExpectedPaths) == 0 {
		return "", "", ""
	}
	for _, entry := range inFlight {
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
