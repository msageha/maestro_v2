package model

import "time"

// RolloutState はロールアウトグループの状態を表す文字列列挙型。
type RolloutState string

const (
	// RolloutStatePending indicates the rollout group has not yet started.
	RolloutStatePending RolloutState = "pending"
	// RolloutStateRunning indicates slots are actively executing.
	RolloutStateRunning RolloutState = "running"
	// RolloutStateSelecting indicates the group is evaluating candidates.
	RolloutStateSelecting RolloutState = "selecting"
	// RolloutStateCompleted indicates the rollout finished with a winner selected.
	RolloutStateCompleted RolloutState = "completed"
	// RolloutStateCancelled indicates the rollout was cancelled before completion.
	RolloutStateCancelled RolloutState = "cancelled"
)

// IsTerminalRolloutState は状態が終端（completed または cancelled）かを返す。
func IsTerminalRolloutState(s RolloutState) bool {
	return s == RolloutStateCompleted || s == RolloutStateCancelled
}

// RolloutGroup は複数候補を並列実行し勝者を選定するグループを表す。
type RolloutGroup struct {
	ID          string             `yaml:"id"`
	TaskID      string             `yaml:"task_id"`
	CommandID   string             `yaml:"command_id"`
	Candidates  []RolloutCandidate `yaml:"candidates"`
	State       RolloutState       `yaml:"state"`
	WinnerIndex *int               `yaml:"winner_index"`
	CreatedAt   time.Time          `yaml:"created_at"`
	CompletedAt *time.Time         `yaml:"completed_at,omitempty"`
}

// RolloutCandidate はロールアウトグループ内の単一候補を表す。
type RolloutCandidate struct {
	SlotIndex  int           `yaml:"slot_index"`
	WorkerID   string        `yaml:"worker_id"`
	BranchName string        `yaml:"branch_name"`
	Fitness    *FitnessScore `yaml:"fitness,omitempty"`
	ResultID   string        `yaml:"result_id"`
	Status     string        `yaml:"status"`
}

// RolloutEligibility はタスクがロールアウト対象かどうかの判定結果を表す。
type RolloutEligibility struct {
	Eligible bool     `yaml:"eligible"`
	Reasons  []string `yaml:"reasons,omitempty"`
}

// RolloutConfig はロールアウト機能の設定を保持する。
type RolloutConfig struct {
	Enabled            *bool `yaml:"enabled,omitempty"`
	MaxConcurrent      *int  `yaml:"max_concurrent,omitempty"`
	MaxParallelPerTask *int  `yaml:"max_parallel_per_task,omitempty"`
	MinBloomLevel      *int  `yaml:"min_bloom_level,omitempty"`
	MaxExpectedPaths   *int  `yaml:"max_expected_paths,omitempty"`
	MinFailureCount    *int  `yaml:"min_failure_count,omitempty"`
}

// EffectiveEnabled returns the configured enabled flag or false as default.
func (r RolloutConfig) EffectiveEnabled() bool {
	if r.Enabled != nil {
		return *r.Enabled
	}
	return false
}

// EffectiveMaxConcurrent returns the configured limit or 2 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (r RolloutConfig) EffectiveMaxConcurrent() int {
	if r.MaxConcurrent != nil {
		return *r.MaxConcurrent
	}
	return 2
}

// EffectiveMaxParallelPerTask returns the configured limit or 2 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (r RolloutConfig) EffectiveMaxParallelPerTask() int {
	if r.MaxParallelPerTask != nil {
		return *r.MaxParallelPerTask
	}
	return 2
}

// EffectiveMinBloomLevel returns the configured minimum Bloom level or 4 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (r RolloutConfig) EffectiveMinBloomLevel() int {
	if r.MinBloomLevel != nil {
		return *r.MinBloomLevel
	}
	return 4
}

// EffectiveMaxExpectedPaths returns the configured limit or 10 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (r RolloutConfig) EffectiveMaxExpectedPaths() int {
	if r.MaxExpectedPaths != nil {
		return *r.MaxExpectedPaths
	}
	return 10
}

// EffectiveMinFailureCount returns the configured minimum or 1 as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (r RolloutConfig) EffectiveMinFailureCount() int {
	if r.MinFailureCount != nil {
		return *r.MinFailureCount
	}
	return 1
}
