package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/daemon/bandit"
	"github.com/msageha/maestro_v2/internal/model"
)

// Bloom-difficulty buckets for contextual model selection. Buckets are
// intentionally coarser than per-level (1..6): with the default warm-up
// thresholds (trace_data_requirement=50, min_samples_before_use=10 per arm)
// six per-level buckets would rarely accumulate enough samples to activate,
// while three difficulty bands keep warm-up reachable at realistic task
// volumes.
const (
	bloomBucketInvalid = -1
	bloomBucketLow     = 0 // Bloom 1-2
	bloomBucketMid     = 1 // Bloom 3-4
	bloomBucketHigh    = 2 // Bloom 5-6
	bloomBucketCount   = 3
)

// bloomBucket maps a Bloom taxonomy level to its difficulty bucket.
// Out-of-range levels (including 0 = unset) return bloomBucketInvalid and
// are served by the global selector only.
func bloomBucket(bloomLevel int) int {
	switch {
	case bloomLevel >= 1 && bloomLevel <= 2:
		return bloomBucketLow
	case bloomLevel >= 3 && bloomLevel <= 4:
		return bloomBucketMid
	case bloomLevel >= 5 && bloomLevel <= 6:
		return bloomBucketHigh
	default:
		return bloomBucketInvalid
	}
}

// banditModelSelector adapts UCB1 bandit selectors to the plan.ModelSelector
// interface. It is the sole production model-selection implementation; the
// daemon owns it (rather than plan) so result_handler can report rewards
// without taking a dependency on plan.
//
// Selection is contextual on the task's Bloom level: each difficulty bucket
// (see bloomBucket) owns an independent UCB1 selector that learns the
// per-difficulty success rate of each model arm. The pre-existing global
// (context-free) selector remains the fallback, so behaviour degrades to the
// old semantics until a bucket clears its own warm-up thresholds.
//
// Reward semantics are success-rate only (Completed=1.0, Failed=0.0 — see
// recordBanditReward). Buckets therefore learn which models *fail* at which
// difficulty, not which model is cheapest: two models that both succeed on
// easy tasks are indistinguishable to this selector.
//
// Semantics:
//   - When the bandit is disabled or nil, SelectModel returns "" so the
//     plan-side AssignWorkers keeps its static BloomLevel→model mapping.
//   - A bucket is consulted only once it has met the warm-up thresholds by
//     itself; otherwise selection falls back to the global selector, which
//     in turn returns "" before its own warm-up completes.
//   - Any bucket-side failure falls back to the global selector rather than
//     aborting selection, so contextual routing never performs worse than
//     the pre-contextual behaviour because of a partial fault.
type banditModelSelector struct {
	global *bandit.Selector
	// buckets holds one selector per Bloom difficulty bucket, indexed by
	// bloomBucket(). nil when bucket construction failed at startup; the
	// selector then degrades to global-only (context-free) behaviour.
	buckets    []*bandit.Selector
	enabled    bool
	traceReq   int
	minSamples int
	// featureEnabled, when non-nil, gates adaptive selection per task
	// difficulty via the feature_profiles config (adaptive_model_selection).
	// SelectModel returns "" (static mapping) for gated-off levels; reward
	// recording is NOT gated so learning continues while selection is off.
	featureEnabled func(bloomLevel int) bool
	log            logFunc
}

// newBanditModelSelector wraps the daemon's phaseC bandit selector.
// Returns nil when the bandit is not configured; callers should treat nil
// as "no adaptive selection" (static fallback remains in effect).
// Bucket selectors replicate the global selector's arms and exploration
// coefficient so all selectors rank the same model set.
func newBanditModelSelector(sel *bandit.Selector, cfg model.BanditConfig, log logFunc) *banditModelSelector {
	if sel == nil || !cfg.EffectiveEnabled() {
		return nil
	}
	if log == nil {
		log = func(LogLevel, string, ...any) {}
	}
	arms := make([]string, 0)
	for name := range sel.PullCounts() {
		arms = append(arms, name)
	}
	buckets := make([]*bandit.Selector, bloomBucketCount)
	for i := range buckets {
		bs, err := bandit.NewSelector(cfg.EffectiveExplorationCoeff())
		if err != nil {
			// Should be unreachable: the global selector was constructed
			// with the same coefficient. Degrade to global-only selection.
			log(LogLevelWarn, "bandit bucket selector init failed: %v; contextual selection disabled", err)
			buckets = nil
			break
		}
		for _, a := range arms {
			bs.AddArm(a)
		}
		buckets[i] = bs
	}
	return &banditModelSelector{
		global:     sel,
		buckets:    buckets,
		enabled:    true,
		traceReq:   cfg.EffectiveTraceDataRequirement(),
		minSamples: cfg.EffectiveMinSamplesBeforeUse(),
		log:        log,
	}
}

// warmedUp reports whether the given selector has accumulated enough total
// pulls (traceReq) and per-arm samples (minSamples) for its UCB1 estimates
// to be trusted. Applied independently to the global selector and to each
// bucket, so a bucket can never activate before the global selector (the
// global selector receives every reward a bucket receives).
func (b *banditModelSelector) warmedUp(sel *bandit.Selector) bool {
	pulls := sel.PullCounts()
	var total int64
	for _, n := range pulls {
		total += n
	}
	if total < int64(b.traceReq) {
		return false
	}
	for _, n := range pulls {
		if n < int64(b.minSamples) {
			return false
		}
	}
	return true
}

