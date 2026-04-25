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
	// GetIntegrationPath returns the filesystem path of the integration worktree
	// for the given command. Used by the dispatcher to resolve the working
	// directory for RunOnIntegration tasks (publish_conflict resolution).
	GetIntegrationPath(commandID string) (string, error)
	// EnsureIntegrationBranchCheckedOut verifies the integration worktree
	// still has the expected integration branch checked out before a
	// RunOnIntegration task runs. Detached or dirty worktrees are refused
	// rather than silently dispatched onto (see RCA in worktree.Manager).
	EnsureIntegrationBranchCheckedOut(commandID string) error
}

// ExecutorGetter provides lazy executor access.
// Satisfied by daemon.ExecutorProvider.
type ExecutorGetter interface {
	GetExecutor() (core.AgentExecutor, error)
}
