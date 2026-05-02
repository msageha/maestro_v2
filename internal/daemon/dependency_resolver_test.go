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
	taskStates        map[string]model.Status // key: "commandID:taskID"
	phases            map[string][]PhaseInfo  // key: commandID
	deps              map[string][]string     // key: "commandID:taskID" → dep task IDs
	systemCommitReady map[string][2]bool      // key: "commandID:taskID" → [isSystemCommit, ready]
	// cancelledReasons records SetPhaseCancelledReason calls so cascade
	// recovery tests can assert the resolver wrote the expected marker.
	cancelledReasons map[string]*string // key: "commandID:phaseID"
	// effectiveStates lets a test simulate the lineage-aware GetEffectiveTaskStatus
	// outcome for a specific (command, task) pair. When a key is present here it
	// overrides taskStates for the GetEffectiveTaskStatus path; the raw GetTaskState
	// path is unaffected. Tests that don't care leave this nil and the default
	// behaviour delegates to GetTaskState.
	effectiveStates map[string]model.Status // key: "commandID:taskID"
}

func (m *mockStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	key := commandID + ":" + taskID
	s, ok := m.taskStates[key]
	if !ok {
		return "", fmt.Errorf("task %s not found in command %s", taskID, commandID)
	}
	return s, nil
}

func (m *mockStateReader) GetEffectiveTaskStatus(commandID, taskID string) (model.Status, error) {
	if m.effectiveStates != nil {
		if s, ok := m.effectiveStates[commandID+":"+taskID]; ok {
			return s, nil
		}
	}
	return m.GetTaskState(commandID, taskID)
}

func (m *mockStateReader) GetEffectiveTaskStatusForCompletion(commandID, taskID string) (model.Status, error) {
	return m.GetEffectiveTaskStatus(commandID, taskID)
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

func (m *mockStateReader) SetPhaseCancelledReason(commandID, phaseID string, reason *string) error {
	if m.cancelledReasons == nil {
		m.cancelledReasons = make(map[string]*string)
	}
	m.cancelledReasons[commandID+":"+phaseID] = reason
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

func (m *mockStateReader) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	return &model.CircuitBreakerState{}, nil
}

func (m *mockStateReader) HasNonTerminalTaskState(commandID string) (bool, error) {
	return false, nil
}

func (m *mockStateReader) GetNonTerminalTaskStates(commandID string) (map[string]model.Status, error) {
	return nil, nil
}

func (m *mockStateReader) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	return nil
}

func (m *mockStateReader) MarkAwaitingFillStallNotified(string, string, string) error { return nil }

func (m *mockStateReader) MarkCircuitBreakerProgress(string) error { return nil }

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

func newTestDependencyResolver(reader StateManager) *DependencyResolver {
	return NewDependencyResolver(reader, log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
}

func TestIsTaskBlocked_NoDeps(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestCheckDependencyFailure_LineageRecovery covers the verify-repair recovery
// case: a dep is cancelled (raw status) but its lineage successor has already
// completed, so the lineage-aware effective status is Completed. The resolver
// must NOT report a dependency failure — otherwise the dependent task would be
// preemptively cancelled while its upstream's repair was actually a success.
func TestCheckDependencyFailure_LineageRecovery(t *testing.T) {
	t.Parallel()
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:dep1": model.StatusCancelled, // raw: superseded
		},
		effectiveStates: map[string]model.Status{
			"cmd1:dep1": model.StatusCompleted, // successor in lineage completed
		},
	}
	dr := newTestDependencyResolver(reader)
	task := &model.Task{
		ID:        "t1",
		CommandID: "cmd1",
		BlockedBy: []string{"dep1"},
	}

	depID, status, err := dr.CheckDependencyFailure(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if depID != "" {
		t.Errorf("expected no dep failure when lineage successor completed, got dep=%q status=%s", depID, status)
	}

	blocked, err := dr.IsTaskBlocked(task)
	if err != nil {
		t.Fatalf("IsTaskBlocked unexpected error: %v", err)
	}
	if blocked {
		t.Error("expected task to be unblocked when lineage successor completed")
	}
}

// TestCheckDependencyFailure_LineageStillRunning covers the in-flight case:
// the raw dep is cancelled (superseded), the lineage successor is still
// running. The dependency must NOT be flagged as failure (so the dependent
// stays pending, not cancelled), AND the task must remain blocked (the
// successor has not completed yet).
func TestCheckDependencyFailure_LineageStillRunning(t *testing.T) {
	t.Parallel()
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:dep1": model.StatusCancelled,
		},
		effectiveStates: map[string]model.Status{
			"cmd1:dep1": model.StatusInProgress, // successor running
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
		t.Errorf("expected no dep failure when lineage successor in flight, got dep=%q", depID)
	}

	blocked, err := dr.IsTaskBlocked(task)
	if err != nil {
		t.Fatalf("IsTaskBlocked unexpected error: %v", err)
	}
	if !blocked {
		t.Error("expected task to remain blocked while lineage successor in flight")
	}
}

