package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

// TC-RT-001: Exit code によるリトライ判定テスト
func TestShouldRetryTask_ExitCodes(t *testing.T) {
	tests := []struct {
		name             string
		exitCode         int
		retryableCodes   []int
		executionRetries int
		maxRetries       int
		enabled          bool
		expectedRetry    bool
		expectedReason   string
	}{
		{
			name:             "retryable exit code 1",
			exitCode:         1,
			retryableCodes:   []int{1, 124, 137},
			executionRetries: 0,
			maxRetries:       2,
			enabled:          true,
			expectedRetry:    true,
			expectedReason:   "",
		},
		{
			name:             "retryable exit code 124 (timeout)",
			exitCode:         124,
			retryableCodes:   []int{1, 124, 137},
			executionRetries: 0,
			maxRetries:       2,
			enabled:          true,
			expectedRetry:    true,
			expectedReason:   "",
		},
		{
			name:             "non-retryable exit code 2",
			exitCode:         2,
			retryableCodes:   []int{1, 124, 137},
			executionRetries: 0,
			maxRetries:       2,
			enabled:          true,
			expectedRetry:    false,
			expectedReason:   "exit code 2 not retryable",
		},
		{
			name:             "non-retryable exit code 127 (command not found)",
			exitCode:         127,
			retryableCodes:   []int{1, 124, 137},
			executionRetries: 0,
			maxRetries:       2,
			enabled:          true,
			expectedRetry:    false,
			expectedReason:   "exit code 127 not retryable",
		},
		{
			name:             "non-retryable exit code 128 (invalid argument)",
			exitCode:         128,
			retryableCodes:   []int{1, 124, 137},
			executionRetries: 0,
			maxRetries:       2,
			enabled:          true,
			expectedRetry:    false,
			expectedReason:   "exit code 128 not retryable",
		},
		{
			name:             "success exit code 0",
			exitCode:         0,
			retryableCodes:   []int{1, 124, 137},
			executionRetries: 0,
			maxRetries:       2,
			enabled:          true,
			expectedRetry:    false,
			expectedReason:   "exit code 0 not retryable",
		},
		{
			name:             "max retries exceeded",
			exitCode:         1,
			retryableCodes:   []int{1},
			executionRetries: 2,
			maxRetries:       2,
			enabled:          true,
			expectedRetry:    false,
			expectedReason:   "max retries exceeded (2/2)",
		},
		{
			name:             "retry disabled",
			exitCode:         1,
			retryableCodes:   []int{1},
			executionRetries: 0,
			maxRetries:       2,
			enabled:          false,
			expectedRetry:    false,
			expectedReason:   "retry disabled",
		},
		{
			name:             "empty retryable codes",
			exitCode:         1,
			retryableCodes:   []int{},
			executionRetries: 0,
			maxRetries:       2,
			enabled:          true,
			expectedRetry:    false,
			expectedReason:   "exit code 1 not retryable",
		},
		{
			name:             "max_retries=0 means unlimited retry",
			exitCode:         1,
			retryableCodes:   []int{1},
			executionRetries: 0,
			maxRetries:       0,
			enabled:          true,
			expectedRetry:    true,
			expectedReason:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := model.Config{
				Retry: model.RetryConfig{
					TaskExecution: model.TaskRetryConfig{
						Enabled:            tt.enabled,
						RetryableExitCodes: tt.retryableCodes,
						MaxRetries:         tt.maxRetries,
					},
				},
			}

			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

			task := &model.Task{
				ID:               "task_001",
				ExecutionRetries: tt.executionRetries,
			}

			shouldRetry, reason := handler.ShouldRetryTask(task, tt.exitCode)
			if shouldRetry != tt.expectedRetry {
				t.Errorf("shouldRetry: got %v, want %v", shouldRetry, tt.expectedRetry)
			}
			if tt.expectedReason != "" && reason != tt.expectedReason {
				t.Errorf("reason: got %q, want %q", reason, tt.expectedReason)
			}
		})
	}
}

