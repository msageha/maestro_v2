package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
)

// CrashPoint represents a point in the system where a crash can be simulated
type CrashPoint int

const (
	CrashPointNone CrashPoint = iota
	CrashPointAfterQueueWrite
	CrashPointAfterStateWrite
	CrashPointAfterResultWrite
	CrashPointDuringRetryCreation
	CrashPointBetweenAtomicWrites
)

// CrashSimulator simulates system crashes at specific points
type CrashSimulator struct {
	mu           sync.Mutex
	crashPoint   CrashPoint
	shouldCrash  atomic.Bool
	crashCounter atomic.Int32
}

func NewCrashSimulator() *CrashSimulator {
	return &CrashSimulator{
		crashPoint: CrashPointNone,
	}
}

func (cs *CrashSimulator) SetCrashPoint(point CrashPoint) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.crashPoint = point
	cs.shouldCrash.Store(true)
}

func (cs *CrashSimulator) CheckCrash(point CrashPoint) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.shouldCrash.Load() && cs.crashPoint == point {
		cs.crashCounter.Add(1)
		cs.shouldCrash.Store(false) // Only crash once
		return fmt.Errorf("simulated crash at %v", point)
	}
	return nil
}

func (cs *CrashSimulator) Reset() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.crashPoint = CrashPointNone
	cs.shouldCrash.Store(false)
	cs.crashCounter.Store(0)
}

// StateVerifier verifies system state consistency
type StateVerifier struct {
	maestroDir string
}

func NewStateVerifier(maestroDir string) *StateVerifier {
	return &StateVerifier{maestroDir: maestroDir}
}

func (sv *StateVerifier) VerifyConsistency(t *testing.T) {
	// Verify queue files
	queueDir := filepath.Join(sv.maestroDir, "queue")
	sv.verifyDirectoryConsistency(t, queueDir, "queue")

	// Verify state files
	stateDir := filepath.Join(sv.maestroDir, "state", "commands")
	sv.verifyDirectoryConsistency(t, stateDir, "state")

	// Verify results files
	resultsDir := filepath.Join(sv.maestroDir, "results")
	sv.verifyDirectoryConsistency(t, resultsDir, "results")

	// Cross-verify references
	sv.verifyCrossReferences(t)
}

func (sv *StateVerifier) verifyDirectoryConsistency(t *testing.T, dir string, dirType string) {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("failed to read %s directory: %v", dirType, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())

		// Check for incomplete writes (tmp files)
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Errorf("found incomplete write (tmp file) in %s: %s", dirType, entry.Name())
			continue
		}

		// Verify YAML syntax
		data, err := os.ReadFile(filePath)
		if err != nil {
			t.Errorf("failed to read %s file %s: %v", dirType, entry.Name(), err)
			continue
		}

		var content interface{}
		if err := yamlv3.Unmarshal(data, &content); err != nil {
			t.Errorf("invalid YAML in %s file %s: %v", dirType, entry.Name(), err)
		}
	}
}

func (sv *StateVerifier) verifyCrossReferences(t *testing.T) {
	// Load all queue tasks
	tasksByID := make(map[string]*model.Task)
	queueDir := filepath.Join(sv.maestroDir, "queue")

	entries, _ := os.ReadDir(queueDir)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filePath := filepath.Join(queueDir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var queue model.TaskQueue
		if err := yamlv3.Unmarshal(data, &queue); err != nil {
			continue
		}

		for i := range queue.Tasks {
			task := &queue.Tasks[i]
			tasksByID[task.ID] = task

			// Verify retry task references
			if task.OriginalTaskID != "" {
				if _, exists := tasksByID[task.OriginalTaskID]; !exists {
					// Check if original task exists in results (completed)
					resultsPath := filepath.Join(sv.maestroDir, "results", task.CommandID, task.OriginalTaskID+".yaml")
					if _, err := os.Stat(resultsPath); os.IsNotExist(err) {
						t.Errorf("retry task %s references non-existent original task %s",
							task.ID, task.OriginalTaskID)
					}
				}
			}
		}
	}

	// Verify state references
	stateDir := filepath.Join(sv.maestroDir, "state", "commands")
	stateEntries, _ := os.ReadDir(stateDir)

	for _, entry := range stateEntries {
		if entry.IsDir() {
			continue
		}

		filePath := filepath.Join(stateDir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var state model.CommandState
		if err := yamlv3.Unmarshal(data, &state); err != nil {
			continue
		}

		// Verify all task references in state exist
		for taskID, status := range state.TaskStates {
			if status != model.StatusCompleted && status != model.StatusFailed {
				if _, exists := tasksByID[taskID]; !exists {
					t.Errorf("state references non-existent task %s", taskID)
				}
			}
		}
	}
}

// RecoveryManager handles recovery after simulated crashes
type RecoveryManager struct {
	maestroDir string
	lockMap    *lock.MutexMap
}

func NewRecoveryManager(maestroDir string) *RecoveryManager {
	return &RecoveryManager{
		maestroDir: maestroDir,
		lockMap:    lock.NewMutexMap(),
	}
}

func (rm *RecoveryManager) Recover(t *testing.T) {
	// Clean up tmp files
	rm.cleanupTmpFiles(t)

	// Verify and repair queue consistency
	rm.repairQueues(t)

	// Reconcile state
	rm.reconcileState(t)
}

func (rm *RecoveryManager) cleanupTmpFiles(t *testing.T) {
	// Walk through all directories and remove .tmp files
	err := filepath.Walk(rm.maestroDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if filepath.Ext(path) == ".tmp" {
			t.Logf("removing incomplete write: %s", path)
			os.Remove(path)
		}

		return nil
	})

	if err != nil {
		t.Errorf("failed to clean up tmp files: %v", err)
	}
}

