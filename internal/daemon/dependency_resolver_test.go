package daemon

import (
	"bytes"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// mockStateReader implements StateReader for testing.
type mockStateReader struct {
	taskStates       map[string]model.Status // key: "commandID:taskID"
	phases           map[string][]PhaseInfo  // key: commandID
	deps             map[string][]string     // key: "commandID:taskID" → dep task IDs
	systemCommitReady map[string][2]bool     // key: "commandID:taskID" → [isSystemCommit, ready]
}

func (m *mockStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	key := commandID + ":" + taskID
	s, ok := m.taskStates[key]
	if !ok {
		return "", fmt.Errorf("task %s not found in command %s", taskID, commandID)
	}
	return s, nil
}

func (m *mockStateReader) GetCommandPhases(commandID string) ([]PhaseInfo, error) {
	p, ok := m.phases[commandID]
	if !ok {
		return nil, fmt.Errorf("command %s not found", commandID)
	}
	return p, nil
}

func (m *mockStateReader) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	key := commandID + ":" + taskID
	return m.deps[key], nil
}

func (m *mockStateReader) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	return nil
}

func (m *mockStateReader) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	if m.taskStates == nil {
		m.taskStates = make(map[string]model.Status)
	}
	m.taskStates[commandID+":"+taskID] = newStatus
	return nil
}

func (m *mockStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	return false, nil
}

func (m *mockStateReader) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	if m.systemCommitReady == nil {
		return false, false, nil
	}
	key := commandID + ":" + taskID
	v, ok := m.systemCommitReady[key]
	if !ok {
		return false, false, nil
	}
	return v[0], v[1], nil
}

func newTestDependencyResolver(reader StateReader) *DependencyResolver {
	return NewDependencyResolver(reader, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
}

func TestIsTaskBlocked_NoDeps(t *testing.T) {
	dr := newTestDependencyResolver(nil)
	task := &model.Task{ID: "t1", BlockedBy: nil}

	blocked, err := dr.IsTaskBlocked(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocked {
		t.Error("task with no deps should not be blocked")
	}
}

func TestIsTaskBlocked_AllCompleted(t *testing.T) {
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:dep1": model.StatusCompleted,
			"cmd1:dep2": model.StatusCompleted,
		},
	}
	dr := newTestDependencyResolver(reader)
	task := &model.Task{
		ID:        "t1",
		CommandID: "cmd1",
		BlockedBy: []string{"dep1", "dep2"},
	}

	blocked, err := dr.IsTaskBlocked(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if blocked {
		t.Error("task should be unblocked when all deps completed")
	}
}

func TestIsTaskBlocked_DepPending(t *testing.T) {
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:dep1": model.StatusCompleted,
			"cmd1:dep2": model.StatusPending,
		},
	}
	dr := newTestDependencyResolver(reader)
	task := &model.Task{
		ID:        "t1",
		CommandID: "cmd1",
		BlockedBy: []string{"dep1", "dep2"},
	}

	blocked, err := dr.IsTaskBlocked(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !blocked {
		t.Error("task should be blocked when dep is pending")
	}
}

func TestCheckDependencyFailure(t *testing.T) {
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:dep1": model.StatusCompleted,
			"cmd1:dep2": model.StatusFailed,
		},
	}
	dr := newTestDependencyResolver(reader)
	task := &model.Task{
		ID:        "t1",
		CommandID: "cmd1",
		BlockedBy: []string{"dep1", "dep2"},
	}

	depID, status, err := dr.CheckDependencyFailure(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depID != "dep2" {
		t.Errorf("expected dep2, got %s", depID)
	}
	if status != model.StatusFailed {
		t.Errorf("expected failed, got %s", status)
	}
}

func TestCheckDependencyFailure_NoFailure(t *testing.T) {
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:dep1": model.StatusCompleted,
		},
	}
	dr := newTestDependencyResolver(reader)
	task := &model.Task{
		ID:        "t1",
		CommandID: "cmd1",
		BlockedBy: []string{"dep1"},
	}

	depID, _, err := dr.CheckDependencyFailure(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depID != "" {
		t.Errorf("expected no failure, got dep=%s", depID)
	}
}

func TestFindTransitiveDependents(t *testing.T) {
	dr := newTestDependencyResolver(nil)
	tasks := []model.Task{
		{ID: "t1"},                        // root
		{ID: "t2", BlockedBy: []string{"t1"}},  // depends on t1
		{ID: "t3", BlockedBy: []string{"t2"}},  // depends on t2
		{ID: "t4", BlockedBy: []string{"t1"}},  // depends on t1
		{ID: "t5"},                        // independent
	}

	dependents := dr.FindTransitiveDependents("cmd1", "t1", tasks)

	depSet := make(map[string]bool)
	for _, d := range dependents {
		depSet[d] = true
	}

	if !depSet["t2"] {
		t.Error("expected t2 in dependents")
	}
	if !depSet["t3"] {
		t.Error("expected t3 in dependents (transitive)")
	}
	if !depSet["t4"] {
		t.Error("expected t4 in dependents")
	}
	if depSet["t5"] {
		t.Error("t5 should not be in dependents")
	}
	if depSet["t1"] {
		t.Error("t1 (the failed task itself) should not be in dependents")
	}
}

func TestCheckPhaseTransitions_ActiveCompleted(t *testing.T) {
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:task1": model.StatusCompleted,
			"cmd1:task2": model.StatusCompleted,
		},
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:              "phase1",
					Name:            "build",
					Status:          model.PhaseStatusActive,
					RequiredTaskIDs: []string{"task1", "task2"},
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(transitions))
	}
	if transitions[0].NewStatus != model.PhaseStatusCompleted {
		t.Errorf("expected completed, got %s", transitions[0].NewStatus)
	}
}

