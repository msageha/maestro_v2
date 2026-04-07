package model

type TaskResultFile struct {
	SchemaVersion int          `yaml:"schema_version"`
	FileType      string       `yaml:"file_type"`
	Results       []TaskResult `yaml:"results"`
	// RejectedSubmissions captures result_write submissions whose
	// best-effort writes (learnings / skill_candidates) were dropped because
	// the worker no longer held a valid lease (e.g. lease epoch revoked
	// after the original result was committed and a stale worker re-sent
	// data attached to the same task_id). Recorded for audit / user
	// notification — these entries do not participate in plan reconcile,
	// rebuild, or metrics counters.
	RejectedSubmissions []RejectedSubmission `yaml:"rejected_submissions,omitempty"`
}

// RejectedSubmission records a result_write whose best-effort side data
// (learnings / skill_candidates) was rejected due to a lease epoch revoke.
// The reporter's core result (if any) lives in Results; this entry is the
// audit-trail record of what was dropped and why.
type RejectedSubmission struct {
	ID                  string   `yaml:"id"`
	TaskID              string   `yaml:"task_id"`
	CommandID           string   `yaml:"command_id"`
	Reporter            string   `yaml:"reporter"`
	Reason              string   `yaml:"reason"`
	RequestLeaseEpoch   int      `yaml:"request_lease_epoch"`
	QueueLeaseEpoch     int      `yaml:"queue_lease_epoch"`
	LostLearnings       []string `yaml:"lost_learnings,omitempty"`
	LostSkillCandidates []string `yaml:"lost_skill_candidates,omitempty"`
	OriginalSummary     string   `yaml:"original_summary,omitempty"`
	// DedupKey is a content fingerprint used to suppress duplicate
	// rejections from a stale worker that retries with identical payload.
	DedupKey  string `yaml:"dedup_key"`
	CreatedAt string `yaml:"created_at"`
}

type TaskResult struct {
	ID                     string                `yaml:"id"`
	TaskID                 string                `yaml:"task_id"`
	CommandID              string                `yaml:"command_id"`
	Status                 Status                `yaml:"status"`
	Summary                string                `yaml:"summary"`
	FilesChanged           []string              `yaml:"files_changed"`
	PartialChangesPossible bool                  `yaml:"partial_changes_possible"`
	RetrySafe              bool                  `yaml:"retry_safe"`
	Notified               bool                  `yaml:"notified"`
	NotifyAttempts         int                   `yaml:"notify_attempts"`
	NotifyLeaseOwner       *string               `yaml:"notify_lease_owner"`
	NotifyLeaseExpiresAt   *string               `yaml:"notify_lease_expires_at"`
	NotifiedAt             *string               `yaml:"notified_at"`
	NotifyLastError        *string               `yaml:"notify_last_error"`
	CreatedAt              string                `yaml:"created_at"`
	QualityGateEvaluation  *QualityGateEvaluation `yaml:"quality_gate_evaluation,omitempty"`
}

type CommandResultFile struct {
	SchemaVersion int             `yaml:"schema_version"`
	FileType      string          `yaml:"file_type"`
	Results       []CommandResult `yaml:"results"`
}

type CommandResult struct {
	ID                   string              `yaml:"id"`
	CommandID            string              `yaml:"command_id"`
	Status               Status              `yaml:"status"`
	Summary              string              `yaml:"summary"`
	Tasks                []CommandResultTask `yaml:"tasks"`
	Notified             bool                `yaml:"notified"`
	NotifyAttempts       int                 `yaml:"notify_attempts"`
	NotifyLeaseOwner     *string             `yaml:"notify_lease_owner"`
	NotifyLeaseExpiresAt *string             `yaml:"notify_lease_expires_at"`
	NotifiedAt           *string             `yaml:"notified_at"`
	NotifyLastError      *string             `yaml:"notify_last_error"`
	CreatedAt            string              `yaml:"created_at"`
}

type CommandResultTask struct {
	TaskID  string `yaml:"task_id"`
	Worker  string `yaml:"worker"`
	Status  Status `yaml:"status"`
	Summary string `yaml:"summary"`
}

// Notifiable provides access to the notification lease fields shared by TaskResult and CommandResult.
// Both *TaskResult and *CommandResult implement this interface.
type Notifiable interface {
	GetResultID() string
	IsNotified() bool
	GetNotifyAttempts() int
	GetNotifyLeaseOwner() *string
	GetNotifyLeaseExpiresAt() *string
	AcquireLease(owner, expiresAt string)
	MarkNotified(at string)
	MarkNotifyFailure(errMsg, backoffOwner, backoffExpiresAt string)
}

// --- TaskResult implements Notifiable ---

func (r *TaskResult) GetResultID() string              { return r.ID }
func (r *TaskResult) IsNotified() bool                 { return r.Notified }
func (r *TaskResult) GetNotifyAttempts() int            { return r.NotifyAttempts }
func (r *TaskResult) GetNotifyLeaseOwner() *string      { return r.NotifyLeaseOwner }
func (r *TaskResult) GetNotifyLeaseExpiresAt() *string  { return r.NotifyLeaseExpiresAt }

func (r *TaskResult) AcquireLease(owner, expiresAt string) {
	r.NotifyLeaseOwner = &owner
	r.NotifyLeaseExpiresAt = &expiresAt
	r.NotifyAttempts++
}

func (r *TaskResult) MarkNotified(at string) {
	r.Notified = true
	r.NotifiedAt = &at
	r.NotifyLeaseOwner = nil
	r.NotifyLeaseExpiresAt = nil
}

func (r *TaskResult) MarkNotifyFailure(errMsg, backoffOwner, backoffExpiresAt string) {
	r.NotifyLastError = &errMsg
	r.NotifyLeaseOwner = &backoffOwner
	r.NotifyLeaseExpiresAt = &backoffExpiresAt
}

// --- CommandResult implements Notifiable ---

func (r *CommandResult) GetResultID() string              { return r.ID }
func (r *CommandResult) IsNotified() bool                 { return r.Notified }
func (r *CommandResult) GetNotifyAttempts() int            { return r.NotifyAttempts }
func (r *CommandResult) GetNotifyLeaseOwner() *string      { return r.NotifyLeaseOwner }
func (r *CommandResult) GetNotifyLeaseExpiresAt() *string  { return r.NotifyLeaseExpiresAt }

func (r *CommandResult) AcquireLease(owner, expiresAt string) {
	r.NotifyLeaseOwner = &owner
	r.NotifyLeaseExpiresAt = &expiresAt
	r.NotifyAttempts++
}

func (r *CommandResult) MarkNotified(at string) {
	r.Notified = true
	r.NotifiedAt = &at
	r.NotifyLeaseOwner = nil
	r.NotifyLeaseExpiresAt = nil
}

func (r *CommandResult) MarkNotifyFailure(errMsg, backoffOwner, backoffExpiresAt string) {
	r.NotifyLastError = &errMsg
	r.NotifyLeaseOwner = &backoffOwner
	r.NotifyLeaseExpiresAt = &backoffExpiresAt
}

// QualityGateEvaluation records the quality gate evaluation result
type QualityGateEvaluation struct {
	Passed       bool     `yaml:"passed"`
	Action       string   `yaml:"action"` // "warn" or "block"
	FailedGates  []string `yaml:"failed_gates,omitempty"`
	EvaluatedAt  string   `yaml:"evaluated_at"`
	SkippedReason string  `yaml:"skipped_reason,omitempty"` // e.g., "emergency_mode", "disabled"
}
