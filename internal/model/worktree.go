package model

// WorktreeStatus represents the lifecycle state of a worker worktree.
type WorktreeStatus string

const (
	WorktreeStatusCreated      WorktreeStatus = "created"
	WorktreeStatusActive       WorktreeStatus = "active"
	WorktreeStatusCommitted    WorktreeStatus = "committed"
	WorktreeStatusIntegrated   WorktreeStatus = "integrated"
	WorktreeStatusPublished    WorktreeStatus = "published"
	WorktreeStatusCleanupDone  WorktreeStatus = "cleanup_done"
	WorktreeStatusConflict     WorktreeStatus = "conflict"
	WorktreeStatusFailed       WorktreeStatus = "failed"
	WorktreeStatusCleanupFailed WorktreeStatus = "cleanup_failed"
)

// IntegrationStatus represents the lifecycle state of an integration branch.
type IntegrationStatus string

const (
	IntegrationStatusCreated    IntegrationStatus = "created"
	IntegrationStatusMerging    IntegrationStatus = "merging"
	IntegrationStatusMerged     IntegrationStatus = "merged"
	IntegrationStatusPublishing IntegrationStatus = "publishing"
	IntegrationStatusPublished  IntegrationStatus = "published"
	IntegrationStatusConflict   IntegrationStatus = "conflict"
	IntegrationStatusFailed     IntegrationStatus = "failed"
)

// WorktreeState tracks the lifecycle of a single worker worktree.
type WorktreeState struct {
	CommandID string         `yaml:"command_id"`
	WorkerID  string         `yaml:"worker_id"`
	Path      string         `yaml:"path"`
	Branch    string         `yaml:"branch"`
	BaseSHA   string         `yaml:"base_sha"`
	Status    WorktreeStatus `yaml:"status"`
	CreatedAt string         `yaml:"created_at"`
	UpdatedAt string         `yaml:"updated_at"`
}

// IntegrationState tracks the lifecycle of an integration branch for a command.
type IntegrationState struct {
	CommandID string            `yaml:"command_id"`
	Branch    string            `yaml:"branch"`
	BaseSHA   string            `yaml:"base_sha"`
	Status    IntegrationStatus `yaml:"status"`
	CreatedAt string            `yaml:"created_at"`
	UpdatedAt string            `yaml:"updated_at"`
}

// MergeConflict describes a merge conflict between a worker branch and the integration branch.
type MergeConflict struct {
	WorkerID      string   `yaml:"worker_id"`
	ConflictFiles []string `yaml:"conflict_files"`
	Message       string   `yaml:"message"`
}

// WorktreeCommandState holds all worktree state for a single command.
// Persisted at .maestro/state/worktrees/{command_id}.yaml
type WorktreeCommandState struct {
	SchemaVersion int                `yaml:"schema_version"`
	FileType      string             `yaml:"file_type"`
	CommandID     string             `yaml:"command_id"`
	Integration   IntegrationState   `yaml:"integration"`
	Workers       []WorktreeState    `yaml:"workers"`
	MergedPhases  map[string]string  `yaml:"merged_phases,omitempty"` // phase_id -> merged_at (tracks which phases have been merged)
	CreatedAt     string             `yaml:"created_at"`
	UpdatedAt     string             `yaml:"updated_at"`
}
