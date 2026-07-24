//revive:disable-next-line:var-naming // package name is intentional; see types.go
package metrics

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// SetUsageCollector overrides the usage collector (tests / alternative
// sources). Passing nil restores the default lazy claude-code collector.
func (h *Handler) SetUsageCollector(c UsageCollector) {
	h.usageCollector = c
	h.usageCollectorSet = c != nil
}

// updateUsage recomputes the metrics.Usage section. Opt-in and best-effort by
// design: disabled config clears the section, and every failure degrades to a
// WARN + keep-previous-data rather than an error — cost tracking must never
// wedge the scan loop.
func (h *Handler) updateUsage(m *model.Metrics) {
	cfg, err := model.LoadConfig(h.maestroDir)
	if err != nil {
		h.logger.Warnf("usage_config_load_failed error=%v", err)
		return
	}
	if !cfg.CostTracking.EffectiveEnabled() {
		m.Usage = nil
		return
	}

	collector := h.usageCollector
	if collector == nil && !h.usageCollectorSet {
		projectRoot := cfg.Maestro.ProjectRoot
		if projectRoot == "" {
			projectRoot = filepath.Dir(h.maestroDir)
		}
		collector = NewClaudeSessionUsageCollector(projectRoot, h.logger)
		h.usageCollector = collector
	}
	if collector == nil {
		return
	}

	agg, err := collector.Collect()
	collectFailed := err != nil
	if collectFailed {
		h.logger.Warnf("usage_collect_failed error=%v", err)
	}
	if agg == nil {
		agg = newUsageAggregate()
	}

	usage := assembleUsageMetrics(cfg, agg, collector.Source(), h.clock.Now())
	if collectFailed {
		usage.Partial = true
	}
	usage.BudgetAlerts = evaluateBudgetAlerts(cfg.CostTracking.Budget, usage)
	h.logNewBudgetAlerts(m.Usage, usage)
	m.Usage = usage
}

// logNewBudgetAlerts WARN-logs alerts that were not present in the previous
// scan's usage section, so a persistent overrun does not spam one WARN per
// scan tick.
func (h *Handler) logNewBudgetAlerts(prev, next *model.UsageMetrics) {
	known := make(map[string]struct{})
	if prev != nil {
		for _, a := range prev.BudgetAlerts {
			known[a] = struct{}{}
		}
	}
	for _, a := range next.BudgetAlerts {
		if _, ok := known[a]; !ok {
			h.logger.Warnf("usage_budget_exceeded %s", a)
		}
	}
}

// assembleUsageMetrics converts a raw aggregate into the persisted usage
// section, filling in configured agents that have no collectable path as
// tokens_known=false ("unknown", never fabricated zeros).
func assembleUsageMetrics(cfg model.Config, agg *UsageAggregate, source string, now time.Time) *model.UsageMetrics {
	usage := &model.UsageMetrics{
		Source:      source,
		CollectedAt: now.UTC().Format(time.RFC3339),
		Agents:      make(map[string]*model.AgentUsage),
		Commands:    make(map[string]*model.CommandUsage),
	}

	// Configured roster: planner / orchestrator / workers.
	type rosterEntry struct {
		id    string
		model string
	}
	roster := []rosterEntry{
		{cfg.Agents.Planner.ID, cfg.Agents.Planner.Model},
		{cfg.Agents.Orchestrator.ID, cfg.Agents.Orchestrator.Model},
	}
	for i := 1; i <= cfg.Agents.Workers.Count; i++ {
		id := fmt.Sprintf("worker%d", i)
		roster = append(roster, rosterEntry{id, model.ResolveWorkerModel(id, cfg)})
	}
	for _, entry := range roster {
		if entry.id == "" {
			continue
		}
		runtime, _ := model.ParseRuntimeFromModel(entry.model)
		known := runtime == model.RuntimeClaudeCode
		usage.Agents[entry.id] = &model.AgentUsage{Runtime: runtime, TokensKnown: known}
		if !known {
			usage.Partial = true
		}
	}

	// Fold in collected tokens. Agents observed in session files but absent
	// from the roster (renamed roles, stale sessions) are still reported.
	for agentID, byModel := range agg.Agents {
		au, ok := usage.Agents[agentID]
		if !ok {
			au = &model.AgentUsage{Runtime: model.RuntimeClaudeCode, TokensKnown: true}
			usage.Agents[agentID] = au
		}
		// Session-file evidence outranks the roster's runtime inference:
		// tokens were collectable, so mark them known.
		au.TokensKnown = true
		au.ByModel = byModel
		for _, t := range byModel {
			au.Totals.Add(t)
		}
		if cost, ok := EstimateCostForModels(byModel); ok {
			c := cost
			au.EstimatedCostUSD = &c
		}
	}

	for commandID, byModel := range agg.Commands {
		cu := &model.CommandUsage{ByModel: byModel}
		for _, t := range byModel {
			cu.Totals.Add(t)
		}
		if cost, ok := EstimateCostForModels(byModel); ok {
			c := cost
			cu.EstimatedCostUSD = &c
		}
		usage.Commands[commandID] = cu
	}
	return usage
}

// evaluateBudgetAlerts compares known estimated costs against configured
// thresholds. Alerts are deterministic strings so the dashboard can render
// them and the handler can dedupe WARN logs across scans.
func evaluateBudgetAlerts(budget model.CostBudgetConfig, usage *model.UsageMetrics) []string {
	var alerts []string
	if total := budget.EffectiveTotalUSD(); total > 0 {
		sum := 0.0
		for _, au := range usage.Agents {
			if au.EstimatedCostUSD != nil {
				sum += *au.EstimatedCostUSD
			}
		}
		if sum > total {
			suffix := ""
			if usage.Partial {
				suffix = " (partial: unknown-runtime agents excluded)"
			}
			alerts = append(alerts, fmt.Sprintf(
				"total estimated cost $%.2f exceeds budget $%.2f%s", sum, total, suffix))
		}
	}
	if perCmd := budget.EffectivePerCommandUSD(); perCmd > 0 {
		ids := make([]string, 0, len(usage.Commands))
		for id := range usage.Commands {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			cu := usage.Commands[id]
			if cu.EstimatedCostUSD != nil && *cu.EstimatedCostUSD > perCmd {
				alerts = append(alerts, fmt.Sprintf(
					"command %s estimated cost $%.2f exceeds per-command budget $%.2f",
					id, *cu.EstimatedCostUSD, perCmd))
			}
		}
	}
	return alerts
}