// SelectModel picks a model arm for the given Bloom level. Selection order:
//  1. the Bloom-bucket selector, when warmed up (contextual path);
//  2. the global selector, when warmed up (pre-contextual behaviour);
//  3. "" — AssignWorkers keeps the static BloomLevel→model mapping.
func (b *banditModelSelector) SelectModel(bloomLevel int, _ string) string {
	if b == nil || !b.enabled || b.global == nil {
		return ""
	}
	if b.featureEnabled != nil && !b.featureEnabled(bloomLevel) {
		b.log(LogLevelDebug, "bandit_select source=static bloom=%d (adaptive_model_selection gated off for this level)", bloomLevel)
		return ""
	}
	if bucket := bloomBucket(bloomLevel); bucket != bloomBucketInvalid && b.buckets != nil {
		if bs := b.buckets[bucket]; b.warmedUp(bs) {
			if arm, err := bs.SelectArm(); err == nil {
				b.log(LogLevelDebug, "bandit_select source=bucket bloom=%d bucket=%d arm=%s", bloomLevel, bucket, arm)
				return arm
			}
			// Bucket-side failure: fall through to the global selector.
		}
	}
	if !b.warmedUp(b.global) {
		b.log(LogLevelDebug, "bandit_select source=static bloom=%d (global warm-up incomplete)", bloomLevel)
		return ""
	}
	arm, err := b.global.SelectArm()
	if err != nil {
		return ""
	}
	b.log(LogLevelDebug, "bandit_select source=global bloom=%d arm=%s", bloomLevel, arm)
	return arm
}

// banditPersistedState is the JSON schema of the bandit statistics snapshot
// persisted at state/bandit_state.json. Only observed statistics travel;
// arm registration and the exploration coefficient stay config-owned, so a
// changed model set simply drops the orphaned arms on restore.
type banditPersistedState struct {
	SchemaVersion int            `json:"schema_version"`
	Global        bandit.State   `json:"global"`
	Buckets       []bandit.State `json:"buckets,omitempty"`
}

// banditStatePath returns the persisted bandit snapshot location.
func banditStatePath(maestroDir string) string {
	return filepath.Join(maestroDir, "state", "bandit_state.json")
}

// LoadState restores previously persisted arm statistics into the global and
// bucket selectors. Missing file is a clean first start; parse errors start
// fresh with a warning (the snapshot is advisory learning state, never
// authoritative). Without this restore the UCB1 warm-up thresholds
// (trace_data_requirement + min_samples_before_use per arm) reset on every
// daemon restart and adaptive selection effectively never activated (X-A
// audit 2026-07-09).
func (b *banditModelSelector) LoadState(path string) {
	if b == nil {
		return
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from the controlled state directory
	if err != nil {
		if !os.IsNotExist(err) {
			b.log(LogLevelWarn, "bandit state load failed path=%s error=%v; starting fresh", path, err)
		}
		return
	}
	var st banditPersistedState
	if err := json.Unmarshal(data, &st); err != nil {
		b.log(LogLevelWarn, "bandit state parse failed path=%s error=%v; starting fresh", path, err)
		return
	}
	b.global.RestoreState(st.Global)
	if b.buckets != nil {
		for i := range b.buckets {
			if i < len(st.Buckets) {
				b.buckets[i].RestoreState(st.Buckets[i])
			}
		}
	}
	var total int64
	for _, n := range b.global.PullCounts() {
		total += n
	}
	b.log(LogLevelInfo, "bandit state restored path=%s global_pulls=%d", path, total)
}

// SaveState persists the current arm statistics. Called at daemon shutdown
// (same lifecycle as FingerprintDB persistence); a crash loses at most the
// statistics accumulated since the last clean shutdown.
func (b *banditModelSelector) SaveState(path string) error {
	if b == nil {
		return nil
	}
	st := banditPersistedState{SchemaVersion: 1, Global: b.global.ExportState()}
	if b.buckets != nil {
		st.Buckets = make([]bandit.State, len(b.buckets))
		for i, bs := range b.buckets {
			st.Buckets[i] = bs.ExportState()
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bandit state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // learning statistics, not secrets
		return fmt.Errorf("write bandit state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename bandit state: %w", err)
	}
	return nil
}

// RecordResult feeds an observed reward into the global selector and, when
// bloomLevel maps to a difficulty bucket, into that bucket as well. A
// bloomLevel of 0 (unknown — e.g. the daemon restarted between dispatch and
// result, losing the in-memory taskID→bloom record) degrades to a
// global-only update. No-op when the selector is disabled or the model name
// is empty.
func (b *banditModelSelector) RecordResult(modelName string, bloomLevel int, reward float64) {
	if b == nil || !b.enabled || b.global == nil || modelName == "" {
		return
	}
	b.global.UpdateReward(modelName, reward)
	if bucket := bloomBucket(bloomLevel); bucket != bloomBucketInvalid && b.buckets != nil {
		b.buckets[bucket].UpdateReward(modelName, reward)
	}
}
