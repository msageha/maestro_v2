package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/msageha/maestro_v2/internal/testutil"
)

// TestConcurrentEnsureWorkerWorktree verifies that multiple goroutines can
// call EnsureWorkerWorktree concurrently without data races or corruption.
func TestConcurrentEnsureWorkerWorktree(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	const numWorkers = 8
	commandID := "cmd_conc_ensure"

	var wg sync.WaitGroup
	errs := make([]error, numWorkers)

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			workerID := fmt.Sprintf("worker%d", idx)
			errs[idx] = wm.EnsureWorkerWorktree(commandID, workerID)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("EnsureWorkerWorktree(worker%d) failed: %v", i, err)
		}
	}

	// Verify all workers are present in state
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if len(state.Workers) != numWorkers {
		t.Errorf("len(state.Workers) = %d, want %d", len(state.Workers), numWorkers)
	}

	// Verify all worktree directories exist
	for i := 0; i < numWorkers; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		wtPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, workerID)
		if _, err := os.Stat(wtPath); os.IsNotExist(err) {
			t.Errorf("worktree directory missing for %s at %s", workerID, wtPath)
		}
	}
}

// TestConcurrentEnsureAndCleanup verifies that EnsureWorkerWorktree and
// CleanupCommand can run concurrently on different commands without races.
func TestConcurrentEnsureAndCleanup(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Pre-create a command that will be cleaned up
	cleanupCmdID := "cmd_conc_cleanup"
	if err := createForCommand(wm, cleanupCmdID, []string{"w1", "w2"}); err != nil {
		t.Fatalf("setup createForCommand failed: %v", err)
	}

	ensureCmdID := "cmd_conc_ensure2"

	var wg sync.WaitGroup
	var ensureErr, cleanupErr error

	// Goroutine 1: ensure new worktrees on a different command
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 4; i++ {
			if err := wm.EnsureWorkerWorktree(ensureCmdID, fmt.Sprintf("worker%d", i)); err != nil {
				ensureErr = fmt.Errorf("EnsureWorkerWorktree(worker%d): %w", i, err)
				return
			}
		}
	}()

	// Goroutine 2: cleanup the pre-created command
	wg.Add(1)
	go func() {
		defer wg.Done()
		cleanupErr = wm.CleanupCommand(cleanupCmdID)
	}()

	wg.Wait()

	if ensureErr != nil {
		t.Errorf("ensure goroutine failed: %v", ensureErr)
	}
	if cleanupErr != nil {
		t.Errorf("cleanup goroutine failed: %v", cleanupErr)
	}

	// Verify: new command's worktrees should exist
	state, err := wm.GetCommandState(ensureCmdID)
	if err != nil {
		t.Fatalf("GetCommandState(%s) failed: %v", ensureCmdID, err)
	}
	if len(state.Workers) != 4 {
		t.Errorf("len(state.Workers) = %d, want 4", len(state.Workers))
	}

	// Verify: cleaned-up command should not have state
	if wm.HasWorktrees(cleanupCmdID) {
		t.Error("HasWorktrees should return false after cleanup")
	}
}

// TestConcurrentSameWorkerEnsure verifies that calling EnsureWorkerWorktree
// for the same worker ID from multiple goroutines is idempotent and safe.
func TestConcurrentSameWorkerEnsure(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_conc_same"
	const goroutines = 10

	var wg sync.WaitGroup
	errs := make([]error, goroutines)

	// All goroutines try to ensure the same worker
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = wm.EnsureWorkerWorktree(commandID, "worker1")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d failed: %v", i, err)
		}
	}

	// Verify exactly one worker entry (no duplicates)
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if len(state.Workers) != 1 {
		t.Errorf("len(state.Workers) = %d, want 1 (no duplicates)", len(state.Workers))
	}
}

