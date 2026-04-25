package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/model"
)

// worktreeVerifyWorkdirResolver resolves the §S1-1 Verification working
// directory for a task by consulting the worktree manager. The selection
// follows the same RunOnMain / RunOnIntegration / worker rules as
// dispatch.resolveTaskWorkingDir so verify executes in the same environment
// as the worker that produced the result.
type worktreeVerifyWorkdirResolver struct {
	wm         *WorktreeManager
	maestroDir string
}

// newWorktreeVerifyWorkdirResolver constructs the resolver used by daemons
// that have worktree isolation enabled.
func newWorktreeVerifyWorkdirResolver(wm *WorktreeManager, maestroDir string) *worktreeVerifyWorkdirResolver {
	return &worktreeVerifyWorkdirResolver{wm: wm, maestroDir: maestroDir}
}

// ResolveVerifyWorkdir returns the working directory for verification.
// Returns the project root for RunOnMain tasks, the integration worktree
// for RunOnIntegration, and the worker worktree otherwise.
func (r *worktreeVerifyWorkdirResolver) ResolveVerifyWorkdir(task *model.Task, workerID string) (string, error) {
	if task == nil {
		return "", fmt.Errorf("task is nil")
	}
	if task.RunOnMain {
		return filepath.Dir(r.maestroDir), nil
	}
	if task.RunOnIntegration {
		if r.wm == nil {
			return "", fmt.Errorf("RunOnIntegration task requires worktree manager")
		}
		return r.wm.GetIntegrationPath(task.CommandID)
	}
	if r.wm == nil {
		// Defensive: worktree mode disabled but resolver was wired anyway.
		return filepath.Dir(r.maestroDir), nil
	}
	return r.wm.GetWorkerPath(task.CommandID, workerID)
}

// projectRootVerifyWorkdirResolver always returns the project root. Used
// when worktree isolation is disabled — every task (including verify) runs
// against the same checkout, so the resolver is a constant function.
type projectRootVerifyWorkdirResolver struct {
	projectRoot string
}

// newProjectRootVerifyWorkdirResolver constructs a resolver that pins verify
// to the project root (parent of .maestro). Used for worktree-disabled
// daemons.
func newProjectRootVerifyWorkdirResolver(maestroDir string) *projectRootVerifyWorkdirResolver {
	return &projectRootVerifyWorkdirResolver{projectRoot: filepath.Dir(maestroDir)}
}

// ResolveVerifyWorkdir always returns the project root.
func (r *projectRootVerifyWorkdirResolver) ResolveVerifyWorkdir(_ *model.Task, _ string) (string, error) {
	return r.projectRoot, nil
}