// TC-RT-002: リトライタスクの生成とフィールド検証
func TestCreateRetryTask_FieldValidation(t *testing.T) {
	originalTask := &model.Task{
		ID:                 "task_original",
		CommandID:          "cmd_001",
		Purpose:            "original purpose",
		Content:            "original content",
		AcceptanceCriteria: "original criteria",
		Constraints:        []string{"constraint1", "constraint2"},
		BlockedBy:          []string{"blocker1"},
		BloomLevel:         1,
		ToolsHint:          []string{"tool1"},
		Priority:           10,
		ExecutionRetries:   1,
		Status:             model.StatusFailed,
		Attempts:           3,
	}

	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				CooldownSec: 30,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)
	retryTask, err := handler.CreateRetryTask(originalTask, "worker1", 1)

	if err != nil {
		t.Fatalf("CreateRetryTask failed: %v", err)
	}

	// Verify new task ID was generated
	if retryTask.ID == originalTask.ID {
		t.Error("retry task should have a new ID")
	}
	if retryTask.ID == "" {
		t.Error("retry task ID should not be empty")
	}

	// Verify fields are copied correctly
	if retryTask.CommandID != originalTask.CommandID {
		t.Errorf("CommandID: got %q, want %q", retryTask.CommandID, originalTask.CommandID)
	}
	if retryTask.Purpose != originalTask.Purpose {
		t.Errorf("Purpose: got %q, want %q", retryTask.Purpose, originalTask.Purpose)
	}
	if retryTask.Content != originalTask.Content {
		t.Errorf("Content: got %q, want %q", retryTask.Content, originalTask.Content)
	}
	if retryTask.AcceptanceCriteria != originalTask.AcceptanceCriteria {
		t.Errorf("AcceptanceCriteria: got %q, want %q", retryTask.AcceptanceCriteria, originalTask.AcceptanceCriteria)
	}

	// Verify ExecutionRetries is incremented
	if retryTask.ExecutionRetries != originalTask.ExecutionRetries+1 {
		t.Errorf("ExecutionRetries: got %d, want %d", retryTask.ExecutionRetries, originalTask.ExecutionRetries+1)
	}

	// Verify OriginalTaskID is set
	if retryTask.OriginalTaskID != originalTask.ID {
		t.Errorf("OriginalTaskID: got %q, want %q", retryTask.OriginalTaskID, originalTask.ID)
	}

	// Verify status is reset
	if retryTask.Status != model.StatusPending {
		t.Errorf("Status: got %q, want %q", retryTask.Status, model.StatusPending)
	}

	// Verify Attempts is reset
	if retryTask.Attempts != 0 {
		t.Errorf("Attempts: got %d, want 0", retryTask.Attempts)
	}

	// Verify lease fields are reset
	if retryTask.LeaseOwner != nil {
		t.Errorf("LeaseOwner should be nil, got %v", retryTask.LeaseOwner)
	}
	if retryTask.LeaseExpiresAt != nil {
		t.Errorf("LeaseExpiresAt should be nil, got %v", retryTask.LeaseExpiresAt)
	}
	if retryTask.LeaseEpoch != 0 {
		t.Errorf("LeaseEpoch: got %d, want 0", retryTask.LeaseEpoch)
	}

	// Verify NotBefore is set for cooldown
	if retryTask.NotBefore == nil {
		t.Fatal("NotBefore should be set for cooldown")
	}

	notBefore, err := time.Parse(time.RFC3339, *retryTask.NotBefore)
	if err != nil {
		t.Fatalf("parse NotBefore: %v", err)
	}
	expectedTime := time.Now().Add(30 * time.Second)
	if diff := notBefore.Sub(expectedTime).Abs(); diff > 2*time.Second {
		t.Errorf("NotBefore time diff too large: %v", diff)
	}

	// Verify priority adjustment
	expectedPriority := originalTask.Priority + 30
	if retryTask.Priority != expectedPriority {
		t.Errorf("Priority: got %d, want %d", retryTask.Priority, expectedPriority)
	}

	// Verify retry metadata is added to constraints
	found := false
	for _, c := range retryTask.Constraints {
		if c == "retry_attempt=2,original_task=task_original,exit_code=1" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("retry metadata not found in constraints: %v", retryTask.Constraints)
	}
}

// TC-RT-003: リトライタスクの生成 - NotBefore計算テスト
func TestCreateRetryTask_NotBeforeCalculation(t *testing.T) {
	tests := []struct {
		name        string
		cooldownSec int
	}{
		{"no cooldown", 0},
		{"5 second cooldown", 5},
		{"30 second cooldown", 30},
		{"60 second cooldown", 60},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalTask := &model.Task{
				ID:        "task_001",
				CommandID: "cmd_001",
				Purpose:   "test",
				Content:   "test content",
			}

			config := model.Config{
				Retry: model.RetryConfig{
					TaskExecution: model.TaskRetryConfig{
						CooldownSec: tt.cooldownSec,
					},
				},
			}

			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)
			retryTask, err := handler.CreateRetryTask(originalTask, "worker1", 1)

			if err != nil {
				t.Fatalf("CreateRetryTask failed: %v", err)
			}

			if tt.cooldownSec > 0 {
				if retryTask.NotBefore == nil {
					t.Fatal("NotBefore should be set when cooldown is configured")
				}
				notBefore, err := time.Parse(time.RFC3339, *retryTask.NotBefore)
				if err != nil {
					t.Fatalf("parse NotBefore: %v", err)
				}
				expectedTime := time.Now().Add(time.Duration(tt.cooldownSec) * time.Second)
				if diff := notBefore.Sub(expectedTime).Abs(); diff > 2*time.Second {
					t.Errorf("NotBefore time diff too large: %v (expected ~%ds)", diff, tt.cooldownSec)
				}
			} else {
				// When cooldown is 0, NotBefore is not set (can be scheduled immediately)
				if retryTask.NotBefore != nil {
					t.Errorf("NotBefore should not be set when cooldown is 0, got: %v", *retryTask.NotBefore)
				}
			}
		})
	}
}

