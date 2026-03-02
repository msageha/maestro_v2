package model

type TaskResultFile struct {
	SchemaVersion int          `yaml:"schema_version"`
	FileType      string       `yaml:"file_type"`
	Results       []TaskResult `yaml:"results"`
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

// QualityGateEvaluation records the quality gate evaluation result
type QualityGateEvaluation struct {
	Passed       bool     `yaml:"passed"`
	Action       string   `yaml:"action"` // "warn" or "block"
	FailedGates  []string `yaml:"failed_gates,omitempty"`
	EvaluatedAt  string   `yaml:"evaluated_at"`
	SkippedReason string  `yaml:"skipped_reason,omitempty"` // e.g., "emergency_mode", "disabled"
}
