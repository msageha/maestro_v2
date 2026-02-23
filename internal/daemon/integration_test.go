package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// --- File-based StateReader for integration tests (avoids plan→daemon import cycle) ---

type integrationStateReader struct {
	maestroDir string
	lockMap    *lock.MutexMap
}

func (r *integrationStateReader) loadState(commandID string) (*model.CommandState, error) {
	path := filepath.Join(r.maestroDir, "state", "commands", commandID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state model.CommandState
	if err := yamlv3.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (r *integrationStateReader) saveState(state *model.CommandState) error {
	path := filepath.Join(r.maestroDir, "state", "commands", state.CommandID+".yaml")
	return yamlutil.AtomicWrite(path, state)
}

func (r *integrationStateReader) GetTaskState(commandID, taskID string) (model.Status, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return "", err
	}
	status, ok := state.TaskStates[taskID]
	if !ok {
		return "", fmt.Errorf("task %s not found in command %s", taskID, commandID)
	}
	return status, nil
}

func (r *integrationStateReader) GetCommandPhases(commandID string) ([]PhaseInfo, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return nil, err
	}
	if len(state.Phases) == 0 {
		return nil, nil
	}
	phaseNameToID := make(map[string]string)
	for _, p := range state.Phases {
		phaseNameToID[p.Name] = p.PhaseID
	}
	var phases []PhaseInfo
	for _, p := range state.Phases {
		var depIDs []string
		for _, depName := range p.DependsOnPhases {
			if id, ok := phaseNameToID[depName]; ok {
				depIDs = append(depIDs, id)
			}
		}
		phaseTaskSet := make(map[string]bool)
		for _, tid := range p.TaskIDs {
			phaseTaskSet[tid] = true
		}
		var requiredTaskIDs []string
		for _, tid := range state.RequiredTaskIDs {
			if phaseTaskSet[tid] {
				requiredTaskIDs = append(requiredTaskIDs, tid)
			}
		}
		isSystemCommit := state.SystemCommitTaskID != nil && phaseTaskSet[*state.SystemCommitTaskID]
		phases = append(phases, PhaseInfo{
			ID:               p.PhaseID,
			Name:             p.Name,
			Status:           p.Status,
			DependsOn:        depIDs,
			FillDeadlineAt:   p.FillDeadlineAt,
			RequiredTaskIDs:  requiredTaskIDs,
			SystemCommitTask: isSystemCommit,
		})
	}
	return phases, nil
}

func (r *integrationStateReader) GetTaskDependencies(commandID, taskID string) ([]string, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return nil, err
	}
	return state.TaskDependencies[taskID], nil
}

func (r *integrationStateReader) ApplyPhaseTransition(commandID, phaseID string, newStatus model.PhaseStatus) error {
	r.lockMap.Lock(commandID)
	defer r.lockMap.Unlock(commandID)

	state, err := r.loadState(commandID)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range state.Phases {
		if state.Phases[i].PhaseID == phaseID {
			state.Phases[i].Status = newStatus
			if model.IsPhaseTerminal(newStatus) {
				state.Phases[i].CompletedAt = &now
			}
			break
		}
	}
	state.UpdatedAt = now
	return r.saveState(state)
}

func (r *integrationStateReader) UpdateTaskState(commandID, taskID string, newStatus model.Status, cancelledReason string) error {
	r.lockMap.Lock(commandID)
	defer r.lockMap.Unlock(commandID)

	state, err := r.loadState(commandID)
	if err != nil {
		return err
	}
	if state.TaskStates == nil {
		state.TaskStates = make(map[string]model.Status)
	}
	state.TaskStates[taskID] = newStatus
	if cancelledReason != "" {
		if state.CancelledReasons == nil {
			state.CancelledReasons = make(map[string]string)
		}
		state.CancelledReasons[taskID] = cancelledReason
	}
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return r.saveState(state)
}

func (r *integrationStateReader) IsSystemCommitReady(commandID, taskID string) (bool, bool, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return false, false, err
	}
	if state.SystemCommitTaskID == nil || *state.SystemCommitTaskID != taskID {
		return false, false, nil
	}
	if len(state.Phases) == 0 {
		for tid, s := range state.TaskStates {
			if tid == taskID {
				continue
			}
			if !model.IsTerminal(s) {
				return true, false, nil
			}
		}
		return true, true, nil
	}
	for _, phase := range state.Phases {
		if !model.IsPhaseTerminal(phase.Status) {
			return true, false, nil
		}
	}
	return true, true, nil
}

func (r *integrationStateReader) IsCommandCancelRequested(commandID string) (bool, error) {
	state, err := r.loadState(commandID)
	if err != nil {
		return false, err
	}
	return state.Cancel.Requested, nil
}

