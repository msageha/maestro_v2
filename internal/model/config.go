// Package model defines the data structures for Maestro's configuration, state, and queue entries.
package model

type Config struct {
	Project      ProjectConfig      `yaml:"project"`
	Maestro      MaestroConfig      `yaml:"maestro"`
	Agents       AgentsConfig       `yaml:"agents"`
	Continuous   ContinuousConfig   `yaml:"continuous"`
	Watcher      WatcherConfig      `yaml:"watcher"`
	Retry        RetryConfig        `yaml:"retry"`
	Queue        QueueConfig        `yaml:"queue"`
	Limits       LimitsConfig       `yaml:"limits"`
	Daemon       DaemonConfig       `yaml:"daemon"`
	Logging      LoggingConfig      `yaml:"logging"`
	QualityGates QualityGatesConfig `yaml:"quality_gates"`
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

	// Clear confirmation settings (used by clearAndConfirm)
	ClearConfirmTimeoutSec int `yaml:"clear_confirm_timeout_sec"` // Per-attempt confirmation window (default 5s)
	ClearConfirmPollMs     int `yaml:"clear_confirm_poll_ms"`     // Polling interval within confirmation window (default 250ms)
	ClearMaxAttempts       int `yaml:"clear_max_attempts"`        // Total send attempts including initial (default 3)
	ClearRetryBackoffMs    int `yaml:"clear_retry_backoff_ms"`    // Base backoff between attempts; doubles each retry (default 500ms)
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

type QualityGatesConfig struct {
	Enabled       bool                       `yaml:"enabled"`
	SkipGates     bool                       `yaml:"skip_gates"`      // 緊急モードフラグ
	Thresholds    QualityGateThresholds      `yaml:"thresholds"`
	Enforcement   QualityGateEnforcement     `yaml:"enforcement"`
}

type QualityGateThresholds struct {
	MaxTaskFailureRate      float64 `yaml:"max_task_failure_rate"`       // タスク失敗率閾値（0.0-1.0）
	MinTaskSuccessRate      float64 `yaml:"min_task_success_rate"`       // タスク成功率閾値（0.0-1.0）
	MaxConsecutiveFailures  int     `yaml:"max_consecutive_failures"`    // 連続失敗数の上限
	MaxTaskDurationSec      int     `yaml:"max_task_duration_sec"`       // タスク実行時間の上限（秒）
	MaxPendingTasks         int     `yaml:"max_pending_tasks"`           // ペンディングタスク数の上限
}

type QualityGateEnforcement struct {
	PreTaskCheck   bool   `yaml:"pre_task_check"`    // タスク実行前チェック
	PostTaskCheck  bool   `yaml:"post_task_check"`   // タスク実行後チェック
	FailureAction  string `yaml:"failure_action"`    // 失敗時の動作: "warn", "block"
	LogViolations  bool   `yaml:"log_violations"`    // 違反をログに記録
}
