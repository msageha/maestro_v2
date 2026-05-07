package daemon

import (
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// TestProjectRootVerifyWorkdirResolver_ReturnsProjectRoot は worktree 無効
// モードで verify が project root (=.maestro の親ディレクトリ) を返すことを
// 検証する。worktree 無効環境では worker も main も同じ checkout を使う
// ため、verify も同じディレクトリで走る。
func TestProjectRootVerifyWorkdirResolver_ReturnsProjectRoot(t *testing.T) {
	maestroDir := "/tmp/test-project/.maestro"
	r := newProjectRootVerifyWorkdirResolver(maestroDir)

	dir, err := r.ResolveVerifyWorkdir(&model.Task{ID: "t1"}, "worker1")
	if err != nil {
		t.Fatalf("ResolveVerifyWorkdir: %v", err)
	}
	if dir != "/tmp/test-project" {
		t.Errorf("dir = %q, want /tmp/test-project", dir)
	}

	// RunOnMain も RunOnIntegration もない通常タスクと同じ動作
	dir, err = r.ResolveVerifyWorkdir(&model.Task{ID: "t1", RunOnMain: true}, "worker1")
	if err != nil {
		t.Fatalf("ResolveVerifyWorkdir RunOnMain: %v", err)
	}
	if dir != "/tmp/test-project" {
		t.Errorf("RunOnMain dir = %q, want /tmp/test-project", dir)
	}
}

// TestWorktreeVerifyWorkdirResolver_RunOnMain は RunOnMain=true のタスクが
// project root を返すことを検証する。最終 verify などで worker worktree
// ではなく merged main 側を verify する場合の経路。
func TestWorktreeVerifyWorkdirResolver_RunOnMain(t *testing.T) {
	maestroDir := "/tmp/test-project/.maestro"
	// wm が nil でも RunOnMain は project root を返す (wm 不要パス)
	r := newWorktreeVerifyWorkdirResolver(nil, maestroDir)

	dir, err := r.ResolveVerifyWorkdir(&model.Task{ID: "t1", RunOnMain: true}, "worker1")
	if err != nil {
		t.Fatalf("ResolveVerifyWorkdir: %v", err)
	}
	if dir != "/tmp/test-project" {
		t.Errorf("dir = %q, want /tmp/test-project", dir)
	}
}

// TestWorktreeVerifyWorkdirResolver_NilTask は nil task でエラーを返す
// ことを検証する。caller の bug を早期に発見するためのガード。
func TestWorktreeVerifyWorkdirResolver_NilTask(t *testing.T) {
	r := newWorktreeVerifyWorkdirResolver(nil, "/tmp/.maestro")
	if _, err := r.ResolveVerifyWorkdir(nil, "worker1"); err == nil {
		t.Error("expected error for nil task")
	}
}

// TestWorktreeVerifyWorkdirResolver_RunOnIntegrationRequiresManager は
// RunOnIntegration が WorktreeManager を必須とすることを検証する。
func TestWorktreeVerifyWorkdirResolver_RunOnIntegrationRequiresManager(t *testing.T) {
	r := newWorktreeVerifyWorkdirResolver(nil, "/tmp/.maestro")
	if _, err := r.ResolveVerifyWorkdir(&model.Task{ID: "t1", RunOnIntegration: true}, "worker1"); err == nil {
		t.Error("expected error for RunOnIntegration without manager")
	}
}

// TestWorktreeVerifyWorkdirResolver_WorkerTaskUsesProjectRoot pins the
// new contract: a normal worker task verify runs at project root, NOT
// the worker's git-worktree. The earlier "worker_worktree" path
// failed for any project that uses a package-manager-managed cache
// (`node_modules/`, `.gradle/`, …) because those caches are gitignored
// and never appear in a fresh `git worktree` checkout — the package-
// proxy regression hit `pnpm turbo` with `Command "turbo" not found`
// (Report 2026-05-05 P0-3). Project root has the dependencies
// installed; the worker's diff is invisible here on purpose, the
// worker's own skill-driven self-check is the per-task signal.
func TestWorktreeVerifyWorkdirResolver_WorkerTaskUsesProjectRoot(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	maestroDir := filepath.Join(projectRoot, ".maestro")

	r := newWorktreeVerifyWorkdirResolver(wm, maestroDir)
	got, err := r.ResolveVerifyWorkdir(&model.Task{ID: "t1", CommandID: "cmd_x"}, "worker1")
	if err != nil {
		t.Fatalf("ResolveVerifyWorkdir: %v", err)
	}
	if got != projectRoot {
		t.Errorf("normal worker task verify dir = %q, want project root %q", got, projectRoot)
	}
}