func TestCheckDependencyFailure_NoFailure(t *testing.T) {
	t.Parallel()
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

// TestCheckPhaseTransitions_AllOptionalPhaseCompletes pins Bug-J: a phase
// whose tasks are all classified as optional (empty RequiredTaskIDs) must
// still transition to PhaseStatusCompleted once every task finishes.
// Earlier the resolver bailed out on empty RequiredTaskIDs, leaving the
// phase pinned at Active forever and triggering fast_track_cleanup ->
// MarkIntegrationFailed despite a wholly successful run.
func TestCheckPhaseTransitions_AllOptionalPhaseCompletes(t *testing.T) {
	t.Parallel()
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:task1": model.StatusCompleted,
			"cmd1:task2": model.StatusCompleted,
		},
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:              "phase1",
					Name:            "verification",
					Status:          model.PhaseStatusActive,
					TaskIDs:         []string{"task1", "task2"},
					RequiredTaskIDs: []string{}, // every task optional
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(transitions) != 1 || transitions[0].NewStatus != model.PhaseStatusCompleted {
		t.Errorf("expected all-optional phase to complete, got %+v", transitions)
	}
}

// TestCheckPhaseTransitions_ActiveLineageRepairInFlight pins Bug-D':
// when a phase's required task has been superseded by a repair successor
// that is still in flight, the phase MUST NOT transition to Cancelled.
// Doing so cascades cancellation to dependent phases before the repair
// has any chance to complete, ultimately driving the plan to
// PlanStatusCancelled despite the repair eventually succeeding.
func TestCheckPhaseTransitions_ActiveLineageRepairInFlight(t *testing.T) {
	t.Parallel()
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:task1": model.StatusCancelled, // raw: superseded by repair
		},
		effectiveStates: map[string]model.Status{
			"cmd1:task1": model.StatusPlanned, // lineage successor is queued
		},
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:              "phase1",
					Name:            "runtime_and_fixes",
					Status:          model.PhaseStatusActive,
					RequiredTaskIDs: []string{"task1"},
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, tr := range transitions {
		if tr.PhaseID == "phase1" && tr.NewStatus == model.PhaseStatusCancelled {
			t.Errorf("phase must NOT cancel while lineage successor is in flight; got %+v", tr)
		}
	}
}

// TestCheckPhaseTransitions_ActiveLineageRepairCompleted: once the lineage
// successor has completed, the predecessor is treated as effectively
// completed and the phase transitions to Completed.
func TestCheckPhaseTransitions_ActiveLineageRepairCompleted(t *testing.T) {
	t.Parallel()
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:task1": model.StatusCancelled,
		},
		effectiveStates: map[string]model.Status{
			"cmd1:task1": model.StatusCompleted,
		},
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:              "phase1",
					Name:            "runtime_and_fixes",
					Status:          model.PhaseStatusActive,
					RequiredTaskIDs: []string{"task1"},
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(transitions) != 1 || transitions[0].NewStatus != model.PhaseStatusCompleted {
		t.Errorf("expected phase Completed via lineage successor, got %+v", transitions)
	}
}