// testCanComplete is a simplified version of plan.CanComplete for integration tests.
func testCanComplete(state *model.CommandState) (model.PlanStatus, error) {
	if state.PlanStatus != model.PlanStatusSealed {
		return "", fmt.Errorf("plan_status must be sealed, got %s", state.PlanStatus)
	}

	var nonTerminal []string
	for _, taskID := range state.RequiredTaskIDs {
		status, ok := state.TaskStates[taskID]
		if !ok {
			nonTerminal = append(nonTerminal, taskID+" (unknown)")
			continue
		}
		if !model.IsTerminal(status) {
			nonTerminal = append(nonTerminal, fmt.Sprintf("%s (%s)", taskID, status))
		}
	}
	if len(nonTerminal) > 0 {
		return "", fmt.Errorf("required tasks not terminal: %s", strings.Join(nonTerminal, ", "))
	}

	hasFailed := false
	hasCancelled := false
	for _, taskID := range state.RequiredTaskIDs {
		switch state.TaskStates[taskID] {
		case model.StatusFailed, model.StatusDeadLetter:
			hasFailed = true
		case model.StatusCancelled:
			hasCancelled = true
		}
	}
	if hasFailed {
		return model.PlanStatusFailed, nil
	}
	if hasCancelled {
		return model.PlanStatusCancelled, nil
	}
	return model.PlanStatusCompleted, nil
}

// --- Integration test helpers ---

// newIntegrationDaemon creates a fully wired daemon with mock executor for integration tests.
func newIntegrationDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := newTestDaemon(t)

	// Wire file-based state reader (avoids plan→daemon import cycle)
	lockMap := lock.NewMutexMap()
	reader := &integrationStateReader{maestroDir: d.maestroDir, lockMap: lockMap}
	d.handler = NewQueueHandler(d.maestroDir, d.config, lockMap, d.logger, d.logLevel)
	d.handler.SetStateReader(reader)
	d.handler.SetCanComplete(testCanComplete)

	// Mock executor: always succeeds delivery
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	})
	d.handler.SetBusyChecker(func(string) bool { return false })

	// Ensure dead_letters and state dirs exist
	for _, sub := range []string{"dead_letters", "quarantine", "state"} {
		os.MkdirAll(filepath.Join(d.maestroDir, sub), 0755)
	}

	// Clean up maestroDir before TempDir cleanup to prevent "directory not empty" errors.
	// handleResultWrite's Phase C spawns `go PeriodicScan()` which creates dashboard/metrics
	// files asynchronously — wait for it to finish, then remove.
	t.Cleanup(func() {
		d.handler.scanMu.Lock()
		d.handler.scanMu.Unlock()
		os.RemoveAll(d.maestroDir)
	})

	return d
}

func writeCommand(t *testing.T, d *Daemon, content string) string {
	t.Helper()
	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "command",
		Content: content,
	})
	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("queue write command: %v", resp.Error)
	}
	var result map[string]string
	json.Unmarshal(resp.Data, &result)
	return result["id"]
}

func writeTask(t *testing.T, d *Daemon, target, commandID, content, purpose, criteria string, bloomLevel int) string {
	t.Helper()
	req := makeQueueWriteRequest(t, QueueWriteParams{
		Target:             target,
		Type:               "task",
		CommandID:          commandID,
		Content:            content,
		Purpose:            purpose,
		AcceptanceCriteria: criteria,
		BloomLevel:         bloomLevel,
	})
	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("queue write task: %v", resp.Error)
	}
	var result map[string]string
	json.Unmarshal(resp.Data, &result)
	return result["id"]
}

func writeTaskWithDeps(t *testing.T, d *Daemon, target, commandID, content string, blockedBy []string) string {
	t.Helper()
	req := makeQueueWriteRequest(t, QueueWriteParams{
		Target:             target,
		Type:               "task",
		CommandID:          commandID,
		Content:            content,
		Purpose:            "test",
		AcceptanceCriteria: "passes",
		BloomLevel:         3,
		BlockedBy:          blockedBy,
	})
	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("queue write task with deps: %v", resp.Error)
	}
	var result map[string]string
	json.Unmarshal(resp.Data, &result)
	return result["id"]
}

func writeResult(t *testing.T, d *Daemon, reporter, taskID, commandID, status, summary string, leaseEpoch int) string {
	t.Helper()
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   reporter,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: leaseEpoch,
		Status:     status,
		Summary:    summary,
		RetrySafe:  true,
	})
	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("result write: %v", resp.Error)
	}
	var result map[string]string
	json.Unmarshal(resp.Data, &result)
	return result["result_id"]
}

func writeCancelRequest(t *testing.T, d *Daemon, commandID, reason string) {
	t.Helper()
	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:      "cancel-request",
		CommandID: commandID,
		Reason:    reason,
	})
	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("cancel request: %v", resp.Error)
	}
}

func readCommandQueue(t *testing.T, d *Daemon) model.CommandQueue {
	t.Helper()
	path := filepath.Join(d.maestroDir, "queue", "planner.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return model.CommandQueue{}
	}
	var cq model.CommandQueue
	yamlv3.Unmarshal(data, &cq)
	return cq
}

func readTaskQueue(t *testing.T, d *Daemon, workerID string) model.TaskQueue {
	t.Helper()
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return model.TaskQueue{}
	}
	var tq model.TaskQueue
	yamlv3.Unmarshal(data, &tq)
	return tq
}

func readCommandState(t *testing.T, d *Daemon, commandID string) model.CommandState {
	t.Helper()
	path := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state %s: %v", commandID, err)
	}
	var state model.CommandState
	yamlv3.Unmarshal(data, &state)
	return state
}