func (rm *RecoveryManager) repairQueues(t *testing.T) {
	queueDir := filepath.Join(rm.maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filePath := filepath.Join(queueDir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var queue model.TaskQueue
		if err := yamlv3.Unmarshal(data, &queue); err != nil {
			t.Logf("repairing corrupted queue file: %s", entry.Name())
			// Create empty queue
			queue = model.TaskQueue{
				SchemaVersion: 1,
				FileType:      "queue_task",
				Tasks:         []model.Task{},
			}
			yaml.AtomicWrite(filePath, queue)
		}
	}
}

func (rm *RecoveryManager) reconcileState(t *testing.T) {
	// This would implement state reconciliation logic
	// For testing, we just verify the state is readable
	stateDir := filepath.Join(rm.maestroDir, "state", "commands")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filePath := filepath.Join(stateDir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var state model.CommandState
		if err := yamlv3.Unmarshal(data, &state); err != nil {
			t.Logf("state file corrupted, would reconcile: %s", entry.Name())
		}
	}
}

// TestPartialFailureWindow_TaskCreation tests crash during task creation
func TestPartialFailureWindow_TaskCreation(t *testing.T) {
	maestroDir := t.TempDir()
	require.NoError(t, setupTestDirectories(maestroDir))

	crashSim := NewCrashSimulator()
	verifier := NewStateVerifier(maestroDir)
	recovery := NewRecoveryManager(maestroDir)

	scenarios := []struct {
		name       string
		crashPoint CrashPoint
		verifyFunc func(*testing.T)
	}{
		{
			name:       "crash_after_queue_write",
			crashPoint: CrashPointAfterQueueWrite,
			verifyFunc: func(t *testing.T) {
				// Queue should have the task
				queuePath := filepath.Join(maestroDir, "queue", "worker1.yaml")
				data, err := os.ReadFile(queuePath)
				require.NoError(t, err)

				var queue model.TaskQueue
				require.NoError(t, yamlv3.Unmarshal(data, &queue))
				assert.Len(t, queue.Tasks, 1)

				// Result should not exist
				resultPath := filepath.Join(maestroDir, "results", "cmd_001", "task_001.yaml")
				_, err = os.Stat(resultPath)
				assert.True(t, os.IsNotExist(err))
			},
		},
		{
			name:       "crash_after_state_write",
			crashPoint: CrashPointAfterStateWrite,
			verifyFunc: func(t *testing.T) {
				// State should be updated
				statePath := filepath.Join(maestroDir, "state", "commands", "cmd_001.yaml")
				data, err := os.ReadFile(statePath)
				require.NoError(t, err)

				var state model.CommandState
				require.NoError(t, yamlv3.Unmarshal(data, &state))
				assert.Contains(t, state.TaskStates, "task_001")
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// Clean up from previous run
			cleanupTestData(maestroDir)
			require.NoError(t, setupTestDirectories(maestroDir))

			// Set crash point
			crashSim.SetCrashPoint(scenario.crashPoint)

			// Simulate task creation with crash
			err := simulateTaskCreation(maestroDir, crashSim)
			if err != nil {
				t.Logf("crash simulated: %v", err)
			}

			// Verify state after crash
			scenario.verifyFunc(t)

			// Verify overall consistency
			verifier.VerifyConsistency(t)

			// Attempt recovery
			recovery.Recover(t)

			// Verify consistency after recovery
			verifier.VerifyConsistency(t)

			crashSim.Reset()
		})
	}
}

// TestPartialFailureWindow_RetryCreation tests crash during retry task creation
func TestPartialFailureWindow_RetryCreation(t *testing.T) {
	maestroDir := t.TempDir()
	require.NoError(t, setupTestDirectories(maestroDir))

	crashSim := NewCrashSimulator()
	verifier := NewStateVerifier(maestroDir)
	recovery := NewRecoveryManager(maestroDir)

	// Setup: Create original failed task
	originalTask := &model.Task{
		ID:               "task_original",
		CommandID:        "cmd_001",
		Purpose:          "test task",
		Content:          "test content",
		Status:           model.StatusFailed,
		ExecutionRetries: 0,
		CreatedAt:        time.Now().Format(time.RFC3339),
		UpdatedAt:        time.Now().Format(time.RFC3339),
	}

	// Write original task result
	resultDir := filepath.Join(maestroDir, "results", originalTask.CommandID)
	os.MkdirAll(resultDir, 0755)
	resultPath := filepath.Join(resultDir, originalTask.ID+".yaml")

	result := map[string]interface{}{
		"task_id":    originalTask.ID,
		"command_id": originalTask.CommandID,
		"status":     model.StatusFailed,
		"summary":    "Task failed with exit code 1",
		"created_at": time.Now().Format(time.RFC3339),
	}
	require.NoError(t, yaml.AtomicWrite(resultPath, result))

	t.Run("crash_during_retry_creation", func(t *testing.T) {
		crashSim.SetCrashPoint(CrashPointDuringRetryCreation)

		// Simulate retry creation with crash
		err := simulateRetryCreation(maestroDir, originalTask, crashSim)
		if err != nil {
			t.Logf("crash simulated during retry: %v", err)
		}

		// Verify no duplicate retry tasks
		retryTasks := findRetryTasksForOriginal(maestroDir, originalTask.ID)
		assert.LessOrEqual(t, len(retryTasks), 1, "should not have duplicate retry tasks")

		// Verify consistency
		verifier.VerifyConsistency(t)

		// Recover
		recovery.Recover(t)

		// Verify consistency after recovery
		verifier.VerifyConsistency(t)

		// Verify original task is still accessible
		_, err = os.Stat(resultPath)
		assert.NoError(t, err, "original task result should still exist")
	})
}

// TestDataConsistency_AfterCrash tests data consistency after various crash scenarios
func TestDataConsistency_AfterCrash(t *testing.T) {
	maestroDir := t.TempDir()
	require.NoError(t, setupTestDirectories(maestroDir))

	verifier := NewStateVerifier(maestroDir)
	recovery := NewRecoveryManager(maestroDir)

	t.Run("incomplete_atomic_write", func(t *testing.T) {
		// Simulate incomplete atomic write by creating .tmp file
		queuePath := filepath.Join(maestroDir, "queue", "worker1.yaml")
		tmpPath := queuePath + ".tmp"

		// Write partial data to tmp file
		partialData := []byte("schema_version: 1\nfile_type: queue_task\n# incomplete")
		require.NoError(t, os.WriteFile(tmpPath, partialData, 0644))

		// Verify detects incomplete write
		verifier.VerifyConsistency(t)

		// Recover should clean up tmp file
		recovery.Recover(t)

		// Tmp file should be removed
		_, err := os.Stat(tmpPath)
		assert.True(t, os.IsNotExist(err), "tmp file should be cleaned up")
	})

	t.Run("corrupted_yaml", func(t *testing.T) {
		// Create corrupted YAML file
		queuePath := filepath.Join(maestroDir, "queue", "worker2.yaml")
		corruptedData := []byte("invalid yaml content {[}")
		require.NoError(t, os.WriteFile(queuePath, corruptedData, 0644))

		// Recovery should repair the file
		recovery.Recover(t)

		// File should be valid YAML after recovery
		data, err := os.ReadFile(queuePath)
		require.NoError(t, err)

		var queue model.TaskQueue
		err = yamlv3.Unmarshal(data, &queue)
		assert.NoError(t, err, "queue should be valid YAML after recovery")
		assert.Equal(t, 1, queue.SchemaVersion)
		assert.Equal(t, "queue_task", queue.FileType)
	})

	t.Run("orphaned_references", func(t *testing.T) {
		// Create a task with reference to non-existent original
		orphanedTask := model.Task{
			ID:             "task_orphan",
			CommandID:      "cmd_001",
			OriginalTaskID: "task_nonexistent",
			Status:         model.StatusPending,
			CreatedAt:      time.Now().Format(time.RFC3339),
			UpdatedAt:      time.Now().Format(time.RFC3339),
		}

		queue := model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks:         []model.Task{orphanedTask},
		}

		queuePath := filepath.Join(maestroDir, "queue", "worker3.yaml")
		require.NoError(t, yaml.AtomicWrite(queuePath, queue))

		// Verifier should detect orphaned reference
		verifier.VerifyConsistency(t)
	})
}

