package model

// TaskTracking groups task state management fields within CommandState.
// Embedded with yaml:",inline" to maintain flat YAML serialization.
type TaskTracking struct {
	ExpectedTaskCount  int                 `yaml:"expected_task_count"`
	RequiredTaskIDs    []string            `yaml:"required_task_ids"`
	OptionalTaskIDs    []string            `yaml:"optional_task_ids"`
	TaskDependencies   map[string][]string `yaml:"task_dependencies"`
	TaskStates         map[string]Status   `yaml:"task_states"`
	CancelledReasons   map[string]string   `yaml:"cancelled_reasons"`
	AppliedResultIDs   map[string]string   `yaml:"applied_result_ids"`
	SystemCommitTaskID *string             `yaml:"system_commit_task_id"`
	QueueWriteFailed   map[string]string   `yaml:"queue_write_failed,omitempty"`  // task_id → "workerID:resultID"; set when result committed but queue terminal write failed (H2 sticky error)
	IdempotencyKeys    map[string]string   `yaml:"idempotency_keys,omitempty"`   // idempotency_key → task_id; prevents duplicate task injection on retry
}

// RetryTracking groups retry-related fields within CommandState.
// Embedded with yaml:",inline" to maintain flat YAML serialization.
type RetryTracking struct {
	RetryLineage       map[string]string `yaml:"retry_lineage"`
	RetryEnqueueFailed map[string]string `yaml:"retry_enqueue_failed,omitempty"` // task_id → worker_id; set when state registered but queue add failed
}

// PhaseTracking groups phase lifecycle fields within CommandState.
// Embedded with yaml:",inline" to maintain flat YAML serialization.
type PhaseTracking struct {
	Phases []Phase `yaml:"phases"`
}

// PhaseIndex returns the slice index for the given phaseID by linear search.
// Returns (index, true) if found, (-1, false) otherwise.
// Linear search is used instead of caching because the number of phases per
// command is typically small, and a cache would risk returning stale data
// when the Phases slice is modified.
func (pt *PhaseTracking) PhaseIndex(phaseID string) (int, bool) {
	for i := range pt.Phases {
		if pt.Phases[i].PhaseID == phaseID {
			return i, true
		}
	}
	return -1, false
}

// CommandState は単一コマンドの実行状態を表す。
// プランバージョン、フェーズ構成、タスク依存関係、完了ポリシーなど
// コマンドのライフサイクル全体を管理する。
// サブ構造体は yaml:",inline" で埋め込まれ、YAML シリアライゼーションの
// フラット構造を維持する。
type CommandState struct {
	SchemaVersion    int                 `yaml:"schema_version"`
	FileType         string              `yaml:"file_type"`
	CommandID        string              `yaml:"command_id"`
	PlanVersion      int                 `yaml:"plan_version"`
	PlanStatus       PlanStatus          `yaml:"plan_status"`
	CompletionPolicy CompletionPolicy    `yaml:"completion_policy"`
	Cancel           CancelState         `yaml:"cancel"`
	CircuitBreaker   CircuitBreakerState `yaml:"circuit_breaker"`
	TaskTracking     `yaml:",inline"`
	RetryTracking    `yaml:",inline"`
	PhaseTracking    `yaml:",inline"`
	LastReconciledAt *string `yaml:"last_reconciled_at"`
	CreatedAt        string  `yaml:"created_at"`
	UpdatedAt        string  `yaml:"updated_at"`
}

// CircuitBreakerState tracks per-command circuit breaker counters.
type CircuitBreakerState struct {
	ConsecutiveFailures int     `yaml:"consecutive_failures"`
	LastProgressAt      *string `yaml:"last_progress_at,omitempty"`
	Tripped             bool    `yaml:"tripped"`
	TrippedAt           *string `yaml:"tripped_at,omitempty"`
	TripReason          *string `yaml:"trip_reason,omitempty"`
}

// CompletionPolicy はコマンドの完了判定ポリシーを定義する。
// 必須・任意タスクの失敗時の挙動や依存関係失敗時のポリシーを指定する。
type CompletionPolicy struct {
	Mode                    string `yaml:"mode"`
	AllowDynamicTasks       bool   `yaml:"allow_dynamic_tasks"`
	OnRequiredFailed        string `yaml:"on_required_failed"`
	OnRequiredCancelled     string `yaml:"on_required_cancelled"`
	OnOptionalFailed        string `yaml:"on_optional_failed"`
	DependencyFailurePolicy string `yaml:"dependency_failure_policy"`
}

// CancelState はコマンドのキャンセル要求の状態を保持する。
type CancelState struct {
	Requested   bool    `yaml:"requested"`
	RequestedAt *string `yaml:"requested_at"`
	RequestedBy *string `yaml:"requested_by"`
	Reason      *string `yaml:"reason"`
}

// Phase はコマンド実行計画内の単一フェーズを表す。
// タスクのグルーピングと実行順序の制御に使用され、フェーズ間の依存関係を持つ。
type Phase struct {
	PhaseID          string            `yaml:"phase_id"`
	Name             string            `yaml:"name"`
	Type             string            `yaml:"type"` // "concrete" or "deferred"
	Status           PhaseStatus       `yaml:"status"`
	DependsOnPhases  []string          `yaml:"depends_on_phases"`
	TaskIDs          []string          `yaml:"task_ids"`
	Constraints      *PhaseConstraints `yaml:"constraints"`
	ActivatedAt      *string           `yaml:"activated_at"`
	CompletedAt      *string           `yaml:"completed_at"`
	FillDeadlineAt   *string           `yaml:"fill_deadline_at"`
	FillingStartedAt *string           `yaml:"filling_started_at,omitempty"`
	ReopenedAt       *string           `yaml:"reopened_at"`
}

// PhaseInfo represents phase metadata from command state.
// Used by StateReader implementations to return phase data without exposing
// the full Phase struct or YAML serialization details.
type PhaseInfo struct {
	ID               string
	Name             string
	Status           PhaseStatus
	DependsOn        []string // phase IDs
	FillDeadlineAt   *string
	RequiredTaskIDs  []string
	SystemCommitTask bool
}

// PhaseConstraints はフェーズに適用される制約条件を定義する。
// 最大タスク数、許可される Bloom レベル、タイムアウトを指定する。
type PhaseConstraints struct {
	MaxTasks           int   `yaml:"max_tasks"`
	AllowedBloomLevels []int `yaml:"allowed_bloom_levels"`
	TimeoutMinutes     int   `yaml:"timeout_minutes"`
}
