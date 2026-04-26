// Package bridge provides integration between plan execution and the daemon.
package bridge

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/plan"
)

// PlanExecutorImpl implements daemon.PlanExecutor by delegating to plan package functions.
type PlanExecutorImpl struct {
	MaestroDir string
	Config     model.Config
	LockMap    *lock.MutexMap

	// ModelSelector is an optional adaptive model selection hook. When set,
	// plan operations thread it into AssignWorkers so that the selector's
	// pick may override the static BloomLevel→model mapping. Access guarded
	// by selectorMu to allow late binding from the daemon after startup.
	selectorMu sync.RWMutex
	selector   plan.ModelSelector
}

// SetModelSelector installs (or clears, with nil) the adaptive model selector.
// Safe to call concurrently with in-flight plan operations; subsequent calls
// take effect on the next Submit / AddRetryTask / AddTask invocation.
//
// The daemon invokes this via the core.PlanExecutorModelSelectorSettable
// interface, handing us a core.ModelSelector. Since core.ModelSelector and
// plan.ModelSelector share the same method set, any concrete impl of one
// satisfies the other; we assert to avoid a wrapper allocation on the hot
// path.
func (pe *PlanExecutorImpl) SetModelSelector(s core.ModelSelector) {
	pe.selectorMu.Lock()
	defer pe.selectorMu.Unlock()
	if s == nil {
		pe.selector = nil
		return
	}
	if p, ok := s.(plan.ModelSelector); ok {
		pe.selector = p
		return
	}
	// Concrete type implemented core.ModelSelector but, oddly, not
	// plan.ModelSelector. Adapt via a thin shim so wiring still works.
	pe.selector = coreSelectorAdapter{s}
}

// coreSelectorAdapter bridges a core.ModelSelector into plan.ModelSelector
// when the direct interface-to-interface assertion fails (e.g., pointer vs
// value receiver mismatch in an unusual impl). It is allocation-free per
// call since it simply forwards to the embedded selector.
type coreSelectorAdapter struct{ core.ModelSelector }

func (c coreSelectorAdapter) SelectModel(bloomLevel int, taskName string) string {
	return c.ModelSelector.SelectModel(bloomLevel, taskName)
}

// getSelector returns the currently-installed selector (may be nil).
func (pe *PlanExecutorImpl) getSelector() plan.ModelSelector {
	pe.selectorMu.RLock()
	defer pe.selectorMu.RUnlock()
	return pe.selector
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
	out, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", op, ErrMarshalPlanResult, err)
	}
	return out, nil
}

type submitParams struct {
	CommandID string `json:"command_id"`
	TasksFile string `json:"tasks_file"`
	TasksData string `json:"tasks_data,omitempty"`
	PhaseName string `json:"phase_name"`
	DryRun    bool   `json:"dry_run"`
}

// Submit implements core.PlanExecutor by parsing params and calling plan.Submit.
func (pe *PlanExecutorImpl) Submit(params json.RawMessage) (json.RawMessage, error) {
	return parseAndExecute("submit", params, func(p submitParams) (*plan.SubmitResult, error) {
		return plan.Submit(plan.SubmitOptions{
			CommandID:             p.CommandID,
			TasksFile:             p.TasksFile,
			TasksData:             []byte(p.TasksData),
			PhaseName:             p.PhaseName,
			DryRun:                p.DryRun,
			MaestroDir:            pe.MaestroDir,
			Config:                pe.Config,
			LockMap:               pe.LockMap,
			ModelSelector:         pe.getSelector(),
			RequireVerifySnapshot: true,
		})
	})
}

type completeParams struct {
	CommandID string `json:"command_id"`
	Summary   string `json:"summary"`
}

// Complete implements core.PlanExecutor by parsing params and calling plan.Complete.
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
	CommandID          string                   `json:"command_id"`
	RetryOf            string                   `json:"retry_of"`
	Purpose            string                   `json:"purpose"`
	Content            string                   `json:"content"`
	AcceptanceCriteria string                   `json:"acceptance_criteria"`
	DefinitionOfDone   []string                 `json:"definition_of_done,omitempty"`
	Constraints        []string                 `json:"constraints"`
	BlockedBy          []string                 `json:"blocked_by"`
	BloomLevel         int                      `json:"bloom_level"`
	ToolsHint          []string                 `json:"tools_hint"`
	PersonaHint        string                   `json:"persona_hint"`
	SkillRefs          []string                 `json:"skill_refs"`
	ExpectedPaths      []string                 `json:"expected_paths"`
	DefinitionOfAbort  *model.DefinitionOfAbort `json:"definition_of_abort"`
}

