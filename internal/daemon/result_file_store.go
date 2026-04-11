package daemon

import (
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
	return resultFilePath(s.maestroDir, reporter)
}

func (s *fsResultFileStore) QueueFilePath(reporter string) string {
	return taskQueuePath(s.maestroDir, reporter)
}

func (s *fsResultFileStore) LoadResultFile(reporter string) (model.TaskResultFile, error) {
	rf, _, err := loadYAMLFile[model.TaskResultFile](s.ResultFilePath(reporter), true)
	return rf, err
}

func (s *fsResultFileStore) SaveResultFile(reporter string, rf model.TaskResultFile) error {
	return yamlutil.AtomicWrite(s.ResultFilePath(reporter), rf)
}

func (s *fsResultFileStore) LoadQueueFile(reporter string) (model.TaskQueue, error) {
	tq, _, err := loadYAMLFile[model.TaskQueue](s.QueueFilePath(reporter), false)
	return tq, err
}

func (s *fsResultFileStore) SaveQueueFile(reporter string, tq model.TaskQueue) error {
	return yamlutil.AtomicWrite(s.QueueFilePath(reporter), tq)
}

func (s *fsResultFileStore) LoadCommandState(commandID string) (model.CommandState, error) {
	cs, _, err := loadYAMLFile[model.CommandState](commandStatePath(s.maestroDir, commandID), false)
	return cs, err
}