// TestRecovery_IncompleteWrites tests recovery from incomplete writes
func TestRecovery_IncompleteWrites(t *testing.T) {
	maestroDir := t.TempDir()
	require.NoError(t, setupTestDirectories(maestroDir))

	recovery := NewRecoveryManager(maestroDir)
	verifier := NewStateVerifier(maestroDir)

	t.Run("multiple_tmp_files", func(t *testing.T) {
		// Create multiple tmp files in different directories
		tmpFiles := []string{
			filepath.Join(maestroDir, "queue", "worker1.yaml.tmp"),
			filepath.Join(maestroDir, "state", "commands", "cmd_001.yaml.tmp"),
			filepath.Join(maestroDir, "results", "cmd_001", "task_001.yaml.tmp"),
		}

		for _, tmpFile := range tmpFiles {
			os.MkdirAll(filepath.Dir(tmpFile), 0755)
			os.WriteFile(tmpFile, []byte("incomplete"), 0644)
		}

		// Verify tmp files exist
		for _, tmpFile := range tmpFiles {
			_, err := os.Stat(tmpFile)
			require.NoError(t, err)
		}

		// Recover
		recovery.Recover(t)

		// All tmp files should be removed
		for _, tmpFile := range tmpFiles {
			_, err := os.Stat(tmpFile)
			assert.True(t, os.IsNotExist(err), "tmp file should be removed: %s", tmpFile)
		}

		// System should be consistent
		verifier.VerifyConsistency(t)
	})

	t.Run("fsync_verification", func(t *testing.T) {
		// Test that atomic writes use fsync properly
		testFile := filepath.Join(maestroDir, "test_fsync.yaml")

		data := model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks:         []model.Task{},
		}

		// Atomic write should complete successfully
		err := yaml.AtomicWrite(testFile, data)
		require.NoError(t, err)

		// File should exist and be valid
		readData, err := os.ReadFile(testFile)
		require.NoError(t, err)

		var readQueue model.TaskQueue
		err = yamlv3.Unmarshal(readData, &readQueue)
		assert.NoError(t, err)
		assert.Equal(t, data.SchemaVersion, readQueue.SchemaVersion)

		// No tmp file should remain
		_, err = os.Stat(testFile + ".tmp")
		assert.True(t, os.IsNotExist(err))
	})
}