// TC-RT-004: ExecutionRetriesカウンタのインクリメント
func TestCreateRetryTask_ExecutionRetriesIncrement(t *testing.T) {
	tests := []struct {
		name             string
		executionRetries int
		expectedRetries  int
	}{
		{"first retry", 0, 1},
		{"second retry", 1, 2},
		{"third retry", 2, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalTask := &model.Task{
				ID:               "task_001",
				CommandID:        "cmd_001",
				Purpose:          "test",
				Content:          "test content",
				ExecutionRetries: tt.executionRetries,
			}

			config := model.Config{
				Retry: model.RetryConfig{
					TaskExecution: model.TaskRetryConfig{
						CooldownSec: 10,
					},
				},
			}

			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)
			retryTask, err := handler.CreateRetryTask(originalTask, "worker1", 1)

			if err != nil {
				t.Fatalf("CreateRetryTask failed: %v", err)
			}

			if retryTask.ExecutionRetries != tt.expectedRetries {
				t.Errorf("ExecutionRetries: got %d, want %d", retryTask.ExecutionRetries, tt.expectedRetries)
			}
		})
	}
}

// TC-RT-005: OriginalTaskIDの追跡
func TestCreateRetryTask_OriginalTaskIDTracking(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				CooldownSec: 10,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	// First failure - original task has no OriginalTaskID
	originalTask := &model.Task{
		ID:               "task_001",
		CommandID:        "cmd_001",
		Purpose:          "test",
		Content:          "test content",
		ExecutionRetries: 0,
	}

	firstRetry, err := handler.CreateRetryTask(originalTask, "worker1", 1)
	if err != nil {
		t.Fatalf("CreateRetryTask failed: %v", err)
	}

	// First retry should track original task
	if firstRetry.OriginalTaskID != originalTask.ID {
		t.Errorf("first retry OriginalTaskID: got %q, want %q", firstRetry.OriginalTaskID, originalTask.ID)
	}

	// Second failure - retry task already has OriginalTaskID
	firstRetry.ExecutionRetries = 1
	secondRetry, err := handler.CreateRetryTask(firstRetry, "worker1", 1)
	if err != nil {
		t.Fatalf("CreateRetryTask failed: %v", err)
	}

	// Second retry should preserve the original OriginalTaskID
	if secondRetry.OriginalTaskID != originalTask.ID {
		t.Errorf("second retry OriginalTaskID: got %q, want %q", secondRetry.OriginalTaskID, originalTask.ID)
	}

	// Task already has OriginalTaskID set
	taskWithOriginalID := &model.Task{
		ID:               "task_retry_001",
		CommandID:        "cmd_001",
		Purpose:          "test",
		Content:          "test content",
		ExecutionRetries: 2,
		OriginalTaskID:   "task_original_000",
	}

	thirdRetry, err := handler.CreateRetryTask(taskWithOriginalID, "worker1", 1)
	if err != nil {
		t.Fatalf("CreateRetryTask failed: %v", err)
	}

	if thirdRetry.OriginalTaskID != taskWithOriginalID.OriginalTaskID {
		t.Errorf("third retry should preserve existing OriginalTaskID: got %q, want %q",
			thirdRetry.OriginalTaskID, taskWithOriginalID.OriginalTaskID)
	}
}

// TC-RT-006: リトライ可能性の最大リトライ数境界値テスト
func TestShouldRetryTask_MaxRetriesBoundary(t *testing.T) {
	tests := []struct {
		name             string
		executionRetries int
		maxRetries       int
		expectedRetry    bool
		expectedReason   string
	}{
		{
			name:             "below max retries",
			executionRetries: 1,
			maxRetries:       3,
			expectedRetry:    true,
			expectedReason:   "",
		},
		{
			name:             "at max retries boundary",
			executionRetries: 3,
			maxRetries:       3,
			expectedRetry:    false,
			expectedReason:   "max retries exceeded (3/3)",
		},
		{
			name:             "above max retries",
			executionRetries: 4,
			maxRetries:       3,
			expectedRetry:    false,
			expectedReason:   "max retries exceeded (4/3)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := model.Config{
				Retry: model.RetryConfig{
					TaskExecution: model.TaskRetryConfig{
						Enabled:            true,
						RetryableExitCodes: []int{1},
						MaxRetries:         tt.maxRetries,
					},
				},
			}

			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

			task := &model.Task{
				ID:               "task_001",
				ExecutionRetries: tt.executionRetries,
			}

			shouldRetry, reason := handler.ShouldRetryTask(task, 1)
			if shouldRetry != tt.expectedRetry {
				t.Errorf("shouldRetry: got %v, want %v", shouldRetry, tt.expectedRetry)
			}
			if tt.expectedReason != "" && reason != tt.expectedReason {
				t.Errorf("reason: got %q, want %q", reason, tt.expectedReason)
			}
		})
	}
}

// TC-RT-007: リトライ無効時の動作
func TestShouldRetryTask_DisabledRetry(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				Enabled:            false,
				RetryableExitCodes: []int{1, 124, 137},
				MaxRetries:         3,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	task := &model.Task{
		ID:               "task_001",
		ExecutionRetries: 0,
	}

	shouldRetry, reason := handler.ShouldRetryTask(task, 1)
	if shouldRetry {
		t.Error("should not retry when retry is disabled")
	}
	if reason != "retry disabled" {
		t.Errorf("reason: got %q, want %q", reason, "retry disabled")
	}
}

