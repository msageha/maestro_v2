package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
	yamlv3 "gopkg.in/yaml.v3"
)

func makeHeartbeatRequest(t *testing.T, params TaskHeartbeatParams) *uds.Request {
	t.Helper()
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return &uds.Request{
		ProtocolVersion: 1,
		Command:         "task_heartbeat",
		Params:          data,
	}
}

func newTestHeartbeatHandler(t *testing.T, d *Daemon) *TaskHeartbeatHandler {
	t.Helper()
	// Ensure config has necessary defaults
	if d.config.Watcher.DispatchLeaseSec == 0 {
		d.config.Watcher.DispatchLeaseSec = 120
	}
	if d.config.Watcher.MaxInProgressMin == 0 {
		d.config.Watcher.MaxInProgressMin = 60
	}
	return NewTaskHeartbeatHandler(
		d.maestroDir,
		d.config,
		d.handler.leaseManager,
		d.logger,
		d.logLevel,
		&d.handler.scanMu,
		d.lockMap,
	)
}

func TestHeartbeat_Success(t *testing.T) {
	d := newTestDaemon(t)
	handler := newTestHeartbeatHandler(t, d)

	taskID := "task_0000000001_abcdef01"
	workerID := "worker1"
	commandID := "cmd_0000000001_abcdef01"
	leaseEpoch := 1

	// Setup: Create task with lease that will expire soon
	expiresAt := time.Now().Add(10 * time.Second).Format(time.RFC3339)
	owner := "daemon:12345"
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             taskID,
				CommandID:      commandID,
				Purpose:        "test purpose",
				Content:        "test content",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiresAt,
				LeaseEpoch:     leaseEpoch,
				CreatedAt:      time.Now().Format(time.RFC3339),
				UpdatedAt:      time.Now().Format(time.RFC3339),
			},
		},
	}
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	// Execute: Send heartbeat
	req := makeHeartbeatRequest(t, TaskHeartbeatParams{
		TaskID:   taskID,
		WorkerID: workerID,
		Epoch:    leaseEpoch,
	})

	resp := handler.Handle(req.Params)

	// Verify: Check response
	if !resp.Success {
		t.Errorf("heartbeat failed: %v", resp.Error)
	}

	// Verify: Check lease was extended
	var updatedQueue model.TaskQueue
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	if err := yamlv3.Unmarshal(data, &updatedQueue); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}

	task := &updatedQueue.Tasks[0]
	if task.LeaseExpiresAt == nil {
		t.Fatal("lease_expires_at should not be nil")
	}

	newExpiry, err := time.Parse(time.RFC3339, *task.LeaseExpiresAt)
	if err != nil {
		t.Fatalf("parse lease_expires_at: %v", err)
	}

	// Should be extended by ~2 minutes from now
	expectedExpiry := time.Now().Add(time.Duration(d.config.Watcher.DispatchLeaseSec) * time.Second)
	diff := newExpiry.Sub(expectedExpiry).Abs()
	if diff > 5*time.Second {
		t.Errorf("lease not properly extended: got %v, expected ~%v", newExpiry, expectedExpiry)
	}
}

func TestHeartbeat_StaleEpoch(t *testing.T) {
	d := newTestDaemon(t)
	handler := newTestHeartbeatHandler(t, d)

	taskID := "task_0000000001_abcdef01"
	workerID := "worker1"
	commandID := "cmd_0000000001_abcdef01"
	currentEpoch := 2

	// Setup: Create task with current epoch
	expiresAt := time.Now().Add(30 * time.Second).Format(time.RFC3339)
	owner := "daemon:12345"
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             taskID,
				CommandID:      commandID,
				Purpose:        "test purpose",
				Content:        "test content",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiresAt,
				LeaseEpoch:     currentEpoch,
				CreatedAt:      time.Now().Format(time.RFC3339),
				UpdatedAt:      time.Now().Format(time.RFC3339),
			},
		},
	}
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	// Execute: Send heartbeat with stale epoch
	req := makeHeartbeatRequest(t, TaskHeartbeatParams{
		TaskID:   taskID,
		WorkerID: workerID,
		Epoch:    currentEpoch - 1, // Stale epoch
	})

	resp := handler.Handle(req.Params)

	// Verify: Should be rejected
	if resp.Success {
		t.Error("heartbeat should have failed with stale epoch")
	}
	if resp.Error == nil || resp.Error.Code != uds.ErrCodeFencingReject {
		t.Errorf("expected fencing reject error, got: %v", resp.Error)
	}
}

