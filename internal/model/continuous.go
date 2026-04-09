package model

// Continuous は継続実行モードの状態を表す。
// 現在のイテレーション番号、上限、連続失敗数、一時停止状態などを管理する。
type Continuous struct {
	SchemaVersion    int              `yaml:"schema_version"`
	FileType         string           `yaml:"file_type"`
	CurrentIteration int              `yaml:"current_iteration"`
	MaxIterations    int              `yaml:"max_iterations"` // 0 means unlimited (no iteration cap); positive value enforces a stop
	// ConsecutiveFailures tracks the number of consecutive failed commands observed by
	// the continuous handler. Reset to 0 on any non-failed command. Used by the
	// pre-generation gate driven by ContinuousConfig.MaxConsecutiveFailures.
	ConsecutiveFailures int              `yaml:"consecutive_failures"`
	Status              ContinuousStatus `yaml:"status"`
	PausedReason     *string          `yaml:"paused_reason"`
	LastCommandID    *string          `yaml:"last_command_id"`
	UpdatedAt        string           `yaml:"updated_at"`
}
