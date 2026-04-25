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

// TestWorktreeVerifyWorkdirResolver_WorkerPathDelegated は通常タスクが
// WorktreeManager.GetWorkerPath に委譲され、その path を返すことを検証。
// 実 manager を使うのは重いので、テスト用 manager fixture を使う。
func TestWorktreeVerifyWorkdirResolver_WorkerPathDelegated(t *testing.T) {
	// initTestGitRepo は git init 済みの project root を生成する。
	// EnsureWorkerWorktree が main branch SHA を rev-parse するため必要。
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)
	maestroDir := filepath.Join(projectRoot, ".maestro")

	commandID := "cmd_verify_workdir_test"
	workerID := "worker1"

	if err := wm.EnsureWorkerWorktree(commandID, workerID); err != nil {
		t.Fatalf("EnsureWorkerWorktree: %v", err)
	}
	want, err := wm.GetWorkerPath(commandID, workerID)
	if err != nil {
		t.Fatalf("GetWorkerPath: %v", err)
	}

	r := newWorktreeVerifyWorkdirResolver(wm, maestroDir)
	got, err := r.ResolveVerifyWorkdir(&model.Task{ID: "t1", CommandID: commandID}, workerID)
	if err != nil {
		t.Fatalf("ResolveVerifyWorkdir: %v", err)
	}
	if got != want {
		t.Errorf("worker dir = %q, want %q (must match wm.GetWorkerPath so verify sees worker uncommitted changes)", got, want)
	}
}