func readResultFile(t *testing.T, d *Daemon, workerID string) model.TaskResultFile {
	t.Helper()
	path := filepath.Join(d.maestroDir, "results", workerID+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return model.TaskResultFile{}
	}
	var rf model.TaskResultFile
	yamlv3.Unmarshal(data, &rf)
	return rf
}

// --- 12.1 Integration Test Scenarios ---

// Scenario 1: Normal flow — command → dispatch → task → result → state update
func TestIntegration_NormalFlow(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_0000000001_aabbcc01"
	taskID := "task_0000000001_aabbcc01"
	workerID := "worker1"

	// Step 1: Write a command to planner queue
	cmdID := writeCommand(t, d, "implement feature X")
	if cmdID == "" {
		t.Fatal("expected command ID")
	}

	// Verify command is pending
	cq := readCommandQueue(t, d)
	if len(cq.Commands) != 1 || cq.Commands[0].Status != model.StatusPending {
		t.Fatalf("expected 1 pending command, got %+v", cq.Commands)
	}

	// Step 2: Run periodic scan — dispatches command to planner
	d.handler.PeriodicScan()
	cq = readCommandQueue(t, d)
	if len(cq.Commands) != 1 || cq.Commands[0].Status != model.StatusInProgress {
		t.Fatalf("expected command in_progress after dispatch, got %s", cq.Commands[0].Status)
	}

	// Step 3: Simulate plan submit — create state + queue task
	setupCommandState(t, d, commandID, []string{taskID})
	setupWorkerQueue(t, d, workerID, taskID, commandID, 1)

	// Step 4: Write task result
	resultID := writeResult(t, d, workerID, taskID, commandID, "completed", "done", 1)
	if resultID == "" {
		t.Fatal("expected result ID")
	}

	// Verify result was recorded
	rf := readResultFile(t, d, workerID)
	if len(rf.Results) != 1 || rf.Results[0].Status != model.StatusCompleted {
		t.Fatalf("expected 1 completed result, got %+v", rf.Results)
	}

	// Verify queue task updated to completed
	tq := readTaskQueue(t, d, workerID)
	if len(tq.Tasks) != 1 || tq.Tasks[0].Status != model.StatusCompleted {
		t.Fatalf("expected task completed in queue, got %s", tq.Tasks[0].Status)
	}

	// Verify state updated
	state := readCommandState(t, d, commandID)
	if state.TaskStates[taskID] != model.StatusCompleted {
		t.Errorf("task state = %s, want completed", state.TaskStates[taskID])
	}
}

