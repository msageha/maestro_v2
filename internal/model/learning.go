package model

// Learning represents a single learning entry accumulated from worker task results.
type Learning struct {
	ResultID  string `yaml:"result_id"`
	CommandID string `yaml:"command_id"`
	Content   string `yaml:"content"`
	CreatedAt string `yaml:"created_at"`
}

// LearningsFile is the on-disk format for .maestro/state/learnings.yaml.
type LearningsFile struct {
	SchemaVersion int        `yaml:"schema_version"`
	FileType      string     `yaml:"file_type"` // "state_learnings"
	Learnings     []Learning `yaml:"learnings"`
}
