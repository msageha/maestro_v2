package model

// SkillCandidate represents a candidate skill pattern discovered from worker reports.
type SkillCandidate struct {
	ID          string   `yaml:"id"`
	Content     string   `yaml:"content"`
	Occurrences int      `yaml:"occurrences"`
	CommandIDs  []string `yaml:"command_ids"`
	CreatedAt   string   `yaml:"created_at"`
	UpdatedAt   string   `yaml:"updated_at"`
	Status      string   `yaml:"status"` // pending, approved, rejected
}

// SkillCandidatesFile is the on-disk format for .maestro/state/skill_candidates.yaml.
type SkillCandidatesFile struct {
	SchemaVersion int              `yaml:"schema_version"`
	FileType      string           `yaml:"file_type"` // "state_skill_candidates"
	Candidates    []SkillCandidate `yaml:"candidates"`
}
