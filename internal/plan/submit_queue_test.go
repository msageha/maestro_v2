package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil"
)

// TestValidateTasksInput_RejectsNilExpectedPaths verifies that a task with
// nil expected_paths fails validation.
func TestValidateTasksInput_RejectsNilExpectedPaths(t *testing.T) {
	tasks := []TaskInput{
		{
			Name:               "t1",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
			DefinitionOfAbort:  func() *model.DefinitionOfAbort { d := model.DefaultDefinitionOfAbort(); return &d }(),
			// ExpectedPaths intentionally omitted
		},
	}
	errs := ValidateTasksInput(tasks)
	if errs == nil {
		t.Fatal("expected validation error for nil expected_paths, got nil")
	}
	if !strings.Contains(errs.Error(), "expected_paths") {
		t.Errorf("expected error mentioning expected_paths, got: %s", errs.Error())
	}
}

// TestValidateTasksInput_RejectsNilDefinitionOfAbort verifies the same for
// definition_of_abort.
func TestValidateTasksInput_RejectsNilDefinitionOfAbort(t *testing.T) {
	tasks := []TaskInput{
		{
			Name:               "t1",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
			ExpectedPaths:      []string{"src/main.go"},
			// DefinitionOfAbort intentionally omitted
		},
	}
	errs := ValidateTasksInput(tasks)
	if errs == nil {
		t.Fatal("expected validation error for nil definition_of_abort, got nil")
	}
	if !strings.Contains(errs.Error(), "definition_of_abort") {
		t.Errorf("expected error mentioning definition_of_abort, got: %s", errs.Error())
	}
}

// --- Bug F: run_on_main / run_on_integration propagation ---

// TestWriteQueueEntries_PropagatesRunOnMain verifies that `run_on_main: true`
// on a TaskInput is reflected in the resulting queue task. Without this, a
// Planner-submitted verification task under `plan submit --phase verification`
// silently runs in the worker's worktree instead of main, producing false
// FAIL when the verification targets the merged/published state (Bug F).
func TestWriteQueueEntries_PropagatesRunOnMain(t *testing.T) {
	maestroDir := testutil.SetupDirWithQueues(t, 1)
	lm := lock.NewMutexMap()

	taskID := "task_0000000100_aaaaaaaa"
	tasks := []TaskInput{
		{
			Name:               "main-verify",
			Purpose:            "verify main state",
			Content:            "run tests on main",
			AcceptanceCriteria: "tests pass",
			DefinitionOfDone:   []string{"all command-scoped verify commands pass", "no files outside expected paths change"},
			BloomLevel:         3,
			RunOnMain:          true,
		},
	}
	assignments := []WorkerAssignment{
		{TaskName: "main-verify", WorkerID: "worker1", Model: "sonnet"},
	}
	nameToID := map[string]string{"main-verify": taskID}

	if err := writeQueueEntries(maestroDir, assignments, tasks, nameToID,
		"cmd_0000000100_aabbccdd", "2025-01-01T00:00:00Z", lm); err != nil {
		t.Fatalf("writeQueueEntries error: %v", err)
	}

	queueFile := filepath.Join(maestroDir, "queue", "worker1.yaml")
	data, err := os.ReadFile(queueFile)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	if len(tq.Tasks) != 1 {
		t.Fatalf("tasks len=%d, want 1", len(tq.Tasks))
	}
	if !tq.Tasks[0].RunOnMain {
		t.Errorf("Task.RunOnMain = false; Bug F regression: plan submit must propagate RunOnMain from TaskInput")
	}
	if got := tq.Tasks[0].DefinitionOfDone; len(got) != 2 || got[0] != "all command-scoped verify commands pass" {
		t.Errorf("Task.DefinitionOfDone = %#v; plan submit must preserve explicit done conditions", got)
	}
	if tq.Tasks[0].RunOnIntegration {
		t.Errorf("Task.RunOnIntegration should be false when only RunOnMain is set")
	}
	// §S0-1: RunOnMain は verification セマンティクスなので Admission Control の
	// verify バケットに分類される必要がある。未分類 (空文字) のままだと verify
	// 同時実行制限が効かず、複数の RunOnMain タスクが並走しうる。
	if got, want := tq.Tasks[0].OperationType, model.OperationTypeVerify; got != want {
		t.Errorf("Task.OperationType = %q, want %q (RunOnMain must classify as verify for §S0-1 admission)", got, want)
	}
}

