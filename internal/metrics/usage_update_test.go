package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/msageha/maestro_v2/internal/model"
)

// fakeUsageCollector returns a fixed aggregate (or error) for handler tests.
type fakeUsageCollector struct {
	agg *UsageAggregate
	err error
}

func (f *fakeUsageCollector) Collect() (*UsageAggregate, error) { return f.agg, f.err }
func (f *fakeUsageCollector) Source() string                    { return "fake collector" }

// recordingLogger captures Warnf lines.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) Warnf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *recordingLogger) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func writeCostTrackingConfig(t *testing.T, maestroDir string, enabled bool, totalUSD, perCommandUSD float64) {
	t.Helper()
	cfg := fmt.Sprintf(`project:
  name: "test"
maestro:
  version: "2.0.0"
agents:
  orchestrator:
    id: "orchestrator"
    model: "claude-opus-4-8"
  planner:
    id: "planner"
    model: "claude-opus-4-8"
  workers:
    count: 2
    default_model: "claude-sonnet-5"
    models:
      worker2: "codex"
cost_tracking:
  enabled: %v
  budget:
    total_usd: %v
    per_command_usd: %v
`, enabled, totalUSD, perCommandUSD)
	if err := os.WriteFile(filepath.Join(maestroDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sampleAggregate() *UsageAggregate {
	agg := newUsageAggregate()
	addTokens(agg.Agents, "worker1", "claude-sonnet-5", model.TokenTotals{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	addTokens(agg.Commands, "cmd-1", "claude-sonnet-5", model.TokenTotals{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	return agg
}

func runUpdateMetricsWithUsage(t *testing.T, maestroDir string, h *Handler) model.Metrics {
	t.Helper()
	err := h.UpdateMetrics(model.CommandQueue{}, nil, model.NotificationQueue{}, time.Now(), &ScanCounters{}, Gauges{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(maestroDir, "state", "metrics.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m model.Metrics
	if err := yamlv3.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUpdateMetrics_UsageDisabledByDefault(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	writeCostTrackingConfig(t, maestroDir, false, 0, 0)
	h := newTestHandler(maestroDir)
	h.SetUsageCollector(&fakeUsageCollector{agg: sampleAggregate()})

	m := runUpdateMetricsWithUsage(t, maestroDir, h)
	if m.Usage != nil {
		t.Errorf("usage section must be absent when cost_tracking is disabled, got %+v", m.Usage)
	}
}

func TestUpdateMetrics_UsageEnabled(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	writeCostTrackingConfig(t, maestroDir, true, 0, 0)
	h := newTestHandler(maestroDir)
	h.SetUsageCollector(&fakeUsageCollector{agg: sampleAggregate()})

	m := runUpdateMetricsWithUsage(t, maestroDir, h)
	if m.Usage == nil {
		t.Fatal("usage section missing")
	}
	// worker1 (claude-code, collected), worker2 (codex, unknown),
	// planner + orchestrator (claude-code, zero usage).
	w1 := m.Usage.Agents["worker1"]
	if w1 == nil || !w1.TokensKnown || w1.Totals.InputTokens != 1_000_000 {
		t.Errorf("worker1 usage = %+v, want known with 1M input", w1)
	}
	// sonnet $3/$15: 1M in + 1M out = $18
	if w1.EstimatedCostUSD == nil || *w1.EstimatedCostUSD != 18.0 {
		t.Errorf("worker1 cost = %v, want 18.0", w1.EstimatedCostUSD)
	}
	w2 := m.Usage.Agents["worker2"]
	if w2 == nil || w2.TokensKnown || w2.Runtime != model.RuntimeCodex {
		t.Errorf("worker2 usage = %+v, want codex tokens_known=false", w2)
	}
	if !m.Usage.Partial {
		t.Error("partial must be true when a codex worker is configured")
	}
	cmd := m.Usage.Commands["cmd-1"]
	if cmd == nil || cmd.EstimatedCostUSD == nil || *cmd.EstimatedCostUSD != 18.0 {
		t.Errorf("cmd-1 usage = %+v, want cost 18.0", cmd)
	}
	if len(m.Usage.BudgetAlerts) != 0 {
		t.Errorf("no budget configured, alerts = %v", m.Usage.BudgetAlerts)
	}
}

func TestUpdateMetrics_BudgetAlerts(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	writeCostTrackingConfig(t, maestroDir, true, 10.0, 5.0)
	logger := &recordingLogger{}
	clk := &settableClock{now: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)}
	h := NewHandler(maestroDir, logger, clk)
	h.SetUsageCollector(&fakeUsageCollector{agg: sampleAggregate()})

	m := runUpdateMetricsWithUsage(t, maestroDir, h)
	if m.Usage == nil {
		t.Fatal("usage section missing")
	}
	if len(m.Usage.BudgetAlerts) != 2 {
		t.Fatalf("alerts = %v, want total + per-command", m.Usage.BudgetAlerts)
	}
	warned := strings.Join(logger.all(), "\n")
	if !strings.Contains(warned, "usage_budget_exceeded") {
		t.Errorf("expected usage_budget_exceeded WARN, got logs:\n%s", warned)
	}

	// A second scan with unchanged overrun must not re-log the same alert.
	// The clock advances past the collection throttle so the alert
	// evaluation (and its dedup) genuinely re-runs.
	clk.now = clk.now.Add(2 * model.DefaultCostCollectIntervalSec * time.Second)
	before := len(logger.all())
	_ = runUpdateMetricsWithUsage(t, maestroDir, h)
	var newWarns int
	for _, l := range logger.all()[before:] {
		if strings.Contains(l, "usage_budget_exceeded") {
			newWarns++
		}
	}
	if newWarns != 0 {
		t.Errorf("persistent alert re-logged %d times, want 0", newWarns)
	}
}

func TestUpdateMetrics_CollectorErrorDegradesToPartial(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	writeCostTrackingConfig(t, maestroDir, true, 0, 0)
	logger := &recordingLogger{}
	h := NewHandler(maestroDir, logger, testClock{})
	h.SetUsageCollector(&fakeUsageCollector{err: errors.New("boom")})

	m := runUpdateMetricsWithUsage(t, maestroDir, h)
	if m.Usage == nil {
		t.Fatal("usage section missing — collector errors must not drop the section")
	}
	if !m.Usage.Partial {
		t.Error("partial must be true when collection failed")
	}
	warned := strings.Join(logger.all(), "\n")
	if !strings.Contains(warned, "usage_collect_failed") {
		t.Errorf("expected usage_collect_failed WARN, got:\n%s", warned)
	}
}

// countingCollector counts Collect calls so throttle tests can assert how
// many collection passes actually ran.
type countingCollector struct {
	calls int
	agg   *UsageAggregate
}

func (c *countingCollector) Collect() (*UsageAggregate, error) { c.calls++; return c.agg, nil }
func (c *countingCollector) Source() string                    { return "counting collector" }

// settableClock is a Clock whose time the test advances explicitly.
type settableClock struct{ now time.Time }

func (c *settableClock) Now() time.Time { return c.now }

func TestUpdateMetrics_UsageCollectionThrottled(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	writeCostTrackingConfig(t, maestroDir, true, 0, 0) // no collect_interval_sec: default 60s
	clk := &settableClock{now: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)}
	h := NewHandler(maestroDir, discardLogger{}, clk)
	collector := &countingCollector{agg: sampleAggregate()}
	h.SetUsageCollector(collector)

	m := runUpdateMetricsWithUsage(t, maestroDir, h)
	if collector.calls != 1 {
		t.Fatalf("first scan collect calls = %d, want 1", collector.calls)
	}
	if m.Usage == nil {
		t.Fatal("usage section missing after first scan")
	}
	firstCollectedAt := m.Usage.CollectedAt

	// Second scan inside the interval: no collection pass, and the previous
	// usage section is preserved (not dropped, not recomputed).
	clk.now = clk.now.Add(10 * time.Second)
	m = runUpdateMetricsWithUsage(t, maestroDir, h)
	if collector.calls != 1 {
		t.Errorf("throttled scan collect calls = %d, want still 1", collector.calls)
	}
	if m.Usage == nil {
		t.Fatal("throttled scan must keep the previous usage section")
	}
	if m.Usage.CollectedAt != firstCollectedAt {
		t.Errorf("throttled scan collected_at = %s, want unchanged %s", m.Usage.CollectedAt, firstCollectedAt)
	}

	// Past the interval: collection runs again.
	clk.now = clk.now.Add(51 * time.Second) // 61s since the first pass
	m = runUpdateMetricsWithUsage(t, maestroDir, h)
	if collector.calls != 2 {
		t.Errorf("post-interval scan collect calls = %d, want 2", collector.calls)
	}
	if m.Usage == nil || m.Usage.CollectedAt == firstCollectedAt {
		t.Errorf("post-interval scan must recompute usage, got %+v", m.Usage)
	}
}

func TestUpdateMetrics_UsageCollectIntervalZeroCollectsEveryScan(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := `project:
  name: "test"
maestro:
  version: "2.0.0"
agents:
  orchestrator:
    id: "orchestrator"
    model: "claude-opus-4-8"
  planner:
    id: "planner"
    model: "claude-opus-4-8"
  workers:
    count: 1
    default_model: "claude-sonnet-5"
cost_tracking:
  enabled: true
  collect_interval_sec: 0
`
	if err := os.WriteFile(filepath.Join(maestroDir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := &settableClock{now: time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)}
	h := NewHandler(maestroDir, discardLogger{}, clk)
	collector := &countingCollector{agg: sampleAggregate()}
	h.SetUsageCollector(collector)

	_ = runUpdateMetricsWithUsage(t, maestroDir, h)
	_ = runUpdateMetricsWithUsage(t, maestroDir, h) // same clock instant
	if collector.calls != 2 {
		t.Errorf("collect_interval_sec=0 collect calls = %d, want 2 (every scan)", collector.calls)
	}
}

func TestUpdateMetrics_NoConfigFileSkipsUsage(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	// No config.yaml at all: usage update is skipped, metrics still written.
	h := newTestHandler(maestroDir)
	m := runUpdateMetricsWithUsage(t, maestroDir, h)
	if m.Usage != nil {
		t.Errorf("usage = %+v, want nil without config", m.Usage)
	}
}

func TestAssembleUsageMetrics_UnknownRuntimeNotZeroSpend(t *testing.T) {
	t.Parallel()
	cfg := model.Config{}
	cfg.Agents.Planner = model.AgentConfig{ID: "planner", Model: "claude-opus-4-8"}
	cfg.Agents.Orchestrator = model.AgentConfig{ID: "orchestrator", Model: "claude-opus-4-8"}
	cfg.Agents.Workers.Count = 1
	cfg.Agents.Workers.DefaultModel = "gemini"

	usage := assembleUsageMetrics(cfg, newUsageAggregate(), "src", time.Now())
	w1 := usage.Agents["worker1"]
	if w1 == nil || w1.TokensKnown || w1.Runtime != model.RuntimeGemini {
		t.Errorf("worker1 = %+v, want gemini tokens_known=false", w1)
	}
	if !usage.Partial {
		t.Error("partial must be true with a gemini worker")
	}
	if w1.EstimatedCostUSD != nil {
		t.Error("unknown runtime must not carry a cost estimate")
	}
}
