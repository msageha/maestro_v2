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
	// SkillName is the kebab-case name assigned when the candidate was
	// approved (staged). Empty until approval.
	SkillName string `yaml:"skill_name,omitempty"`
	// StagedPath is the project-root-relative path of the staged SKILL.md
	// generated at approval (e.g. ".maestro/state/skill_staging/<name>/SKILL.md").
	// Promotion into a live skill library is a human git operation; the
	// daemon never copies or commits the staged file anywhere.
	StagedPath string `yaml:"staged_path,omitempty"`
	// SimilarSkills lists existing library skills ("<role>/<name>") whose
	// content resembles this candidate, computed at registration time as a
	// dedup hint for the operator (a strong match should lead to reject).
	SimilarSkills []string `yaml:"similar_skills,omitempty"`
}

// SkillCandidatesFile is the on-disk format for .maestro/state/skill_candidates.yaml.
type SkillCandidatesFile struct {
	SchemaVersion int              `yaml:"schema_version"`
	FileType      string           `yaml:"file_type"` // "state_skill_candidates"
	Candidates    []SkillCandidate `yaml:"candidates"`
}
