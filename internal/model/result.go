package model

// TaskResultFile はタスク結果ファイルの YAML 構造を表す。
// Worker が報告したタスク実行結果の永続化に使用される。
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

// TaskResult は単一タスクの実行結果を表す。
// ステータス、サマリー、変更ファイル一覧、通知状態などを保持する。
type TaskResult struct {
	ID                     string   `yaml:"id"`
	TaskID                 string   `yaml:"task_id"`
	CommandID              string   `yaml:"command_id"`
	Status                 Status   `yaml:"status"`
	Summary                string   `yaml:"summary"`
	FilesChanged           []string `yaml:"files_changed"`
	PartialChangesPossible bool     `yaml:"partial_changes_possible"`
	RetrySafe              bool     `yaml:"retry_safe"`
	NotifiableBase         `yaml:",inline"`
	CreatedAt              string                 `yaml:"created_at"`
	QualityGateEvaluation  *QualityGateEvaluation `yaml:"quality_gate_evaluation,omitempty"`
}

// CommandResultFile はコマンド結果ファイルの YAML 構造を表す。
// コマンド全体の完了結果を Orchestrator に通知するために使用される。
type CommandResultFile struct {
	SchemaVersion int             `yaml:"schema_version"`
	FileType      string          `yaml:"file_type"`
	Results       []CommandResult `yaml:"results"`
}

// CommandResult はコマンド全体の実行結果を表す。
// 配下の全タスク結果を集約し、Orchestrator への通知状態を管理する。
type CommandResult struct {
	ID             string              `yaml:"id"`
	CommandID      string              `yaml:"command_id"`
	Status         Status              `yaml:"status"`
	Summary        string              `yaml:"summary"`
	TaskStats      TaskStats           `yaml:"task_stats"`
	Tasks          []CommandResultTask `yaml:"tasks"`
	NotifiableBase `yaml:",inline"`
	CreatedAt      string `yaml:"created_at"`
}

// CommandResultTask holds per-task outcome information within a command result.
type CommandResultTask struct {
	TaskID  string `yaml:"task_id"`
	Worker  string `yaml:"worker"`
	Status  Status `yaml:"status"`
	Summary string `yaml:"summary"`
}

// TaskStats holds aggregated task statistics derived from the actual task
// results. This provides a reliable, machine-readable source of truth for
// task counts, independent of the free-text Summary field.
type TaskStats struct {
	Total     int `yaml:"total" json:"total"`
	Completed int `yaml:"completed" json:"completed"`
	Failed    int `yaml:"failed" json:"failed"`
	Cancelled int `yaml:"cancelled" json:"cancelled"`
}

// ComputeTaskStats derives TaskStats from a slice of CommandResultTask.
func ComputeTaskStats(tasks []CommandResultTask) TaskStats {
	stats := TaskStats{Total: len(tasks)}
	for _, t := range tasks {
		switch t.Status {
		case StatusCompleted:
			stats.Completed++
		case StatusFailed:
			stats.Failed++
		case StatusCancelled:
			stats.Cancelled++
		}
	}
	return stats
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
	IsNotifyDeadLettered() bool
	MarkNotifyDeadLetter(at, reason string)
	GetNotifyDeadLetterReason() *string
}

// NotifiableBase holds notification lease state and implements the shared
// methods of the Notifiable interface. Embed in TaskResult and CommandResult.
//
// NotifyDeadLettered* fields are set when retry attempts exceed the per-domain
// maximum and the daemon transitions the result into a terminal "notification
// exhausted" state. This prevents silent loss of completed results that the
// downstream consumer (Planner for TaskResult, Orchestrator for CommandResult)
// could never be told about.
type NotifiableBase struct {
	Notified               bool    `yaml:"notified"`
	NotifyAttempts         int     `yaml:"notify_attempts"`
	NotifyLeaseOwner       *string `yaml:"notify_lease_owner"`
	NotifyLeaseExpiresAt   *string `yaml:"notify_lease_expires_at"`
	NotifiedAt             *string `yaml:"notified_at"`
	NotifyLastError        *string `yaml:"notify_last_error"`
	NotifyDeadLettered     bool    `yaml:"notify_dead_lettered,omitempty"`
	NotifyDeadLetteredAt   *string `yaml:"notify_dead_lettered_at,omitempty"`
	NotifyDeadLetterReason *string `yaml:"notify_dead_letter_reason,omitempty"`
}