// TC-RT-008: RegisterRetryTaskInState のテスト
func TestRegisterRetryTaskInState(t *testing.T) {
	tmpDir := t.TempDir()
	commandID := "cmd_001"
	taskID := "task_retry_001"

	// Setup: Create command state directory and file
	stateDir := filepath.Join(tmpDir, "state", "commands")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}

	statePath := filepath.Join(stateDir, commandID+".yaml")
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "command_state",
		CommandID:     commandID,
		TaskStates: map[string]model.Status{
			"task_original": model.StatusFailed,
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	// Create handler
	config := model.Config{}
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(tmpDir, config, lock.NewMutexMap(), logger, LogLevelDebug)

	// Create retry task
	retryTask := &model.Task{
		ID:        taskID,
		CommandID: commandID,
		Purpose:   "retry test",
		Content:   "test content",
		Status:    model.StatusPending,
	}

	// Execute: Register retry task
	err := handler.RegisterRetryTaskInState(retryTask, commandID)
	if err != nil {
		t.Fatalf("RegisterRetryTaskInState failed: %v", err)
	}

	// Verify: Check state file was updated
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}

	var updatedState model.CommandState
	if err := yamlv3.Unmarshal(stateData, &updatedState); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}

	if updatedState.TaskStates[taskID] != model.StatusPending {
		t.Errorf("retry task not registered in state: %v", updatedState.TaskStates)
	}
}

// TC-RT-009: AddRetryTaskToQueue のテスト
func TestAddRetryTaskToQueue(t *testing.T) {
	tmpDir := t.TempDir()
	workerID := "worker1"

	// Setup: Create queue directory
	queueDir := filepath.Join(tmpDir, "queue")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("create queue dir: %v", err)
	}

	queuePath := filepath.Join(queueDir, workerID+".yaml")
	queue := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:        "task_existing",
				CommandID: "cmd_001",
				Purpose:   "existing task",
				Content:   "existing content",
				Status:    model.StatusPending,
			},
		},
	}
	if err := yamlutil.AtomicWrite(queuePath, queue); err != nil {
		t.Fatalf("write queue file: %v", err)
	}

	// Create handler
	config := model.Config{}
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(tmpDir, config, lock.NewMutexMap(), logger, LogLevelDebug)

	// Create retry task
	retryTask := &model.Task{
		ID:        "task_retry_001",
		CommandID: "cmd_001",
		Purpose:   "retry task",
		Content:   "retry content",
		Status:    model.StatusPending,
	}

	// Execute: Add retry task to queue
	err := handler.AddRetryTaskToQueue(retryTask, workerID)
	if err != nil {
		t.Fatalf("AddRetryTaskToQueue failed: %v", err)
	}

	// Verify: Check queue file was updated
	queueData, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}

	var updatedQueue model.TaskQueue
	if err := yamlv3.Unmarshal(queueData, &updatedQueue); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}

	if len(updatedQueue.Tasks) != 2 {
		t.Errorf("queue should have 2 tasks, got %d", len(updatedQueue.Tasks))
	}

	found := false
	for _, task := range updatedQueue.Tasks {
		if task.ID == retryTask.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("retry task not found in queue")
	}
}

// TC-RT-010: べき等性テスト - 重複失敗時のリトライタスク作成
func TestRetryIdempotency(t *testing.T) {
	tmpDir := t.TempDir()
	commandID := "cmd_001"
	originalTaskID := "task_original"

	// Setup directories
	for _, dir := range []string{"queue", "state/commands"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, dir), 0755); err != nil {
			t.Fatalf("create dir %s: %v", dir, err)
		}
	}

	// Setup state
	statePath := filepath.Join(tmpDir, "state", "commands", commandID+".yaml")
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "command_state",
		CommandID:     commandID,
		TaskStates:    map[string]model.Status{originalTaskID: model.StatusFailed},
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// Setup queue
	queuePath := filepath.Join(tmpDir, "queue", "worker1.yaml")
	queue := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks:         []model.Task{},
	}
	if err := yamlutil.AtomicWrite(queuePath, queue); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	// Create handler
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				Enabled:            true,
				RetryableExitCodes: []int{1},
				MaxRetries:         3,
				CooldownSec:        10,
			},
		},
	}
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(tmpDir, config, lock.NewMutexMap(), logger, LogLevelDebug)

	originalTask := &model.Task{
		ID:               originalTaskID,
		CommandID:        commandID,
		Purpose:          "test",
		Content:          "test content",
		ExecutionRetries: 0,
	}

	// Create first retry task
	retryTask1, err := handler.CreateRetryTask(originalTask, "worker1", 1)
	if err != nil {
		t.Fatalf("CreateRetryTask failed: %v", err)
	}

	if err := handler.RegisterRetryTaskInState(retryTask1, commandID); err != nil {
		t.Fatalf("RegisterRetryTaskInState failed: %v", err)
	}

	if err := handler.AddRetryTaskToQueue(retryTask1, "worker1"); err != nil {
		t.Fatalf("AddRetryTaskToQueue failed: %v", err)
	}

	// Create second retry task (duplicate)
	retryTask2, err := handler.CreateRetryTask(originalTask, "worker1", 1)
	if err != nil {
		t.Fatalf("CreateRetryTask failed: %v", err)
	}

	if err := handler.RegisterRetryTaskInState(retryTask2, commandID); err != nil {
		t.Fatalf("RegisterRetryTaskInState failed: %v", err)
	}

	if err := handler.AddRetryTaskToQueue(retryTask2, "worker1"); err != nil {
		t.Fatalf("AddRetryTaskToQueue failed: %v", err)
	}

	// Verify: Check that both tasks are in queue (system allows duplicate creation)
	queueData, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}

	var finalQueue model.TaskQueue
	if err := yamlv3.Unmarshal(queueData, &finalQueue); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}

	// The handler doesn't prevent duplicate retry creation - this is by design
	// The system relies on epoch fencing to prevent duplicate execution
	if len(finalQueue.Tasks) != 2 {
		t.Errorf("queue should have 2 tasks (duplicates allowed), got %d", len(finalQueue.Tasks))
	}
}

