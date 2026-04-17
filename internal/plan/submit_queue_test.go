package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

func TestApplyTaskDefaults_NilExpectedPaths(t *testing.T) {
	tasks := []TaskInput{
		{
			Name:               "t1",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
		},
	}

	ApplyTaskDefaults(tasks)

	if tasks[0].ExpectedPaths == nil {
		t.Fatal("ExpectedPaths should not be nil after ApplyTaskDefaults")
	}
	if len(tasks[0].ExpectedPaths) != 0 {
		t.Errorf("ExpectedPaths should be empty, got %v", tasks[0].ExpectedPaths)
	}
}

func TestApplyTaskDefaults_NilDefinitionOfAbort(t *testing.T) {
	tasks := []TaskInput{
		{
			Name:               "t1",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
		},
	}

	ApplyTaskDefaults(tasks)

	if tasks[0].DefinitionOfAbort == nil {
		t.Fatal("DefinitionOfAbort should not be nil after ApplyTaskDefaults")
	}
	expected := model.DefaultDefinitionOfAbort()
	if tasks[0].DefinitionOfAbort.MaxRepairCount != expected.MaxRepairCount {
		t.Errorf("MaxRepairCount = %d, want %d", tasks[0].DefinitionOfAbort.MaxRepairCount, expected.MaxRepairCount)
	}
	if tasks[0].DefinitionOfAbort.MaxWallClockSec != expected.MaxWallClockSec {
		t.Errorf("MaxWallClockSec = %d, want %d", tasks[0].DefinitionOfAbort.MaxWallClockSec, expected.MaxWallClockSec)
	}
}

func TestApplyTaskDefaults_PreservesExistingValues(t *testing.T) {
	customAbort := &model.DefinitionOfAbort{
		MaxRepairCount:  10,
		MaxWallClockSec: 7200,
	}
	tasks := []TaskInput{
		{
			Name:               "t1",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
			ExpectedPaths:      []string{"src/main.go"},
			DefinitionOfAbort:  customAbort,
		},
	}

	ApplyTaskDefaults(tasks)

	if len(tasks[0].ExpectedPaths) != 1 || tasks[0].ExpectedPaths[0] != "src/main.go" {
		t.Errorf("ExpectedPaths should be preserved, got %v", tasks[0].ExpectedPaths)
	}
	if tasks[0].DefinitionOfAbort.MaxRepairCount != 10 {
		t.Errorf("MaxRepairCount should be preserved, got %d", tasks[0].DefinitionOfAbort.MaxRepairCount)
	}
	if tasks[0].DefinitionOfAbort.MaxWallClockSec != 7200 {
		t.Errorf("MaxWallClockSec should be preserved, got %d", tasks[0].DefinitionOfAbort.MaxWallClockSec)
	}
}

func TestApplyTaskDefaults_MultipleTasks(t *testing.T) {
	tasks := []TaskInput{
		{Name: "t1", Purpose: "p", Content: "c", AcceptanceCriteria: "ac", BloomLevel: 1},
		{Name: "t2", Purpose: "p", Content: "c", AcceptanceCriteria: "ac", BloomLevel: 2,
			ExpectedPaths: []string{"existing.go"}},
	}

	ApplyTaskDefaults(tasks)

	for i, task := range tasks {
		if task.ExpectedPaths == nil {
			t.Errorf("tasks[%d].ExpectedPaths should not be nil", i)
		}
		if task.DefinitionOfAbort == nil {
			t.Errorf("tasks[%d].DefinitionOfAbort should not be nil", i)
		}
	}
	if len(tasks[1].ExpectedPaths) != 1 {
		t.Errorf("tasks[1].ExpectedPaths should be preserved with 1 item, got %d", len(tasks[1].ExpectedPaths))
	}
}

// --- C-A3: Flock-protected RMW tests ---

func TestWriteQueueEntries_ConcurrentFlockProtection(t *testing.T) {
	// Concurrent writers to the same worker queue should not lose entries.
	maestroDir := testutil.SetupDirWithQueues(t, 1)
	lm := lock.NewMutexMap()

	const goroutines = 10
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := fmt.Sprintf("task_0000000%03d_aaaaaaaa", idx)
			assignments := []WorkerAssignment{
				{TaskName: fmt.Sprintf("t%d", idx), WorkerID: "worker1", Model: "sonnet"},
			}
			tasks := []TaskInput{
				{
					Name:               fmt.Sprintf("t%d", idx),
					Purpose:            "test",
					Content:            "content",
					AcceptanceCriteria: "ac",
					BloomLevel:         1,
				},
			}
			nameToID := map[string]string{fmt.Sprintf("t%d", idx): taskID}
			err := writeQueueEntries(maestroDir, assignments, tasks, nameToID,
				"cmd_0000000001_aabbccdd", "2025-01-01T00:00:00Z", lm)
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("writeQueueEntries error: %v", err)
		}
	}

	// Verify all entries are present (no lost writes)
	queueFile := filepath.Join(maestroDir, "queue", "worker1.yaml")
	data, err := os.ReadFile(queueFile)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	if len(tq.Tasks) != goroutines {
		t.Errorf("queue tasks = %d, want %d (no lost writes)", len(tq.Tasks), goroutines)
	}
}

// --- C-A4: Shared lock for reads ---

func TestCheckCommandNotCancelled_ConcurrentReads(t *testing.T) {
	// Concurrent reads with shared locks should all succeed without blocking.
	maestroDir := testutil.SetupDirWithQueues(t, 1)
	commandID := "cmd_0000000070_aabbccdd"
	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	const goroutines = 10
	errCh := make(chan error, goroutines)
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- checkCommandNotCancelled(maestroDir, commandID)
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Errorf("checkCommandNotCancelled error: %v", err)
		}
	}
}

func TestCheckCommandNotCancelled_SharedLockCompatibility(t *testing.T) {
	// Verify that LOCK_SH is compatible (multiple shared locks can coexist).
	maestroDir := testutil.SetupDirWithQueues(t, 1)
	commandID := "cmd_0000000071_aabbccdd"
	writePlannerQueue(t, maestroDir, commandID, model.StatusInProgress)

	lockPath := queueFlockPath(maestroDir, "planner.yaml")

	// Acquire first shared lock
	f1, err := acquireFlock(lockPath, syscall.LOCK_SH)
	if err != nil {
		t.Fatalf("acquire first shared lock: %v", err)
	}
	defer releaseFlock(f1)

	// Acquire second shared lock (should not block)
	f2, err := acquireFlock(lockPath, syscall.LOCK_SH)
	if err != nil {
		t.Fatalf("acquire second shared lock: %v", err)
	}
	defer releaseFlock(f2)

	// Both locks held: reads should succeed
	err = checkCommandNotCancelled(maestroDir, commandID)
	if err != nil {
		t.Errorf("checkCommandNotCancelled under shared locks: %v", err)
	}
}

func TestAcquireFlock_ExclusiveBlocksExclusive(t *testing.T) {
	// Verify that LOCK_EX|LOCK_NB fails when another LOCK_EX is held.
	maestroDir := testutil.SetupDir(t)
	lockPath := queueFlockPath(maestroDir, "test.yaml")

	f1, err := acquireFlock(lockPath, syscall.LOCK_EX)
	if err != nil {
		t.Fatalf("acquire exclusive lock: %v", err)
	}
	defer releaseFlock(f1)

	// Non-blocking attempt should fail
	_, err = acquireFlock(lockPath, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		t.Error("expected error when acquiring second exclusive lock, got nil")
	}
}