func TestCheckPhaseTransitions_ActiveCompleted(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	// The resolver must record the upstream dep in CancelledReason via
	// the canonical helper so a later cascade recovery can find it
	// without parsing free-form text.
	if transitions[0].CancelledReason == "" {
		t.Errorf("expected CancelledReason to carry the cascade marker, got empty")
	}
	if dep, ok := model.DependencyCascadeDepID(&transitions[0].CancelledReason); !ok || dep != "phase1" {
		t.Errorf("expected cascade marker to point at phase1, got CancelledReason=%q", transitions[0].CancelledReason)
	}
}

// TestCheckPhaseTransitions_RecoversCascadeCancelledPhase asserts that
// when an upstream failed phase is reopened (e.g. via add-retry-task →
// reopenPhase: failed→active), the downstream phase that was previously
// cascade-cancelled is auto-recovered to pending so plan progression
// resumes. Without this recovery, the planner would stay stuck waiting
// for a phase that the state
// machine has frozen as terminal even though its blocker is back in
// flight.
func TestCheckPhaseTransitions_RecoversCascadeCancelledPhase(t *testing.T) {
	t.Parallel()
	cascadeReason := model.NewDependencyCascadeCancelReason("phase1")
	reader := &mockStateReader{
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					// Upstream was failed, now reopened to active
					// (via add-retry-task).
					ID:     "phase1",
					Name:   "foundation",
					Status: model.PhaseStatusActive,
				},
				{
					// Downstream was cancelled by the resolver while
					// phase1 was failed; the cascade marker is recorded.
					ID:              "phase2",
					Name:            "features",
					Status:          model.PhaseStatusCancelled,
					DependsOn:       []string{"phase1"},
					CancelledReason: &cascadeReason,
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var recovery *PhaseTransitionResult
	for i := range transitions {
		if transitions[i].PhaseID == "phase2" {
			recovery = &transitions[i]
		}
	}
	if recovery == nil {
		t.Fatalf("expected a recovery transition for phase2; got %+v", transitions)
	}
	if recovery.OldStatus != model.PhaseStatusCancelled {
		t.Errorf("OldStatus = %q, want %q", recovery.OldStatus, model.PhaseStatusCancelled)
	}
	if recovery.NewStatus != model.PhaseStatusPending {
		t.Errorf("NewStatus = %q, want %q (cascade recovery flips back to pending)", recovery.NewStatus, model.PhaseStatusPending)
	}
	if !recovery.ClearCancelledReason {
		t.Errorf("ClearCancelledReason must be true so the stale marker is wiped")
	}
}

// TestCheckPhaseTransitions_DoesNotRecoverManualCancellation pins the
// safety boundary: a phase whose CancelledReason is nil (no cascade
// marker) was cancelled out-of-band, and the resolver MUST NOT touch
// it even if the dep happens to be active. The recovery is restricted
// to scheduler-derived cascade cancellations.
func TestCheckPhaseTransitions_DoesNotRecoverManualCancellation(t *testing.T) {
	t.Parallel()
	reader := &mockStateReader{
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:     "phase1",
					Name:   "foundation",
					Status: model.PhaseStatusActive,
				},
				{
					// CancelledReason is nil → operator/manual cancel.
					ID:              "phase2",
					Name:            "features",
					Status:          model.PhaseStatusCancelled,
					DependsOn:       []string{"phase1"},
					CancelledReason: nil,
				},
			},
		},
	}
	dr := newTestDependencyResolver(reader)

	transitions, err := dr.CheckPhaseTransitions("cmd1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, tr := range transitions {
		if tr.PhaseID == "phase2" {
			t.Errorf("manual cancellation must remain terminal, got transition %+v", tr)
		}
	}
}