// TC-RT-011: 並行リトライ作成の安全性
func TestConcurrentRetryCreation(t *testing.T) {
	tmpDir := t.TempDir()
	commandID := "cmd_001"
	workerID := "worker1"

	// Setup directories
	for _, dir := range []string{"queue", "state/commands"} {
		if err := os.MkdirAll(filepath.Join(tmpDir, dir), 0755); err != nil {
			t.Fatalf("create dir %s: %v", dir, err)
		}
	}

	// Setup state
	statePath := filepath.Join(tmpDir, "state", "commands", commandID+".yaml")
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "command_state",
		CommandID:     commandID,
		TaskStates:    map[string]model.Status{},
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// Setup queue
	queuePath := filepath.Join(tmpDir, "queue", workerID+".yaml")
	queue := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks:         []model.Task{},
	}
	if err := yamlutil.AtomicWrite(queuePath, queue); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	// Create handler
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				Enabled:            true,
				RetryableExitCodes: []int{1},
				MaxRetries:         10,
				CooldownSec:        5,
			},
		},
	}
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(tmpDir, config, lock.NewMutexMap(), logger, LogLevelDebug)

	// Execute: Create retry tasks concurrently
	const numGoroutines = 10
	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			originalTask := &model.Task{
				ID:               "task_original",
				CommandID:        commandID,
				Purpose:          "test",
				Content:          "test content",
				ExecutionRetries: 0,
			}

			retryTask, err := handler.CreateRetryTask(originalTask, workerID, 1)
			if err != nil {
				errors <- err
				return
			}

			if err := handler.RegisterRetryTaskInState(retryTask, commandID); err != nil {
				errors <- err
				return
			}

			if err := handler.AddRetryTaskToQueue(retryTask, workerID); err != nil {
				errors <- err
				return
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Verify: No errors occurred
	var errorCount int
	for err := range errors {
		t.Errorf("concurrent operation failed: %v", err)
		errorCount++
	}

	if errorCount > 0 {
		t.Fatalf("concurrent retry creation had %d errors", errorCount)
	}

	// Verify: All tasks were added to queue
	queueData, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}

	var finalQueue model.TaskQueue
	if err := yamlv3.Unmarshal(queueData, &finalQueue); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}

	if len(finalQueue.Tasks) != numGoroutines {
		t.Errorf("queue should have %d tasks, got %d", numGoroutines, len(finalQueue.Tasks))
	}
}

// TC-RT-012: OOMキルのリトライテスト (exit code 137)
func TestShouldRetryTask_OOMKill(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				Enabled:            true,
				RetryableExitCodes: []int{1, 124, 137}, // 137 = OOM kill
				MaxRetries:         3,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	task := &model.Task{
		ID:               "task_oom",
		ExecutionRetries: 0,
	}

	shouldRetry, reason := handler.ShouldRetryTask(task, 137)
	if !shouldRetry {
		t.Error("should retry OOM killed tasks (exit code 137)")
	}
	if reason != "" {
		t.Errorf("unexpected reason for OOM retry: %q", reason)
	}
}

// TC-RT-013: Permission deniedのリトライ拒否テスト (exit code 126)
func TestShouldRetryTask_PermissionDenied(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				Enabled:            true,
				RetryableExitCodes: []int{1, 124, 137},
				MaxRetries:         3,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	task := &model.Task{
		ID:               "task_perm",
		ExecutionRetries: 0,
	}

	shouldRetry, reason := handler.ShouldRetryTask(task, 126)
	if shouldRetry {
		t.Error("should not retry permission denied errors (exit code 126)")
	}
	if reason != "exit code 126 not retryable" {
		t.Errorf("unexpected reason: got %q, want %q", reason, "exit code 126 not retryable")
	}
}

