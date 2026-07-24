package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeUsageMetricsFixture(t *testing.T, maestroDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(maestroDir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `schema_version: 1
file_type: "state_metrics"
usage:
  source: "claude-code session files (~/.claude/projects)"
  partial: true
  collected_at: "2026-07-23T10:00:00Z"
  agents:
    worker1:
      runtime: claude-code
      tokens_known: true
      totals:
        input_tokens: 1000
        output_tokens: 200
        cache_read_input_tokens: 50
        cache_creation_input_tokens: 30
      estimated_cost_usd: 0.1234
    worker2:
      runtime: codex
      tokens_known: false
  commands:
    cmd-1:
      totals:
        input_tokens: 1000
        output_tokens: 200
        cache_read_input_tokens: 50
        cache_creation_input_tokens: 30
      estimated_cost_usd: 0.1234
  budget_alerts:
    - "command cmd-1 estimated cost $0.12 exceeds per-command budget $0.10"
`
	if err := os.WriteFile(filepath.Join(maestroDir, "state", "metrics.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWriteUsageSection_RendersUsageAndAlerts(t *testing.T) {
	t.Parallel()
	maestroDir := t.TempDir()
	writeUsageMetricsFixture(t, maestroDir)
	f := NewDashboardFormatter(maestroDir)

	var sb strings.Builder
	f.writeUsageSection(&sb)
	out := sb.String()

	for _, want := range []string{
		"## Cost / Token Usage",
		"| worker1 | claude-code | 1000 | 200 | 50 | 30 | $0.1234 |",
		"| worker2 | codex | unknown | unknown | unknown | unknown | unknown |",
		"| `cmd-1` | 1000 | 200 | 50 | 30 | $0.1234 |",
		"Partial data",
		"BUDGET: command cmd-1 estimated cost $0.12 exceeds per-command budget $0.10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage section missing %q:\n%s", want, out)
		}
	}
}

func TestWriteUsageSection_SilentWithoutUsage(t *testing.T) {
	t.Parallel()
	maestroDir := t.TempDir()
	f := NewDashboardFormatter(maestroDir)

	var sb strings.Builder
	f.writeUsageSection(&sb)
	if sb.Len() != 0 {
		t.Errorf("expected no output without metrics.yaml, got:\n%s", sb.String())
	}

	// metrics.yaml without a usage section is also silent.
	if err := os.MkdirAll(filepath.Join(maestroDir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(maestroDir, "state", "metrics.yaml"),
		[]byte("schema_version: 1\nfile_type: \"state_metrics\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb.Reset()
	f.writeUsageSection(&sb)
	if sb.Len() != 0 {
		t.Errorf("expected no output without usage section, got:\n%s", sb.String())
	}
}
