package model

// WorktreeStatus represents the lifecycle state of a worker worktree.
type WorktreeStatus string

const (
	// WorktreeStatusCreated indicates the worktree has been initialized but not yet activated.
	WorktreeStatusCreated WorktreeStatus = "created"
	// WorktreeStatusActive indicates the worktree is actively receiving task work.
	WorktreeStatusActive WorktreeStatus = "active"
	// WorktreeStatusCommitted indicates all task changes have been committed to the worktree branch.
	WorktreeStatusCommitted WorktreeStatus = "committed"
	// WorktreeStatusIntegrated indicates the worktree branch has been merged into the integration branch.
	WorktreeStatusIntegrated WorktreeStatus = "integrated"
	// WorktreeStatusPublished indicates the integration branch has been published to the base branch.
	WorktreeStatusPublished WorktreeStatus = "published"
	// WorktreeStatusCleanupDone indicates the worktree has been removed after successful completion.
	WorktreeStatusCleanupDone WorktreeStatus = "cleanup_done"
	// WorktreeStatusConflict indicates a merge conflict was encountered during integration.
	WorktreeStatusConflict WorktreeStatus = "conflict"
	// WorktreeStatusResolving marks a worker that is in the conflict-resolution
	// pipeline. The resolver agent has been dispatched and the daemon is
	// waiting for it to commit (or fail). Reachable from conflict; can return
	// to conflict on retryable failure or terminate via integrated/failed.
	WorktreeStatusResolving WorktreeStatus = "resolving"
	// WorktreeStatusFailed indicates the worktree processing failed.
	WorktreeStatusFailed WorktreeStatus = "failed"
	// WorktreeStatusCleanupFailed indicates the worktree cleanup process failed.
	WorktreeStatusCleanupFailed WorktreeStatus = "cleanup_failed"
)

// IntegrationStatus represents the lifecycle state of an integration branch.
type IntegrationStatus string

const (
	// IntegrationStatusCreated indicates the integration branch has been set up but not yet started.
	IntegrationStatusCreated IntegrationStatus = "created"
	// IntegrationStatusMerging indicates worker branches are being merged into the integration branch.
	IntegrationStatusMerging IntegrationStatus = "merging"
	// IntegrationStatusMerged indicates all worker branches have been successfully merged.
	IntegrationStatusMerged IntegrationStatus = "merged"
	// IntegrationStatusPublishing indicates the integration branch is being published to the base branch.
	IntegrationStatusPublishing IntegrationStatus = "publishing"
	// IntegrationStatusPublished indicates the integration branch has been published to the base branch.
	IntegrationStatusPublished IntegrationStatus = "published"
	// IntegrationStatusConflict indicates a merge conflict was encountered.
	IntegrationStatusConflict IntegrationStatus = "conflict"
	// IntegrationStatusPartialMerge indicates some but not all worker branches were merged.
	IntegrationStatusPartialMerge IntegrationStatus = "partial_merge"
	// IntegrationStatusFailed indicates the integration process failed.
	IntegrationStatusFailed IntegrationStatus = "failed"
	// IntegrationStatusQuarantined is a terminal state set when merge attempts
	// have failed repeatedly (see mergeFailureQuarantineThreshold). Operator
	// intervention via CLI is required to recover.
	IntegrationStatusQuarantined IntegrationStatus = "quarantined"
)

// WorktreeState tracks the lifecycle of a single worker worktree.
type WorktreeState struct {
	CommandID string         `yaml:"command_id"`
	WorkerID  string         `yaml:"worker_id"`
	Path      string         `yaml:"path"`
	Branch    string         `yaml:"branch"`
	BaseSHA   string         `yaml:"base_sha"`
	Status                     WorktreeStatus `yaml:"status"`
	ConflictResolutionAttempts int            `yaml:"conflict_resolution_attempts,omitempty"`
	CreatedAt                  string         `yaml:"created_at"`
	UpdatedAt                  string         `yaml:"updated_at"`
}

// IntegrationState tracks the lifecycle of an integration branch for a command.
type IntegrationState struct {
	CommandID string            `yaml:"command_id"`
	Branch    string            `yaml:"branch"`
	BaseSHA   string            `yaml:"base_sha"`
	Status    IntegrationStatus `yaml:"status"`
	CreatedAt string            `yaml:"created_at"`
	UpdatedAt string            `yaml:"updated_at"`
	// MergeFailureCount counts consecutive merge attempts that failed in a way
	// that left the worktree in an unrecoverable state. Reset on successful merge.
	MergeFailureCount int `yaml:"merge_failure_count,omitempty"`
	// QuarantinedAt records when the integration entered Quarantined state.
	QuarantinedAt string `yaml:"quarantined_at,omitempty"`
	// QuarantineReason describes why the integration was quarantined.
	QuarantineReason string `yaml:"quarantine_reason,omitempty"`
	// StallSignaled is set once a worktree_stalled planner signal has been
	// emitted for this command, to prevent re-emission on every scan.
	StallSignaled bool `yaml:"stall_signaled,omitempty"`
}

// MergeConflict describes a merge conflict between a worker branch and the integration branch.
type MergeConflict struct {
	WorkerID      string   `yaml:"worker_id"`
	ConflictFiles []string `yaml:"conflict_files"`
	Message       string   `yaml:"message"`
	BaseRef       string   `yaml:"base_ref,omitempty"`   // conflict の base（共通祖先）の ref
	OursRef       string   `yaml:"ours_ref,omitempty"`   // integration 側の ref
	TheirsRef     string   `yaml:"theirs_ref,omitempty"` // worker 側の ref
}

// WorktreeCommandState holds all worktree state for a single command.
// Persisted at .maestro/state/worktrees/{command_id}.yaml
type WorktreeCommandState struct {
	SchemaVersion int               `yaml:"schema_version"`
	FileType      string            `yaml:"file_type"`
	CommandID     string            `yaml:"command_id"`
	Integration   IntegrationState  `yaml:"integration"`
	Workers       []WorktreeState   `yaml:"workers"`
	MergedPhases  map[string]string `yaml:"merged_phases,omitempty"` // phase_id -> merged_at (tracks which phases have been merged)
	// CommitFailedWorkers tracks worker IDs whose auto-commit failed during a phase merge.
	// Publish-to-base is blocked while this list is non-empty so unmerged worker changes
	// are never silently published.
	CommitFailedWorkers []string `yaml:"commit_failed_workers,omitempty"`
	CreatedAt           string   `yaml:"created_at"`
	UpdatedAt           string   `yaml:"updated_at"`
}
