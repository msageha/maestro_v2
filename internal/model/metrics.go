package model

type Metrics struct {
	SchemaVersion   int             `yaml:"schema_version"`
	FileType        string          `yaml:"file_type"`
	QueueDepth      QueueDepth      `yaml:"queue_depth"`
	Counters        MetricsCounters `yaml:"counters"`
	// WorktreeCommandsStalled is a gauge: number of commands whose integration
	// branch has been flagged as stalled by Phase A's stall detection step.
	WorktreeCommandsStalled int `yaml:"worktree_commands_stalled"`
	// BakFilesCount is a gauge: total number of .bak files present anywhere
	// under the maestro directory at scan time.
	BakFilesCount   int     `yaml:"bak_files_count"`
	DaemonHeartbeat *string `yaml:"daemon_heartbeat"`
	UpdatedAt       *string `yaml:"updated_at"`
}

type QueueDepth struct {
	Planner      int            `yaml:"planner"`
	Orchestrator int            `yaml:"orchestrator"`
	Workers      map[string]int `yaml:"workers"`
}

type MetricsCounters struct {
	CommandsDispatched    int `yaml:"commands_dispatched"`
	TasksDispatched       int `yaml:"tasks_dispatched"`
	TasksCompleted        int `yaml:"tasks_completed"`
	TasksFailed           int `yaml:"tasks_failed"`
	TasksCancelled        int `yaml:"tasks_cancelled"`
	DeadLetters           int `yaml:"dead_letters"`
	ReconciliationRepairs int `yaml:"reconciliation_repairs"`
	NotificationRetries   int `yaml:"notification_retries"`
	LeaseRenewals         int `yaml:"lease_renewals"`
	LeaseExtensions       int `yaml:"lease_extensions"`
	LeaseReleases         int `yaml:"lease_releases"`
}
