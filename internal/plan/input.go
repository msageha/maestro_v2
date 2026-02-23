package plan

type SubmitInput struct {
	Tasks  []TaskInput  `yaml:"tasks"`
	Phases []PhaseInput `yaml:"phases"`
}

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
}

type PhaseInput struct {
	Name            string           `yaml:"name"`
	Type            string           `yaml:"type"` // "concrete" or "deferred"
	DependsOnPhases []string         `yaml:"depends_on_phases"`
	Tasks           []TaskInput      `yaml:"tasks"`
	Constraints     *ConstraintInput `yaml:"constraints"`
}

type ConstraintInput struct {
	MaxTasks           int   `yaml:"max_tasks"`
	AllowedBloomLevels []int `yaml:"allowed_bloom_levels"`
	TimeoutMinutes     int   `yaml:"timeout_minutes"`
}
