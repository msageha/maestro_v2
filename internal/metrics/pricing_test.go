package metrics

import (
	"math"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestLookupModelPrice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		modelID string
		wantIn  float64
		wantOut float64
		wantOK  bool
	}{
		{"fable", "claude-fable-5", 10, 50, true},
		{"opus 4.8", "claude-opus-4-8", 5, 25, true},
		{"opus 4.5 dated", "claude-opus-4-5-20251101", 5, 25, true},
		{"opus 4.1 legacy pricing", "claude-opus-4-1-20250805", 15, 75, true},
		{"opus 4.0 legacy pricing", "claude-opus-4-0", 15, 75, true},
		{"sonnet 5 with 1m suffix", "claude-sonnet-5[1m]", 3, 15, true},
		{"sonnet 4.6", "claude-sonnet-4-6", 3, 15, true},
		{"haiku", "claude-haiku-4-5-20251001", 1, 5, true},
		{"family alias opus", "opus", 5, 25, true},
		{"family alias sonnet", "sonnet", 3, 15, true},
		{"unknown model", "gpt-5", 0, 0, false},
		{"codex", "codex", 0, 0, false},
		{"gemini", "gemini-2.5-pro", 0, 0, false},
		{"empty", "", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, ok := LookupModelPrice(tt.modelID)
			if ok != tt.wantOK {
				t.Fatalf("LookupModelPrice(%q) ok=%v, want %v", tt.modelID, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if p.InputPerMTok != tt.wantIn || p.OutputPerMTok != tt.wantOut {
				t.Errorf("LookupModelPrice(%q) = %+v, want in=%v out=%v", tt.modelID, p, tt.wantIn, tt.wantOut)
			}
		})
	}
}

func TestEstimateCostUSD(t *testing.T) {
	t.Parallel()
	// 1M of each counter on sonnet ($3/$15):
	// input 3 + output 15 + cache_read 0.3 + cache_write(5m) 3.75 = 22.05
	totals := model.TokenTotals{
		InputTokens:              1_000_000,
		OutputTokens:             1_000_000,
		CacheReadInputTokens:     1_000_000,
		CacheCreationInputTokens: 1_000_000,
	}
	cost, ok := EstimateCostUSD("claude-sonnet-5", totals)
	if !ok {
		t.Fatal("expected known price for claude-sonnet-5")
	}
	if math.Abs(cost-22.05) > 1e-9 {
		t.Errorf("cost = %v, want 22.05", cost)
	}
}

func TestEstimateCostUSD_CacheTTLSplit(t *testing.T) {
	t.Parallel()
	// With an explicit 5m/1h split, 1h writes are priced at 2x input.
	// sonnet input $3: 1M @1h -> 6.0; same 1M without split -> 3.75.
	withSplit := model.TokenTotals{
		CacheCreationInputTokens: 1_000_000,
		CacheCreation1hTokens:    1_000_000,
	}
	cost, ok := EstimateCostUSD("claude-sonnet-5", withSplit)
	if !ok || math.Abs(cost-6.0) > 1e-9 {
		t.Errorf("1h split cost = %v (ok=%v), want 6.0", cost, ok)
	}

	noSplit := model.TokenTotals{CacheCreationInputTokens: 1_000_000}
	cost, ok = EstimateCostUSD("claude-sonnet-5", noSplit)
	if !ok || math.Abs(cost-3.75) > 1e-9 {
		t.Errorf("no-split cost = %v (ok=%v), want 3.75", cost, ok)
	}
}

func TestEstimateCostUSD_UnknownModel(t *testing.T) {
	t.Parallel()
	if _, ok := EstimateCostUSD("codex", model.TokenTotals{InputTokens: 100}); ok {
		t.Error("expected unknown price for codex")
	}
}

func TestEstimateCostForModels_MixedKnown(t *testing.T) {
	t.Parallel()
	byModel := map[string]model.TokenTotals{
		"claude-sonnet-5": {OutputTokens: 1_000_000}, // $15
		"totally-unknown": {OutputTokens: 1_000_000}, // unpriced
	}
	cost, known := EstimateCostForModels(byModel)
	if known {
		t.Error("expected known=false when a model has no price")
	}
	if math.Abs(cost-15.0) > 1e-9 {
		t.Errorf("cost = %v, want 15.0 (priced models only)", cost)
	}
}
