package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// ResultFileStore abstracts file I/O for result and queue files used by
// resultWritePhaseA. Implementations must be safe for use under the caller's
// lock discipline (the caller serializes access via file + mutex locks).
type ResultFileStore interface {
	LoadResultFile(reporter string) (model.TaskResultFile, error)
	SaveResultFile(reporter string, rf model.TaskResultFile) error
	LoadQueueFile(reporter string) (model.TaskQueue, error)
	SaveQueueFile(reporter string, tq model.TaskQueue) error
	LoadCommandState(commandID string) (model.CommandState, error)
	ResultFilePath(reporter string) string
	QueueFilePath(reporter string) string
}

// fsResultFileStore is the filesystem-backed implementation of ResultFileStore.
type fsResultFileStore struct {
	maestroDir string
}

func newFSResultFileStore(maestroDir string) *fsResultFileStore {
	return &fsResultFileStore{maestroDir: maestroDir}
}

func (s *fsResultFileStore) ResultFilePath(reporter string) string {
	return filepath.Join(s.maestroDir, "results", reporter+".yaml")
}

func (s *fsResultFileStore) QueueFilePath(reporter string) string {
	return filepath.Join(s.maestroDir, "queue", reporter+".yaml")
}

func (s *fsResultFileStore) LoadResultFile(reporter string) (model.TaskResultFile, error) {
	var rf model.TaskResultFile
	data, err := os.ReadFile(s.ResultFilePath(reporter))
	if err == nil {
		if err := yamlv3.Unmarshal(data, &rf); err != nil {
			return rf, fmt.Errorf("parse results file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return rf, fmt.Errorf("read results file: %w", err)
	}
	return rf, nil
}

func (s *fsResultFileStore) SaveResultFile(reporter string, rf model.TaskResultFile) error {
	return yamlutil.AtomicWrite(s.ResultFilePath(reporter), rf)
}

func (s *fsResultFileStore) LoadQueueFile(reporter string) (model.TaskQueue, error) {
	var tq model.TaskQueue
	data, err := os.ReadFile(s.QueueFilePath(reporter))
	if err != nil {
		return tq, fmt.Errorf("read worker queue: %w", err)
	}
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		return tq, fmt.Errorf("parse worker queue: %w", err)
	}
	return tq, nil
}

func (s *fsResultFileStore) SaveQueueFile(reporter string, tq model.TaskQueue) error {
	return yamlutil.AtomicWrite(s.QueueFilePath(reporter), tq)
}

func (s *fsResultFileStore) LoadCommandState(commandID string) (model.CommandState, error) {
	var cs model.CommandState
	path := filepath.Join(s.maestroDir, "state", "commands", commandID+".yaml")
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from a controlled application directory
	if err != nil {
		return cs, err
	}
	if err := yamlv3.Unmarshal(data, &cs); err != nil {
		return cs, fmt.Errorf("parse state: %w", err)
	}
	return cs, nil
}
