package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

func TestValidateFencing_TaskNotFound_ReturnsNegativeIndex(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)

	tq := &model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks:         []model.Task{}, // empty queue
	}
	rf := &model.TaskResultFile{}

	params := ResultWriteParams{
		Reporter:   "worker1",
		TaskID:     "task_0000000001_nonexist",
		CommandID:  "cmd_0000000001_abcdef01",
		LeaseEpoch: 1,
	}

	taskIdx, _, err := d.api.result.validateFencing(tq, rf, params, model.StatusCompleted)
	if err == nil {
		t.Fatal("expected error for task not found in empty queue")
	}
	if taskIdx != -1 {
		t.Errorf("taskIdx = %d, want -1", taskIdx)
	}

	rwErr, ok := err.(*resultWriteError)
	if !ok {
		t.Fatalf("expected *resultWriteError, got %T", err)
	}
	if rwErr.Code != uds.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", rwErr.Code, uds.ErrCodeNotFound)
	}
}

func TestValidateFencing_TaskNotFound_NonEmptyQueue(t *testing.T) {
	t.Parallel()
	d := newTestDaemon(t)

	owner := "worker1"
	tq := &model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:         "task_0000000001_existing",
				CommandID:  "cmd_0000000001_abcdef01",
				Status:     model.StatusInProgress,
				LeaseOwner: &owner,
				LeaseEpoch: 1,
			},
		},
	}
	rf := &model.TaskResultFile{}

	params := ResultWriteParams{
		Reporter:   "worker1",
		TaskID:     "task_0000000001_different", // not in queue
		CommandID:  "cmd_0000000001_abcdef01",
		LeaseEpoch: 1,
	}

	taskIdx, _, err := d.api.result.validateFencing(tq, rf, params, model.StatusCompleted)
	if err == nil {
		t.Fatal("expected error for task not found in queue")
	}
	if taskIdx != -1 {
		t.Errorf("taskIdx = %d, want -1", taskIdx)
	}

	rwErr, ok := err.(*resultWriteError)
	if !ok {
		t.Fatalf("expected *resultWriteError, got %T", err)
	}
	if rwErr.Code != uds.ErrCodeNotFound {
		t.Errorf("error code = %q, want %q", rwErr.Code, uds.ErrCodeNotFound)
	}
}