// TestConcurrentMergeOperations verifies that MergeToIntegration and
// other state-mutating operations can run concurrently on different commands.
func TestConcurrentMergeOperations(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// Set up two independent commands with committed worker changes
	cmds := []string{"cmd_conc_merge1", "cmd_conc_merge2"}
	for _, cmdID := range cmds {
		if err := createForCommand(wm, cmdID, []string{"w1"}); err != nil {
			t.Fatalf("createForCommand(%s) failed: %v", cmdID, err)
		}
		// Create a file in the worker worktree and commit
		wtPath := filepath.Join(projectRoot, ".maestro", "worktrees", cmdID, "w1")
		filePath := filepath.Join(wtPath, fmt.Sprintf("file_%s.txt", cmdID))
		if err := os.WriteFile(filePath, []byte("content for "+cmdID+"\n"), 0644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
		if err := wm.CommitWorkerChanges(cmdID, "w1", "[test] "+cmdID); err != nil {
			t.Fatalf("CommitWorkerChanges(%s) failed: %v", cmdID, err)
		}
	}

	mergeErrs := make([]error, len(cmds))
	mergeConflicts := make([][]interface{}, len(cmds))

	if raceEnabled {
		// Under -race, Git command overhead makes concurrent execution
		// extremely slow (10min+ timeout). Run sequentially; the race
		// detector still instruments the shared-state code paths.
		for i, cmdID := range cmds {
			conflicts, err := wm.MergeToIntegration(context.Background(), cmdID, []string{"w1"}, nil)
			mergeErrs[i] = err
			if len(conflicts) > 0 {
				mergeConflicts[i] = make([]interface{}, len(conflicts))
			}
		}
	} else {
		var wg sync.WaitGroup
		for i, cmdID := range cmds {
			wg.Add(1)
			go func(idx int, cid string) {
				defer wg.Done()
				conflicts, err := wm.MergeToIntegration(context.Background(), cid, []string{"w1"}, nil)
				mergeErrs[idx] = err
				if len(conflicts) > 0 {
					mergeConflicts[idx] = make([]interface{}, len(conflicts))
				}
			}(i, cmdID)
		}
		wg.Wait()
	}

	for i, err := range mergeErrs {
		if err != nil {
			t.Errorf("MergeToIntegration(%s) failed: %v", cmds[i], err)
		}
	}
	for i, c := range mergeConflicts {
		if len(c) > 0 {
			t.Errorf("MergeToIntegration(%s) had unexpected conflicts", cmds[i])
		}
	}

	// Verify both commands have merged status
	for _, cmdID := range cmds {
		state, err := wm.GetCommandState(cmdID)
		if err != nil {
			t.Fatalf("GetCommandState(%s) failed: %v", cmdID, err)
		}
		if state.Integration.Status != "merged" {
			t.Errorf("command %s integration status = %q, want %q",
				cmdID, state.Integration.Status, "merged")
		}
	}
}

// TestConcurrentReadWriteOperations verifies that concurrent reads
// (GetWorkerPath, GetCommandState, HasWorktrees) and writes
// (EnsureWorkerWorktree) do not race.
func TestConcurrentReadWriteOperations(t *testing.T) {
	t.Parallel()
	projectRoot := testutil.InitTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_conc_rw"
	// Bootstrap with one worker so reads have something to find
	if err := wm.EnsureWorkerWorktree(commandID, "worker0"); err != nil {
		t.Fatalf("initial EnsureWorkerWorktree failed: %v", err)
	}

	var wg sync.WaitGroup

	// Writer goroutines: add more workers
	for i := 1; i <= 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = wm.EnsureWorkerWorktree(commandID, fmt.Sprintf("worker%d", idx))
		}(i)
	}

	// Reader goroutines: concurrent reads
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = wm.GetWorkerPath(commandID, "worker0")
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = wm.GetCommandState(commandID)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = wm.HasWorktrees(commandID)
		}()
	}

	wg.Wait()

	// Verify final state consistency
	state, err := wm.GetCommandState(commandID)
	if err != nil {
		t.Fatalf("GetCommandState failed: %v", err)
	}
	if len(state.Workers) != 5 {
		t.Errorf("len(state.Workers) = %d, want 5", len(state.Workers))
	}
}
