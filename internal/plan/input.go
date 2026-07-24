package plan

import "github.com/msageha/maestro_v2/internal/model"

// SubmitInput represents the top-level input for plan submission, containing either tasks or phases.
type SubmitInput struct {
	Tasks  []TaskInput  `yaml:"tasks"`
	Phases []PhaseInput `yaml:"phases"`
}

// TaskInput represents a single task definition provided by the planner agent.
type TaskInput struct {
	Name               string                   `yaml:"name"`
	Purpose            string                   `yaml:"purpose"`
	Content            string                   `yaml:"content"`
	AcceptanceCriteria string                   `yaml:"acceptance_criteria"`
	Constraints        []string                 `yaml:"constraints"`
	BlockedBy          []string                 `yaml:"blocked_by"`
	BloomLevel         int                      `yaml:"bloom_level"`
	Required           bool                     `yaml:"required"`
	ToolsHint          []string                 `yaml:"tools_hint"`
	PersonaHint        string                   `yaml:"persona_hint"`
	SkillRefs          []string                 `yaml:"skill_refs"`
	ExpectedPaths      []string                 `yaml:"expected_paths,omitempty"`
	DefinitionOfAbort  *model.DefinitionOfAbort `yaml:"definition_of_abort,omitempty"`
	DefinitionOfDone   []string                 `yaml:"definition_of_done,omitempty"`
	OperationType      string                   `yaml:"operation_type,omitempty"`

	// WorkerID, when set, pins this task to the named worker (e.g.
	// "worker1") instead of relying on bloom-derived auto-assignment.
	// Mirrors the `plan add-task --worker-id` flag so a single Planner
	// can express the same lane intent on a fresh submit. Validated at
	// submit time against agents.workers.count; an unknown worker
	// surfaces as a VALIDATION_ERROR.
	WorkerID string `yaml:"worker_id,omitempty"`

	// RequiredCapabilities restricts auto-assignment to workers advertising
	// every listed capability tag (agents.workers.capabilities, or the
	// runtime defaults — see model.DefaultCapabilitiesForRuntime). Hard
	// filter: when no configured worker satisfies the set, submit fails with
	// an actionable ErrNoAvailableWorker instead of silently widening the
	// pool. Ignored when WorkerID pins the task (the pin wins, with a WARN
	// on mismatch).
	RequiredCapabilities []string `yaml:"required_capabilities,omitempty"`
	// PreferredCapabilities biases auto-assignment toward workers advertising
	// more of the listed tags. Soft only: assignment still succeeds when no
	// worker matches any tag.
	PreferredCapabilities []string `yaml:"preferred_capabilities,omitempty"`

	// RunOnMain instructs the dispatcher to run this task in the main working
	// directory instead of the worker's worktree. Use for read-only verification
	// tasks that must evaluate the merged state on the main branch.
	// Mutually exclusive with RunOnIntegration.
	RunOnMain bool `yaml:"run_on_main,omitempty"`
	// RunOnIntegration instructs the dispatcher to run this task in the
	// integration worktree for the associated command. Use for publish_conflict
	// resolution tasks that must operate directly on the integration branch.
	// Mutually exclusive with RunOnMain.
	RunOnIntegration bool `yaml:"run_on_integration,omitempty"`

	// ResumeHint optionally overrides the resume-eligibility policy for this
	// task ("allow" / "deny" / empty = default). See model.Task.ResumeHint
	// for the policy semantics (issue #55).
	ResumeHint string `yaml:"resume_hint,omitempty"`
}

// PhaseInput represents a phase definition containing grouped tasks with ordering constraints.
type PhaseInput struct {
	Name            string           `yaml:"name"`
	Type            string           `yaml:"type"` // "concrete" or "deferred"
	DependsOnPhases []string         `yaml:"depends_on_phases"`
	Tasks           []TaskInput      `yaml:"tasks"`
	Constraints     *ConstraintInput `yaml:"constraints"`
}

// ConstraintInput defines resource and validation constraints for a deferred phase.
type ConstraintInput struct {
	MaxTasks           int   `yaml:"max_tasks"`
	AllowedBloomLevels []int `yaml:"allowed_bloom_levels"`
	TimeoutMinutes     int   `yaml:"timeout_minutes"`
}
