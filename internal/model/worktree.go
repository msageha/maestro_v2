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
	// IntegrationStatusPublishFailed indicates that publishing the integration branch to the base branch failed.
	IntegrationStatusPublishFailed IntegrationStatus = "publish_failed"
	// IntegrationStatusFailed indicates the integration process failed.
	IntegrationStatusFailed IntegrationStatus = "failed"
	// IntegrationStatusQuarantined is a terminal state set when merge or publish
	// attempts have failed repeatedly (see mergeFailureQuarantineThreshold /
	// publishFailureQuarantineThreshold). Operator intervention via CLI is
	// required to recover.
	IntegrationStatusQuarantined IntegrationStatus = "quarantined"
)

// QuarantineSource identifies what caused an integration to enter quarantine.
type QuarantineSource string

const (
	// QuarantineSourceMerge indicates quarantine caused by repeated merge failures.
	QuarantineSourceMerge QuarantineSource = "merge"
	// QuarantineSourcePublish indicates quarantine caused by repeated publish failures.
	QuarantineSourcePublish QuarantineSource = "publish"
)

// WorktreeState tracks the lifecycle of a single worker worktree.
type WorktreeState struct {
	CommandID                  string         `yaml:"command_id"`
	WorkerID                   string         `yaml:"worker_id"`
	Path                       string         `yaml:"path"`
	Branch                     string         `yaml:"branch"`
	BaseSHA                    string         `yaml:"base_sha"`
	Status                     WorktreeStatus `yaml:"status"`
	ConflictResolutionAttempts int            `yaml:"conflict_resolution_attempts,omitempty"`
	// ConflictBranchHead records the worker branch's HEAD SHA at the moment
	// the merge into integration first detected a conflict. It is set by
	// MergeToIntegration when the worker transitions to WorktreeStatusConflict
	// and consulted by tryMergeWorker's deferred-merge guard to detect
	// non-Claude Workers (codex / gemini) that self-commit the resolution
	// — those produce a clean worktree but advance the worker branch HEAD
	// past this snapshot, signalling that the merge can proceed instead of
	// being deferred forever.
	ConflictBranchHead string `yaml:"conflict_branch_head,omitempty"`
	// ConflictIntegrationHead records the integration branch's HEAD SHA at the
	// moment the merge into integration first detected a conflict (the "ours"
	// side the resolver was asked to reconcile against). It is set by
	// MergeToIntegration alongside ConflictBranchHead and consulted by
	// ResumeMerge's lost-update guard: if the integration HEAD has advanced
	// past this snapshot by the time the worker's resolution is merged (e.g.
	// another conflicted worker's resolution landed first), the resolution is
	// stale — merging it with `-X theirs` would silently overwrite content
	// integrated after the snapshot. The guard instead resets the worker to
	// active so the standard merge pipeline re-detects the conflict against
	// the current integration HEAD and dispatches a fresh resolution.
	ConflictIntegrationHead string `yaml:"conflict_integration_head,omitempty"`
	// ConflictEscalated guards one-shot emission of the conflict-escalation
	// planner notification. R7 sets it true when it escalates a worker whose
	// ConflictResolutionAttempts has reached the max, so the escalation
	// notification + repair are emitted exactly once instead of on every
	// reconcile scan while the (unrecoverable) conflict persists. Mirrors the
	// IntegrationState.StallSignaled / PublishConflictSignaled one-shot guards.
	// setWorkerStatus clears it (together with ConflictResolutionAttempts) when
	// the worker re-enters conflict from a clean (non-conflict, non-resolving)
	// state — a fresh conflict episode in a later phase — so a genuinely new
	// conflict can escalate again.
	ConflictEscalated bool   `yaml:"conflict_escalated,omitempty"`
	CreatedAt         string `yaml:"created_at"`
	UpdatedAt         string `yaml:"updated_at"`
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
	// QuarantineSource identifies the subsystem that triggered quarantine
	// (merge or publish). Used for structured quarantine-type checks in
	// recovery operations instead of string-matching QuarantineReason.
	QuarantineSource QuarantineSource `yaml:"quarantine_source,omitempty"`
	// PublishFailureCount counts consecutive publish attempts that failed.
	// Reset on successful publish.
	PublishFailureCount int `yaml:"publish_failure_count,omitempty"`
	// NextPublishRetryAt is the earliest time the next publish retry is allowed (RFC3339).
	// Set by recordPublishFailure with exponential backoff. Cleared on successful publish
	// or quarantine.
	NextPublishRetryAt string `yaml:"next_publish_retry_at,omitempty"`
	// StallSignaled is set once a worktree_stalled planner signal has been
	// emitted for this command, to prevent re-emission on every scan.
	StallSignaled bool `yaml:"stall_signaled,omitempty"`
	// PublishConflictFiles lists files that conflicted during the last
	// publish attempt (integration → base merge). Populated by
	// PublishToBase when a forward-merge of base into integration fails.
	// Cleared on successful publish or by RetryPublish.
	PublishConflictFiles []string `yaml:"publish_conflict_files,omitempty"`
	// PublishConflictSignaled guards one-shot emission of the
	// publish_conflict planner signal per quarantine event.
	PublishConflictSignaled bool `yaml:"publish_conflict_signaled,omitempty"`
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

// CandidateWorktree records an A/B candidate-exclusive worktree + branch
// (docs/design/ab_candidate_selection.md §3). Unlike worker worktrees these
// are task-scoped and never merged directly: the selection pipeline merges
// the winner's branch into the canonical worker's branch.
type CandidateWorktree struct {
	TaskID    string `yaml:"task_id"`
	Path      string `yaml:"path"`
	Branch    string `yaml:"branch"`
	BaseSHA   string `yaml:"base_sha"`
	CreatedAt string `yaml:"created_at"`
	UpdatedAt string `yaml:"updated_at"`
}

// ABSelectionMarker records an in-flight A/B candidate selection on the
// integration worktree (docs/design/ab_candidate_selection.md §5.0).
type ABSelectionMarker struct {
	GroupID   string `yaml:"group_id"`
	PreSHA    string `yaml:"pre_sha"`
	StartedAt string `yaml:"started_at"`
}

// WorktreeCommandState holds all worktree state for a single command.
// Persisted at .maestro/state/worktrees/{command_id}.yaml
type WorktreeCommandState struct {
	SchemaVersion int              `yaml:"schema_version"`
	FileType      string           `yaml:"file_type"`
	CommandID     string           `yaml:"command_id"`
	Integration   IntegrationState `yaml:"integration"`
	Workers       []WorktreeState  `yaml:"workers"`
	// Candidates tracks A/B candidate worktrees (task-scoped, see
	// CandidateWorktree). Kept — with their worktrees and branches — for
	// audit until command cleanup removes them; group resolution does NOT
	// delete candidate artifacts.
	Candidates []CandidateWorktree `yaml:"candidates,omitempty"`
	// ABSelection is the durable in-flight marker for a candidate selection
	// run borrowing the integration worktree. Set before the first git
	// mutation, cleared after the worktree is restored to PreSHA. Startup
	// Reconcile restores and clears a stale marker after a daemon crash.
	ABSelection  *ABSelectionMarker `yaml:"ab_selection,omitempty"`
	MergedPhases map[string]string  `yaml:"merged_phases,omitempty"` // phase_id -> merged_at (tracks which phases have been merged)
	// CommitFailedWorkers tracks worker IDs whose auto-commit failed during a phase merge.
	// Publish-to-base is blocked while this list is non-empty so unmerged worker changes
	// are never silently published.
	CommitFailedWorkers []string `yaml:"commit_failed_workers,omitempty"`
	CreatedAt           string   `yaml:"created_at"`
	UpdatedAt           string   `yaml:"updated_at"`
}