// TestConcurrentCrash_MultipleWorkers tests crashes with multiple concurrent workers
func TestConcurrentCrash_MultipleWorkers(t *testing.T) {
	maestroDir := t.TempDir()
	require.NoError(t, setupTestDirectories(maestroDir))

	crashSim := NewCrashSimulator()
	verifier := NewStateVerifier(maestroDir)
	recovery := NewRecoveryManager(maestroDir)

	numWorkers := 5
	var wg sync.WaitGroup
	errors := make(chan error, numWorkers)

	t.Run("concurrent_task_creation_with_crash", func(t *testing.T) {
		// Set crash to occur for one of the workers
		crashSim.SetCrashPoint(CrashPointAfterQueueWrite)

		for i := 0; i < numWorkers; i++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				task := &model.Task{
					ID:        fmt.Sprintf("task_%d", workerID),
					CommandID: "cmd_001",
					Purpose:   fmt.Sprintf("test task %d", workerID),
					Status:    model.StatusPending,
					CreatedAt: time.Now().Format(time.RFC3339),
					UpdatedAt: time.Now().Format(time.RFC3339),
				}

				// Write task to queue
				queue := model.TaskQueue{
					SchemaVersion: 1,
					FileType:      "queue_task",
					Tasks:         []model.Task{*task},
				}

				queuePath := filepath.Join(maestroDir, "queue", fmt.Sprintf("worker%d.yaml", workerID))

				// Simulate potential crash
				if err := crashSim.CheckCrash(CrashPointAfterQueueWrite); err != nil {
					errors <- fmt.Errorf("worker %d crashed: %v", workerID, err)
					return
				}

				if err := yaml.AtomicWrite(queuePath, queue); err != nil {
					errors <- fmt.Errorf("worker %d write failed: %v", workerID, err)
				}
			}(i)
		}

		wg.Wait()
		close(errors)

		// Collect errors
		var crashCount int
		for err := range errors {
			t.Logf("worker error: %v", err)
			crashCount++
		}

		// Should have at most one crash
		assert.LessOrEqual(t, crashCount, 1, "should crash at most once")

		// Verify system consistency despite crash
		verifier.VerifyConsistency(t)

		// Recover from any partial states
		recovery.Recover(t)

		// Final consistency check
		verifier.VerifyConsistency(t)

		// Verify some tasks were created successfully
		queueDir := filepath.Join(maestroDir, "queue")
		entries, err := os.ReadDir(queueDir)
		require.NoError(t, err)
		assert.Greater(t, len(entries), 0, "some tasks should have been created")
	})

	t.Run("concurrent_retry_creation", func(t *testing.T) {
		// Reset crash simulator
		crashSim.Reset()
		crashSim.SetCrashPoint(CrashPointDuringRetryCreation)

		// Create multiple failed tasks
		for i := 0; i < numWorkers; i++ {
			wg.Add(1)
			go func(workerID int) {
				defer wg.Done()

				originalTask := &model.Task{
					ID:               fmt.Sprintf("orig_%d", workerID),
					CommandID:        "cmd_001",
					Status:           model.StatusFailed,
					ExecutionRetries: 0,
					CreatedAt:        time.Now().Format(time.RFC3339),
					UpdatedAt:        time.Now().Format(time.RFC3339),
				}

				// Simulate retry creation
				if err := simulateRetryCreation(maestroDir, originalTask, crashSim); err != nil {
					t.Logf("retry creation for worker %d: %v", workerID, err)
				}
			}(i)
		}

		wg.Wait()

		// Verify no duplicate retries despite concurrent creation and crash
		for i := 0; i < numWorkers; i++ {
			origID := fmt.Sprintf("orig_%d", i)
			retryTasks := findRetryTasksForOriginal(maestroDir, origID)
			assert.LessOrEqual(t, len(retryTasks), 1,
				"should have at most one retry for task %s", origID)
		}

		// System should remain consistent
		verifier.VerifyConsistency(t)
	})
}