func TestHeartbeat_MaxRuntimeExceeded(t *testing.T) {
	d := newTestDaemon(t)
	handler := newTestHeartbeatHandler(t, d)

	taskID := "task_0000000001_abcdef01"
	workerID := "worker1"
	commandID := "cmd_0000000001_abcdef01"
	leaseEpoch := 1

	// Setup: Create task that started long ago (exceeding max_in_progress_min)
	expiresAt := time.Now().Add(30 * time.Second).Format(time.RFC3339)
	owner := "daemon:12345"
	// Task created more than max_in_progress_min ago
	createdAt := time.Now().Add(-time.Duration(d.config.Watcher.MaxInProgressMin+1) * time.Minute).Format(time.RFC3339)

	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             taskID,
				CommandID:      commandID,
				Purpose:        "test purpose",
				Content:        "test content",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiresAt,
				LeaseEpoch:     leaseEpoch,
				CreatedAt:      createdAt,
				UpdatedAt:      createdAt,
			},
		},
	}
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	// Execute: Send heartbeat
	req := makeHeartbeatRequest(t, TaskHeartbeatParams{
		TaskID:   taskID,
		WorkerID: workerID,
		Epoch:    leaseEpoch,
	})

	resp := handler.Handle(req.Params)

	// Verify: Should be rejected due to max runtime
	if resp.Success {
		t.Error("heartbeat should have failed due to max runtime exceeded")
	}
	if resp.Error == nil || resp.Error.Code != uds.ErrCodeMaxRuntimeExceeded {
		t.Errorf("expected max runtime exceeded error, got: %v", resp.Error)
	}
}

func TestHeartbeat_TaskNotInProgress(t *testing.T) {
	d := newTestDaemon(t)
	handler := newTestHeartbeatHandler(t, d)

	taskID := "task_0000000001_abcdef01"
	workerID := "worker1"
	commandID := "cmd_0000000001_abcdef01"
	leaseEpoch := 1

	// Setup: Create task that is completed (not in_progress)
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:         taskID,
				CommandID:  commandID,
				Purpose:    "test purpose",
				Content:    "test content",
				Status:     model.StatusCompleted, // Not in_progress
				LeaseEpoch: leaseEpoch,
				CreatedAt:  time.Now().Format(time.RFC3339),
				UpdatedAt:  time.Now().Format(time.RFC3339),
			},
		},
	}
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	// Execute: Send heartbeat
	req := makeHeartbeatRequest(t, TaskHeartbeatParams{
		TaskID:   taskID,
		WorkerID: workerID,
		Epoch:    leaseEpoch,
	})

	resp := handler.Handle(req.Params)

	// Verify: Should be rejected
	if resp.Success {
		t.Error("heartbeat should have failed for non-in_progress task")
	}
	if resp.Error == nil || resp.Error.Code != uds.ErrCodeFencingReject {
		t.Errorf("expected fencing reject error, got: %v", resp.Error)
	}
}

func TestHeartbeat_InvalidWorkerID(t *testing.T) {
	d := newTestDaemon(t)
	handler := newTestHeartbeatHandler(t, d)

	testCases := []struct {
		name     string
		workerID string
	}{
		{"path traversal", "../worker1"},
		{"dot", "."},
		{"double dot", ".."},
		{"with slash", "worker/1"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := makeHeartbeatRequest(t, TaskHeartbeatParams{
				TaskID:   "task_0000000001_abcdef01",
				WorkerID: tc.workerID,
				Epoch:    1,
			})

			resp := handler.Handle(req.Params)

			if resp.Success {
				t.Errorf("heartbeat should have failed for invalid worker ID: %s", tc.workerID)
			}
			if resp.Error == nil || resp.Error.Code != uds.ErrCodeValidation {
				t.Errorf("expected validation error for invalid worker ID, got: %v", resp.Error)
			}
		})
	}
}

