package reconcile

import (
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// TestR1ConsumeQueueWriteFailed_ClearsWhenQueueTerminal verifies the H2
// sticky-error cleanup path: a QueueWriteFailed marker in command state is
// cleared once the corresponding worker queue task has reached a terminal
// state (e.g. after Phase 1 of R1 repaired the mismatch in an earlier scan).
func TestR1ConsumeQueueWriteFailed_ClearsWhenQueueTerminal(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000001_abcdef01"
	taskID := "task_0000000001_abcdef01"
	workerID := "worker1"
	resultID := "res_0000000001_abcdef01"

	// Worker queue: task already terminal (queue was repaired by some earlier
	// scan or by Phase 1 of the current scan).
	owner := workerID
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{{
			ID:         taskID,
			CommandID:  commandID,
			Status:     model.StatusCompleted,
			LeaseOwner: &owner,
			LeaseEpoch: 1,
			CreatedAt:  "2026-01-01T00:00:00Z",
			UpdatedAt:  "2026-01-01T00:00:00Z",
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", workerID+".yaml"), tq); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	// Command state with the sticky marker.
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{taskID: model.StatusCompleted},
			QueueWriteFailed: map[string]string{
				taskID: workerID + ":" + resultID,
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	run := newRun(&deps)
	repairs := r1ConsumeQueueWriteFailed(run)

	if len(repairs) != 1 {
		t.Fatalf("expected 1 repair, got %d (%+v)", len(repairs), repairs)
	}
	if repairs[0].TaskID != taskID || repairs[0].CommandID != commandID || repairs[0].Pattern != PatternR1 {
		t.Errorf("unexpected repair: %+v", repairs[0])
	}

	got, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if _, exists := got.QueueWriteFailed[taskID]; exists {
		t.Errorf("QueueWriteFailed[%s] should be cleared, got map=%v", taskID, got.QueueWriteFailed)
	}
	if got.LastReconciledAt == nil {
		t.Error("expected LastReconciledAt to be set after cleanup")
	}
}

// TestR1ConsumeQueueWriteFailed_KeepsWhenQueueStillInProgress verifies that
// the marker is preserved when the queue task has not yet been repaired —
// the next scan will retry the cleanup.
func TestR1ConsumeQueueWriteFailed_KeepsWhenQueueStillInProgress(t *testing.T) {
	t.Parallel()
	maestroDir := testutil.SetupDir(t)
	deps := newTestDeps(t, maestroDir)

	commandID := "cmd_0000000002_abcdef02"
	taskID := "task_0000000002_abcdef02"
	workerID := "worker1"
	resultID := "res_0000000002_abcdef02"

	owner := workerID
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{{
			ID:         taskID,
			CommandID:  commandID,
			Status:     model.StatusInProgress, // not yet repaired
			LeaseOwner: &owner,
			LeaseEpoch: 1,
			CreatedAt:  "2026-01-01T00:00:00Z",
			UpdatedAt:  "2026-01-01T00:00:00Z",
		}},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "queue", workerID+".yaml"), tq); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     commandID,
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{taskID: model.StatusCompleted},
			QueueWriteFailed: map[string]string{
				taskID: workerID + ":" + resultID,
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "commands", commandID+".yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	run := newRun(&deps)
	repairs := r1ConsumeQueueWriteFailed(run)

	if len(repairs) != 0 {
		t.Errorf("expected no repairs while queue is still in_progress, got %+v", repairs)
	}
	got, err := run.loadState(statePath)
	if err != nil {
		t.Fatalf("reload state: %v", err)
	}
	if got.QueueWriteFailed[taskID] != workerID+":"+resultID {
		t.Errorf("marker should be preserved, got %v", got.QueueWriteFailed)
	}
}

func TestParseQueueWriteFailedValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in           string
		wantWorker   string
		wantResultID string
	}{
		{"worker1:res_abc", "worker1", "res_abc"},
		{"worker10:res_xyz_123", "worker10", "res_xyz_123"},
		{"", "", ""},
		{"worker1", "", ""},
		{":res_abc", "", ""},
		{"worker1:", "", ""},
	}
	for _, tt := range tests {
		w, r := parseQueueWriteFailedValue(tt.in)
		if w != tt.wantWorker || r != tt.wantResultID {
			t.Errorf("parseQueueWriteFailedValue(%q) = (%q, %q), want (%q, %q)",
				tt.in, w, r, tt.wantWorker, tt.wantResultID)
		}
	}
}