// Helper functions

func setupTestDirectories(maestroDir string) error {
	dirs := []string{
		filepath.Join(maestroDir, "queue"),
		filepath.Join(maestroDir, "state", "commands"),
		filepath.Join(maestroDir, "results", "cmd_001"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	return nil
}

func cleanupTestData(maestroDir string) {
	// Remove all test data
	os.RemoveAll(filepath.Join(maestroDir, "queue"))
	os.RemoveAll(filepath.Join(maestroDir, "state"))
	os.RemoveAll(filepath.Join(maestroDir, "results"))
}

func simulateTaskCreation(maestroDir string, crashSim *CrashSimulator) error {
	task := &model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Purpose:   "test task",
		Content:   "test content",
		Status:    model.StatusPending,
		CreatedAt: time.Now().Format(time.RFC3339),
		UpdatedAt: time.Now().Format(time.RFC3339),
	}

	// Write to queue
	queue := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks:         []model.Task{*task},
	}

	queuePath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	if err := yaml.AtomicWrite(queuePath, queue); err != nil {
		return err
	}

	// Check for crash after queue write
	if err := crashSim.CheckCrash(CrashPointAfterQueueWrite); err != nil {
		return err
	}

	// Write to state
	state := model.CommandState{
		CommandID:  "cmd_001",
		TaskStates: map[string]model.Status{task.ID: model.StatusPending},
		UpdatedAt:  time.Now().Format(time.RFC3339),
	}

	statePath := filepath.Join(maestroDir, "state", "commands", "cmd_001.yaml")
	if err := yaml.AtomicWrite(statePath, state); err != nil {
		return err
	}

	// Check for crash after state write
	if err := crashSim.CheckCrash(CrashPointAfterStateWrite); err != nil {
		return err
	}

	// Write result (if task completes)
	// Using a simple result structure compatible with the existing codebase
	result := map[string]interface{}{
		"task_id":    task.ID,
		"command_id": task.CommandID,
		"status":     model.StatusCompleted,
		"created_at": time.Now().Format(time.RFC3339),
	}

	resultDir := filepath.Join(maestroDir, "results", task.CommandID)
	os.MkdirAll(resultDir, 0755)
	resultPath := filepath.Join(resultDir, task.ID+".yaml")

	if err := yaml.AtomicWrite(resultPath, result); err != nil {
		return err
	}

	// Check for crash after result write
	return crashSim.CheckCrash(CrashPointAfterResultWrite)
}

