package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// nowUTC returns the current time in UTC formatted as RFC3339.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// readModifyWriteResultFile performs the common read-modify-write pattern on the planner result file.
// It reads the existing result file, calls modifyFn to apply changes, and atomically writes the result.
// If modifyFn returns an error, the write is skipped and the error is returned.
func readModifyWriteResultFile(maestroDir string, modifyFn func(rf *model.CommandResultFile) error) error {
	path := filepath.Join(maestroDir, "results", "planner.yaml")

	var rf model.CommandResultFile
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err == nil {
		if perr := yamlv3.Unmarshal(data, &rf); perr != nil {
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
func readModifyWriteCommandQueue(maestroDir string, modifyFn func(cq *model.CommandQueue) (writeNeeded bool)) error {
	path := filepath.Join(maestroDir, "queue", "planner.yaml")

	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		return fmt.Errorf("read planner queue: %w", err)
	}

	var cq model.CommandQueue
	if err := yamlv3.Unmarshal(data, &cq); err != nil {
		return fmt.Errorf("parse planner queue: %w", err)
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
	queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(workerID))

	var tq model.TaskQueue
	data, err := os.ReadFile(queueFile) //nolint:gosec // queueFile is constructed from a controlled application queue directory
	if err == nil {
		if err := yamlv3.Unmarshal(data, &tq); err != nil {
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