// Scenario 2: Cancel flow — cancel-request → pending/in_progress tasks cancelled
func TestIntegration_CancelFlow(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_0000000002_aabbcc02"
	taskID1 := "task_0000000002_aabbcc01"
	taskID2 := "task_0000000002_aabbcc02"

	// Setup: command in planner queue (in_progress, already dispatched)
	now := time.Now().UTC().Format(time.RFC3339)
	plannerOwner := "planner"
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:         commandID,
				Content:    "feature X",
				Status:     model.StatusInProgress,
				LeaseOwner: &plannerOwner,
				LeaseEpoch: 1,
				Attempts:   1,
				CreatedAt:  now,
				UpdatedAt:  now,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	// Setup: command state with 2 tasks
	state := model.CommandState{
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       commandID,
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: []string{taskID1, taskID2},
		TaskStates:      map[string]model.Status{taskID1: model.StatusPending, taskID2: model.StatusInProgress},
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	statePath := filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml")
	yamlutil.AtomicWrite(statePath, state)

	// Setup queue with pending + in_progress tasks
	owner := "worker1"
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{ID: taskID1, CommandID: commandID, Status: model.StatusPending, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
			{ID: taskID2, CommandID: commandID, Status: model.StatusInProgress, LeaseOwner: &owner, LeaseEpoch: 1, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	// Issue cancel request
	writeCancelRequest(t, d, commandID, "user requested")

	// Verify cancel.requested is set in state
	updatedState := readCommandState(t, d, commandID)
	if !updatedState.Cancel.Requested {
		t.Fatal("expected cancel.requested = true")
	}

	// Run scan — should cancel pending + interrupt in_progress
	d.handler.PeriodicScan()

	tqAfter := readTaskQueue(t, d, "worker1")
	for _, task := range tqAfter.Tasks {
		if task.Status != model.StatusCancelled {
			t.Errorf("task %s status = %s, want cancelled", task.ID, task.Status)
		}
	}
}

// Scenario 3: Dependency failure propagation — failed task → dependent cancelled
func TestIntegration_DependencyFailurePropagation(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_0000000003_aabbcc03"
	taskA := "task_0000000003_aabbcc01"
	taskB := "task_0000000003_aabbcc02" // depends on taskA

	// Setup state
	state := model.CommandState{
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       commandID,
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: []string{taskA, taskB},
		TaskStates:      map[string]model.Status{taskA: model.StatusFailed, taskB: model.StatusPending},
		TaskDependencies: map[string][]string{
			taskB: {taskA},
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml"), state)

	// Setup queue: taskB pending with blocked_by
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{ID: taskB, CommandID: commandID, Status: model.StatusPending, BlockedBy: []string{taskA}, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	// Run periodic scan — should detect dependency failure and cancel taskB
	d.handler.PeriodicScan()

	tqAfter := readTaskQueue(t, d, "worker1")
	if len(tqAfter.Tasks) != 1 || tqAfter.Tasks[0].Status != model.StatusCancelled {
		t.Fatalf("expected taskB cancelled, got %+v", tqAfter.Tasks)
	}
}

// Scenario 4: Lease expiry recovery — expired lease → back to pending
func TestIntegration_LeaseExpiryRecovery(t *testing.T) {
	d := newIntegrationDaemon(t)
	taskID := "task_0000000004_aabbcc01"
	commandID := "cmd_0000000004_aabbcc04"

	// Setup: task with expired lease
	owner := "worker1"
	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             taskID,
				CommandID:      commandID,
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expired,
				LeaseEpoch:     1,
				CreatedAt:      "2026-01-01T00:00:00Z",
				UpdatedAt:      "2026-01-01T00:00:00Z",
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	// Run scan — should recover expired lease
	d.handler.PeriodicScan()

	tqAfter := readTaskQueue(t, d, "worker1")
	if len(tqAfter.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tqAfter.Tasks))
	}
	task := tqAfter.Tasks[0]
	if task.Status != model.StatusPending {
		t.Errorf("status = %s, want pending", task.Status)
	}
	if task.LeaseOwner != nil {
		t.Error("expected lease_owner nil after recovery")
	}
}

// Scenario 4b: Lease expiry with busy agent — lease extended
func TestIntegration_LeaseExpiryBusyExtend(t *testing.T) {
	d := newIntegrationDaemon(t)
	d.handler.SetBusyChecker(func(string) bool { return true }) // always busy

	taskID := "task_0000000004_aabbcc02"
	commandID := "cmd_0000000004_aabbcc05"

	owner := "worker1"
	expired := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	recentUpdate := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             taskID,
				CommandID:      commandID,
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expired,
				LeaseEpoch:     1,
				CreatedAt:      "2026-01-01T00:00:00Z",
				UpdatedAt:      recentUpdate,
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	d.handler.PeriodicScan()

	tqAfter := readTaskQueue(t, d, "worker1")
	task := tqAfter.Tasks[0]
	if task.Status != model.StatusInProgress {
		t.Errorf("status = %s, want in_progress (busy extend)", task.Status)
	}
	if task.LeaseOwner == nil {
		t.Error("expected lease_owner retained")
	}
}

// Scenario 5: Dead letter — max_attempts exceeded → dead_letters/ archive
func TestIntegration_DeadLetter(t *testing.T) {
	d := newIntegrationDaemon(t)
	d.config.Retry.CommandDispatch = 3
	// Recreate handler with updated config so DeadLetterProcessor picks up retry config
	lockMap := d.handler.lockMap
	reader := &integrationStateReader{maestroDir: d.maestroDir, lockMap: lockMap}
	d.handler = NewQueueHandler(d.maestroDir, d.config, lockMap, d.logger, d.logLevel)
	d.handler.SetStateReader(reader)
	d.handler.SetCanComplete(testCanComplete)
	d.handler.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{result: agent.ExecResult{Success: true}}, nil
	})
	d.handler.SetBusyChecker(func(string) bool { return false })

	// Setup: command with attempts >= max
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []model.Command{
			{
				ID:        "cmd_0000000005_aabbcc06",
				Content:   "will be dead lettered",
				Status:    model.StatusPending,
				Attempts:  5,
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "planner.yaml"), cq)

	d.handler.PeriodicScan()

	// Verify command removed from queue
	cqAfter := readCommandQueue(t, d)
	pendingCount := 0
	for _, cmd := range cqAfter.Commands {
		if cmd.Status == model.StatusPending {
			pendingCount++
		}
	}
	if pendingCount != 0 {
		t.Errorf("expected 0 pending commands, got %d", pendingCount)
	}

	// Verify dead_letters directory has the archive
	dlDir := filepath.Join(d.maestroDir, "dead_letters")
	entries, _ := os.ReadDir(dlDir)
	if len(entries) == 0 {
		t.Error("expected dead letter archive file")
	}
}

// Scenario 6: Backpressure — queue full → reject
func TestIntegration_Backpressure(t *testing.T) {
	d := newIntegrationDaemon(t)
	d.config.Limits.MaxPendingCommands = 2

	// Write up to limit
	writeCommand(t, d, "cmd 1")
	writeCommand(t, d, "cmd 2")

	// 3rd should fail
	req := makeQueueWriteRequest(t, QueueWriteParams{
		Type:    "command",
		Content: "cmd 3",
	})
	resp := d.handleQueueWrite(req)
	if resp.Success {
		t.Fatal("expected backpressure rejection")
	}
	if resp.Error.Code != uds.ErrCodeBackpressure {
		t.Errorf("error code = %q, want BACKPRESSURE", resp.Error.Code)
	}
}

// Scenario 7: Result write idempotency — duplicate task_id accepted
func TestIntegration_ResultWriteIdempotency(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_0000000007_aabbcc07"
	taskID := "task_0000000007_aabbcc07"
	workerID := "worker1"

	setupWorkerQueue(t, d, workerID, taskID, commandID, 1)
	setupCommandState(t, d, commandID, []string{taskID})

	// First write
	resultID1 := writeResult(t, d, workerID, taskID, commandID, "completed", "done", 1)
	if resultID1 == "" {
		t.Fatal("expected result ID")
	}

	// Duplicate write — should succeed (idempotent)
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "done again",
		RetrySafe:  true,
	})
	resp := d.handleResultWrite(req)
	if !resp.Success {
		t.Fatalf("duplicate result write should succeed: %v", resp.Error)
	}

	// Verify only 1 result recorded
	rf := readResultFile(t, d, workerID)
	if len(rf.Results) != 1 {
		t.Errorf("expected 1 result (idempotent), got %d", len(rf.Results))
	}
}

