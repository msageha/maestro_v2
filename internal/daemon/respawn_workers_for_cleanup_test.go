package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/testutil/mocks"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// TestRespawnWorkerPanesForCleanup_EvictsBeforeCleanup asserts the Phase
// B integration: every worker attached to the worktree being torn down
// has its tmux pane respawned to the project root before `git worktree
// remove` runs. Without this, claude-code's Stop hook can fire from
// inside the just-deleted worktree directory and posix_spawn '/bin/sh'
// fails with ENOENT.
//
// Setup uses a real WorktreeManager so the GetCommandState path is
// exercised end-to-end (state file → in-memory state → worker iteration);
// the executor side is stubbed via mocks.MockExecutor so the test can
// assert each worker's RespawnPaneToProjectRoot was actually called.
func TestRespawnWorkerPanesForCleanup_EvictsBeforeCleanup(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_phase_b_respawn"
	state := &model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "worktree_command_state",
		CommandID:     commandID,
		Workers: []model.WorktreeState{
			{CommandID: commandID, WorkerID: "worker1", Branch: "maestro/" + commandID + "/worker1",
				Path: filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "worker1")},
			{CommandID: commandID, WorkerID: "worker2", Branch: "maestro/" + commandID + "/worker2",
				Path: filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "worker2")},
		},
		CreatedAt: "2026-04-28T00:00:00Z",
		UpdatedAt: "2026-04-28T00:00:00Z",
	}
	stateDir := filepath.Join(projectRoot, ".maestro", "state", "worktrees")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, commandID+".yaml"), state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)
	// Override maestroDir-bound worktreeManager from the helper with one
	// rooted at the real git repo so loadState can find the file we just
	// wrote.
	qh.maestroDir = filepath.Join(projectRoot, ".maestro")
	qh.SetWorktreeManager(wm)

	exec := &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}
	qh.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return exec, nil
	})

	if err := qh.respawnWorkerPanesForCleanup(commandID); err != nil {
		t.Fatalf("respawnWorkerPanesForCleanup: %v", err)
	}
	if got, want := exec.RespawnedWorkers, []string{"worker1", "worker2"}; !equalStrings(got, want) {
		t.Errorf("RespawnedWorkers = %v, want %v", got, want)
	}
	// Each eviction must be scoped to that worker's own worktree path so a
	// pane already re-assigned to another command is left untouched
	// (stale-eviction guard, E2E 2026-06-11).
	wantScopes := []string{
		filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "worker1"),
		filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "worker2"),
	}
	if !equalStrings(exec.RespawnedScopes, wantScopes) {
		t.Errorf("RespawnedScopes = %v, want %v", exec.RespawnedScopes, wantScopes)
	}
}

// TestRespawnWorkerPanesForCleanup_FailureBlocksCleanup pins the codex-
// recommended fail-closed behaviour: if the pane respawn errors, the
// caller (stepCleanupWorktrees) must NOT proceed with `git worktree
// remove`, otherwise the very warning we're trying to suppress can
// continue firing. Surfacing the first non-nil respawn error is the
// signal that lets stepCleanupWorktrees defer this cleanup item.
func TestRespawnWorkerPanesForCleanup_FailureBlocksCleanup(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_phase_b_respawn_fail"
	state := &model.WorktreeCommandState{
		SchemaVersion: 1,
		FileType:      "worktree_command_state",
		CommandID:     commandID,
		Workers: []model.WorktreeState{
			{CommandID: commandID, WorkerID: "worker1"},
		},
		CreatedAt: "2026-04-28T00:00:00Z",
		UpdatedAt: "2026-04-28T00:00:00Z",
	}
	stateDir := filepath.Join(projectRoot, ".maestro", "state", "worktrees")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := yamlutil.AtomicWrite(filepath.Join(stateDir, commandID+".yaml"), state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)
	qh.maestroDir = filepath.Join(projectRoot, ".maestro")
	qh.SetWorktreeManager(wm)

	exec := &mocks.MockExecutor{
		Result:            agent.ExecResult{Success: true},
		RespawnReturnsErr: fmt.Errorf("simulated tmux respawn failure"),
	}
	qh.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return exec, nil
	})

	err := qh.respawnWorkerPanesForCleanup(commandID)
	if err == nil {
		t.Fatal("expected error so stepCleanupWorktrees defers `git worktree remove`")
	}
	if exec.RespawnedWorkers[0] != "worker1" {
		t.Errorf("respawn was not attempted before failure: %v", exec.RespawnedWorkers)
	}
}

// TestRespawnWorkerPanesForCleanup_MissingStateIsFailOpen pins the
// behaviour when no worktree state exists for the commandID (already
// torn down, never created, or worktree is disabled). The respawn step
// must not block cleanup — there is no live pane to evict.
func TestRespawnWorkerPanesForCleanup_MissingStateIsFailOpen(t *testing.T) {
	t.Parallel()
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	var logBuf bytes.Buffer
	qh := newPhaseBTestQueueHandler(t, &logBuf)
	qh.maestroDir = filepath.Join(projectRoot, ".maestro")
	qh.SetWorktreeManager(wm)

	exec := &mocks.MockExecutor{Result: agent.ExecResult{Success: true}}
	qh.execProvider.SetFactory(func(string, model.WatcherConfig, string) (AgentExecutor, error) {
		return exec, nil
	})

	if err := qh.respawnWorkerPanesForCleanup("cmd_no_state"); err != nil {
		t.Fatalf("missing state should be fail-open, got: %v", err)
	}
	if len(exec.RespawnedWorkers) != 0 {
		t.Errorf("expected no respawn for missing state, got: %v", exec.RespawnedWorkers)
	}
	_ = context.Background()
	_ = log.New
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
