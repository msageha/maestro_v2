package model

type Continuous struct {
	SchemaVersion    int              `yaml:"schema_version"`
	FileType         string           `yaml:"file_type"`
	CurrentIteration int              `yaml:"current_iteration"`
	MaxIterations    int              `yaml:"max_iterations"`
	Status           ContinuousStatus `yaml:"status"`
	PausedReason     *string          `yaml:"paused_reason"`
	LastCommandID    *string          `yaml:"last_command_id"`
	UpdatedAt        *string          `yaml:"updated_at"`
}