func TestCheckPhaseTransitions_ActiveFailed(t *testing.T) {
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:task1": model.StatusCompleted,
			"cmd1:task2": model.StatusFailed,
		},
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:              "phase1",
					Name:            "build",
					Status:          model.PhaseStatusActive,
					RequiredTaskIDs: []string{"task1", "task2"},
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(transitions))
	}
	if transitions[0].NewStatus != model.PhaseStatusFailed {
		t.Errorf("expected failed, got %s", transitions[0].NewStatus)
	}
}

func TestCheckPhaseTransitions_PendingActivation(t *testing.T) {
	reader := &mockStateReader{
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:     "phase1",
					Name:   "foundation",
					Status: model.PhaseStatusCompleted,
				},
				{
					ID:        "phase2",
					Name:      "features",
					Status:    model.PhaseStatusPending,
					DependsOn: []string{"phase1"},
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(transitions))
	}
	if transitions[0].NewStatus != model.PhaseStatusAwaitingFill {
		t.Errorf("expected awaiting_fill, got %s", transitions[0].NewStatus)
	}
}

func TestCheckPhaseTransitions_CascadeCancel(t *testing.T) {
	reader := &mockStateReader{
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:     "phase1",
					Name:   "foundation",
					Status: model.PhaseStatusFailed,
				},
				{
					ID:        "phase2",
					Name:      "features",
					Status:    model.PhaseStatusPending,
					DependsOn: []string{"phase1"},
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(transitions))
	}
	if transitions[0].NewStatus != model.PhaseStatusCancelled {
		t.Errorf("expected cancelled, got %s", transitions[0].NewStatus)
	}
}

func TestCheckPhaseTransitions_AwaitingFillTimeout(t *testing.T) {
	pastDeadline := time.Now().Add(-time.Hour).Format(time.RFC3339)
	reader := &mockStateReader{
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:             "phase1",
					Name:           "build",
					Status:         model.PhaseStatusAwaitingFill,
					FillDeadlineAt: &pastDeadline,
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(transitions) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(transitions))
	}
	if transitions[0].NewStatus != model.PhaseStatusTimedOut {
		t.Errorf("expected timed_out, got %s", transitions[0].NewStatus)
	}
}

func TestCheckPhaseTransitions_AwaitingFillNotExpired(t *testing.T) {
	futureDeadline := time.Now().Add(time.Hour).Format(time.RFC3339)
	reader := &mockStateReader{
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:             "phase1",
					Name:           "build",
					Status:         model.PhaseStatusAwaitingFill,
					FillDeadlineAt: &futureDeadline,
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(transitions) != 0 {
		t.Errorf("expected no transitions, got %d", len(transitions))
	}
}

func TestBuildAwaitingFillNotification(t *testing.T) {
	dr := newTestDependencyResolver(nil)
	phase := PhaseInfo{
		ID:   "phase2",
		Name: "features",
	}

	msg := dr.BuildAwaitingFillNotification("cmd1", phase)
	if msg == "" {
		t.Error("expected non-empty notification")
	}
	expected := "phase:features phase_id:phase2 status:awaiting_fill command_id:cmd1"
	if !contains(msg, expected) {
		t.Errorf("expected %q in message, got %q", expected, msg)
	}
}

func TestIsTaskBlocked_NilStateReader(t *testing.T) {
	dr := newTestDependencyResolver(nil)
	task := &model.Task{
		ID:        "t1",
		CommandID: "cmd1",
		BlockedBy: []string{"dep1"},
	}

	blocked, err := dr.IsTaskBlocked(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !blocked {
		t.Error("task with deps and nil state reader should be blocked")
	}
}

func TestIsSystemCommitReady_NotSystemCommit(t *testing.T) {
	reader := &mockStateReader{
		systemCommitReady: nil, // no entries → all tasks are non-system-commit
	}
	dr := newTestDependencyResolver(reader)

	isSys, ready, err := dr.IsSystemCommitReady("cmd1", "task1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isSys {
		t.Error("expected isSystemCommit=false")
	}
	if ready {
		t.Error("expected ready=false")
	}
}

func TestIsSystemCommitReady_PhasesNotTerminal(t *testing.T) {
	reader := &mockStateReader{
		systemCommitReady: map[string][2]bool{
			"cmd1:sys_task": {true, false}, // is system commit, NOT ready
		},
	}
	dr := newTestDependencyResolver(reader)

	isSys, ready, err := dr.IsSystemCommitReady("cmd1", "sys_task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isSys {
		t.Error("expected isSystemCommit=true")
	}
	if ready {
		t.Error("expected ready=false (phases not all terminal)")
	}
}

func TestIsSystemCommitReady_AllPhasesTerminal(t *testing.T) {
	reader := &mockStateReader{
		systemCommitReady: map[string][2]bool{
			"cmd1:sys_task": {true, true}, // is system commit, ready
		},
	}
	dr := newTestDependencyResolver(reader)

	isSys, ready, err := dr.IsSystemCommitReady("cmd1", "sys_task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isSys {
		t.Error("expected isSystemCommit=true")
	}
	if !ready {
		t.Error("expected ready=true (all phases terminal)")
	}
}

func TestIsSystemCommitReady_NilStateReader(t *testing.T) {
	dr := newTestDependencyResolver(nil)

	isSys, ready, err := dr.IsSystemCommitReady("cmd1", "sys_task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isSys || ready {
		t.Error("nil state reader should return false, false")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
