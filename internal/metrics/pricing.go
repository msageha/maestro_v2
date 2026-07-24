//revive:disable-next-line:var-naming // package name is intentional; see types.go
package metrics

import (
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// ModelPrice holds USD-per-million-token prices for one model family.
//
// Price tables go stale; this is why tokens (not dollars) are the primary
// record in state/metrics.yaml — estimated costs can be recomputed from the
// persisted per-model token counts after updating this table.
//
// Source: Anthropic model pricing as bundled in the claude-api reference
// (cached 2026-06-24). Cache multipliers follow the documented prompt-caching
// economics: reads at 0.1x input, 5-minute-TTL writes at 1.25x input,
// 1-hour-TTL writes at 2x input.
type ModelPrice struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

const (
	cacheReadMultiplier    = 0.1
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.0
)

// modelPriceTable maps model-ID prefixes to prices. Longest prefix wins.
// Models that do not match any prefix have unknown pricing: their tokens are
// recorded but no cost is derived.
var modelPriceTable = []struct {
	prefix string
	price  ModelPrice
}{
	{"claude-fable-5", ModelPrice{10, 50}},
	{"claude-mythos-5", ModelPrice{10, 50}},
	// Opus 4.5 and later are $5/$25; the older 4.0/4.1 generation was
	// $15/$75 and must not fall through to the cheaper prefix.
	{"claude-opus-4-0", ModelPrice{15, 75}},
	{"claude-opus-4-1", ModelPrice{15, 75}},
	{"claude-opus-4-2025", ModelPrice{15, 75}}, // claude-opus-4-20250514 full ID
	{"claude-opus-4", ModelPrice{5, 25}},
	{"claude-sonnet-5", ModelPrice{3, 15}},
	{"claude-sonnet-4", ModelPrice{3, 15}},
	{"claude-haiku-4-5", ModelPrice{1, 5}},
}

// familyAliasPrices prices the short aliases used in maestro config
// (workers.models: "opus" etc.) that can also appear as the model field in
// session records when the CLI echoes the alias.
var familyAliasPrices = map[string]ModelPrice{
	"opus":   {5, 25},
	"sonnet": {3, 15},
	"haiku":  {1, 5},
}

// LookupModelPrice returns the price for a model ID, matching by longest
// prefix after stripping maestro's "[1m]" context-window suffix. ok=false
// means the model has no known price and cost must be reported as unknown.
func LookupModelPrice(modelID string) (ModelPrice, bool) {
	name := strings.TrimSuffix(strings.TrimSpace(modelID), "[1m]")
	if p, ok := familyAliasPrices[name]; ok {
		return p, true
	}
	best := -1
	var found ModelPrice
	for _, entry := range modelPriceTable {
		if strings.HasPrefix(name, entry.prefix) && len(entry.prefix) > best {
			best = len(entry.prefix)
			found = entry.price
		}
	}
	if best < 0 {
		return ModelPrice{}, false
	}
	return found, true
}

// EstimateCostUSD derives the estimated cost for token totals under a model.
// ok=false means the model's price is unknown and no estimate is produced.
//
// Cache-write pricing uses the ephemeral 5m/1h split when the runtime
// reported it; otherwise the whole cache_creation count is priced at the
// 5-minute-TTL multiplier (1.25x), which may underestimate 1-hour-TTL writes.
func EstimateCostUSD(modelID string, t model.TokenTotals) (float64, bool) {
	price, ok := LookupModelPrice(modelID)
	if !ok {
		return 0, false
	}
	const mtok = 1_000_000.0
	cost := float64(t.InputTokens) / mtok * price.InputPerMTok
	cost += float64(t.OutputTokens) / mtok * price.OutputPerMTok
	cost += float64(t.CacheReadInputTokens) / mtok * price.InputPerMTok * cacheReadMultiplier

	split := t.CacheCreation5mTokens + t.CacheCreation1hTokens
	if split > 0 {
		cost += float64(t.CacheCreation5mTokens) / mtok * price.InputPerMTok * cacheWrite5mMultiplier
		cost += float64(t.CacheCreation1hTokens) / mtok * price.InputPerMTok * cacheWrite1hMultiplier
		// Any remainder not covered by the split falls back to the 5m rate.
		if rest := t.CacheCreationInputTokens - split; rest > 0 {
			cost += float64(rest) / mtok * price.InputPerMTok * cacheWrite5mMultiplier
		}
	} else {
		cost += float64(t.CacheCreationInputTokens) / mtok * price.InputPerMTok * cacheWrite5mMultiplier
	}
	return cost, true
}

// EstimateCostForModels sums the estimated cost over a per-model token map.
// known is false when at least one model in the map has unknown pricing (the
// returned cost then covers only the priced models).
func EstimateCostForModels(byModel map[string]model.TokenTotals) (cost float64, known bool) {
	known = true
	for modelID, totals := range byModel {
		c, ok := EstimateCostUSD(modelID, totals)
		if !ok {
			known = false
			continue
		}
		cost += c
	}
	return cost, known
}
