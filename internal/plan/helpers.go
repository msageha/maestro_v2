package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// readModifyWriteQueue performs the common read-modify-write pattern on a worker queue file.
// It reads the existing queue, calls modifyFn to apply changes, and atomically writes the result.
func readModifyWriteQueue(maestroDir string, workerID string, modifyFn func(tq *model.TaskQueue)) error {
	queueFile := filepath.Join(maestroDir, "queue", workerIDToQueueFile(workerID))

	var tq model.TaskQueue
	data, err := os.ReadFile(queueFile)
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

	modifyFn(&tq)

	if err := yamlutil.AtomicWrite(queueFile, tq); err != nil {
		return fmt.Errorf("write queue %s: %w", workerID, err)
	}
	return nil
}
