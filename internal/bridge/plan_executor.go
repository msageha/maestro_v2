// Package bridge provides integration between plan execution and the daemon.
package bridge

import (
	"encoding/json"
	"fmt"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
)

// PlanExecutorImpl implements daemon.PlanExecutor by delegating to plan package functions.
type PlanExecutorImpl struct {
	MaestroDir string
	Config     model.Config
	LockMap    *lock.MutexMap
}

// parseAndExecute is a generic helper that unmarshals JSON params, executes a
// function, and marshals the result. It eliminates the repeated
// unmarshal→execute→marshal boilerplate across all PlanExecutorImpl methods.
// The op parameter names the operation for error context (e.g. "submit", "complete").
func parseAndExecute[P any, R any](op string, data []byte, execute func(P) (R, error)) ([]byte, error) {
	var params P
	if err := json.Unmarshal(data, &params); err != nil {
		return nil, fmt.Errorf("%s: parse params: %w", op, err)
	}
	result, err := execute(params)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return json.Marshal(result)
}

type submitParams struct {
	CommandID string `json:"command_id"`
	TasksFile string `json:"tasks_file"`
	PhaseName string `json:"phase_name"`
	DryRun    bool   `json:"dry_run"`
}

func (pe *PlanExecutorImpl) Submit(params json.RawMessage) (json.RawMessage, error) {
	return parseAndExecute("submit", params, func(p submitParams) (*plan.SubmitResult, error) {
		return plan.Submit(plan.SubmitOptions{
			CommandID:  p.CommandID,
			TasksFile:  p.TasksFile,
			PhaseName:  p.PhaseName,
			DryRun:     p.DryRun,
			MaestroDir: pe.MaestroDir,
			Config:     pe.Config,
			LockMap:    pe.LockMap,
		})
	})
}

type completeParams struct {
	CommandID string `json:"command_id"`
	Summary   string `json:"summary"`
}

func (pe *PlanExecutorImpl) Complete(params json.RawMessage) (json.RawMessage, error) {
	return parseAndExecute("complete", params, func(p completeParams) (*plan.CompleteResult, error) {
		return plan.Complete(plan.CompleteOptions{
			CommandID:  p.CommandID,
			Summary:    p.Summary,
			MaestroDir: pe.MaestroDir,
			Config:     pe.Config,
			LockMap:    pe.LockMap,
		})
	})
}

type retryParams struct {
	CommandID          string   `json:"command_id"`
	RetryOf            string   `json:"retry_of"`
	Purpose            string   `json:"purpose"`
	Content            string   `json:"content"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	Constraints        []string `json:"constraints"`
	BlockedBy          []string `json:"blocked_by"`
	BloomLevel         int      `json:"bloom_level"`
	ToolsHint          []string `json:"tools_hint"`
	PersonaHint        string   `json:"persona_hint"`
	SkillRefs          []string `json:"skill_refs"`
}

func (pe *PlanExecutorImpl) AddRetryTask(params json.RawMessage) (json.RawMessage, error) {
	return parseAndExecute("add_retry_task", params, func(p retryParams) (*plan.RetryResult, error) {
		return plan.AddRetryTask(plan.RetryOptions{
			CommandID:          p.CommandID,
			RetryOf:            p.RetryOf,
			Purpose:            p.Purpose,
			Content:            p.Content,
			AcceptanceCriteria: p.AcceptanceCriteria,
			Constraints:        p.Constraints,
			BlockedBy:          p.BlockedBy,
			BloomLevel:         p.BloomLevel,
			ToolsHint:          p.ToolsHint,
			PersonaHint:        p.PersonaHint,
			SkillRefs:          p.SkillRefs,
			MaestroDir:         pe.MaestroDir,
			Config:             pe.Config,
			LockMap:            pe.LockMap,
		})
	})
}

type rebuildParams struct {
	CommandID string `json:"command_id"`
}

type rebuildResult struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`
}

func (pe *PlanExecutorImpl) Rebuild(params json.RawMessage) (json.RawMessage, error) {
	return parseAndExecute("rebuild", params, func(p rebuildParams) (*rebuildResult, error) {
		err := plan.Rebuild(plan.RebuildOptions{
			CommandID:  p.CommandID,
			MaestroDir: pe.MaestroDir,
			LockMap:    pe.LockMap,
		})
		if err != nil {
			return nil, err
		}
		return &rebuildResult{CommandID: p.CommandID, Status: "rebuilt"}, nil
	})
}