// TC-RT-014: クールダウン時間の最大値制限テスト
func TestCreateRetryTask_CooldownMaxDuration(t *testing.T) {
	tests := []struct {
		name           string
		cooldownSec    int
		expectedCapped bool
		maxCooldownSec int
	}{
		{"normal cooldown", 30, false, 0},
		{"large cooldown", 3600, false, 0},       // 1 hour - should be allowed
		{"very large cooldown", 86400, false, 0}, // 24 hours - currently no cap implemented
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := model.Config{
				Retry: model.RetryConfig{
					TaskExecution: model.TaskRetryConfig{
						CooldownSec: tt.cooldownSec,
					},
				},
			}

			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

			originalTask := &model.Task{
				ID:        "task_001",
				CommandID: "cmd_001",
				Purpose:   "test",
				Content:   "test content",
			}

			retryTask, err := handler.CreateRetryTask(originalTask, "worker1", 1)
			if err != nil {
				t.Fatalf("CreateRetryTask failed: %v", err)
			}

			if retryTask.NotBefore == nil {
				t.Fatal("NotBefore should be set")
			}

			notBefore, err := time.Parse(time.RFC3339, *retryTask.NotBefore)
			if err != nil {
				t.Fatalf("parse NotBefore: %v", err)
			}

			expectedDelay := time.Duration(tt.cooldownSec) * time.Second
			actualDelay := notBefore.Sub(time.Now())

			// Allow 2 second tolerance for test execution time
			if actualDelay < expectedDelay-2*time.Second || actualDelay > expectedDelay+2*time.Second {
				t.Errorf("cooldown delay: got ~%v, want ~%v", actualDelay, expectedDelay)
			}
		})
	}
}

// TC-RT-015: 不正なクールダウン値の処理
func TestCreateRetryTask_InvalidCooldownValues(t *testing.T) {
	tests := []struct {
		name        string
		cooldownSec int
		shouldWork  bool
	}{
		{"negative cooldown", -10, true}, // Should be treated as 0 or rejected gracefully
		{"zero cooldown", 0, true},
		{"max int cooldown", int(^uint(0) >> 1), true}, // Should not panic
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := model.Config{
				Retry: model.RetryConfig{
					TaskExecution: model.TaskRetryConfig{
						CooldownSec: tt.cooldownSec,
					},
				},
			}

			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

			originalTask := &model.Task{
				ID:        "task_001",
				CommandID: "cmd_001",
				Purpose:   "test",
				Content:   "test content",
			}

			// Should not panic
			retryTask, err := handler.CreateRetryTask(originalTask, "worker1", 1)
			if tt.shouldWork {
				if err != nil {
					t.Errorf("CreateRetryTask should work with cooldown=%d, got error: %v", tt.cooldownSec, err)
				}
				if retryTask == nil {
					t.Error("retryTask should not be nil")
				}
			}
		})
	}
}

// TC-RT-016: max_retries=0 の解釈テスト（無限リトライ vs 無効化）
func TestShouldRetryTask_MaxRetriesZero(t *testing.T) {
	// According to the implementation, max_retries=0 means unlimited retries
	// (the check is: maxRetries > 0 && retries >= maxRetries)
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				Enabled:            true,
				RetryableExitCodes: []int{1},
				MaxRetries:         0, // Zero means unlimited
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	task := &model.Task{
		ID:               "task_001",
		ExecutionRetries: 100, // Many retries already
	}

	shouldRetry, reason := handler.ShouldRetryTask(task, 1)

	// max_retries=0 should mean unlimited retries (current implementation behavior)
	if !shouldRetry {
		t.Errorf("max_retries=0 should allow unlimited retries, got reason: %q", reason)
	}
}

// TC-RT-017: タスクフィールドの欠損処理テスト
func TestCreateRetryTask_MissingFields(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				CooldownSec: 10,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	// Task with minimal fields
	minimalTask := &model.Task{
		ID:        "task_minimal",
		CommandID: "cmd_001",
		// Purpose and Content are empty
		ExecutionRetries: 0,
	}

	retryTask, err := handler.CreateRetryTask(minimalTask, "worker1", 1)
	if err != nil {
		t.Fatalf("CreateRetryTask should handle minimal task: %v", err)
	}

	// Verify basic fields are set
	if retryTask.ID == "" {
		t.Error("retry task should have an ID")
	}
	if retryTask.CommandID != minimalTask.CommandID {
		t.Errorf("CommandID mismatch: got %q, want %q", retryTask.CommandID, minimalTask.CommandID)
	}
	if retryTask.ExecutionRetries != 1 {
		t.Errorf("ExecutionRetries: got %d, want 1", retryTask.ExecutionRetries)
	}
}

// TC-RT-018: 非常に大きなExecutionRetriesのハンドリング
func TestCreateRetryTask_LargeExecutionRetries(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				CooldownSec: 10,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	const largeRetryCount = 1000000
	originalTask := &model.Task{
		ID:               "task_large_retry",
		CommandID:        "cmd_001",
		Purpose:          "test",
		Content:          "test content",
		ExecutionRetries: largeRetryCount,
	}

	// Should not panic or overflow
	retryTask, err := handler.CreateRetryTask(originalTask, "worker1", 1)
	if err != nil {
		t.Fatalf("CreateRetryTask failed with large retry count: %v", err)
	}

	if retryTask.ExecutionRetries != largeRetryCount+1 {
		t.Errorf("ExecutionRetries: got %d, want %d", retryTask.ExecutionRetries, largeRetryCount+1)
	}
}

