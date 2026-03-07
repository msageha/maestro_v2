package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

func newTestDeadLetterProcessor(maestroDir string, cfg model.Config) *DeadLetterProcessor {
	dlp := NewDeadLetterProcessor(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	return dlp
}

func TestDeadLetter_CommandDeadLetter(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{CommandDispatch: 3},
	}
	dlp := newTestDeadLetterProcessor(maestroDir, cfg)

	// Create state dir for post-processing
	if err := os.MkdirAll(filepath.Join(maestroDir, "state", "commands"), 0755); err != nil {
		t.Fatal(err)
	}

	cq := model.CommandQueue{
		Commands: []model.Command{
			{
				ID:        "cmd_001",
				Status:    model.StatusPending,
				Attempts:  3,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "cmd_002",
				Status:    model.StatusPending,
				Attempts:  1,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "cmd_003",
				Status:    model.StatusInProgress,
				Attempts:  5,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	dirty := false
	results := dlp.ProcessCommandDeadLetters(&cq, &dirty)

	if len(results) != 1 {
		t.Fatalf("expected 1 dead letter result, got %d", len(results))
	}
	if results[0].EntryID != "cmd_001" {
		t.Errorf("expected cmd_001, got %s", results[0].EntryID)
	}
	if results[0].QueueType != "planner" {
		t.Errorf("expected planner queue type, got %s", results[0].QueueType)
	}
	if !dirty {
		t.Error("dirty should be true")
	}

	// Queue should have cmd_002 and cmd_003 remaining (cmd_001 removed)
	if len(cq.Commands) != 2 {
		t.Fatalf("expected 2 commands remaining, got %d", len(cq.Commands))
	}
	if cq.Commands[0].ID != "cmd_002" {
		t.Errorf("first command should be cmd_002, got %s", cq.Commands[0].ID)
	}
	if cq.Commands[1].ID != "cmd_003" {
		t.Errorf("second command should be cmd_003, got %s", cq.Commands[1].ID)
	}

	// Verify archive file was created
	archiveDir := filepath.Join(maestroDir, "dead_letters")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("read archive dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 archive file, got %d", len(entries))
	}
}

func TestDeadLetter_CommandDeadLetter_WithState(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{CommandDispatch: 2},
	}
	dlp := newTestDeadLetterProcessor(maestroDir, cfg)

	// Create state file for cmd_001
	stateDir := filepath.Join(maestroDir, "state", "commands")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "command_state",
		CommandID:     "cmd_001",
		PlanStatus:    model.PlanStatusSealed,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_001.yaml"), &state); err != nil {
		t.Fatal(err)
	}

	cq := model.CommandQueue{
		Commands: []model.Command{
			{
				ID:        "cmd_001",
				Status:    model.StatusPending,
				Attempts:  2,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	dirty := false
	results := dlp.ProcessCommandDeadLetters(&cq, &dirty)

	if len(results) != 1 {
		t.Fatalf("expected 1 dead letter result, got %d", len(results))
	}

	// Verify state was updated to failed
	data, err := os.ReadFile(filepath.Join(stateDir, "cmd_001.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var updatedState model.CommandState
	if err := yamlv3.Unmarshal(data, &updatedState); err != nil {
		t.Fatal(err)
	}
	if updatedState.PlanStatus != model.PlanStatusFailed {
		t.Errorf("expected plan_status=failed, got %s", updatedState.PlanStatus)
	}

	// Verify orchestrator notification was buffered (not written to disk)
	pending := dlp.DrainPendingNotifications()
	if len(pending) != 1 {
		t.Fatalf("expected 1 buffered notification, got %d", len(pending))
	}
	if pending[0].Type != "command_failed" {
		t.Errorf("expected command_failed type, got %s", pending[0].Type)
	}
	if pending[0].CommandID != "cmd_001" {
		t.Errorf("expected cmd_001 command ID, got %s", pending[0].CommandID)
	}
}

func TestDeadLetter_CommandDeadLetter_TerminalStateSkipped(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{CommandDispatch: 2},
	}
	dlp := newTestDeadLetterProcessor(maestroDir, cfg)

	// Create state file already in terminal status
	stateDir := filepath.Join(maestroDir, "state", "commands")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "command_state",
		CommandID:     "cmd_001",
		PlanStatus:    model.PlanStatusCompleted,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, "cmd_001.yaml"), &state); err != nil {
		t.Fatal(err)
	}

	cq := model.CommandQueue{
		Commands: []model.Command{
			{
				ID:        "cmd_001",
				Status:    model.StatusPending,
				Attempts:  2,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	dirty := false
	dlp.ProcessCommandDeadLetters(&cq, &dirty)

	// State should remain completed (not overwritten to failed)
	data, err := os.ReadFile(filepath.Join(stateDir, "cmd_001.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var updatedState model.CommandState
	if err := yamlv3.Unmarshal(data, &updatedState); err != nil {
		t.Fatal(err)
	}
	if updatedState.PlanStatus != model.PlanStatusCompleted {
		t.Errorf("terminal state should not be overwritten, got %s", updatedState.PlanStatus)
	}
}

func TestDeadLetter_TaskDeadLetter(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{TaskDispatch: 5},
	}
	dlp := newTestDeadLetterProcessor(maestroDir, cfg)

	// Create state and results dirs
	for _, sub := range []string{"state/commands", "results"} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}

	tq := &taskQueueEntry{
		Path: filepath.Join(maestroDir, "queue", "worker1.yaml"),
		Queue: model.TaskQueue{
			Tasks: []model.Task{
				{
					ID:        "task_001",
					CommandID: "cmd_001",
					Status:    model.StatusPending,
					Attempts:  5,
					CreatedAt: time.Now().UTC().Format(time.RFC3339),
					UpdatedAt: time.Now().UTC().Format(time.RFC3339),
				},
				{
					ID:        "task_002",
					CommandID: "cmd_001",
					Status:    model.StatusPending,
					Attempts:  2,
					CreatedAt: time.Now().UTC().Format(time.RFC3339),
					UpdatedAt: time.Now().UTC().Format(time.RFC3339),
				},
			},
		},
	}

	dirty := false
	results := dlp.ProcessTaskDeadLetters(tq, &dirty)

	if len(results) != 1 {
		t.Fatalf("expected 1 dead letter result, got %d", len(results))
	}
	if results[0].EntryID != "task_001" {
		t.Errorf("expected task_001, got %s", results[0].EntryID)
	}
	if results[0].QueueType != "worker1" {
		t.Errorf("expected worker1 queue type, got %s", results[0].QueueType)
	}
	if !dirty {
		t.Error("dirty should be true")
	}

	// Queue should have only task_002
	if len(tq.Queue.Tasks) != 1 {
		t.Fatalf("expected 1 task remaining, got %d", len(tq.Queue.Tasks))
	}
	if tq.Queue.Tasks[0].ID != "task_002" {
		t.Errorf("remaining task should be task_002, got %s", tq.Queue.Tasks[0].ID)
	}

	// Verify synthetic result was written
	resultPath := filepath.Join(maestroDir, "results", "worker1.yaml")
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var rf model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &rf); err != nil {
		t.Fatal(err)
	}
	if len(rf.Results) != 1 {
		t.Fatalf("expected 1 synthetic result, got %d", len(rf.Results))
	}
	if rf.Results[0].TaskID != "task_001" {
		t.Errorf("expected task_001, got %s", rf.Results[0].TaskID)
	}
	if rf.Results[0].Status != model.StatusFailed {
		t.Errorf("expected failed status, got %s", rf.Results[0].Status)
	}
}

func TestDeadLetter_TaskDeadLetter_StateUpdate(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{TaskDispatch: 3},
	}
	dlp := newTestDeadLetterProcessor(maestroDir, cfg)

	// Create state and results dirs
	for _, sub := range []string{"state/commands", "results"} {
		if err := os.MkdirAll(filepath.Join(maestroDir, sub), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Create command state
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "command_state",
		CommandID:     "cmd_001",
		PlanStatus:    model.PlanStatusSealed,
		TaskStates:    map[string]model.Status{"task_001": model.StatusPending},
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := yamlutil.AtomicWrite(filepath.Join(maestroDir, "state", "commands", "cmd_001.yaml"), &state); err != nil {
		t.Fatal(err)
	}

	tq := &taskQueueEntry{
		Path: filepath.Join(maestroDir, "queue", "worker1.yaml"),
		Queue: model.TaskQueue{
			Tasks: []model.Task{
				{
					ID:        "task_001",
					CommandID: "cmd_001",
					Status:    model.StatusPending,
					Attempts:  3,
					CreatedAt: time.Now().UTC().Format(time.RFC3339),
					UpdatedAt: time.Now().UTC().Format(time.RFC3339),
				},
			},
		},
	}

	dirty := false
	dlp.ProcessTaskDeadLetters(tq, &dirty)

	// Verify state was updated
	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "commands", "cmd_001.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var updatedState model.CommandState
	if err := yamlv3.Unmarshal(data, &updatedState); err != nil {
		t.Fatal(err)
	}
	if updatedState.TaskStates["task_001"] != model.StatusFailed {
		t.Errorf("expected task state failed, got %s", updatedState.TaskStates["task_001"])
	}
}

func TestDeadLetter_NotificationDeadLetter(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{OrchestratorNotificationDispatch: 3},
	}
	dlp := newTestDeadLetterProcessor(maestroDir, cfg)

	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{
				ID:        "ntf_001",
				CommandID: "cmd_001",
				Type:      "command_completed",
				Status:    model.StatusPending,
				Attempts:  3,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "ntf_002",
				CommandID: "cmd_002",
				Type:      "command_completed",
				Status:    model.StatusPending,
				Attempts:  1,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	dirty := false
	results := dlp.ProcessNotificationDeadLetters(&nq, &dirty)

	if len(results) != 1 {
		t.Fatalf("expected 1 dead letter result, got %d", len(results))
	}
	if results[0].EntryID != "ntf_001" {
		t.Errorf("expected ntf_001, got %s", results[0].EntryID)
	}
	if results[0].QueueType != "orchestrator" {
		t.Errorf("expected orchestrator queue type, got %s", results[0].QueueType)
	}
	if !dirty {
		t.Error("dirty should be true")
	}

	// Queue should have only ntf_002
	if len(nq.Notifications) != 1 {
		t.Fatalf("expected 1 notification remaining, got %d", len(nq.Notifications))
	}
	if nq.Notifications[0].ID != "ntf_002" {
		t.Errorf("remaining notification should be ntf_002, got %s", nq.Notifications[0].ID)
	}
}

func TestDeadLetter_MaxAttemptsZero_InfiniteRetries(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{
			CommandDispatch:                  0,
			TaskDispatch:                     0,
			OrchestratorNotificationDispatch: 0,
		},
	}
	dlp := newTestDeadLetterProcessor(maestroDir, cfg)

	// Commands with high attempts should not be dead-lettered when maxAttempts=0
	cq := model.CommandQueue{
		Commands: []model.Command{
			{
				ID:       "cmd_001",
				Status:   model.StatusPending,
				Attempts: 999,
			},
		},
	}

	dirty := false
	results := dlp.ProcessCommandDeadLetters(&cq, &dirty)
	if len(results) != 0 {
		t.Errorf("expected 0 dead letters with maxAttempts=0 (infinite), got %d", len(results))
	}
	if dirty {
		t.Error("dirty should be false")
	}

	// Same for tasks
	tq := &taskQueueEntry{
		Path: filepath.Join(maestroDir, "queue", "worker1.yaml"),
		Queue: model.TaskQueue{
			Tasks: []model.Task{
				{
					ID:        "task_001",
					CommandID: "cmd_001",
					Status:    model.StatusPending,
					Attempts:  999,
				},
			},
		},
	}

	dirty = false
	results = dlp.ProcessTaskDeadLetters(tq, &dirty)
	if len(results) != 0 {
		t.Errorf("expected 0 dead letters for tasks with maxAttempts=0, got %d", len(results))
	}

	// Same for notifications
	nq := model.NotificationQueue{
		Notifications: []model.Notification{
			{
				ID:       "ntf_001",
				Status:   model.StatusPending,
				Attempts: 999,
			},
		},
	}

	dirty = false
	results = dlp.ProcessNotificationDeadLetters(&nq, &dirty)
	if len(results) != 0 {
		t.Errorf("expected 0 dead letters for notifications with maxAttempts=0, got %d", len(results))
	}
}

func TestDeadLetter_NonPendingNotDeadLettered(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{CommandDispatch: 1},
	}
	dlp := newTestDeadLetterProcessor(maestroDir, cfg)

	// in_progress entries should NOT be dead-lettered even if attempts >= max
	cq := model.CommandQueue{
		Commands: []model.Command{
			{
				ID:       "cmd_001",
				Status:   model.StatusInProgress,
				Attempts: 5,
			},
			{
				ID:       "cmd_002",
				Status:   model.StatusCompleted,
				Attempts: 5,
			},
		},
	}

	dirty := false
	results := dlp.ProcessCommandDeadLetters(&cq, &dirty)
	if len(results) != 0 {
		t.Errorf("expected 0 dead letters for non-pending entries, got %d", len(results))
	}
	if dirty {
		t.Error("dirty should be false")
	}
	if len(cq.Commands) != 2 {
		t.Errorf("all commands should be retained, got %d", len(cq.Commands))
	}
}

func TestDeadLetter_TaskDuplicateSyntheticResult(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Retry: model.RetryConfig{TaskDispatch: 2},
	}
	dlp := newTestDeadLetterProcessor(maestroDir, cfg)

	// Create results dir with existing synthetic result
	resultsDir := filepath.Join(maestroDir, "results")
	if err := os.MkdirAll(resultsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing failed result for task_001
	rf := model.TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []model.TaskResult{
			{
				ID:        "res_existing",
				TaskID:    "task_001",
				CommandID: "cmd_001",
				Status:    model.StatusFailed,
				Summary:   "already dead-lettered",
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	if err := yamlutil.AtomicWrite(filepath.Join(resultsDir, "worker1.yaml"), rf); err != nil {
		t.Fatal(err)
	}

	// Create state dir
	if err := os.MkdirAll(filepath.Join(maestroDir, "state", "commands"), 0755); err != nil {
		t.Fatal(err)
	}

	tq := &taskQueueEntry{
		Path: filepath.Join(maestroDir, "queue", "worker1.yaml"),
		Queue: model.TaskQueue{
			Tasks: []model.Task{
				{
					ID:        "task_001",
					CommandID: "cmd_001",
					Status:    model.StatusPending,
					Attempts:  2,
					CreatedAt: time.Now().UTC().Format(time.RFC3339),
					UpdatedAt: time.Now().UTC().Format(time.RFC3339),
				},
			},
		},
	}

	dirty := false
	dlp.ProcessTaskDeadLetters(tq, &dirty)

	// Verify no duplicate result was added (idempotency check)
	data, err := os.ReadFile(filepath.Join(resultsDir, "worker1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var updatedRF model.TaskResultFile
	if err := yamlv3.Unmarshal(data, &updatedRF); err != nil {
		t.Fatal(err)
	}
	if len(updatedRF.Results) != 1 {
		t.Errorf("expected 1 result (no duplicate), got %d", len(updatedRF.Results))
	}
}

func TestDeadLetter_PeriodicScanIntegration(t *testing.T) {
	maestroDir := setupTestMaestroDir(t)
	cfg := model.Config{
		Agents:  model.AgentsConfig{Workers: model.WorkerConfig{Count: 2}},
		Watcher: model.WatcherConfig{DispatchLeaseSec: 300},
		Queue:   model.QueueConfig{PriorityAgingSec: 60},
		Retry:   model.RetryConfig{CommandDispatch: 2},
	}
	qh := NewQueueHandler(maestroDir, cfg, lock.NewMutexMap(), log.New(&bytes.Buffer{}, "", 0), LogLevelDebug)
	qh.SetExecutorFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return &mockExecutor{}, nil
	})

	// Create state dir
	if err := os.MkdirAll(filepath.Join(maestroDir, "state", "commands"), 0755); err != nil {
		t.Fatal(err)
	}

	// Write command with attempts >= max
	cq := model.CommandQueue{
		SchemaVersion: 1,
		FileType:      "command_queue",
		Commands: []model.Command{
			{
				ID:        "cmd_dead",
				Status:    model.StatusPending,
				Attempts:  2,
				Priority:  1,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
			{
				ID:        "cmd_alive",
				Status:    model.StatusPending,
				Attempts:  0,
				Priority:  1,
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	plannerPath := filepath.Join(maestroDir, "queue", "planner.yaml")
	if err := yamlutil.AtomicWrite(plannerPath, cq); err != nil {
		t.Fatal(err)
	}

	qh.PeriodicScan()

	// Read back the queue
	data, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatal(err)
	}
	var result model.CommandQueue
	if err := yamlv3.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}

	// cmd_dead should be removed, cmd_alive should be dispatched (in_progress)
	if len(result.Commands) != 1 {
		t.Fatalf("expected 1 command remaining, got %d", len(result.Commands))
	}
	if result.Commands[0].ID != "cmd_alive" {
		t.Errorf("remaining command should be cmd_alive, got %s", result.Commands[0].ID)
	}
	if result.Commands[0].Status != model.StatusInProgress {
		t.Errorf("cmd_alive should be dispatched (in_progress), got %s", result.Commands[0].Status)
	}

	// Verify archive was created
	archiveDir := filepath.Join(maestroDir, "dead_letters")
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		t.Fatalf("read archive dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 archive file, got %d", len(entries))
	}
}