// Scenario 8: Fencing — stale lease epoch rejected
func TestIntegration_ResultWriteFencing(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_0000000008_aabbcc08"
	taskID := "task_0000000008_aabbcc08"
	workerID := "worker1"

	// Setup with lease_epoch=3
	setupWorkerQueue(t, d, workerID, taskID, commandID, 3)
	setupCommandState(t, d, commandID, []string{taskID})

	// Write with stale epoch=1
	req := makeResultWriteRequest(t, ResultWriteParams{
		Reporter:   workerID,
		TaskID:     taskID,
		CommandID:  commandID,
		LeaseEpoch: 1,
		Status:     "completed",
		Summary:    "stale",
		RetrySafe:  true,
	})
	resp := d.handleResultWrite(req)
	if resp.Success {
		t.Fatal("expected fencing rejection")
	}
	if resp.Error.Code != uds.ErrCodeFencingReject {
		t.Errorf("error code = %q, want FENCING_REJECT", resp.Error.Code)
	}
}

// Scenario 9: Reconciliation R1 — results terminal + queue in_progress → fix queue
func TestIntegration_ReconciliationR1(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_0000000009_aabbcc09"
	taskID := "task_0000000009_aabbcc09"
	workerID := "worker1"

	// Setup inconsistent state: result says completed, queue says in_progress
	owner := workerID
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:         taskID,
				CommandID:  commandID,
				Status:     model.StatusInProgress,
				LeaseOwner: &owner,
				LeaseEpoch: 1,
				CreatedAt:  "2026-01-01T00:00:00Z",
				UpdatedAt:  "2026-01-01T00:00:00Z",
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	// Result says completed
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_0000000009_aabbcc09",
				TaskID:    taskID,
				CommandID: commandID,
				Status:    model.StatusCompleted,
				Summary:   "done",
				Notified:  true,
				CreatedAt: "2026-01-01T00:00:00Z",
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "results", "worker1.yaml"), rf)

	// Run scan with reconciliation
	d.handler.PeriodicScan()

	// Verify queue was fixed
	tqAfter := readTaskQueue(t, d, workerID)
	if len(tqAfter.Tasks) == 0 {
		t.Fatal("expected tasks after reconciliation")
	}
	for _, task := range tqAfter.Tasks {
		if task.ID == taskID && task.Status == model.StatusInProgress {
			t.Error("R1: queue should no longer be in_progress after reconciliation")
		}
	}
}

// Scenario 10: Reconciliation R2 — results terminal + state non-terminal → fix state
func TestIntegration_ReconciliationR2(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_0000000010_aabbcc10"
	taskID := "task_0000000010_aabbcc10"

	// State says in_progress
	state := model.CommandState{
		SchemaVersion:   1,
		FileType:        "state_command",
		CommandID:       commandID,
		PlanStatus:      model.PlanStatusSealed,
		RequiredTaskIDs: []string{taskID},
		TaskStates:      map[string]model.Status{taskID: model.StatusInProgress},
		AppliedResultIDs: map[string]string{},
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml"), state)

	// Queue says completed (terminal)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{ID: taskID, CommandID: commandID, Status: model.StatusCompleted, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", "worker1.yaml"), tq)

	// Result says completed
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_0000000010_aabbcc10",
				TaskID:    taskID,
				CommandID: commandID,
				Status:    model.StatusCompleted,
				Summary:   "done",
				Notified:  true,
				CreatedAt: "2026-01-01T00:00:00Z",
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "results", "worker1.yaml"), rf)

	d.handler.PeriodicScan()

	// Verify state was fixed
	stateAfter := readCommandState(t, d, commandID)
	if stateAfter.TaskStates[taskID] != model.StatusCompleted {
		t.Errorf("R2: task state = %s, want completed", stateAfter.TaskStates[taskID])
	}
}

