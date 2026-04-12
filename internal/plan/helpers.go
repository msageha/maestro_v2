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