// IsNotified reports whether the notification for this result has been sent.
func (n *NotifiableBase) IsNotified() bool { return n.Notified }

// GetNotifyAttempts returns the number of notification delivery attempts made.
func (n *NotifiableBase) GetNotifyAttempts() int { return n.NotifyAttempts }

// GetNotifyLeaseOwner returns the owner of the current notification lease, or nil.
func (n *NotifiableBase) GetNotifyLeaseOwner() *string { return n.NotifyLeaseOwner }

// GetNotifyLeaseExpiresAt returns the expiry time of the current notification lease, or nil.
func (n *NotifiableBase) GetNotifyLeaseExpiresAt() *string { return n.NotifyLeaseExpiresAt }

// AcquireLease sets the notification lease owner and expiry, incrementing the attempt counter.
func (n *NotifiableBase) AcquireLease(owner, expiresAt string) {
	n.NotifyLeaseOwner = &owner
	n.NotifyLeaseExpiresAt = &expiresAt
	n.NotifyAttempts++
}

// MarkNotified marks the result as successfully notified at the given timestamp.
func (n *NotifiableBase) MarkNotified(at string) {
	n.Notified = true
	n.NotifiedAt = &at
	n.NotifyLeaseOwner = nil
	n.NotifyLeaseExpiresAt = nil
}

// MarkNotifyFailure records a failed notification attempt and sets the backoff lease.
func (n *NotifiableBase) MarkNotifyFailure(errMsg, backoffOwner, backoffExpiresAt string) {
	n.NotifyLastError = &errMsg
	n.NotifyLeaseOwner = &backoffOwner
	n.NotifyLeaseExpiresAt = &backoffExpiresAt
}

// IsNotifyDeadLettered reports whether notification retries were exhausted
// and the result was transitioned into a dead-letter terminal state.
func (n *NotifiableBase) IsNotifyDeadLettered() bool { return n.NotifyDeadLettered }

// MarkNotifyDeadLetter marks the result as notify-dead-lettered and clears any
// outstanding backoff lease so that findUnnotifiedExcluding does not waste
// attempts re-scanning the entry.
func (n *NotifiableBase) MarkNotifyDeadLetter(at, reason string) {
	n.NotifyDeadLettered = true
	n.NotifyDeadLetteredAt = &at
	n.NotifyDeadLetterReason = &reason
	n.NotifyLeaseOwner = nil
	n.NotifyLeaseExpiresAt = nil
}

// GetNotifyDeadLetterReason returns the human-readable reason recorded at
// dead-letter time, or nil if the result has not been dead-lettered.
func (n *NotifiableBase) GetNotifyDeadLetterReason() *string { return n.NotifyDeadLetterReason }

// GetResultID returns the unique identifier of the task result.
func (r *TaskResult) GetResultID() string { return r.ID }

// GetResultID returns the unique identifier of the command result.
func (r *CommandResult) GetResultID() string { return r.ID }

// QualityGateEvaluation records the quality gate evaluation result
type QualityGateEvaluation struct {
	Passed        bool     `yaml:"passed"`
	Action        string   `yaml:"action"` // "warn" or "block"
	FailedGates   []string `yaml:"failed_gates,omitempty"`
	EvaluatedAt   string   `yaml:"evaluated_at"`
	SkippedReason string   `yaml:"skipped_reason,omitempty"` // e.g., "emergency_mode", "disabled"
}