// Scenario 11: YAML corruption recovery — quarantine → .bak restore → skeleton
func TestIntegration_YAMLCorruptionRecovery(t *testing.T) {
	d := newIntegrationDaemon(t)

	queuePath := filepath.Join(d.maestroDir, "queue", "worker1.yaml")

	// Write valid data first (creates .bak)
	tq := model.TaskQueue{SchemaVersion: 1, FileType: "queue_task", Tasks: []model.Task{}}
	yamlutil.AtomicWrite(queuePath, tq)

	// Corrupt the file
	os.WriteFile(queuePath, []byte("{{{{invalid yaml\x00\x01"), 0644)

	// Attempt to recover
	err := yamlutil.RecoverCorruptedFile(d.maestroDir, queuePath, "queue_task")
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}

	// Verify file is now valid YAML
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read recovered file: %v", err)
	}
	var recovered model.TaskQueue
	if err := yamlv3.Unmarshal(data, &recovered); err != nil {
		t.Fatalf("recovered file is not valid YAML: %v", err)
	}

	// Verify quarantine dir has the corrupted file
	quarDir := filepath.Join(d.maestroDir, "quarantine")
	entries, _ := os.ReadDir(quarDir)
	if len(entries) == 0 {
		t.Error("expected corrupted file in quarantine/")
	}
}

// Scenario 12: Notification queue write + dispatch
func TestIntegration_NotificationFlow(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_0000000012_aabbcc12"

	req := makeQueueWriteRequest(t, QueueWriteParams{
		Target:           "orchestrator",
		Type:             "notification",
		CommandID:        commandID,
		Content:          "command completed",
		SourceResultID:   "res_0000000012_aabbcc12",
		NotificationType: "command_completed",
	})
	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("notification write: %v", resp.Error)
	}

	var result map[string]string
	json.Unmarshal(resp.Data, &result)
	if result["id"] == "" {
		t.Fatal("expected notification ID")
	}

	// Verify notification is pending
	nqPath := filepath.Join(d.maestroDir, "queue", "orchestrator.yaml")
	data, _ := os.ReadFile(nqPath)
	var nq model.NotificationQueue
	yamlv3.Unmarshal(data, &nq)
	if len(nq.Notifications) != 1 || nq.Notifications[0].Status != model.StatusPending {
		t.Fatalf("expected 1 pending notification, got %+v", nq.Notifications)
	}

	// Run scan — dispatches notification
	d.handler.PeriodicScan()

	data, _ = os.ReadFile(nqPath)
	yamlv3.Unmarshal(data, &nq)
	if len(nq.Notifications) != 1 || nq.Notifications[0].Status != model.StatusCompleted {
		t.Fatalf("expected notification completed after dispatch, got %s", nq.Notifications[0].Status)
	}
}

// Scenario 13: Continuous mode — iteration tracking + max_iterations stop
func TestIntegration_ContinuousMode(t *testing.T) {
	d := newIntegrationDaemon(t)
	d.config.Continuous.Enabled = true
	d.config.Continuous.MaxIterations = 2

	// Initialize continuous state
	cont := model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 0,
		MaxIterations:    2,
		Status:           model.ContinuousStatusRunning,
	}
	contPath := filepath.Join(d.maestroDir, "state", "continuous.yaml")
	yamlutil.AtomicWrite(contPath, cont)

	// Re-initialize continuous handler with test config
	ch := NewContinuousHandler(d.maestroDir, d.config, d.handler.lockMap, d.logger, d.logLevel)
	ch.SetNotifySender(func(string, string) error { return nil })

	// Simulate 2 iterations
	cmdID1 := "cmd_0000000013_aabbcc01"
	cmdID2 := "cmd_0000000013_aabbcc02"

	ch.CheckAndAdvance(cmdID1, model.StatusCompleted)

	// Read state
	data, _ := os.ReadFile(contPath)
	var contAfter model.Continuous
	yamlv3.Unmarshal(data, &contAfter)
	if contAfter.CurrentIteration != 1 {
		t.Errorf("iteration = %d, want 1", contAfter.CurrentIteration)
	}
	if contAfter.Status != model.ContinuousStatusRunning {
		t.Errorf("status = %s, want running", contAfter.Status)
	}

	ch.CheckAndAdvance(cmdID2, model.StatusCompleted)
	data, _ = os.ReadFile(contPath)
	yamlv3.Unmarshal(data, &contAfter)
	if contAfter.CurrentIteration != 2 {
		t.Errorf("iteration = %d, want 2", contAfter.CurrentIteration)
	}
	if contAfter.Status != model.ContinuousStatusStopped {
		t.Errorf("status = %s, want stopped (max_iterations reached)", contAfter.Status)
	}
}

// Scenario 13b: Continuous mode idempotency — same command_id doesn't double-increment
func TestIntegration_ContinuousModeIdempotency(t *testing.T) {
	d := newIntegrationDaemon(t)
	d.config.Continuous.Enabled = true
	d.config.Continuous.MaxIterations = 10

	cont := model.Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 0,
		MaxIterations:    10,
		Status:           model.ContinuousStatusRunning,
	}
	contPath := filepath.Join(d.maestroDir, "state", "continuous.yaml")
	yamlutil.AtomicWrite(contPath, cont)

	ch := NewContinuousHandler(d.maestroDir, d.config, d.handler.lockMap, d.logger, d.logLevel)
	ch.SetNotifySender(func(string, string) error { return nil })

	cmdID := "cmd_0000000013_aabbcc03"
	ch.CheckAndAdvance(cmdID, model.StatusCompleted)
	ch.CheckAndAdvance(cmdID, model.StatusCompleted) // duplicate

	data, _ := os.ReadFile(contPath)
	var contAfter model.Continuous
	yamlv3.Unmarshal(data, &contAfter)
	if contAfter.CurrentIteration != 1 {
		t.Errorf("iteration = %d, want 1 (idempotent guard)", contAfter.CurrentIteration)
	}
}

