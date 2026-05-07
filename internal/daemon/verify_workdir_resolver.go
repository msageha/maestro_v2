package daemon

import (
	"fmt"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/model"
)

// worktreeVerifyWorkdirResolver resolves the §S1-1 Verification working
// directory for a task. It honours RunOnMain / RunOnIntegration explicit
// targets, and otherwise pins worker tasks to the project root.
//
// Why not the worker worktree for normal tasks: a `git worktree add`
// linked worktree shares the .git store with project_root but is
// otherwise a fresh checkout — `node_modules/`, `vendor/`, `.gradle/`,
// `.cache/`, target/, build/, and similar package-manager-managed
// caches that are gitignored never appear there. Running language
// tooling like `pnpm turbo` / `npm test` / `gradle test` from the
// worker worktree therefore fails with "command not found" or "module
// not found" before exercising any of the worker's diff (Report
// 2026-05-05 P0-3 — pnpm turbo on Flutter monorepo). Pinning verify
// to project_root sidesteps the dependency-isolation problem
// language-agnostically; per-task diff visibility is the worker's own
// responsibility (skill-driven self-verification before result write).
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
// Returns the project root for normal worker tasks and for RunOnMain;
// the integration worktree for RunOnIntegration.
func (r *worktreeVerifyWorkdirResolver) ResolveVerifyWorkdir(task *model.Task, _ string) (string, error) {
	if task == nil {
		return "", fmt.Errorf("task is nil")
	}
	if task.RunOnIntegration {
		if r.wm == nil {
			return "", fmt.Errorf("RunOnIntegration task requires worktree manager")
		}
		return r.wm.GetIntegrationPath(task.CommandID)
	}
	// RunOnMain and normal worker tasks both verify against the project
	// root. The worker's diff lives in its worktree and is not visible
	// here on purpose — see the type comment.
	return filepath.Dir(r.maestroDir), nil
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
