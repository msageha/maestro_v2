package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// YAMLSignalStore is a file-backed implementation of worktree.SignalStore.
// It serializes concurrent updates via the shared MutexMap using the
// "queue:planner_signals" key, matching the locking convention used by other
// queue files in this package.
//
// The store reads .maestro/queue/planner_signals.yaml on every call so it
// remains consistent with the scan loop's read-modify-write cycle. There is
// no in-memory caching: callers are expected to invoke this only from the
// resolver pipeline (which is rare relative to general queue I/O).
type YAMLSignalStore struct {
	maestroDir string
	lockMap    *lock.MutexMap
}

// NewYAMLSignalStore constructs a YAMLSignalStore.
func NewYAMLSignalStore(maestroDir string, lockMap *lock.MutexMap) *YAMLSignalStore {
	return &YAMLSignalStore{maestroDir: maestroDir, lockMap: lockMap}
}

// UpdateMergeConflictSignal performs an RMW on the merge_conflict signal
// matching (commandID, phaseID, workerID). The on-disk queue is loaded under
// the "queue:planner_signals" lock, the matching signal (or nil) is passed to
// fn, and the queue is written back atomically iff fn returns nil and a
// matching signal existed. fn may also be called with nil; in that case it is
// expected to either return an error (no signal to update) or be a no-op.
func (s *YAMLSignalStore) UpdateMergeConflictSignal(commandID, phaseID, workerID string, fn func(*model.PlannerSignal) error) error {
	s.lockMap.Lock("queue:planner_signals")
	defer s.lockMap.Unlock("queue:planner_signals")

	path := filepath.Join(s.maestroDir, "queue", "planner_signals.yaml")
	var sq model.PlannerSignalQueue

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if uerr := yamlv3.Unmarshal(data, &sq); uerr != nil {
			return fmt.Errorf("parse planner_signals: %w", uerr)
		}
	case os.IsNotExist(err):
		// fall through with empty queue
	default:
		return fmt.Errorf("read planner_signals: %w", err)
	}

	idx := -1
	for i := range sq.Signals {
		sig := &sq.Signals[i]
		if sig.Kind == "merge_conflict" &&
			sig.CommandID == commandID &&
			sig.PhaseID == phaseID &&
			sig.WorkerID == workerID {
			idx = i
			break
		}
	}

	var target *model.PlannerSignal
	if idx >= 0 {
		cp := sq.Signals[idx]
		target = &cp
	}
	if cbErr := fn(target); cbErr != nil {
		return cbErr
	}
	if idx < 0 {
		// No signal existed and fn accepted nil — nothing to persist.
		return nil
	}
	sq.Signals[idx] = *target
	if sq.SchemaVersion == 0 {
		sq.SchemaVersion = 1
		sq.FileType = "planner_signal_queue"
	}
	if err := yamlutil.AtomicWrite(path, sq); err != nil {
		return fmt.Errorf("write planner_signals: %w", err)
	}
	return nil
}
