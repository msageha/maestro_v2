package reconcile

import (
	"os"
	"strings"

	"github.com/msageha/maestro_v2/internal/daemon/core"
)

// Run is the per-scan context created by Engine.Reconcile().
// It holds the directory cache and exposes helper methods used by patterns.
//
// Lock ordering convention for the reconcile package:
//
//	state and queue locks MUST NOT be held simultaneously.
//
// When both are needed, use a three-phase pattern:
//
//	Phase 1: acquire state lock → read/snapshot → release state lock
//	Phase 2: acquire queue lock → inspect/modify → release queue lock
//	Phase 3: acquire state lock → verify + apply changes → release state lock
//
// This avoids deadlocks between subsystems (e.g. reconcile vs. dispatch/result
// handlers) that may acquire these locks in different contexts.
type Run struct {
	core.LogMixin
	Deps     *Deps
	dirCache map[string][]os.DirEntry
}

func newRun(deps *Deps) *Run {
	return &Run{
		LogMixin: core.LogMixin{DL: deps.DL},
		Deps:     deps,
		dirCache: make(map[string][]os.DirEntry, 4),
	}
}

// cachedReadDir returns cached directory entries for the given path within a single Reconcile scan.
func (r *Run) cachedReadDir(dir string) ([]os.DirEntry, error) {
	if entries, ok := r.dirCache[dir]; ok {
		return entries, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	r.dirCache[dir] = entries
	return entries, nil
}

// stuckThresholdSec returns the threshold in seconds for considering a state "stuck".
func (r *Run) stuckThresholdSec() int {
	leaseSec := r.Deps.Config.Watcher.DispatchLeaseSec
	if leaseSec <= 0 {
		leaseSec = 300
	}
	return leaseSec * 2
}

// extractWorkerID extracts the worker ID from a queue filename.
func extractWorkerID(filename string) string {
	if !strings.HasPrefix(filename, "worker") || !strings.HasSuffix(filename, ".yaml") {
		return ""
	}
	return strings.TrimSuffix(filename, ".yaml")
}

// removeFromSlice removes a target string from a slice.
func removeFromSlice(s []string, target string) []string {
	var result []string
	for _, v := range s {
		if v != target {
			result = append(result, v)
		}
	}
	return result
}