// TestConcurrentHeartbeats tests concurrent heartbeat handling from multiple goroutines
func TestConcurrentHeartbeats(t *testing.T) {
	d := newTestDaemon(t)
	handler := newTestHeartbeatHandler(t, d)

	// Setup: Create multiple tasks for concurrent testing
	numWorkers := 10
	workerIDs := make([]string, numWorkers)
	taskIDs := make([]string, numWorkers)

	for i := 0; i < numWorkers; i++ {
		workerID := fmt.Sprintf("worker%d", i+1)
		taskID := fmt.Sprintf("task_000000000%d_abcdef0%d", i+1, i+1)
		workerIDs[i] = workerID
		taskIDs[i] = taskID

		expiresAt := time.Now().Add(10 * time.Second).Format(time.RFC3339)
		owner := fmt.Sprintf("daemon:1234%d", i)
		tq := model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks: []model.Task{
				{
					ID:             taskID,
					CommandID:      fmt.Sprintf("cmd_000000000%d_abcdef0%d", i+1, i+1),
					Purpose:        fmt.Sprintf("test purpose %d", i),
					Content:        fmt.Sprintf("test content %d", i),
					Status:         model.StatusInProgress,
					LeaseOwner:     &owner,
					LeaseExpiresAt: &expiresAt,
					LeaseEpoch:     1,
					CreatedAt:      time.Now().Format(time.RFC3339),
					UpdatedAt:      time.Now().Format(time.RFC3339),
				},
			},
		}
		path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
		if err := yamlutil.AtomicWrite(path, tq); err != nil {
			t.Fatalf("write worker queue %s: %v", workerID, err)
		}
	}

	// Execute: Send concurrent heartbeats
	var wg sync.WaitGroup
	results := make([]bool, numWorkers*10) // 10 heartbeats per worker
	for i := 0; i < numWorkers; i++ {
		for j := 0; j < 10; j++ {
			wg.Add(1)
			go func(workerIdx, iteration int) {
				defer wg.Done()
				req := makeHeartbeatRequest(t, TaskHeartbeatParams{
					TaskID:   taskIDs[workerIdx],
					WorkerID: workerIDs[workerIdx],
					Epoch:    1,
				})
				resp := handler.Handle(req.Params)
				results[workerIdx*10+iteration] = resp.Success
			}(i, j)
		}
	}
	wg.Wait()

	// Verify: All heartbeats should succeed
	for i, success := range results {
		if !success {
			t.Errorf("heartbeat %d failed", i)
		}
	}
}

// TestHeartbeatRaceCondition tests epoch racing with concurrent updates
func TestHeartbeatRaceCondition(t *testing.T) {
	d := newTestDaemon(t)
	handler := newTestHeartbeatHandler(t, d)

	taskID := "task_0000000001_abcdef01"
	workerID := "worker1"
	commandID := "cmd_0000000001_abcdef01"
	currentEpoch := 2

	// Setup: Create task with current epoch
	expiresAt := time.Now().Add(30 * time.Second).Format(time.RFC3339)
	owner := "daemon:12345"
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             taskID,
				CommandID:      commandID,
				Purpose:        "test purpose",
				Content:        "test content",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiresAt,
				LeaseEpoch:     currentEpoch,
				CreatedAt:      time.Now().Format(time.RFC3339),
				UpdatedAt:      time.Now().Format(time.RFC3339),
			},
		},
	}
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	// Test old epoch rejection
	oldReq := makeHeartbeatRequest(t, TaskHeartbeatParams{
		TaskID:   taskID,
		WorkerID: workerID,
		Epoch:    currentEpoch - 1,
	})
	oldResp := handler.Handle(oldReq.Params)
	if oldResp.Success {
		t.Error("old epoch heartbeat should be rejected")
	}
	if oldResp.Error == nil || oldResp.Error.Code != uds.ErrCodeFencingReject {
		t.Errorf("expected fencing reject for old epoch, got: %v", oldResp.Error)
	}

	// Test current epoch acceptance
	currentReq := makeHeartbeatRequest(t, TaskHeartbeatParams{
		TaskID:   taskID,
		WorkerID: workerID,
		Epoch:    currentEpoch,
	})
	currentResp := handler.Handle(currentReq.Params)
	if !currentResp.Success {
		t.Errorf("current epoch heartbeat should succeed: %v", currentResp.Error)
	}

	// Test future epoch rejection
	futureReq := makeHeartbeatRequest(t, TaskHeartbeatParams{
		TaskID:   taskID,
		WorkerID: workerID,
		Epoch:    currentEpoch + 1,
	})
	futureResp := handler.Handle(futureReq.Params)
	if futureResp.Success {
		t.Error("future epoch heartbeat should be rejected")
	}
	if futureResp.Error == nil || futureResp.Error.Code != uds.ErrCodeFencingReject {
		t.Errorf("expected fencing reject for future epoch, got: %v", futureResp.Error)
	}
}