// Scenario 14: Metrics update verification
func TestIntegration_MetricsUpdate(t *testing.T) {
	d := newIntegrationDaemon(t)

	// Write a command
	writeCommand(t, d, "test for metrics")

	// Run scan
	d.handler.PeriodicScan()

	// Read metrics
	metricsPath := filepath.Join(d.maestroDir, "state", "metrics.yaml")
	data, err := os.ReadFile(metricsPath)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	var m model.Metrics
	if err := yamlv3.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse metrics: %v", err)
	}

	if m.DaemonHeartbeat == nil {
		t.Error("expected daemon_heartbeat set")
	}
	if m.Counters.CommandsDispatched < 1 {
		t.Errorf("commands_dispatched = %d, want >= 1", m.Counters.CommandsDispatched)
	}
}

// Scenario 15: Dashboard generation
func TestIntegration_DashboardGeneration(t *testing.T) {
	d := newIntegrationDaemon(t)

	writeCommand(t, d, "test dashboard")
	d.handler.PeriodicScan()

	dashPath := filepath.Join(d.maestroDir, "dashboard.md")
	data, err := os.ReadFile(dashPath)
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	content := string(data)
	if len(content) == 0 {
		t.Error("dashboard.md is empty")
	}
}

// Scenario 16: Full pipeline — command write → dispatch → task write → dispatch → result → state
func TestIntegration_FullPipeline(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_0000000016_aabbcc16"
	taskID := "task_0000000016_aabbcc16"
	workerID := "worker1"

	// 1. Write command
	cmdID := writeCommand(t, d, "full pipeline test")
	if cmdID == "" {
		t.Fatal("expected command ID")
	}

	// 2. Dispatch command
	d.handler.PeriodicScan()
	cq := readCommandQueue(t, d)
	if cq.Commands[0].Status != model.StatusInProgress {
		t.Fatalf("command not dispatched: %s", cq.Commands[0].Status)
	}

	// 3. Simulate plan submit (create state + task queue entry)
	state := model.CommandState{
		SchemaVersion:    1,
		FileType:         "state_command",
		CommandID:        commandID,
		PlanStatus:       model.PlanStatusSealed,
		ExpectedTaskCount: 1,
		RequiredTaskIDs:  []string{taskID},
		TaskStates:       map[string]model.Status{taskID: model.StatusPending},
		TaskDependencies: map[string][]string{},
		AppliedResultIDs: map[string]string{},
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "state", "commands", commandID+".yaml"), state)

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:        taskID,
				CommandID: commandID,
				Purpose:   "implement feature",
				Content:   "write code",
				BloomLevel: 3,
				Status:    model.StatusPending,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	yamlutil.AtomicWrite(filepath.Join(d.maestroDir, "queue", workerID+".yaml"), tq)

	// 4. Dispatch task
	d.handler.PeriodicScan()
	tqAfter := readTaskQueue(t, d, workerID)
	if tqAfter.Tasks[0].Status != model.StatusInProgress {
		t.Fatalf("task not dispatched: %s", tqAfter.Tasks[0].Status)
	}
	leaseEpoch := tqAfter.Tasks[0].LeaseEpoch

	// 5. Write result
	resultID := writeResult(t, d, workerID, taskID, commandID, "completed", "all good", leaseEpoch)
	if resultID == "" {
		t.Fatal("expected result ID")
	}

	// 6. Verify end state
	stateAfter := readCommandState(t, d, commandID)
	if stateAfter.TaskStates[taskID] != model.StatusCompleted {
		t.Errorf("final task state = %s, want completed", stateAfter.TaskStates[taskID])
	}

	rf := readResultFile(t, d, workerID)
	found := false
	for _, r := range rf.Results {
		if r.TaskID == taskID && r.Status == model.StatusCompleted {
			found = true
		}
	}
	if !found {
		t.Error("result not found in results file")
	}
}

// Scenario 17: Cancel before submit — cancel-request on non-submitted command
func TestIntegration_CancelBeforeSubmit(t *testing.T) {
	d := newIntegrationDaemon(t)

	// Write command
	cmdID := writeCommand(t, d, "will be cancelled before submit")
	if cmdID == "" {
		t.Fatal("expected command ID")
	}

	// Cancel it (no state/commands/ exists yet)
	writeCancelRequest(t, d, cmdID, "changed mind")

	// Verify the queue entry was cancelled
	cq := readCommandQueue(t, d)
	found := false
	for _, cmd := range cq.Commands {
		if cmd.ID == cmdID {
			if cmd.Status != model.StatusCancelled {
				t.Errorf("status = %s, want cancelled", cmd.Status)
			}
			if cmd.CancelReason == nil || *cmd.CancelReason != "changed mind" {
				t.Errorf("cancel_reason = %v, want 'changed mind'", cmd.CancelReason)
			}
			found = true
		}
	}
	if !found {
		t.Error("cancelled command not found in queue")
	}
}

