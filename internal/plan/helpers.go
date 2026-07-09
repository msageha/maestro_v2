package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// nowUTC returns the current time in UTC formatted as RFC3339.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// buildAllowedBloomMap builds a set of allowed bloom levels from the given slice.
// If the slice is empty, all levels from BloomLevelMin to BloomLevelMax are allowed.
func buildAllowedBloomMap(allowedLevels []int) map[int]bool {
	m := make(map[int]bool)
	if len(allowedLevels) > 0 {
		for _, l := range allowedLevels {
			m[l] = true
		}
	} else {
		for l := BloomLevelMin; l <= BloomLevelMax; l++ {
			m[l] = true
		}
	}
	return m
}

// readModifyWriteResultFile performs the common read-modify-write pattern on the planner result file.
// It reads the existing result file, calls modifyFn to apply changes, and atomically writes the result.
// If modifyFn returns an error, the write is skipped and the error is returned.
func readModifyWriteResultFile(maestroDir string, modifyFn func(rf *model.CommandResultFile) error) error {
	path := filepath.Join(maestroDir, "results", "planner.yaml")

	var rf model.CommandResultFile
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err == nil {
		if perr := yamlutil.SafeUnmarshal(data, &rf); perr != nil {
			return fmt.Errorf("parse existing result file: %w", perr)
		}
	}
	if rf.SchemaVersion == 0 {
		rf.SchemaVersion = 1
		rf.FileType = "result_command"
	}

	if err := modifyFn(&rf); err != nil {
		return err
	}

	return yamlutil.AtomicWrite(path, rf)
}

// readModifyWriteCommandQueue performs the common read-modify-write pattern on the planner queue file.
// It reads the existing queue file, calls modifyFn to apply changes, and atomically writes the result.
// If modifyFn returns false, the write is skipped (no-op).
//
// A missing planner.yaml is treated as an empty queue rather than a hard
// error. The file is created lazily on first write — a fresh setup, a
// formation that has not yet dispatched a planner command, or a manual
// cleanup can all leave it absent. Earlier versions surfaced this case as
// `INTERNAL_ERROR: open .../queue/planner.yaml: no such file or directory`,
// which sometimes wedged plan_complete after side-effects had already
// landed (Report 2026-05-04 issue-2). Treating absence as empty keeps the
// flow idempotent and allows the caller's modifyFn to populate the queue
// from scratch.
func readModifyWriteCommandQueue(maestroDir string, modifyFn func(cq *model.CommandQueue) (writeNeeded bool)) error {
	path := plannerQueueFilePath(maestroDir)

	var cq model.CommandQueue
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	switch {
	case err == nil:
		if err := yamlutil.SafeUnmarshal(data, &cq); err != nil {
			return fmt.Errorf("parse planner queue: %w", err)
		}
	case errors.Is(err, os.ErrNotExist):
		// Empty queue — modifyFn may populate it. Initialise schema
		// metadata so the first write produces a well-formed file.
		cq.SchemaVersion = 1
		cq.FileType = "queue_command"
	default:
		return fmt.Errorf("read planner queue: %w", err)
	}

	if !modifyFn(&cq) {
		return nil
	}

	return yamlutil.AtomicWrite(path, cq)
}

// readModifyWriteQueue performs the common read-modify-write pattern on a worker queue file.
// It reads the existing queue, calls modifyFn to apply changes, and atomically writes the result.
// If modifyFn panics, the original queue state is preserved (no write occurs) and the panic
// is converted to an error.
func readModifyWriteQueue(maestroDir string, workerID string, modifyFn func(tq *model.TaskQueue)) (retErr error) {
	queueFile := workerQueuePath(maestroDir, workerID)

	var tq model.TaskQueue
	data, err := os.ReadFile(queueFile) //nolint:gosec // queueFile is constructed from a controlled application queue directory
	if err == nil {
		if err := yamlutil.SafeUnmarshal(data, &tq); err != nil {
			return fmt.Errorf("parse existing queue %s: %w", workerID, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read queue %s: %w", workerID, err)
	}
	if tq.SchemaVersion == 0 {
		tq.SchemaVersion = 1
		tq.FileType = "queue_task"
	}

	// Recover from panics in modifyFn to prevent queue corruption.
	// The original queue data is not written back on panic.
	func() {
		defer func() {
			if r := recover(); r != nil {
				retErr = fmt.Errorf("modifyFn panicked for queue %s: %v", workerID, r)
			}
		}()
		modifyFn(&tq)
	}()
	if retErr != nil {
		return retErr
	}

	if err := yamlutil.AtomicWrite(queueFile, tq); err != nil {
		return fmt.Errorf("write queue %s: %w", workerID, err)
	}
	return nil
}