// AddRetryTask implements core.PlanExecutor by parsing params and calling plan.AddRetryTask.
func (pe *PlanExecutorImpl) AddRetryTask(params json.RawMessage) (json.RawMessage, error) {
	return parseAndExecute("add_retry_task", params, func(p retryParams) (*plan.RetryResult, error) {
		return plan.AddRetryTask(plan.RetryOptions{
			CommandID:          p.CommandID,
			RetryOf:            p.RetryOf,
			Purpose:            p.Purpose,
			Content:            p.Content,
			AcceptanceCriteria: p.AcceptanceCriteria,
			DefinitionOfDone:   p.DefinitionOfDone,
			Constraints:        p.Constraints,
			BlockedBy:          p.BlockedBy,
			BloomLevel:         p.BloomLevel,
			ToolsHint:          p.ToolsHint,
			PersonaHint:        p.PersonaHint,
			SkillRefs:          p.SkillRefs,
			ExpectedPaths:      p.ExpectedPaths,
			DefinitionOfAbort:  p.DefinitionOfAbort,
			MaestroDir:         pe.MaestroDir,
			Config:             pe.Config,
			LockMap:            pe.LockMap,
			ModelSelector:      pe.getSelector(),
		})
	})
}

type injectParams struct {
	CommandID          string                   `json:"command_id"`
	Purpose            string                   `json:"purpose"`
	Content            string                   `json:"content"`
	AcceptanceCriteria string                   `json:"acceptance_criteria"`
	DefinitionOfDone   []string                 `json:"definition_of_done,omitempty"`
	Constraints        []string                 `json:"constraints"`
	BlockedBy          []string                 `json:"blocked_by"`
	BloomLevel         int                      `json:"bloom_level"`
	Required           bool                     `json:"required"`
	ToolsHint          []string                 `json:"tools_hint"`
	PersonaHint        string                   `json:"persona_hint"`
	SkillRefs          []string                 `json:"skill_refs"`
	ExpectedPaths      []string                 `json:"expected_paths"`
	DefinitionOfAbort  *model.DefinitionOfAbort `json:"definition_of_abort"`
	WorkerID           string                   `json:"worker_id"`
	TargetPhase        string                   `json:"target_phase,omitempty"`
	IdempotencyKey     string                   `json:"idempotency_key,omitempty"`
	RunOnMain          bool                     `json:"run_on_main,omitempty"`
	RunOnIntegration   bool                     `json:"run_on_integration,omitempty"`
}

// AddTask implements core.PlanExecutor by parsing params and calling plan.AddTask.
func (pe *PlanExecutorImpl) AddTask(params json.RawMessage) (json.RawMessage, error) {
	return parseAndExecute("add_task", params, func(p injectParams) (*plan.InjectResult, error) {
		return plan.AddTask(plan.InjectOptions{
			CommandID:          p.CommandID,
			Purpose:            p.Purpose,
			Content:            p.Content,
			AcceptanceCriteria: p.AcceptanceCriteria,
			DefinitionOfDone:   p.DefinitionOfDone,
			Constraints:        p.Constraints,
			BlockedBy:          p.BlockedBy,
			BloomLevel:         p.BloomLevel,
			Required:           p.Required,
			ToolsHint:          p.ToolsHint,
			PersonaHint:        p.PersonaHint,
			SkillRefs:          p.SkillRefs,
			ExpectedPaths:      p.ExpectedPaths,
			DefinitionOfAbort:  p.DefinitionOfAbort,
			TargetWorkerID:     p.WorkerID,
			TargetPhase:        p.TargetPhase,
			IdempotencyKey:     p.IdempotencyKey,
			RunOnMain:          p.RunOnMain,
			RunOnIntegration:   p.RunOnIntegration,
			MaestroDir:         pe.MaestroDir,
			Config:             pe.Config,
			LockMap:            pe.LockMap,
			ModelSelector:      pe.getSelector(),
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

// Rebuild implements core.PlanExecutor by parsing params and calling plan.Rebuild.
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