// Scenario 18: Task queue write with backpressure per worker
func TestIntegration_TaskBackpressure(t *testing.T) {
	d := newIntegrationDaemon(t)
	d.config.Limits.MaxPendingTasksPerWorker = 2

	commandID := "cmd_0000000018_aabbcc18"

	// Write 2 tasks
	writeTask(t, d, "worker1", commandID, "task1", "purpose", "criteria", 3)
	writeTask(t, d, "worker1", commandID, "task2", "purpose", "criteria", 3)

	// 3rd should fail
	req := makeQueueWriteRequest(t, QueueWriteParams{
		Target:             "worker1",
		Type:               "task",
		CommandID:          commandID,
		Content:            "task3",
		Purpose:            "purpose",
		AcceptanceCriteria: "criteria",
		BloomLevel:         3,
	})
	resp := d.handleQueueWrite(req)
	if resp.Success {
		t.Fatal("expected task backpressure rejection")
	}
	if resp.Error.Code != uds.ErrCodeBackpressure {
		t.Errorf("error code = %q, want BACKPRESSURE", resp.Error.Code)
	}
}

// Scenario 19: Notification idempotency — same source_result_id deduped
func TestIntegration_NotificationIdempotency(t *testing.T) {
	d := newIntegrationDaemon(t)
	commandID := "cmd_0000000019_aabbcc19"
	sourceResultID := "res_0000000019_aabbcc19"

	// Write notification
	req := makeQueueWriteRequest(t, QueueWriteParams{
		Target:           "orchestrator",
		Type:             "notification",
		CommandID:        commandID,
		Content:          "done",
		SourceResultID:   sourceResultID,
		NotificationType: "command_completed",
	})
	resp := d.handleQueueWrite(req)
	if !resp.Success {
		t.Fatalf("first notification: %v", resp.Error)
	}
	var result1 map[string]string
	json.Unmarshal(resp.Data, &result1)

	// Duplicate
	resp2 := d.handleQueueWrite(req)
	if !resp2.Success {
		t.Fatalf("duplicate notification should succeed: %v", resp2.Error)
	}
	var result2 map[string]string
	json.Unmarshal(resp2.Data, &result2)

	if result2["duplicate"] != "true" {
		t.Errorf("expected duplicate=true, got %q", result2["duplicate"])
	}

	// Verify only 1 notification in queue
	nqPath := filepath.Join(d.maestroDir, "queue", "orchestrator.yaml")
	data, _ := os.ReadFile(nqPath)
	var nq model.NotificationQueue
	yamlv3.Unmarshal(data, &nq)
	if len(nq.Notifications) != 1 {
		t.Errorf("expected 1 notification (deduped), got %d", len(nq.Notifications))
	}
}

// Scenario 20: Graceful shutdown — daemon cleanup verification
func TestIntegration_GracefulShutdown(t *testing.T) {
	d := newIntegrationDaemon(t)

	// Create socket file to verify cleanup
	sockPath := filepath.Join(d.maestroDir, uds.DefaultSocketName)
	os.WriteFile(sockPath, []byte("test"), 0600)

	// Create lock file
	lockDir := filepath.Join(d.maestroDir, "locks")
	os.MkdirAll(lockDir, 0755)

	d.Shutdown()

	// Socket should be removed
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Error("expected socket file removed after shutdown")
	}
}

// Scenario 21: Queue write validation
func TestIntegration_QueueWriteValidation(t *testing.T) {
	d := newIntegrationDaemon(t)

	tests := []struct {
		name   string
		params QueueWriteParams
	}{
		{"empty type", QueueWriteParams{Type: "", Content: "x"}},
		{"command without content", QueueWriteParams{Type: "command"}},
		{"task without command_id", QueueWriteParams{Type: "task", Content: "x", Purpose: "p", AcceptanceCriteria: "a", BloomLevel: 3}},
		{"task without purpose", QueueWriteParams{Type: "task", CommandID: "cmd_0000000001_abcdef01", Content: "x", AcceptanceCriteria: "a", BloomLevel: 3}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeQueueWriteRequest(t, tt.params)
			resp := d.handleQueueWrite(req)
			if resp.Success {
				t.Error("expected validation error")
			}
		})
	}
}

// Scenario 22: Command dispatch + lease assignment
func TestIntegration_CommandDispatchLease(t *testing.T) {
	d := newIntegrationDaemon(t)

	writeCommand(t, d, "test lease")
	d.handler.PeriodicScan()

	cq := readCommandQueue(t, d)
	cmd := cq.Commands[0]
	if cmd.Status != model.StatusInProgress {
		t.Errorf("status = %s, want in_progress", cmd.Status)
	}
	if cmd.LeaseOwner == nil {
		t.Error("expected lease_owner set")
	}
	if cmd.LeaseEpoch < 1 {
		t.Errorf("lease_epoch = %d, want >= 1", cmd.LeaseEpoch)
	}
	if cmd.Attempts < 1 {
		t.Errorf("attempts = %d, want >= 1", cmd.Attempts)
	}
}

func init() {
	// Suppress unused import warnings if any test uses fmt
	_ = fmt.Sprint
}