func TestCheckPhaseTransitions_AwaitingFillTimeout(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	dr := newTestDependencyResolver(nil)
	task := &model.Task{
		ID:        "t1",
		CommandID: "cmd1",
		BlockedBy: []string{"dep1"},
	}

	blocked, err := dr.IsTaskBlocked(task)
	if err == nil {
		t.Fatal("expected error for nil state manager, got nil")
	}
	if !blocked {
		t.Error("task with deps and nil state reader should be blocked")
	}
}

func TestIsSystemCommitReady_NotSystemCommit(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// mockStateReaderErrNotFound returns ErrStateNotFound for GetCommandPhases
type mockStateReaderErrNotFound struct{}

func (m *mockStateReaderErrNotFound) GetTaskState(commandID, taskID string) (model.Status, error) {
	return "", ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) GetEffectiveTaskStatus(commandID, taskID string) (model.Status, error) {
	return "", ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) GetEffectiveTaskStatusForCompletion(commandID, taskID string) (model.Status, error) {
	return "", ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) GetCommandPhases(commandID string) ([]PhaseInfo, error) {
	return nil, ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	return nil, ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	return ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) SetPhaseCancelledReason(commandID, phaseID string, reason *string) error {
	return ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	return ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) IsCommandCancelRequested(commandID string) (bool, error) {
	return false, ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) GetCircuitBreakerState(commandID string) (*model.CircuitBreakerState, error) {
	return nil, ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) HasNonTerminalTaskState(commandID string) (bool, error) {
	return false, ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) GetNonTerminalTaskStates(commandID string) (map[string]model.Status, error) {
	return nil, ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) TripCircuitBreaker(commandID string, reason string, progressTimeoutMinutes int) error {
	return ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) MarkAwaitingFillStallNotified(string, string, string) error {
	return ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) MarkCircuitBreakerProgress(string) error {
	return ErrStateNotFound
}

func (m *mockStateReaderErrNotFound) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	return false, false, ErrStateNotFound
}

// TestCheckPhaseTransitions_StateNotFound verifies that CheckPhaseTransitions
// returns nil when state doesn't exist (command not yet submitted by planner).
func TestCheckPhaseTransitions_StateNotFound(t *testing.T) {
	t.Parallel()
	reader := &mockStateReaderErrNotFound{}
	dr := newTestDependencyResolver(reader)

	// Should return nil, nil when state doesn't exist (no error)
	transitions, err := dr.CheckPhaseTransitions("cmd_not_yet_submitted")
	if err != nil {
		t.Fatalf("expected nil error when state not found, got: %v", err)
	}
	if transitions != nil {
		t.Errorf("expected nil transitions when state not found, got: %v", transitions)
	}
}

// TestCheckPhaseTransitions_SameCycleCascade verifies that when an active phase
// completes and a pending phase depends on it, both transitions are returned in
// the same CheckPhaseTransitions call. This is the two-pass fix for the bug where
// stepWorktreeFastTrackCleanup killed pending phases in the same scan cycle that
// their dependency phase completed — before they could activate.
func TestCheckPhaseTransitions_SameCycleCascade(t *testing.T) {
	t.Parallel()
	reader := &mockStateReader{
		taskStates: map[string]model.Status{
			"cmd1:task1": model.StatusCompleted,
		},
		phases: map[string][]PhaseInfo{
			"cmd1": {
				{
					ID:              "phase1",
					Name:            "foundation",
					Status:          model.PhaseStatusActive,
					RequiredTaskIDs: []string{"task1"},
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

	if len(transitions) != 2 {
		t.Fatalf("expected 2 transitions (phase1→completed, phase2→awaiting_fill), got %d: %v",
			len(transitions), transitions)
	}

	byPhase := make(map[string]model.PhaseStatus, len(transitions))
	for _, tr := range transitions {
		byPhase[tr.PhaseID] = tr.NewStatus
	}

	if byPhase["phase1"] != model.PhaseStatusCompleted {
		t.Errorf("phase1: expected completed, got %s", byPhase["phase1"])
	}
	if byPhase["phase2"] != model.PhaseStatusAwaitingFill {
		t.Errorf("phase2: expected awaiting_fill, got %s", byPhase["phase2"])
	}
}
