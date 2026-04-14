package model

import (
	"context"
	"time"
)

// JudgeDecision は Judge による勝者選定の結果を表す。
// WinnerIndex は nil の場合、勝者が決定されなかったことを示す。
type JudgeDecision struct {
	WinnerIndex *int          `yaml:"winner_index"`
	Reasoning   string        `yaml:"reasoning"`
	Model       string        `yaml:"model"`
	Duration    time.Duration `yaml:"duration"`
}

// JudgeFunc は Tie 発生時に呼び出される Judge 関数の型定義。
// scores は比較対象の FitnessScore 群、metadata は各候補の補助情報（nil 可）。
type JudgeFunc func(ctx context.Context, scores []FitnessScore, metadata []map[string]string) (JudgeDecision, error)

// JudgeConfig は Judge 機能の設定を保持する。
type JudgeConfig struct {
	Enabled    *bool   `yaml:"enabled,omitempty"`
	Model      *string `yaml:"model,omitempty"`
	TimeoutSec *int    `yaml:"timeout_sec,omitempty"`
}

// EffectiveEnabled returns the configured enabled flag or false as default.
func (j JudgeConfig) EffectiveEnabled() bool {
	if j.Enabled != nil {
		return *j.Enabled
	}
	return false
}

// EffectiveModel returns the configured model or "opus" as default.
func (j JudgeConfig) EffectiveModel() string {
	if j.Model != nil {
		return *j.Model
	}
	return "opus"
}

// EffectiveTimeoutSec returns the configured timeout or 60 seconds as default.
// nil (unset) returns the default; explicit 0 returns 0.
func (j JudgeConfig) EffectiveTimeoutSec() int {
	if j.TimeoutSec != nil {
		return *j.TimeoutSec
	}
	return 60
}