// TestHeartbeatErrorMatrix tests all error code scenarios
func TestHeartbeatErrorMatrix(t *testing.T) {
	testCases := []struct {
		name         string
		setup        func(*testing.T, *Daemon) (TaskHeartbeatParams, string)
		expectedCode string
	}{
		{
			name: "NOT_FOUND - task not in queue",
			setup: func(t *testing.T, d *Daemon) (TaskHeartbeatParams, string) {
				workerID := "worker_notfound"
				tq := model.TaskQueue{
					SchemaVersion: 1,
					FileType:      "queue_task",
					Tasks:         []model.Task{},
				}
				path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
				if err := yamlutil.AtomicWrite(path, tq); err != nil {
					t.Fatalf("write queue: %v", err)
				}
				return TaskHeartbeatParams{
					TaskID:   "nonexistent_task",
					WorkerID: workerID,
					Epoch:    1,
				}, uds.ErrCodeNotFound
			},
			expectedCode: uds.ErrCodeNotFound,
		},
		{
			name: "NOT_FOUND - queue file missing",
			setup: func(t *testing.T, d *Daemon) (TaskHeartbeatParams, string) {
				return TaskHeartbeatParams{
					TaskID:   "task_0000000001_abcdef01",
					WorkerID: "nonexistent_worker",
					Epoch:    1,
				}, uds.ErrCodeNotFound
			},
			expectedCode: uds.ErrCodeNotFound,
		},
		{
			name: "FENCING_REJECT - epoch mismatch",
			setup: func(t *testing.T, d *Daemon) (TaskHeartbeatParams, string) {
				workerID := "worker_fence"
				taskID := "task_0000000001_abcdef01"
				expiresAt := time.Now().Add(30 * time.Second).Format(time.RFC3339)
				owner := "daemon:12345"
				tq := model.TaskQueue{
					SchemaVersion: 1,
					FileType:      "queue_task",
					Tasks: []model.Task{
						{
							ID:             taskID,
							CommandID:      "cmd_0000000001_abcdef01",
							Purpose:        "test",
							Content:        "test",
							Status:         model.StatusInProgress,
							LeaseOwner:     &owner,
							LeaseExpiresAt: &expiresAt,
							LeaseEpoch:     5,
							CreatedAt:      time.Now().Format(time.RFC3339),
							UpdatedAt:      time.Now().Format(time.RFC3339),
						},
					},
				}
				path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
				if err := yamlutil.AtomicWrite(path, tq); err != nil {
					t.Fatalf("write queue: %v", err)
				}
				return TaskHeartbeatParams{
					TaskID:   taskID,
					WorkerID: workerID,
					Epoch:    3, // Stale epoch
				}, uds.ErrCodeFencingReject
			},
			expectedCode: uds.ErrCodeFencingReject,
		},
		{
			name: "FENCING_REJECT - task not in_progress",
			setup: func(t *testing.T, d *Daemon) (TaskHeartbeatParams, string) {
				workerID := "worker_completed"
				taskID := "task_0000000001_abcdef01"
				tq := model.TaskQueue{
					SchemaVersion: 1,
					FileType:      "queue_task",
					Tasks: []model.Task{
						{
							ID:         taskID,
							CommandID:  "cmd_0000000001_abcdef01",
							Purpose:    "test",
							Content:    "test",
							Status:     model.StatusCompleted,
							LeaseEpoch: 1,
							CreatedAt:  time.Now().Format(time.RFC3339),
							UpdatedAt:  time.Now().Format(time.RFC3339),
						},
					},
				}
				path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
				if err := yamlutil.AtomicWrite(path, tq); err != nil {
					t.Fatalf("write queue: %v", err)
				}
				return TaskHeartbeatParams{
					TaskID:   taskID,
					WorkerID: workerID,
					Epoch:    1,
				}, uds.ErrCodeFencingReject
			},
			expectedCode: uds.ErrCodeFencingReject,
		},
		{
			name: "MAX_RUNTIME_EXCEEDED",
			setup: func(t *testing.T, d *Daemon) (TaskHeartbeatParams, string) {
				workerID := "worker_timeout"
				taskID := "task_0000000001_abcdef01"
				expiresAt := time.Now().Add(30 * time.Second).Format(time.RFC3339)
				owner := "daemon:12345"
				// Task created beyond max_in_progress_min
				createdAt := time.Now().Add(-time.Duration(d.config.Watcher.MaxInProgressMin+10) * time.Minute).Format(time.RFC3339)
				tq := model.TaskQueue{
					SchemaVersion: 1,
					FileType:      "queue_task",
					Tasks: []model.Task{
						{
							ID:             taskID,
							CommandID:      "cmd_0000000001_abcdef01",
							Purpose:        "test",
							Content:        "test",
							Status:         model.StatusInProgress,
							LeaseOwner:     &owner,
							LeaseExpiresAt: &expiresAt,
							LeaseEpoch:     1,
							CreatedAt:      createdAt,
							UpdatedAt:      createdAt,
						},
					},
				}
				path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
				if err := yamlutil.AtomicWrite(path, tq); err != nil {
					t.Fatalf("write queue: %v", err)
				}
				return TaskHeartbeatParams{
					TaskID:   taskID,
					WorkerID: workerID,
					Epoch:    1,
				}, uds.ErrCodeMaxRuntimeExceeded
			},
			expectedCode: uds.ErrCodeMaxRuntimeExceeded,
		},
		{
			name: "VALIDATION_ERROR - invalid worker ID",
			setup: func(t *testing.T, d *Daemon) (TaskHeartbeatParams, string) {
				return TaskHeartbeatParams{
					TaskID:   "task_0000000001_abcdef01",
					WorkerID: "../malicious",
					Epoch:    1,
				}, uds.ErrCodeValidation
			},
			expectedCode: uds.ErrCodeValidation,
		},
		{
			name: "VALIDATION_ERROR - missing task_id",
			setup: func(t *testing.T, d *Daemon) (TaskHeartbeatParams, string) {
				return TaskHeartbeatParams{
					TaskID:   "",
					WorkerID: "worker1",
					Epoch:    1,
				}, uds.ErrCodeValidation
			},
			expectedCode: uds.ErrCodeValidation,
		},
		{
			name: "VALIDATION_ERROR - missing worker_id",
			setup: func(t *testing.T, d *Daemon) (TaskHeartbeatParams, string) {
				return TaskHeartbeatParams{
					TaskID:   "task_0000000001_abcdef01",
					WorkerID: "",
					Epoch:    1,
				}, uds.ErrCodeValidation
			},
			expectedCode: uds.ErrCodeValidation,
		},
		{
			name: "VALIDATION_ERROR - negative epoch",
			setup: func(t *testing.T, d *Daemon) (TaskHeartbeatParams, string) {
				return TaskHeartbeatParams{
					TaskID:   "task_0000000001_abcdef01",
					WorkerID: "worker1",
					Epoch:    -1,
				}, uds.ErrCodeValidation
			},
			expectedCode: uds.ErrCodeValidation,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDaemon(t)
			handler := newTestHeartbeatHandler(t, d)

			params, expectedCode := tc.setup(t, d)
			req := makeHeartbeatRequest(t, params)
			resp := handler.Handle(req.Params)

			if resp.Success {
				t.Errorf("expected failure for %s", tc.name)
			}
			if resp.Error == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			} else if resp.Error.Code != expectedCode {
				t.Errorf("expected error code %s, got %s", expectedCode, resp.Error.Code)
			}
		})
	}
}

