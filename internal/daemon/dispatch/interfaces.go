// Package dispatch handles priority sorting, agent executor dispatch,
// quality gate evaluation, and content enrichment for task/command delivery.
package dispatch

import (
	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/quality"
)

// GateChecker abstracts the quality gate evaluation capability.
// Satisfied by daemon.QualityGateDaemon.
type GateChecker interface {
	EvaluateGateWithResult(gateType string, evalContext map[string]interface{}) (*quality.EvaluationResult, error)
}

// PreTaskGateEvaluator encapsulates pre-task quality gate evaluation logic.
type PreTaskGateEvaluator interface {
	ShouldEvaluate() bool
	EvaluatePreTask(task *model.Task, workerID string) (*model.QualityGateEvaluation, error)
	StoreEvaluation(taskID string, evaluation *model.QualityGateEvaluation)
	SkippedEvaluation(reason string) *model.QualityGateEvaluation
}

// WorktreeResolver abstracts worktree path operations needed for dispatch.
// Satisfied by worktree.Manager.
type WorktreeResolver interface {
	GetWorkerPath(commandID, workerID string) (string, error)
	EnsureWorkerWorktree(commandID, workerID string) error
	// EnsureCandidateWorktree lazily creates the candidate-exclusive
	// worktree + branch for an A/B candidate task (task.ABGroupID != "").
	// Candidates never run inside worker worktrees — their work must stay
	// off the worker branch until selection picks a winner.
	EnsureCandidateWorktree(commandID, taskID string) (path, branch string, err error)
	// GetIntegrationPath returns the filesystem path of the integration worktree
	// for the given command. Used by the dispatcher to resolve the working
	// directory for RunOnIntegration tasks (publish_conflict resolution).
	GetIntegrationPath(commandID string) (string, error)
	// EnsureIntegrationBranchCheckedOut verifies the integration worktree
	// still has the expected integration branch checked out before a
	// RunOnIntegration task runs. Detached or dirty worktrees are refused
	// rather than silently dispatched onto (see RCA in worktree.Manager).
	EnsureIntegrationBranchCheckedOut(commandID string) error
	// GetIntegrationStatus returns the lifecycle status of the integration
	// branch for commandID. Implementations return an error wrapping
	// os.ErrNotExist when the command has no worktree state file (worktree
	// mode disabled or never tracked), so callers can fall back to permissive
	// behaviour via errors.Is. Used by the run_on_main pre-flight to reject
	// verification tasks that arrive before the integration branch has been
	// published into base — running those would inspect stale main and fail
	// spuriously.
	GetIntegrationStatus(commandID string) (model.IntegrationStatus, error)
	// RefreshWorkerWorktreeToIntegrationHead fast-forwards a worker worktree
	// to the integration branch's latest HEAD when the worker is strictly
	// behind. The dispatcher calls this just before handing a worker a new
	// task so a worker reused across phases never executes against a state
	// that is missing sibling-worker commits already merged into integration.
	// Returns nil for already-current / worker-ahead worktrees, and a non-nil
	// error for dirty/diverged/git-failure cases (treated as fail-closed by
	// the caller).
	RefreshWorkerWorktreeToIntegrationHead(commandID, workerID string) error
}

// ExecutorGetter provides lazy executor access.
// Satisfied by daemon.ExecutorProvider.
type ExecutorGetter interface {
	GetExecutor() (core.AgentExecutor, error)
}

// TaskAliveChecker abstracts the queue-state probe the dispatcher's
// inline retry loop uses to short-circuit retries against tasks that have
// already finished. The retry loop holds the dispatch goroutine for up
// to retry.task_dispatch_inline_retries * (delay·2^n) seconds; without
// this probe a worker that completed (or whose lease was revoked) keeps
// receiving paste→Enter waves until the loop exhausts its budget,
// burning Claude tokens and risking a duplicate envelope.
//
// IsDispatchActive must return false the moment the task is no longer
// owned by this dispatch attempt — i.e. queue entry is terminal, the
// lease epoch has been bumped by a fencing reject, or the queue entry
// is gone. Returning true for "unknown / IO error" preserves the legacy
// retry behaviour so a transient stat failure does not cause spurious
// aborts; the next retry sweep will pick up the actual change.
type TaskAliveChecker interface {
	IsDispatchActive(workerID, taskID string, expectedLeaseEpoch int) bool
}
