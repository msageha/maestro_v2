package model

// Metrics は Daemon が収集するシステム全体のメトリクスを表す。
// キュー深度、カウンター、ヘルスチェック情報を格納する。
type Metrics struct {
	SchemaVersion int             `yaml:"schema_version"`
	FileType      string          `yaml:"file_type"`
	QueueDepth    QueueDepth      `yaml:"queue_depth"`
	Counters      MetricsCounters `yaml:"counters"`
	// WorktreeCommandsStalled is a gauge: number of commands whose integration
	// branch has been flagged as stalled by Phase A's stall detection step.
	WorktreeCommandsStalled int `yaml:"worktree_commands_stalled"`
	// BakFilesCount is a gauge: total number of .bak files present anywhere
	// under the maestro directory at scan time.
	BakFilesCount   int     `yaml:"bak_files_count"`
	DaemonHeartbeat *string `yaml:"daemon_heartbeat"`
	UpdatedAt       *string `yaml:"updated_at"`
	// Usage holds per-agent / per-command token usage and derived cost
	// estimates collected from local runtime session files (currently
	// claude-code only). nil when cost tracking is disabled or has never
	// run. The section is recomputed from source files on every scan
	// (gauge semantics, like TasksCompleted): it self-heals after restarts
	// and shrinks when the runtime prunes its own session history.
	Usage *UsageMetrics `yaml:"usage,omitempty"`
}

// UsageMetrics is the cost/token tracking section of state/metrics.yaml.
// Tokens are the primary record; EstimatedCostUSD values are derived from a
// price table at collection time and can be recomputed from ByModel token
// counts whenever the price table is updated.
type UsageMetrics struct {
	// Source names the collection path (e.g. "claude-code session files
	// (~/.claude/projects)"). Collection never scrapes tmux panes and
	// never calls external billing APIs.
	Source string `yaml:"source"`
	// Partial is true when at least one agent's usage could not be
	// collected (non-claude runtime, or a collector error). Totals are
	// then a lower bound, not a fleet-wide figure.
	Partial bool `yaml:"partial"`
	// CollectedAt is the RFC3339 timestamp of the last successful collection.
	CollectedAt string `yaml:"collected_at"`
	// Agents maps agent ID (worker1..N / planner / orchestrator) to usage.
	Agents map[string]*AgentUsage `yaml:"agents,omitempty"`
	// Commands maps command ID to usage attributed via delivery envelopes.
	Commands map[string]*CommandUsage `yaml:"commands,omitempty"`
	// BudgetAlerts lists active budget-threshold violations (empty when
	// budgets are unset or not exceeded).
	BudgetAlerts []string `yaml:"budget_alerts,omitempty"`
}

// AgentUsage is per-agent token usage. TokensKnown=false marks agents whose
// runtime has no supported collection path (codex / gemini); their token
// fields are zero and MUST be presented as "unknown", not as zero spend.
type AgentUsage struct {
	Runtime          string                 `yaml:"runtime"`
	TokensKnown      bool                   `yaml:"tokens_known"`
	Totals           TokenTotals            `yaml:"totals,omitempty"`
	ByModel          map[string]TokenTotals `yaml:"by_model,omitempty"`
	EstimatedCostUSD *float64               `yaml:"estimated_cost_usd,omitempty"`
}

// CommandUsage is per-command token usage (claude-code agents only; work a
// codex/gemini worker performed for the command is not included).
type CommandUsage struct {
	Totals           TokenTotals            `yaml:"totals,omitempty"`
	ByModel          map[string]TokenTotals `yaml:"by_model,omitempty"`
	EstimatedCostUSD *float64               `yaml:"estimated_cost_usd,omitempty"`
}

// TokenTotals aggregates the usage counters reported by the runtime.
// CacheCreation5m/1h are the ephemeral-TTL split of CacheCreationInputTokens
// when the runtime reports it (used for more accurate cache-write pricing).
type TokenTotals struct {
	InputTokens              int64 `yaml:"input_tokens"`
	OutputTokens             int64 `yaml:"output_tokens"`
	CacheReadInputTokens     int64 `yaml:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `yaml:"cache_creation_input_tokens"`
	CacheCreation5mTokens    int64 `yaml:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens    int64 `yaml:"cache_creation_1h_tokens,omitempty"`
}

// Add accumulates other into t.
func (t *TokenTotals) Add(other TokenTotals) {
	t.InputTokens += other.InputTokens
	t.OutputTokens += other.OutputTokens
	t.CacheReadInputTokens += other.CacheReadInputTokens
	t.CacheCreationInputTokens += other.CacheCreationInputTokens
	t.CacheCreation5mTokens += other.CacheCreation5mTokens
	t.CacheCreation1hTokens += other.CacheCreation1hTokens
}

// IsZero reports whether every counter is zero.
func (t TokenTotals) IsZero() bool {
	return t.InputTokens == 0 && t.OutputTokens == 0 &&
		t.CacheReadInputTokens == 0 && t.CacheCreationInputTokens == 0
}

// QueueDepth は各キューの現在の深度（未処理アイテム数）を表す。
type QueueDepth struct {
	Planner      int            `yaml:"planner"`
	Orchestrator int            `yaml:"orchestrator"`
	Workers      map[string]int `yaml:"workers"`
}

// MetricsCounters はシステム全体の累積カウンターを保持する。
// ディスパッチ数、完了数、失敗数、リース操作数などを追跡する。
type MetricsCounters struct {
	CommandsDispatched         int `yaml:"commands_dispatched"`
	TasksDispatched            int `yaml:"tasks_dispatched"`
	TasksCompleted             int `yaml:"tasks_completed"`
	TasksFailed                int `yaml:"tasks_failed"`
	TasksCancelled             int `yaml:"tasks_cancelled"`
	DeadLetters                int `yaml:"dead_letters"`
	ReconciliationRepairs      int `yaml:"reconciliation_repairs"`
	NotificationRetries        int `yaml:"notification_retries"`
	SignalDeliveries           int `yaml:"signal_deliveries"`
	SignalRetries              int `yaml:"signal_retries"`
	SignalDeadLetters          int `yaml:"signal_dead_letters"`
	SignalInlineRetrySuccesses int `yaml:"signal_inline_retry_successes"`
	LeaseRenewals              int `yaml:"lease_renewals"`
	LeaseExtensions            int `yaml:"lease_extensions"`
	LeaseReleases              int `yaml:"lease_releases"`
}