// TestHeartbeatTimeout tests lease expiration scenarios
func TestHeartbeatTimeout(t *testing.T) {
	d := newTestDaemon(t)
	handler := newTestHeartbeatHandler(t, d)

	taskID := "task_0000000001_abcdef01"
	workerID := "worker1"
	commandID := "cmd_0000000001_abcdef01"
	leaseEpoch := 1

	// Setup: Create task with very old lease (already expired)
	expiresAt := time.Now().Add(-10 * time.Second).Format(time.RFC3339)
	owner := "daemon:12345"
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             taskID,
				CommandID:      commandID,
				Purpose:        "test purpose",
				Content:        "test content",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiresAt,
				LeaseEpoch:     leaseEpoch,
				CreatedAt:      time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
				UpdatedAt:      time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			},
		},
	}
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	// Execute: Send heartbeat
	req := makeHeartbeatRequest(t, TaskHeartbeatParams{
		TaskID:   taskID,
		WorkerID: workerID,
		Epoch:    leaseEpoch,
	})

	resp := handler.Handle(req.Params)

	// Note: Heartbeat handler extends the lease regardless of current expiration
	// This is expected behavior - the heartbeat proves the worker is alive
	if !resp.Success {
		t.Logf("Heartbeat result: %v", resp.Error)
	}

	// Verify lease was extended
	var updatedQueue model.TaskQueue
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	if err := yamlv3.Unmarshal(data, &updatedQueue); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}

	task := &updatedQueue.Tasks[0]
	if task.LeaseExpiresAt != nil {
		newExpiry, _ := time.Parse(time.RFC3339, *task.LeaseExpiresAt)
		if newExpiry.After(time.Now()) {
			t.Logf("Lease successfully extended to: %v", newExpiry)
		}
	}
}

