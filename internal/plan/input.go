package plan

// SubmitInput represents the top-level input for plan submission, containing either tasks or phases.
type SubmitInput struct {
	Tasks  []TaskInput  `yaml:"tasks"`
	Phases []PhaseInput `yaml:"phases"`
}

// TaskInput represents a single task definition provided by the planner agent.
type TaskInput struct {
	Name               string   `yaml:"name"`
	Purpose            string   `yaml:"purpose"`
	Content            string   `yaml:"content"`
	AcceptanceCriteria string   `yaml:"acceptance_criteria"`
	Constraints        []string `yaml:"constraints"`
	BlockedBy          []string `yaml:"blocked_by"`
	BloomLevel         int      `yaml:"bloom_level"`
	Required           bool     `yaml:"required"`
	ToolsHint          []string `yaml:"tools_hint"`
	PersonaHint        string   `yaml:"persona_hint"`
	SkillRefs          []string `yaml:"skill_refs"`
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
