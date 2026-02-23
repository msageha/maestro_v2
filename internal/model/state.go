package model

type CommandState struct {
	SchemaVersion      int                 `yaml:"schema_version"`
	FileType           string              `yaml:"file_type"`
	CommandID          string              `yaml:"command_id"`
	PlanVersion        int                 `yaml:"plan_version"`
	PlanStatus         PlanStatus          `yaml:"plan_status"`
	CompletionPolicy   CompletionPolicy    `yaml:"completion_policy"`
	Cancel             CancelState         `yaml:"cancel"`
	ExpectedTaskCount  int                 `yaml:"expected_task_count"`
	RequiredTaskIDs    []string            `yaml:"required_task_ids"`
	OptionalTaskIDs    []string            `yaml:"optional_task_ids"`
	TaskDependencies   map[string][]string `yaml:"task_dependencies"`
	TaskStates         map[string]Status   `yaml:"task_states"`
	CancelledReasons   map[string]string   `yaml:"cancelled_reasons"`
	AppliedResultIDs   map[string]string   `yaml:"applied_result_ids"`
	SystemCommitTaskID *string             `yaml:"system_commit_task_id"`
	RetryLineage       map[string]string   `yaml:"retry_lineage"`
	Phases             []Phase             `yaml:"phases"`
	LastReconciledAt   *string             `yaml:"last_reconciled_at"`
	CreatedAt          string              `yaml:"created_at"`
	UpdatedAt          string              `yaml:"updated_at"`
}

type CompletionPolicy struct {
	Mode                    string `yaml:"mode"`
	AllowDynamicTasks       bool   `yaml:"allow_dynamic_tasks"`
	OnRequiredFailed        string `yaml:"on_required_failed"`
	OnRequiredCancelled     string `yaml:"on_required_cancelled"`
	OnOptionalFailed        string `yaml:"on_optional_failed"`
	DependencyFailurePolicy string `yaml:"dependency_failure_policy"`
}

type CancelState struct {
	Requested   bool    `yaml:"requested"`
	RequestedAt *string `yaml:"requested_at"`
	RequestedBy *string `yaml:"requested_by"`
	Reason      *string `yaml:"reason"`
}

type Phase struct {
	PhaseID         string            `yaml:"phase_id"`
	Name            string            `yaml:"name"`
	Type            string            `yaml:"type"` // "concrete" or "deferred"
	Status          PhaseStatus       `yaml:"status"`
	DependsOnPhases []string          `yaml:"depends_on_phases"`
	TaskIDs         []string          `yaml:"task_ids"`
	Constraints     *PhaseConstraints `yaml:"constraints"`
	ActivatedAt     *string           `yaml:"activated_at"`
	CompletedAt     *string           `yaml:"completed_at"`
	FillDeadlineAt  *string           `yaml:"fill_deadline_at"`
	ReopenedAt      *string           `yaml:"reopened_at"`
}

type PhaseConstraints struct {
	MaxTasks           int   `yaml:"max_tasks"`
	AllowedBloomLevels []int `yaml:"allowed_bloom_levels"`
	TimeoutMinutes     int   `yaml:"timeout_minutes"`
}