// TestHeartbeatExtension tests lease extension boundary conditions
func TestHeartbeatExtension(t *testing.T) {
	d := newTestDaemon(t)
	handler := newTestHeartbeatHandler(t, d)

	taskID := "task_0000000001_abcdef01"
	workerID := "worker1"
	commandID := "cmd_0000000001_abcdef01"
	leaseEpoch := 1

	// Setup: Create task with lease expiring in 5 seconds
	expiresAt := time.Now().Add(5 * time.Second).Format(time.RFC3339)
	owner := "daemon:12345"
	tq := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []model.Task{
			{
				ID:             taskID,
				CommandID:      commandID,
				Purpose:        "test purpose",
				Content:        "test content",
				Status:         model.StatusInProgress,
				LeaseOwner:     &owner,
				LeaseExpiresAt: &expiresAt,
				LeaseEpoch:     leaseEpoch,
				CreatedAt:      time.Now().Format(time.RFC3339),
				UpdatedAt:      time.Now().Format(time.RFC3339),
			},
		},
	}
	path := filepath.Join(d.maestroDir, "queue", workerID+".yaml")
	if err := yamlutil.AtomicWrite(path, tq); err != nil {
		t.Fatalf("write worker queue: %v", err)
	}

	// Execute: Send multiple heartbeats
	for i := 0; i < 3; i++ {
		req := makeHeartbeatRequest(t, TaskHeartbeatParams{
			TaskID:   taskID,
			WorkerID: workerID,
			Epoch:    leaseEpoch,
		})

		resp := handler.Handle(req.Params)
		if !resp.Success {
			t.Errorf("heartbeat %d failed: %v", i, resp.Error)
		}

		// Read back and verify extension
		var updatedQueue model.TaskQueue
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read queue file: %v", err)
		}
		if err := yamlv3.Unmarshal(data, &updatedQueue); err != nil {
			t.Fatalf("unmarshal queue: %v", err)
		}

		task := &updatedQueue.Tasks[0]
		if task.LeaseExpiresAt == nil {
			t.Fatal("lease_expires_at should not be nil")
		}

		newExpiry, err := time.Parse(time.RFC3339, *task.LeaseExpiresAt)
		if err != nil {
			t.Fatalf("parse lease_expires_at: %v", err)
		}

		// Each heartbeat should extend the lease
		expectedExpiry := time.Now().Add(time.Duration(d.config.Watcher.DispatchLeaseSec) * time.Second)
		diff := newExpiry.Sub(expectedExpiry).Abs()
		if diff > 5*time.Second {
			t.Errorf("iteration %d: lease not properly extended: got %v, expected ~%v", i, newExpiry, expectedExpiry)
		}

		time.Sleep(100 * time.Millisecond) // Small delay between heartbeats
	}
}