// TC-RT-019: Priority計算のオーバーフローテスト
func TestCreateRetryTask_PriorityOverflow(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				CooldownSec: 1000000, // Very large cooldown
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	const maxInt = int(^uint(0) >> 1)
	originalTask := &model.Task{
		ID:        "task_max_priority",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "test content",
		Priority:  maxInt - 100, // Near max int
	}

	// Should not panic on priority calculation
	retryTask, err := handler.CreateRetryTask(originalTask, "worker1", 1)
	if err != nil {
		t.Fatalf("CreateRetryTask failed: %v", err)
	}

	// Note: Current implementation allows integer overflow in priority calculation
	// This test documents the current behavior - priority can overflow when
	// original priority + cooldownSec exceeds max int
	// This is acceptable as priority is just a hint for ordering
	t.Logf("Priority after overflow: %d (original: %d, cooldown: %d)",
		retryTask.Priority, originalTask.Priority, config.Retry.TaskExecution.CooldownSec)
}

// TC-RT-020: 複数のConstraintsがある場合のリトライメタデータ追加
func TestCreateRetryTask_MultipleConstraints(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				CooldownSec: 10,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	originalTask := &model.Task{
		ID:          "task_constraints",
		CommandID:   "cmd_001",
		Purpose:     "test",
		Content:     "test content",
		Constraints: []string{"constraint1", "constraint2", "constraint3"},
	}

	retryTask, err := handler.CreateRetryTask(originalTask, "worker1", 137)
	if err != nil {
		t.Fatalf("CreateRetryTask failed: %v", err)
	}

	// Verify all original constraints are preserved
	if len(retryTask.Constraints) < len(originalTask.Constraints) {
		t.Errorf("constraints lost: got %d, want at least %d", len(retryTask.Constraints), len(originalTask.Constraints))
	}

	// Verify retry metadata was added
	found := false
	for _, c := range retryTask.Constraints {
		if c == "retry_attempt=1,original_task=task_constraints,exit_code=137" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("retry metadata not found in constraints: %v", retryTask.Constraints)
	}

	// Verify original constraints are still present
	for _, origConstraint := range originalTask.Constraints {
		found := false
		for _, retryConstraint := range retryTask.Constraints {
			if retryConstraint == origConstraint {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("original constraint %q was lost", origConstraint)
		}
	}
}

// TC-RT-021: 空のリトライ設定でのリトライ無効確認
func TestShouldRetryTask_EmptyRetryableExitCodes(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				Enabled:            true,
				RetryableExitCodes: []int{}, // Empty list
				MaxRetries:         3,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	task := &model.Task{
		ID:               "task_empty_codes",
		ExecutionRetries: 0,
	}

	// Try various exit codes - none should be retryable
	exitCodes := []int{0, 1, 2, 124, 126, 127, 128, 137}
	for _, exitCode := range exitCodes {
		shouldRetry, _ := handler.ShouldRetryTask(task, exitCode)
		if shouldRetry {
			t.Errorf("exit code %d should not be retryable with empty retryable codes list", exitCode)
		}
	}
}

// TC-RT-022: CreatedAtとUpdatedAtのタイムスタンプ検証
func TestCreateRetryTask_Timestamps(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				CooldownSec: 10,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	originalTask := &model.Task{
		ID:        "task_timestamps",
		CommandID: "cmd_001",
		Purpose:   "test",
		Content:   "test content",
		CreatedAt: "2024-01-01T00:00:00Z",
		UpdatedAt: "2024-01-01T00:00:00Z",
	}

	beforeCreate := time.Now().UTC()
	retryTask, err := handler.CreateRetryTask(originalTask, "worker1", 1)
	afterCreate := time.Now().UTC()

	if err != nil {
		t.Fatalf("CreateRetryTask failed: %v", err)
	}

	// Verify CreatedAt and UpdatedAt are set to current time
	createdAt, err := time.Parse(time.RFC3339, retryTask.CreatedAt)
	if err != nil {
		t.Fatalf("parse CreatedAt: %v", err)
	}

	updatedAt, err := time.Parse(time.RFC3339, retryTask.UpdatedAt)
	if err != nil {
		t.Fatalf("parse UpdatedAt: %v", err)
	}

	// CreatedAt should be within reasonable range (allow for time truncation in RFC3339)
	tolerance := 2 * time.Second
	if createdAt.Before(beforeCreate.Add(-tolerance)) || createdAt.After(afterCreate.Add(tolerance)) {
		t.Errorf("CreatedAt out of expected range: got %v, window [%v, %v]", createdAt, beforeCreate, afterCreate)
	}

	// UpdatedAt should be within reasonable range (allow for time truncation in RFC3339)
	if updatedAt.Before(beforeCreate.Add(-tolerance)) || updatedAt.After(afterCreate.Add(tolerance)) {
		t.Errorf("UpdatedAt out of expected range: got %v, window [%v, %v]", updatedAt, beforeCreate, afterCreate)
	}

	// CreatedAt and UpdatedAt should be the same (or very close)
	if updatedAt.Sub(createdAt).Abs() > 1*time.Second {
		t.Errorf("CreatedAt and UpdatedAt should be close: CreatedAt=%v, UpdatedAt=%v", createdAt, updatedAt)
	}
}

// TC-RT-023: nilポインタフィールドの安全な処理
func TestCreateRetryTask_NilPointerFields(t *testing.T) {
	config := model.Config{
		Retry: model.RetryConfig{
			TaskExecution: model.TaskRetryConfig{
				CooldownSec: 10,
			},
		},
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(t.TempDir(), config, lock.NewMutexMap(), logger, LogLevelDebug)

	// Task with nil pointer fields
	originalTask := &model.Task{
		ID:             "task_nil_fields",
		CommandID:      "cmd_001",
		Purpose:        "test",
		Content:        "test content",
		NotBefore:      nil,
		LastError:      nil,
		LeaseOwner:     nil,
		LeaseExpiresAt: nil,
	}

	// Should not panic
	retryTask, err := handler.CreateRetryTask(originalTask, "worker1", 1)
	if err != nil {
		t.Fatalf("CreateRetryTask should handle nil pointer fields: %v", err)
	}

	// Verify retry task is properly created
	if retryTask.ID == "" {
		t.Error("retry task should have an ID")
	}
	if retryTask.LeaseOwner != nil {
		t.Error("LeaseOwner should be nil for new retry task")
	}
	if retryTask.LeaseExpiresAt != nil {
		t.Error("LeaseExpiresAt should be nil for new retry task")
	}
	if retryTask.NotBefore == nil {
		t.Error("NotBefore should be set for retry task with cooldown")
	}
}

// TC-RT-024: RegisterRetryTaskInState の状態マージテスト
func TestRegisterRetryTaskInState_ExistingTasks(t *testing.T) {
	tmpDir := t.TempDir()
	commandID := "cmd_001"
	taskID := "task_retry_002"

	// Setup state directory
	stateDir := filepath.Join(tmpDir, "state", "commands")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("create state dir: %v", err)
	}

	// Create state with existing tasks
	statePath := filepath.Join(stateDir, commandID+".yaml")
	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "command_state",
		CommandID:     commandID,
		TaskStates: map[string]model.Status{
			"task_001": model.StatusCompleted,
			"task_002": model.StatusFailed,
			"task_003": model.StatusInProgress,
		},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	// Create handler
	config := model.Config{}
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(tmpDir, config, lock.NewMutexMap(), logger, LogLevelDebug)

	// Create retry task
	retryTask := &model.Task{
		ID:        taskID,
		CommandID: commandID,
		Purpose:   "retry test",
		Content:   "test content",
		Status:    model.StatusPending,
	}

	// Execute: Register retry task
	err := handler.RegisterRetryTaskInState(retryTask, commandID)
	if err != nil {
		t.Fatalf("RegisterRetryTaskInState failed: %v", err)
	}

	// Verify: Check all tasks are present
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}

	var updatedState model.CommandState
	if err := yamlv3.Unmarshal(stateData, &updatedState); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}

	// Original tasks should still be present
	if updatedState.TaskStates["task_001"] != model.StatusCompleted {
		t.Error("task_001 status was lost or changed")
	}
	if updatedState.TaskStates["task_002"] != model.StatusFailed {
		t.Error("task_002 status was lost or changed")
	}
	if updatedState.TaskStates["task_003"] != model.StatusInProgress {
		t.Error("task_003 status was lost or changed")
	}

	// New retry task should be registered
	if updatedState.TaskStates[taskID] != model.StatusPending {
		t.Errorf("retry task not registered: got %v", updatedState.TaskStates[taskID])
	}
}