// TestWriteQueueEntries_PropagatesRunOnIntegration verifies publish_conflict
// resolution tasks (run_on_integration) are correctly flagged on the queue task.
func TestWriteQueueEntries_PropagatesRunOnIntegration(t *testing.T) {
	maestroDir := testutil.SetupDirWithQueues(t, 1)
	lm := lock.NewMutexMap()

	taskID := "task_0000000101_aaaaaaaa"
	tasks := []TaskInput{
		{
			Name:               "resolve-integration",
			Purpose:            "resolve publish conflict on integration",
			Content:            "resolve conflicts",
			AcceptanceCriteria: "no conflict markers",
			BloomLevel:         3,
			RunOnIntegration:   true,
		},
	}
	assignments := []WorkerAssignment{
		{TaskName: "resolve-integration", WorkerID: "worker1", Model: "sonnet"},
	}
	nameToID := map[string]string{"resolve-integration": taskID}

	if err := writeQueueEntries(maestroDir, assignments, tasks, nameToID,
		"cmd_0000000101_aabbccdd", "2025-01-01T00:00:00Z", lm); err != nil {
		t.Fatalf("writeQueueEntries error: %v", err)
	}

	queueFile := filepath.Join(maestroDir, "queue", "worker1.yaml")
	data, err := os.ReadFile(queueFile)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	if !tq.Tasks[0].RunOnIntegration {
		t.Errorf("Task.RunOnIntegration = false; Bug F regression")
	}
	if tq.Tasks[0].RunOnMain {
		t.Errorf("Task.RunOnMain should be false when only RunOnIntegration is set")
	}
	// §S0-1: RunOnIntegration は publish_conflict 解決タスク用なので repair
	// バケットに分類される必要がある (元は rollout だったが rollout 機能廃止に伴い repair へ移行)。
	if got, want := tq.Tasks[0].OperationType, model.OperationTypeRepair; got != want {
		t.Errorf("Task.OperationType = %q, want %q (RunOnIntegration must classify as repair for §S0-1 admission)", got, want)
	}
}

func TestWriteQueueEntries_UsesExplicitOperationType(t *testing.T) {
	maestroDir := testutil.SetupDirWithQueues(t, 1)
	lm := lock.NewMutexMap()

	taskID := "task_0000000102_aaaaaaaa"
	tasks := []TaskInput{
		{
			Name:               "repair-without-run-flags",
			Purpose:            "repair failed implementation",
			Content:            "fix the failing task",
			AcceptanceCriteria: "verification passes",
			BloomLevel:         3,
			OperationType:      model.OperationTypeRepair,
		},
	}
	assignments := []WorkerAssignment{
		{TaskName: "repair-without-run-flags", WorkerID: "worker1", Model: "sonnet"},
	}
	nameToID := map[string]string{"repair-without-run-flags": taskID}

	if err := writeQueueEntries(maestroDir, assignments, tasks, nameToID,
		"cmd_0000000102_aabbccdd", "2025-01-01T00:00:00Z", lm); err != nil {
		t.Fatalf("writeQueueEntries error: %v", err)
	}

	queueFile := filepath.Join(maestroDir, "queue", "worker1.yaml")
	data, err := os.ReadFile(queueFile)
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var tq model.TaskQueue
	if err := yamlv3.Unmarshal(data, &tq); err != nil {
		t.Fatalf("parse queue: %v", err)
	}
	if got, want := tq.Tasks[0].OperationType, model.OperationTypeRepair; got != want {
		t.Errorf("Task.OperationType = %q, want %q", got, want)
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
