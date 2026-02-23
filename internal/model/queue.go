package model

type CommandQueue struct {
	SchemaVersion int       `yaml:"schema_version"`
	FileType      string    `yaml:"file_type"`
	Commands      []Command `yaml:"commands"`
}

type Command struct {
	ID               string  `yaml:"id"`
	Content          string  `yaml:"content"`
	Priority         int     `yaml:"priority"`
	Status           Status  `yaml:"status"`
	Attempts         int     `yaml:"attempts"`
	LastError        *string `yaml:"last_error"`
	DeadLetteredAt   *string `yaml:"dead_lettered_at"`
	DeadLetterReason *string `yaml:"dead_letter_reason"`
	LeaseOwner       *string `yaml:"lease_owner"`
	LeaseExpiresAt   *string `yaml:"lease_expires_at"`
	LeaseEpoch       int     `yaml:"lease_epoch"`
	CancelReason     *string `yaml:"cancel_reason"`
	CancelRequestedAt *string `yaml:"cancel_requested_at"`
	CancelRequestedBy *string `yaml:"cancel_requested_by"`
	CreatedAt        string  `yaml:"created_at"`
	UpdatedAt        string  `yaml:"updated_at"`
}

type TaskQueue struct {
	SchemaVersion int    `yaml:"schema_version"`
	FileType      string `yaml:"file_type"`
	Tasks         []Task `yaml:"tasks"`
}

type Task struct {
	ID               string   `yaml:"id"`
	CommandID        string   `yaml:"command_id"`
	Purpose          string   `yaml:"purpose"`
	Content          string   `yaml:"content"`
	AcceptanceCriteria string `yaml:"acceptance_criteria"`
	Constraints      []string `yaml:"constraints"`
	BlockedBy        []string `yaml:"blocked_by"`
	BloomLevel       int      `yaml:"bloom_level"`
	ToolsHint        []string `yaml:"tools_hint,omitempty"`
	Priority         int      `yaml:"priority"`
	Status           Status   `yaml:"status"`
	Attempts         int      `yaml:"attempts"`
	LastError        *string  `yaml:"last_error"`
	DeadLetteredAt   *string  `yaml:"dead_lettered_at"`
	DeadLetterReason *string  `yaml:"dead_letter_reason"`
	LeaseOwner       *string  `yaml:"lease_owner"`
	LeaseExpiresAt   *string  `yaml:"lease_expires_at"`
	LeaseEpoch       int      `yaml:"lease_epoch"`
	CreatedAt        string   `yaml:"created_at"`
	UpdatedAt        string   `yaml:"updated_at"`
}

type NotificationQueue struct {
	SchemaVersion int            `yaml:"schema_version"`
	FileType      string         `yaml:"file_type"`
	Notifications []Notification `yaml:"notifications"`
}

type Notification struct {
	ID               string  `yaml:"id"`
	CommandID        string  `yaml:"command_id"`
	Type             string  `yaml:"type"`
	SourceResultID   string  `yaml:"source_result_id"`
	Content          string  `yaml:"content"`
	Priority         int     `yaml:"priority"`
	Status           Status  `yaml:"status"`
	Attempts         int     `yaml:"attempts"`
	LastError        *string `yaml:"last_error"`
	DeadLetteredAt   *string `yaml:"dead_lettered_at"`
	DeadLetterReason *string `yaml:"dead_letter_reason"`
	LeaseOwner       *string `yaml:"lease_owner"`
	LeaseExpiresAt   *string `yaml:"lease_expires_at"`
	LeaseEpoch       int     `yaml:"lease_epoch"`
	CreatedAt        string  `yaml:"created_at"`
	UpdatedAt        string  `yaml:"updated_at"`
}
