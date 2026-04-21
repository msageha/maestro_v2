package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/bandit"
	"github.com/msageha/maestro_v2/internal/model"
)

// banditModelSelector adapts a *bandit.Selector + BanditConfig to the
// core.ModelSelector interface. It implements the same gated-selection
// policy as plan.AdaptiveModelSelector but lives on the daemon side so
// result_handler can also report rewards without depending on plan.
//
// Semantics:
//   - When the bandit is disabled or nil, SelectModel returns "" so the
//     plan-side AssignWorkers keeps its static BloomLevel→model mapping.
//   - Before enough traces / per-arm samples have accumulated, SelectModel
//     also returns "" to avoid biasing early assignments.
//   - Once warmed up, returns the UCB1-selected arm name.
type banditModelSelector struct {
	bandit     *bandit.Selector
	enabled    bool
	traceReq   int
	minSamples int
}

// newBanditModelSelector wraps the daemon's phaseC bandit selector.
// Returns nil when the bandit is not configured; callers should treat nil
// as "no adaptive selection" (static fallback remains in effect).
func newBanditModelSelector(sel *bandit.Selector, cfg model.BanditConfig) *banditModelSelector {
	if sel == nil || !cfg.EffectiveEnabled() {
		return nil
	}
	return &banditModelSelector{
		bandit:     sel,
		enabled:    true,
		traceReq:   cfg.EffectiveTraceDataRequirement(),
		minSamples: cfg.EffectiveMinSamplesBeforeUse(),
	}
}

// SelectModel returns an empty string when the bandit has not yet met its
// warm-up thresholds, signaling to AssignWorkers to keep the static model.
// Otherwise returns the UCB1-selected arm.
func (b *banditModelSelector) SelectModel(_ int, _ string) string {
	if b == nil || !b.enabled || b.bandit == nil {
		return ""
	}
	pulls := b.bandit.PullCounts()
	var total int64
	for _, n := range pulls {
		total += n
	}
	if total < int64(b.traceReq) {
		return ""
	}
	for _, n := range pulls {
		if n < int64(b.minSamples) {
			return ""
		}
	}
	arm, err := b.bandit.SelectArm()
	if err != nil {
		return ""
	}
	return arm
}

// RecordResult feeds an observed reward back into the bandit. No-op when
// the selector is disabled or the model name is empty.
func (b *banditModelSelector) RecordResult(modelName string, reward float64) {
	if b == nil || !b.enabled || b.bandit == nil || modelName == "" {
		return
	}
	b.bandit.UpdateReward(modelName, reward)
}
