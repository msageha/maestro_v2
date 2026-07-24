package status

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func f64(v float64) *float64 { return &v }

func TestSummarizeUsage_Nil(t *testing.T) {
	t.Parallel()
	if got := summarizeUsage(nil); got != nil {
		t.Errorf("summarizeUsage(nil) = %+v, want nil", got)
	}
}

func TestSummarizeUsage_SumsKnownAgentsOnly(t *testing.T) {
	t.Parallel()
	u := &model.UsageMetrics{
		Partial:     true,
		CollectedAt: "2026-07-23T10:00:00Z",
		Agents: map[string]*model.AgentUsage{
			"worker1": {
				Runtime:          model.RuntimeClaudeCode,
				TokensKnown:      true,
				Totals:           model.TokenTotals{InputTokens: 100, OutputTokens: 10},
				EstimatedCostUSD: f64(1.5),
			},
			"worker2": {Runtime: model.RuntimeCodex, TokensKnown: false},
		},
		BudgetAlerts: []string{"total estimated cost $1.50 exceeds budget $1.00"},
	}
	got := summarizeUsage(u)
	if got.InputTokens != 100 || got.OutputTokens != 10 {
		t.Errorf("tokens = in=%d out=%d, want 100/10", got.InputTokens, got.OutputTokens)
	}
	if got.EstimatedCostUSD == nil || *got.EstimatedCostUSD != 1.5 {
		t.Errorf("cost = %v, want 1.5", got.EstimatedCostUSD)
	}
	if len(got.UnknownAgents) != 1 || got.UnknownAgents[0] != "worker2" {
		t.Errorf("unknown agents = %v, want [worker2]", got.UnknownAgents)
	}
	if !got.Partial || len(got.BudgetAlerts) != 1 {
		t.Errorf("partial/alerts not propagated: %+v", got)
	}
}

func TestFormatUsageLines(t *testing.T) {
	t.Parallel()
	if lines := formatUsageLines(nil); lines != nil {
		t.Errorf("formatUsageLines(nil) = %v, want nil", lines)
	}

	u := &usageStatus{
		CollectedAt:      "2026-07-23T10:00:00Z",
		Partial:          true,
		InputTokens:      100,
		OutputTokens:     10,
		EstimatedCostUSD: f64(1.5),
		UnknownAgents:    []string{"worker2"},
		BudgetAlerts:     []string{"over budget"},
	}
	out := strings.Join(formatUsageLines(u), "\n")
	for _, want := range []string{"in=100", "out=10", "$1.5000", "[partial]", "worker2", "BUDGET ALERT: over budget", "2026-07-23T10:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Unknown pricing renders as unknown, never $0.
	noCost := &usageStatus{InputTokens: 5}
	out = strings.Join(formatUsageLines(noCost), "\n")
	if !strings.Contains(out, "unknown (no price data)") {
		t.Errorf("expected unknown cost marker:\n%s", out)
	}
}

func TestGetUsageStatus_ReadsMetricsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `schema_version: 1
file_type: "state_metrics"
usage:
  source: "claude-code session files"
  partial: false
  collected_at: "2026-07-23T10:00:00Z"
  agents:
    worker1:
      runtime: claude-code
      tokens_known: true
      totals:
        input_tokens: 42
        output_tokens: 7
        cache_read_input_tokens: 0
        cache_creation_input_tokens: 0
      estimated_cost_usd: 0.25
`
	if err := os.WriteFile(filepath.Join(dir, "state", "metrics.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	got := getUsageStatus(dir)
	if got == nil {
		t.Fatal("expected usage status")
	}
	if got.InputTokens != 42 || got.OutputTokens != 7 {
		t.Errorf("tokens = %d/%d, want 42/7", got.InputTokens, got.OutputTokens)
	}
	if got.EstimatedCostUSD == nil || *got.EstimatedCostUSD != 0.25 {
		t.Errorf("cost = %v, want 0.25", got.EstimatedCostUSD)
	}
}

func TestGetUsageStatus_AbsentFileOrSection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got := getUsageStatus(dir); got != nil {
		t.Errorf("missing metrics.yaml: got %+v, want nil", got)
	}
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "metrics.yaml"),
		[]byte("schema_version: 1\nfile_type: \"state_metrics\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := getUsageStatus(dir); got != nil {
		t.Errorf("metrics without usage: got %+v, want nil", got)
	}
}