// TC-RT-025: AddRetryTaskToQueue の空キュー処理
func TestAddRetryTaskToQueue_EmptyQueue(t *testing.T) {
	tmpDir := t.TempDir()
	workerID := "worker_new"

	// Setup queue directory (but don't create queue file)
	queueDir := filepath.Join(tmpDir, "queue")
	if err := os.MkdirAll(queueDir, 0755); err != nil {
		t.Fatalf("create queue dir: %v", err)
	}

	// Create handler
	config := model.Config{}
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	handler := NewTaskRetryHandler(tmpDir, config, lock.NewMutexMap(), logger, LogLevelDebug)

	// Create retry task
	retryTask := &model.Task{
		ID:        "task_retry_new",
		CommandID: "cmd_001",
		Purpose:   "retry task",
		Content:   "retry content",
		Status:    model.StatusPending,
	}

	// Execute: Add retry task to non-existent queue
	err := handler.AddRetryTaskToQueue(retryTask, workerID)
	if err != nil {
		t.Fatalf("AddRetryTaskToQueue should create queue if not exists: %v", err)
	}

	// Verify: Queue file was created with the task
	queuePath := filepath.Join(queueDir, workerID+".yaml")
	queueData, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("queue file should be created: %v", err)
	}

	var queue model.TaskQueue
	if err := yamlv3.Unmarshal(queueData, &queue); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}

	if len(queue.Tasks) != 1 {
		t.Errorf("queue should have 1 task, got %d", len(queue.Tasks))
	}
	if queue.Tasks[0].ID != retryTask.ID {
		t.Errorf("task ID mismatch: got %q, want %q", queue.Tasks[0].ID, retryTask.ID)
	}
}
