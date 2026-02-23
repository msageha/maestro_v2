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

type submitParams struct {
	CommandID string `json:"command_id"`
	TasksFile string `json:"tasks_file"`
	PhaseName string `json:"phase_name"`
	DryRun    bool   `json:"dry_run"`
}

func (pe *PlanExecutorImpl) Submit(params json.RawMessage) (json.RawMessage, error) {
	var p submitParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse submit params: %w", err)
	}

	result, err := plan.Submit(plan.SubmitOptions{
		CommandID:  p.CommandID,
		TasksFile:  p.TasksFile,
		PhaseName:  p.PhaseName,
		DryRun:     p.DryRun,
		MaestroDir: pe.MaestroDir,
		Config:     pe.Config,
		LockMap:    pe.LockMap,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

type completeParams struct {
	CommandID string `json:"command_id"`
	Summary   string `json:"summary"`
}

func (pe *PlanExecutorImpl) Complete(params json.RawMessage) (json.RawMessage, error) {
	var p completeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse complete params: %w", err)
	}

	result, err := plan.Complete(plan.CompleteOptions{
		CommandID:  p.CommandID,
		Summary:    p.Summary,
		MaestroDir: pe.MaestroDir,
		Config:     pe.Config,
		LockMap:    pe.LockMap,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
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
}

func (pe *PlanExecutorImpl) AddRetryTask(params json.RawMessage) (json.RawMessage, error) {
	var p retryParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse retry params: %w", err)
	}

	result, err := plan.AddRetryTask(plan.RetryOptions{
		CommandID:          p.CommandID,
		RetryOf:            p.RetryOf,
		Purpose:            p.Purpose,
		Content:            p.Content,
		AcceptanceCriteria: p.AcceptanceCriteria,
		Constraints:        p.Constraints,
		BlockedBy:          p.BlockedBy,
		BloomLevel:         p.BloomLevel,
		ToolsHint:          p.ToolsHint,
		MaestroDir:         pe.MaestroDir,
		Config:             pe.Config,
		LockMap:            pe.LockMap,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

type rebuildParams struct {
	CommandID string `json:"command_id"`
}

func (pe *PlanExecutorImpl) Rebuild(params json.RawMessage) (json.RawMessage, error) {
	var p rebuildParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("parse rebuild params: %w", err)
	}

	err := plan.Rebuild(plan.RebuildOptions{
		CommandID:  p.CommandID,
		MaestroDir: pe.MaestroDir,
		LockMap:    pe.LockMap,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"command_id": p.CommandID, "status": "rebuilt"})
}
