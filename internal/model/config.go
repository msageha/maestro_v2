// Package model defines the data structures for Maestro's configuration, state, and queue entries.
package model

type Config struct {
	Project    ProjectConfig    `yaml:"project"`
	Maestro    MaestroConfig    `yaml:"maestro"`
	Agents     AgentsConfig     `yaml:"agents"`
	Continuous ContinuousConfig `yaml:"continuous"`
	Watcher    WatcherConfig    `yaml:"watcher"`
	Retry      RetryConfig      `yaml:"retry"`
	Queue      QueueConfig      `yaml:"queue"`
	Limits     LimitsConfig     `yaml:"limits"`
	Daemon     DaemonConfig     `yaml:"daemon"`
	Logging    LoggingConfig    `yaml:"logging"`
}

type ProjectConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type MaestroConfig struct {
	Version     string `yaml:"version"`
	Created     string `yaml:"created"`
	ProjectRoot string `yaml:"project_root"`
}

type AgentsConfig struct {
	Orchestrator AgentConfig  `yaml:"orchestrator"`
	Planner      AgentConfig  `yaml:"planner"`
	Workers      WorkerConfig `yaml:"workers"`
}

type AgentConfig struct {
	ID    string `yaml:"id"`
	Model string `yaml:"model"`
}

type WorkerConfig struct {
	Count        int               `yaml:"count"`
	DefaultModel string            `yaml:"default_model"`
	Models       map[string]string `yaml:"models,omitempty"`
	Boost        bool              `yaml:"boost"`
}

type ContinuousConfig struct {
	Enabled        bool `yaml:"enabled"`
	MaxIterations  int  `yaml:"max_iterations"`
	PauseOnFailure bool `yaml:"pause_on_failure"`
}

type WatcherConfig struct {
	DebounceSec         float64 `yaml:"debounce_sec"`
	ScanIntervalSec     int     `yaml:"scan_interval_sec"`
	DispatchLeaseSec    int     `yaml:"dispatch_lease_sec"`
	MaxInProgressMin    int     `yaml:"max_in_progress_min"`
	BusyCheckInterval   int     `yaml:"busy_check_interval"`
	BusyCheckMaxRetries int     `yaml:"busy_check_max_retries"`
	BusyPatterns        string  `yaml:"busy_patterns"`
	IdleStableSec       int     `yaml:"idle_stable_sec"`
	CooldownAfterClear  int     `yaml:"cooldown_after_clear"`
	NotifyLeaseSec      int     `yaml:"notify_lease_sec"`
	WaitReadyIntervalSec int    `yaml:"wait_ready_interval_sec"`
	WaitReadyMaxRetries  int    `yaml:"wait_ready_max_retries"`
}

type RetryConfig struct {
	CommandDispatch                  int   `yaml:"command_dispatch"`
	TaskDispatch                     int   `yaml:"task_dispatch"`
	OrchestratorNotificationDispatch int   `yaml:"orchestrator_notification_dispatch"`
	TaskExecution                    TaskRetryConfig `yaml:"task_execution"`
}

type TaskRetryConfig struct {
	Enabled            bool  `yaml:"enabled"`
	RetryableExitCodes []int `yaml:"retryable_exit_codes"`
	MaxRetries         int   `yaml:"max_retries"`
	CooldownSec        int   `yaml:"cooldown_sec"`
}

type QueueConfig struct {
	PriorityAgingSec int `yaml:"priority_aging_sec"`
}

type LimitsConfig struct {
	MaxPendingCommands       int `yaml:"max_pending_commands"`
	MaxPendingTasksPerWorker int `yaml:"max_pending_tasks_per_worker"`
	MaxEntryContentBytes     int `yaml:"max_entry_content_bytes"`
	MaxYAMLFileBytes         int `yaml:"max_yaml_file_bytes"`
}

type DaemonConfig struct {
	ShutdownTimeoutSec int `yaml:"shutdown_timeout_sec"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
}