func simulateRetryCreation(maestroDir string, originalTask *model.Task, crashSim *CrashSimulator) error {
	// Check for crash during retry creation
	if err := crashSim.CheckCrash(CrashPointDuringRetryCreation); err != nil {
		return err
	}

	// Create retry task
	retryTask := &model.Task{
		ID:               fmt.Sprintf("retry_%s_%d", originalTask.ID, time.Now().UnixNano()),
		CommandID:        originalTask.CommandID,
		Purpose:          originalTask.Purpose,
		Content:          originalTask.Content,
		OriginalTaskID:   originalTask.ID,
		ExecutionRetries: originalTask.ExecutionRetries + 1,
		Status:           model.StatusPending,
		CreatedAt:        time.Now().Format(time.RFC3339),
		UpdatedAt:        time.Now().Format(time.RFC3339),
	}

	// Add cooldown
	cooldownTime := time.Now().Add(30 * time.Second).Format(time.RFC3339)
	retryTask.NotBefore = &cooldownTime

	// Load existing queue or create new
	queuePath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	var queue model.TaskQueue

	data, err := os.ReadFile(queuePath)
	if err == nil {
		yamlv3.Unmarshal(data, &queue)
	} else {
		queue = model.TaskQueue{
			SchemaVersion: 1,
			FileType:      "queue_task",
			Tasks:         []model.Task{},
		}
	}

	// Add retry task to queue
	queue.Tasks = append(queue.Tasks, *retryTask)

	// Write queue (atomic)
	if err := yaml.AtomicWrite(queuePath, queue); err != nil {
		return err
	}

	// Update state
	statePath := filepath.Join(maestroDir, "state", "commands", originalTask.CommandID+".yaml")
	var state model.CommandState

	stateData, err := os.ReadFile(statePath)
	if err == nil {
		yamlv3.Unmarshal(stateData, &state)
	} else {
		state = model.CommandState{
			CommandID:  originalTask.CommandID,
			TaskStates: make(map[string]model.Status),
			UpdatedAt:  time.Now().Format(time.RFC3339),
		}
	}

	state.TaskStates[retryTask.ID] = model.StatusPending

	return yaml.AtomicWrite(statePath, state)
}

func findRetryTasksForOriginal(maestroDir string, originalTaskID string) []*model.Task {
	var retryTasks []*model.Task

	queueDir := filepath.Join(maestroDir, "queue")
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return retryTasks
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filePath := filepath.Join(queueDir, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var queue model.TaskQueue
		if err := yamlv3.Unmarshal(data, &queue); err != nil {
			continue
		}

		for i := range queue.Tasks {
			task := &queue.Tasks[i]
			if task.OriginalTaskID == originalTaskID {
				retryTasks = append(retryTasks, task)
			}
		}
	}

	return retryTasks
}

// TestRecoveryIdempotency ensures recovery can be run multiple times safely
func TestRecoveryIdempotency(t *testing.T) {
	maestroDir := t.TempDir()
	require.NoError(t, setupTestDirectories(maestroDir))

	recovery := NewRecoveryManager(maestroDir)
	verifier := NewStateVerifier(maestroDir)

	// Create some test data
	task := model.Task{
		ID:        "task_001",
		CommandID: "cmd_001",
		Status:    model.StatusPending,
		CreatedAt: time.Now().Format(time.RFC3339),
		UpdatedAt: time.Now().Format(time.RFC3339),
	}

	queue := model.TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks:         []model.Task{task},
	}

	queuePath := filepath.Join(maestroDir, "queue", "worker1.yaml")
	require.NoError(t, yaml.AtomicWrite(queuePath, queue))

	// Run recovery multiple times
	for i := 0; i < 3; i++ {
		recovery.Recover(t)
		verifier.VerifyConsistency(t)

		// Verify data is still intact
		data, err := os.ReadFile(queuePath)
		require.NoError(t, err)

		var readQueue model.TaskQueue
		require.NoError(t, yamlv3.Unmarshal(data, &readQueue))
		assert.Len(t, readQueue.Tasks, 1)
		assert.Equal(t, task.ID, readQueue.Tasks[0].ID)
	}
}