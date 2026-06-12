package model

import (
	"math"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/ptr"
)

// validConfig returns a Config that passes all validation checks.
func validConfig() Config {
	return Config{
		Project: ProjectConfig{Name: "test-project"},
		Maestro: MaestroConfig{Version: "2.0.0"},
		Agents: AgentsConfig{
			Workers: WorkerConfig{Count: 2},
		},
		Watcher: WatcherConfig{
			BusyCheckInterval:      5,
			BusyCheckMaxRetries:    3,
			NotifyLeaseSec:         120,
			WaitReadyIntervalSec:   2,
			WaitReadyMaxRetries:    15,
			ClearConfirmTimeoutSec: 5,
			ClearConfirmPollMs:     250,
			ClearMaxAttempts:       3,
		},
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidate_EmptyProjectName(t *testing.T) {
	cfg := validConfig()
	cfg.Project.Name = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty project name")
	}
	if !strings.Contains(err.Error(), "project.name") {
		t.Fatalf("expected field path project.name in error, got: %v", err)
	}
}

func TestValidate_EmptyVersion(t *testing.T) {
	cfg := validConfig()
	cfg.Maestro.Version = ""
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty version")
	}
	if !strings.Contains(err.Error(), "maestro.version") {
		t.Fatalf("expected field path maestro.version in error, got: %v", err)
	}
}

func TestValidate_WorkerCountZero(t *testing.T) {
	cfg := validConfig()
	cfg.Agents.Workers.Count = 0
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for worker count 0")
	}
	if !strings.Contains(err.Error(), "agents.workers.count") {
		t.Fatalf("expected field path agents.workers.count in error, got: %v", err)
	}
}

func TestValidate_NegativeWorkerCount(t *testing.T) {
	cfg := validConfig()
	cfg.Agents.Workers.Count = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative worker count")
	}
	if !strings.Contains(err.Error(), "agents.workers.count") {
		t.Fatalf("expected field path agents.workers.count in error, got: %v", err)
	}
}

func TestValidate_WorkerCountExceedsMax(t *testing.T) {
	cfg := validConfig()
	cfg.Agents.Workers.Count = 9
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for worker count exceeding max")
	}
	if !strings.Contains(err.Error(), "agents.workers.count") {
		t.Fatalf("expected field path agents.workers.count in error, got: %v", err)
	}
}

func TestValidate_NegativeRetryFields(t *testing.T) {
	cfg := validConfig()
	cfg.Retry.CommandDispatch = -1
	cfg.Retry.TaskDispatch = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative retry fields")
	}
	if !strings.Contains(err.Error(), "retry.command_dispatch") {
		t.Fatalf("expected retry.command_dispatch in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "retry.task_dispatch") {
		t.Fatalf("expected retry.task_dispatch in error, got: %v", err)
	}
}

func TestValidate_NegativeLimits(t *testing.T) {
	cfg := validConfig()
	cfg.Limits.MaxPendingCommands = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative limits")
	}
	if !strings.Contains(err.Error(), "limits.max_pending_commands") {
		t.Fatalf("expected limits.max_pending_commands in error, got: %v", err)
	}
}

func TestValidate_NegativeWatcherFields(t *testing.T) {
	cfg := validConfig()
	cfg.Watcher.ScanIntervalSec = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative watcher field")
	}
	if !strings.Contains(err.Error(), "watcher.scan_interval_sec") {
		t.Fatalf("expected watcher.scan_interval_sec in error, got: %v", err)
	}
}

func TestValidate_MultipleErrors(t *testing.T) {
	cfg := Config{} // all zero values: name empty, version empty, workers 0
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected multiple errors for zero config")
	}
	errStr := err.Error()
	// Should contain at least these three field errors
	for _, field := range []string{"project.name", "maestro.version", "agents.workers.count"} {
		if !strings.Contains(errStr, field) {
			t.Errorf("expected %s in error, got: %v", field, err)
		}
	}
}

func TestValidate_NegativeDaemonTimeout(t *testing.T) {
	cfg := validConfig()
	cfg.ShutdownTimeoutSec = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative daemon timeout")
	}
	if !strings.Contains(err.Error(), "shutdown_timeout_sec") {
		t.Fatalf("expected shutdown_timeout_sec in error, got: %v", err)
	}
}

func TestValidate_NegativeCircuitBreaker(t *testing.T) {
	cfg := validConfig()
	cfg.CircuitBreaker.MaxConsecutiveFailures = ptr.Int(-1)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative circuit breaker field")
	}
	if !strings.Contains(err.Error(), "circuit_breaker.max_consecutive_failures") {
		t.Fatalf("expected circuit_breaker.max_consecutive_failures in error, got: %v", err)
	}
}

func TestValidate_NegativeLearnings(t *testing.T) {
	cfg := validConfig()
	cfg.Learnings.MaxEntries = ptr.Int(-1)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative learnings max_entries")
	}
	if !strings.Contains(err.Error(), "learnings.max_entries") {
		t.Fatalf("expected learnings.max_entries in error, got: %v", err)
	}
}

// --- Agent model name validation tests ---

func TestValidate_ValidModelNames(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		// NOTE: orchestrator/planner reject non-claude-code runtimes
		// (e.g. "codex" / "gemini-*"); those are covered separately in
		// TestValidate_OrchestratorPlannerRejectsNonClaudeCodeRuntime.
		{"empty (default)", ""},
		{"sonnet", "sonnet"},
		{"opus", "opus"},
		{"haiku", "haiku"},
		{"full claude ID", "claude-sonnet-4-6"},
		{"format with dots", "claude-sonnet-4.5"},
		{"format with underscore", "my_model_v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Agents.Orchestrator.Model = tt.model
			if err := cfg.Validate(); err != nil {
				t.Errorf("expected no error for model %q, got: %v", tt.model, err)
			}
		})
	}
}

func TestValidate_InvalidModelNames(t *testing.T) {
	tests := []struct {
		name     string
		field    string
		setup    func(*Config)
		errField string
	}{
		{
			"orchestrator with spaces",
			"agents.orchestrator.model",
			func(c *Config) { c.Agents.Orchestrator.Model = "bad model" },
			"agents.orchestrator.model",
		},
		{
			"planner with special chars",
			"agents.planner.model",
			func(c *Config) { c.Agents.Planner.Model = "model;rm -rf" },
			"agents.planner.model",
		},
		{
			"worker default with newline",
			"agents.workers.default_model",
			func(c *Config) { c.Agents.Workers.DefaultModel = "model\ninjection" },
			"agents.workers.default_model",
		},
		{
			"worker model override invalid",
			"agents.workers.models.worker1",
			func(c *Config) { c.Agents.Workers.Models = map[string]string{"worker1": "../escape"} },
			"agents.workers.models.worker1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.setup(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected error for invalid model name")
			}
			if !strings.Contains(err.Error(), tt.errField) {
				t.Errorf("expected %q in error, got: %v", tt.errField, err)
			}
		})
	}
}

// TestValidate_OrchestratorPlannerRejectsNonClaudeCodeRuntime verifies that
// non-claude-code runtimes (codex, gemini, gemini-*) are rejected for the
// orchestrator and planner roles. Tool-based role enforcement is only
// available on claude-code; running these roles on codex/gemini bypasses the
// "delegation-only" / "planning-only" contract entirely (see past incidents
// where codex Orchestrator directly modified files on main).
func TestValidate_OrchestratorPlannerRejectsNonClaudeCodeRuntime(t *testing.T) {
	cases := []struct {
		name      string
		role      string // "orchestrator" or "planner"
		modelName string
	}{
		{"orchestrator with codex", "orchestrator", "codex"},
		{"orchestrator with gemini", "orchestrator", "gemini"},
		{"orchestrator with gemini-2.5-pro", "orchestrator", "gemini-2.5-pro"},
		{"planner with codex", "planner", "codex"},
		{"planner with gemini", "planner", "gemini"},
		{"planner with gemini-2.5-pro", "planner", "gemini-2.5-pro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			switch tc.role {
			case "orchestrator":
				cfg.Agents.Orchestrator.Model = tc.modelName
			case "planner":
				cfg.Agents.Planner.Model = tc.modelName
			default:
				t.Fatalf("unknown role %q", tc.role)
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error for %s.model=%q", tc.role, tc.modelName)
			}
			wantField := "agents." + tc.role + ".model"
			if !strings.Contains(err.Error(), wantField) {
				t.Errorf("expected %q in error, got: %v", wantField, err)
			}
		})
	}
}

// TestValidate_WorkersAcceptNonClaudeRuntime confirms codex / gemini are
// accepted for the worker role. The validate_run_on_main pre-flight is the
// cross-runtime safety net for destructive content; orchestrator and
// planner remain claude-code only.
func TestValidate_WorkersAcceptNonClaudeRuntime(t *testing.T) {
	for _, m := range []string{"codex", "gemini-2.5-pro"} {
		t.Run("worker_"+m, func(t *testing.T) {
			cfg := validConfig()
			cfg.Agents.Workers.DefaultModel = m
			cfg.Agents.Workers.Models = map[string]string{"worker1": m}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("expected worker model %q to be accepted, got: %v", m, err)
			}
		})
	}

	// Orchestrator / planner remain rejected: those roles operate on the
	// project root and the validate_run_on_main pre-flight does not bound
	// their blast radius.
	for _, role := range []string{"orchestrator", "planner"} {
		t.Run("role_"+role+"_still_rejected", func(t *testing.T) {
			cfg := validConfig()
			switch role {
			case "orchestrator":
				cfg.Agents.Orchestrator.Model = "codex"
			case "planner":
				cfg.Agents.Planner.Model = "codex"
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected %s rejection for non-claude runtime", role)
			}
			if !strings.Contains(err.Error(), "agents."+role+".model") {
				t.Errorf("expected %s.model in error, got: %v", role, err)
			}
		})
	}
}

func TestIsValidModelName(t *testing.T) {
	valid := []string{
		"", "sonnet", "opus", "haiku",
		"claude-sonnet-4-6", "o3-mini", "gemini-2.5-pro",
		// 1M context variant suffix used by Claude Code's long-context models.
		"claude-opus-4-7[1m]",
		"claude-sonnet-4-6[1m]",
		"claude-haiku-4-5-20251001[1m]",
	}
	for _, name := range valid {
		if !isValidModelName(name) {
			t.Errorf("expected %q to be valid", name)
		}
	}
	invalid := []string{
		" ", "a b", ";cmd", "$var", "model\x00null",
		// Bracket suffix must be a single trailing [..] block of alphanumerics,
		// otherwise it is treated as shell-special and rejected.
		"claude-opus-4-7[1m",
		"claude-opus-4-7]1m[",
		"claude-opus-4-7[1m][2m]",
		"claude-opus-4-7[1 m]",
	}
	for _, name := range invalid {
		if isValidModelName(name) {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

// --- AdmissionControl tests ---

func TestValidate_AdmissionControl_Defaults(t *testing.T) {
	ac := AdmissionControl{}
	if v := ac.EffectiveMaxConcurrentVerify(); v != 2 {
		t.Errorf("expected default verify=2, got %d", v)
	}
	if v := ac.EffectiveMaxConcurrentRepair(); v != 1 {
		t.Errorf("expected default repair=1, got %d", v)
	}
}

func TestValidate_AdmissionControl_Configured(t *testing.T) {
	ac := AdmissionControl{
		MaxConcurrentVerify: 4,
		MaxConcurrentRepair: 2,
	}
	if v := ac.EffectiveMaxConcurrentVerify(); v != 4 {
		t.Errorf("expected verify=4, got %d", v)
	}
	if v := ac.EffectiveMaxConcurrentRepair(); v != 2 {
		t.Errorf("expected repair=2, got %d", v)
	}
}

func TestValidate_AdmissionControl_NegativeValues(t *testing.T) {
	tests := []struct {
		name     string
		cfg      func(*Config)
		errField string
	}{
		{
			"negative verify",
			func(c *Config) { c.AdmissionControl.MaxConcurrentVerify = -1 },
			"admission_control.max_concurrent_verify",
		},
		{
			"negative repair",
			func(c *Config) { c.AdmissionControl.MaxConcurrentRepair = -1 },
			"admission_control.max_concurrent_repair",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.cfg(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.errField) {
				t.Errorf("expected %q in error, got: %v", tt.errField, err)
			}
		})
	}
}

func TestValidate_AdmissionControl_ZeroIsValid(t *testing.T) {
	cfg := validConfig()
	cfg.AdmissionControl = AdmissionControl{} // all zero
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("zero admission_control should be valid (defaults apply), got: %v", err)
	}
}

// --- C-1 EvolutionConfig tests ---

func TestEvolutionConfig_Defaults(t *testing.T) {
	ec := EvolutionConfig{}
	if ec.EffectiveEnabled() {
		t.Error("default Enabled should be false")
	}
	if v := ec.EffectiveMaxMutationsPerRound(); v != 3 {
		t.Errorf("EffectiveMaxMutationsPerRound() = %d, want 3", v)
	}
	if v := ec.EffectiveNoveltyThreshold(); v != 0.99 {
		t.Errorf("EffectiveNoveltyThreshold() = %v, want 0.99", v)
	}
	strategies := ec.EffectiveStrategies()
	if len(strategies) != 3 || strategies[0] != "diff" || strategies[1] != "full" || strategies[2] != "cross" {
		t.Errorf("EffectiveStrategies() = %v, want [diff full cross]", strategies)
	}
}

func TestEvolutionConfig_Configured(t *testing.T) {
	ec := EvolutionConfig{
		Enabled:              ptr.Bool(true),
		MaxMutationsPerRound: ptr.Int(5),
		NoveltyThreshold:     ptr.Float64(0.8),
		Strategies:           []string{"diff"},
	}
	if !ec.EffectiveEnabled() {
		t.Error("Enabled should be true")
	}
	if v := ec.EffectiveMaxMutationsPerRound(); v != 5 {
		t.Errorf("EffectiveMaxMutationsPerRound() = %d, want 5", v)
	}
	if v := ec.EffectiveNoveltyThreshold(); v != 0.8 {
		t.Errorf("EffectiveNoveltyThreshold() = %v, want 0.8", v)
	}
	strategies := ec.EffectiveStrategies()
	if len(strategies) != 1 || strategies[0] != "diff" {
		t.Errorf("EffectiveStrategies() = %v, want [diff]", strategies)
	}
}

// --- C-2 BanditConfig tests ---

func TestBanditConfig_Defaults(t *testing.T) {
	bc := BanditConfig{}
	if bc.EffectiveEnabled() {
		t.Error("default Enabled should be false")
	}
	if v := bc.EffectiveExplorationCoeff(); v != 1.41 {
		t.Errorf("EffectiveExplorationCoeff() = %v, want 1.41", v)
	}
	if v := bc.EffectiveMinSamplesBeforeUse(); v != 10 {
		t.Errorf("EffectiveMinSamplesBeforeUse() = %d, want 10", v)
	}
	if v := bc.EffectiveTraceDataRequirement(); v != 50 {
		t.Errorf("EffectiveTraceDataRequirement() = %d, want 50", v)
	}
}

func TestBanditConfig_Configured(t *testing.T) {
	bc := BanditConfig{
		Enabled:              ptr.Bool(true),
		ExplorationCoeff:     ptr.Float64(2.0),
		MinSamplesBeforeUse:  ptr.Int(20),
		TraceDataRequirement: ptr.Int(100),
	}
	if !bc.EffectiveEnabled() {
		t.Error("Enabled should be true")
	}
	if v := bc.EffectiveExplorationCoeff(); v != 2.0 {
		t.Errorf("EffectiveExplorationCoeff() = %v, want 2.0", v)
	}
	if v := bc.EffectiveMinSamplesBeforeUse(); v != 20 {
		t.Errorf("EffectiveMinSamplesBeforeUse() = %d, want 20", v)
	}
	if v := bc.EffectiveTraceDataRequirement(); v != 100 {
		t.Errorf("EffectiveTraceDataRequirement() = %d, want 100", v)
	}
}

// --- C-3 ExtendedVerificationConfig tests ---

func TestExtendedVerificationConfig_Defaults(t *testing.T) {
	ev := ExtendedVerificationConfig{}
	if ev.EffectiveEnabled() {
		t.Error("default Enabled should be false")
	}
	if v := ev.EffectiveMaxAutoRetries(); v != 2 {
		t.Errorf("EffectiveMaxAutoRetries() = %d, want 2", v)
	}
}

func TestExtendedVerificationConfig_Configured(t *testing.T) {
	ev := ExtendedVerificationConfig{
		Enabled:        ptr.Bool(true),
		MaxAutoRetries: ptr.Int(5),
	}
	if !ev.EffectiveEnabled() {
		t.Error("Enabled should be true")
	}
	if v := ev.EffectiveMaxAutoRetries(); v != 5 {
		t.Errorf("EffectiveMaxAutoRetries() = %d, want 5", v)
	}
}

// --- C-4 SearchConfig tests ---

func TestSearchConfig_Defaults(t *testing.T) {
	sc := SearchConfig{}
	if sc.EffectiveEnabled() {
		t.Error("default Enabled should be false")
	}
	if v := sc.EffectiveMaxDepth(); v != 3 {
		t.Errorf("EffectiveMaxDepth() = %d, want 3", v)
	}
	if v := sc.EffectiveMaxBranching(); v != 4 {
		t.Errorf("EffectiveMaxBranching() = %d, want 4", v)
	}
	if v := sc.EffectivePruneThreshold(); v != 0.3 {
		t.Errorf("EffectivePruneThreshold() = %v, want 0.3", v)
	}
	if v := sc.EffectiveThompsonAlpha(); v != 1.0 {
		t.Errorf("EffectiveThompsonAlpha() = %v, want 1.0", v)
	}
	if v := sc.EffectiveThompsonBeta(); v != 1.0 {
		t.Errorf("EffectiveThompsonBeta() = %v, want 1.0", v)
	}
}

func TestSearchConfig_Configured(t *testing.T) {
	sc := SearchConfig{
		Enabled:        ptr.Bool(true),
		MaxDepth:       ptr.Int(5),
		MaxBranching:   ptr.Int(8),
		PruneThreshold: ptr.Float64(0.5),
		ThompsonAlpha:  ptr.Float64(2.0),
		ThompsonBeta:   ptr.Float64(3.0),
	}
	if !sc.EffectiveEnabled() {
		t.Error("Enabled should be true")
	}
	if v := sc.EffectiveMaxDepth(); v != 5 {
		t.Errorf("EffectiveMaxDepth() = %d, want 5", v)
	}
	if v := sc.EffectiveMaxBranching(); v != 8 {
		t.Errorf("EffectiveMaxBranching() = %d, want 8", v)
	}
	if v := sc.EffectivePruneThreshold(); v != 0.5 {
		t.Errorf("EffectivePruneThreshold() = %v, want 0.5", v)
	}
	if v := sc.EffectiveThompsonAlpha(); v != 2.0 {
		t.Errorf("EffectiveThompsonAlpha() = %v, want 2.0", v)
	}
	if v := sc.EffectiveThompsonBeta(); v != 3.0 {
		t.Errorf("EffectiveThompsonBeta() = %v, want 3.0", v)
	}
}

// --- C-5 SelfImprovementConfig tests ---

func TestSelfImprovementConfig_Defaults(t *testing.T) {
	si := SelfImprovementConfig{}
	if si.EffectiveEnabled() {
		t.Error("default Enabled should be false")
	}
	targets := si.EffectiveTargets()
	if len(targets) != 3 {
		t.Errorf("EffectiveTargets() count = %d, want 3", len(targets))
	}
	excludes := si.EffectiveExcludeTargets()
	if len(excludes) != 3 {
		t.Errorf("EffectiveExcludeTargets() count = %d, want 3", len(excludes))
	}
	if v := si.EffectiveArchiveMaxSize(); v != 100 {
		t.Errorf("EffectiveArchiveMaxSize() = %d, want 100", v)
	}
}

func TestSelfImprovementConfig_Configured(t *testing.T) {
	si := SelfImprovementConfig{
		Enabled:        ptr.Bool(true),
		Targets:        []string{"persona"},
		ExcludeTargets: []string{"fitness"},
		ArchiveMaxSize: ptr.Int(50),
	}
	if !si.EffectiveEnabled() {
		t.Error("Enabled should be true")
	}
	if targets := si.EffectiveTargets(); len(targets) != 1 || targets[0] != "persona" {
		t.Errorf("EffectiveTargets() = %v, want [persona]", targets)
	}
	if excludes := si.EffectiveExcludeTargets(); len(excludes) != 1 || excludes[0] != "fitness" {
		t.Errorf("EffectiveExcludeTargets() = %v, want [fitness]", excludes)
	}
	if v := si.EffectiveArchiveMaxSize(); v != 50 {
		t.Errorf("EffectiveArchiveMaxSize() = %d, want 50", v)
	}
}

// --- Cross-field validation tests ---

func TestValidate_ContinuousEnabled_NegativeMaxIterations(t *testing.T) {
	cfg := validConfig()
	cfg.Continuous.Enabled = true
	cfg.Continuous.MaxIterations = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative max_iterations when continuous enabled")
	}
	if !strings.Contains(err.Error(), "continuous.max_iterations") {
		t.Fatalf("expected continuous.max_iterations in error, got: %v", err)
	}
}

func TestValidate_ContinuousEnabled_NegativeMaxConsecutiveFailures(t *testing.T) {
	cfg := validConfig()
	cfg.Continuous.Enabled = true
	cfg.Continuous.MaxConsecutiveFailures = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative max_consecutive_failures when continuous enabled")
	}
	if !strings.Contains(err.Error(), "continuous.max_consecutive_failures") {
		t.Fatalf("expected continuous.max_consecutive_failures in error, got: %v", err)
	}
}

func TestValidate_ContinuousDisabled_NegativeMaxIterations(t *testing.T) {
	cfg := validConfig()
	cfg.Continuous.Enabled = false
	cfg.Continuous.MaxIterations = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative max_iterations even when continuous disabled")
	}
	if !strings.Contains(err.Error(), "continuous.max_iterations") {
		t.Fatalf("expected continuous.max_iterations in error, got: %v", err)
	}
}

func TestValidate_ContinuousDisabled_NegativeMaxConsecutiveFailuresOK(t *testing.T) {
	cfg := validConfig()
	cfg.Continuous.Enabled = false
	cfg.Continuous.MaxConsecutiveFailures = -1
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("expected no error for negative max_consecutive_failures when continuous disabled, got: %v", err)
	}
}

func TestValidate_ContinuousEnabled_ZeroFieldsOK(t *testing.T) {
	cfg := validConfig()
	cfg.Continuous.Enabled = true
	cfg.Continuous.MaxIterations = 0
	cfg.Continuous.MaxConsecutiveFailures = 0
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("expected no error for zero values (unlimited), got: %v", err)
	}
}

func TestValidate_WorktreeGC_Enabled_InvalidTTL(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.GC.Enabled = true
	cfg.Worktree.GC.TTLHours = ptr.Int(0)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for zero ttl_hours when gc enabled")
	}
	if !strings.Contains(err.Error(), "worktree.gc.ttl_hours") {
		t.Fatalf("expected worktree.gc.ttl_hours in error, got: %v", err)
	}
}

func TestValidate_WorktreeGC_Enabled_InvalidMaxWorktrees(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.GC.Enabled = true
	cfg.Worktree.GC.MaxWorktrees = ptr.Int(-1)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative max_worktrees when gc enabled")
	}
	if !strings.Contains(err.Error(), "worktree.gc.max_worktrees") {
		t.Fatalf("expected worktree.gc.max_worktrees in error, got: %v", err)
	}
}

func TestValidate_WorktreeGC_Disabled_InvalidFieldsOK(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.GC.Enabled = false
	cfg.Worktree.GC.TTLHours = ptr.Int(0)
	cfg.Worktree.GC.MaxWorktrees = ptr.Int(-1)
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("expected no error when gc disabled, got: %v", err)
	}
}

func TestValidate_WorktreeGC_Enabled_ValidFields(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.GC.Enabled = true
	cfg.Worktree.GC.TTLHours = ptr.Int(24)
	cfg.Worktree.GC.MaxWorktrees = ptr.Int(5)
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("expected no error for valid gc config, got: %v", err)
	}
}

func TestValidate_WorktreeMergeStrategy_Invalid(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.MergeStrategy = "invalid"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid merge_strategy")
	}
	if !strings.Contains(err.Error(), "worktree.merge_strategy") {
		t.Fatalf("expected worktree.merge_strategy in error, got: %v", err)
	}
}

func TestValidate_WorktreeMergeStrategy_ValidValues(t *testing.T) {
	for _, ms := range []string{"ort", "ours", "theirs", "recursive", ""} {
		t.Run("strategy_"+ms, func(t *testing.T) {
			cfg := validConfig()
			cfg.Worktree.MergeStrategy = ms
			err := cfg.Validate()
			if err != nil {
				t.Fatalf("expected no error for merge_strategy %q, got: %v", ms, err)
			}
		})
	}
}

func TestValidate_WorktreeGitTimeout_ZeroOrNegative(t *testing.T) {
	tests := []struct {
		name string
		val  int
	}{
		{"zero", 0},
		{"negative", -5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Worktree.GitTimeoutSec = ptr.Int(tt.val)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for git_timeout_sec=%d", tt.val)
			}
			if !strings.Contains(err.Error(), "worktree.git_timeout_sec") {
				t.Fatalf("expected worktree.git_timeout_sec in error, got: %v", err)
			}
		})
	}
}

func TestValidate_WorktreeGitTimeout_Nil_OK(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.GitTimeoutSec = nil
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("expected no error for nil git_timeout_sec, got: %v", err)
	}
}

// --- Upper-bound validation tests ---

func TestValidate_UpperBound_BusyCheckMaxRetries(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxBusyCheckMaxRetries, false},
		{"exceeds_max", MaxBusyCheckMaxRetries + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Watcher.BusyCheckMaxRetries = tt.val
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "watcher.busy_check_max_retries") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_UpperBound_WaitReadyMaxRetries(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxWaitReadyMaxRetries, false},
		{"exceeds_max", MaxWaitReadyMaxRetries + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Watcher.WaitReadyMaxRetries = tt.val
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "watcher.wait_ready_max_retries") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_UpperBound_DispatchLeaseSec(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxDispatchLeaseSec, false},
		{"exceeds_max", MaxDispatchLeaseSec + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Watcher.DispatchLeaseSec = tt.val
			cfg.Watcher.MaxInProgressMin = ptr.Int(MaxMaxInProgressMin) // avoid cross-field constraint
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "watcher.dispatch_lease_sec") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_UpperBound_MaxInProgressMin(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxMaxInProgressMin, false},
		{"exceeds_max", MaxMaxInProgressMin + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Watcher.MaxInProgressMin = ptr.Int(tt.val)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "watcher.max_in_progress_min") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_UpperBound_MaxPendingCommands(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxMaxPendingCommands, false},
		{"exceeds_max", MaxMaxPendingCommands + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Limits.MaxPendingCommands = tt.val
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "limits.max_pending_commands") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_UpperBound_MaxPendingTasksPerWorker(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxMaxPendingTasksPerWorker, false},
		{"exceeds_max", MaxMaxPendingTasksPerWorker + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Limits.MaxPendingTasksPerWorker = tt.val
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "limits.max_pending_tasks_per_worker") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_UpperBound_MaxDeadLetterArchiveFiles(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxMaxDeadLetterArchiveFiles, false},
		{"exceeds_max", MaxMaxDeadLetterArchiveFiles + 1, true},
		{"negative", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Limits.MaxDeadLetterArchiveFiles = ptr.Int(tt.val)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "limits.max_dead_letter_archive_files") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_UpperBound_MaxQuarantineFiles(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxMaxQuarantineFiles, false},
		{"exceeds_max", MaxMaxQuarantineFiles + 1, true},
		{"negative", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Limits.MaxQuarantineFiles = ptr.Int(tt.val)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "limits.max_quarantine_files") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_UpperBound_ShutdownTimeoutSec(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxShutdownTimeoutSec, false},
		{"exceeds_max", MaxShutdownTimeoutSec + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.ShutdownTimeoutSec = tt.val
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "shutdown_timeout_sec") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_UpperBound_MaxWorktrees(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxMaxWorktrees, false},
		{"exceeds_max", MaxMaxWorktrees + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Worktree.GC.Enabled = true
			cfg.Worktree.GC.TTLHours = ptr.Int(24)
			cfg.Worktree.GC.MaxWorktrees = ptr.Int(tt.val)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "worktree.gc.max_worktrees") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

// --- Generic helper function tests ---

func TestEffectiveValue_Int(t *testing.T) {
	tests := []struct {
		name       string
		ptr        *int
		defaultVal int
		want       int
	}{
		{"nil returns default", nil, 42, 42},
		{"non-nil returns value", ptr.Int(7), 42, 7},
		{"zero value returns 0", ptr.Int(0), 42, 0},
		{"negative returns negative", ptr.Int(-1), 42, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveValue(tt.ptr, tt.defaultVal); got != tt.want {
				t.Errorf("effectiveValue() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEffectiveValue_Bool(t *testing.T) {
	tests := []struct {
		name       string
		ptr        *bool
		defaultVal bool
		want       bool
	}{
		{"nil returns default false", nil, false, false},
		{"nil returns default true", nil, true, true},
		{"true ptr returns true", ptr.Bool(true), false, true},
		{"false ptr returns false", ptr.Bool(false), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveValue(tt.ptr, tt.defaultVal); got != tt.want {
				t.Errorf("effectiveValue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEffectiveValue_Float64(t *testing.T) {
	tests := []struct {
		name       string
		ptr        *float64
		defaultVal float64
		want       float64
	}{
		{"nil returns default", nil, 0.99, 0.99},
		{"non-nil returns value", ptr.Float64(1.5), 0.99, 1.5},
		{"zero returns 0", ptr.Float64(0.0), 0.99, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveValue(tt.ptr, tt.defaultVal); got != tt.want {
				t.Errorf("effectiveValue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEffectiveValue_String(t *testing.T) {
	tests := []struct {
		name       string
		ptr        *string
		defaultVal string
		want       string
	}{
		{"nil returns default", nil, "opus", "opus"},
		{"non-nil returns value", ptr.String("sonnet"), "opus", "sonnet"},
		{"empty string returns empty", ptr.String(""), "opus", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveValue(tt.ptr, tt.defaultVal); got != tt.want {
				t.Errorf("effectiveValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveNonZero_Int(t *testing.T) {
	tests := []struct {
		name       string
		val        int
		defaultVal int
		want       int
	}{
		{"zero returns default", 0, 5, 5},
		{"positive returns value", 3, 5, 3},
		{"negative returns value", -1, 5, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveNonZero(tt.val, tt.defaultVal); got != tt.want {
				t.Errorf("effectiveNonZero() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEffectiveNonZero_String(t *testing.T) {
	tests := []struct {
		name       string
		val        string
		defaultVal string
		want       string
	}{
		{"empty returns default", "", "warn", "warn"},
		{"non-empty returns value", "error", "warn", "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveNonZero(tt.val, tt.defaultVal); got != tt.want {
				t.Errorf("effectiveNonZero() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- C-6 ComplexityConfig tests ---

func TestComplexityConfig_Defaults(t *testing.T) {
	cc := ComplexityConfig{}
	if cc.EffectiveEnabled() {
		t.Error("default Enabled should be false")
	}
}

func TestValidate_MaxYAMLFileBytes_Negative(t *testing.T) {
	cfg := validConfig()
	cfg.Limits.MaxYAMLFileBytes = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative MaxYAMLFileBytes")
	}
	if !strings.Contains(err.Error(), "limits.max_yaml_file_bytes") {
		t.Fatalf("expected limits.max_yaml_file_bytes in error, got: %v", err)
	}
}

func TestValidate_MaxYAMLFileBytes_ExceedsMax(t *testing.T) {
	cfg := validConfig()
	cfg.Limits.MaxYAMLFileBytes = MaxMaxYAMLFileBytes + 1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for MaxYAMLFileBytes exceeding max")
	}
	if !strings.Contains(err.Error(), "limits.max_yaml_file_bytes") {
		t.Fatalf("expected limits.max_yaml_file_bytes in error, got: %v", err)
	}
}

func TestValidate_MaxYAMLFileBytes_ValidValues(t *testing.T) {
	for _, val := range []int{0, 1024, DefaultMaxYAMLFileBytes, MaxMaxYAMLFileBytes} {
		cfg := validConfig()
		cfg.Limits.MaxYAMLFileBytes = val
		if err := cfg.Validate(); err != nil {
			t.Errorf("MaxYAMLFileBytes=%d should be valid, got: %v", val, err)
		}
	}
}

// --- Cross-field constraint tests ---

func TestValidate_CircuitBreaker_EnabledWithZeroFailures(t *testing.T) {
	cfg := validConfig()
	cfg.CircuitBreaker.Enabled = true
	cfg.CircuitBreaker.MaxConsecutiveFailures = ptr.Int(0)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error when circuit_breaker.enabled with max_consecutive_failures=0")
	}
	if !strings.Contains(err.Error(), "circuit_breaker.max_consecutive_failures") {
		t.Fatalf("expected circuit_breaker.max_consecutive_failures in error, got: %v", err)
	}
}

func TestValidate_CircuitBreaker_EnabledWithPositiveFailures_OK(t *testing.T) {
	cfg := validConfig()
	cfg.CircuitBreaker.Enabled = true
	cfg.CircuitBreaker.MaxConsecutiveFailures = ptr.Int(3)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidate_CircuitBreaker_EnabledWithNilFailures_OK(t *testing.T) {
	cfg := validConfig()
	cfg.CircuitBreaker.Enabled = true
	cfg.CircuitBreaker.MaxConsecutiveFailures = nil // uses default
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error for nil (uses default), got: %v", err)
	}
}

func TestValidate_Worktree_GCMaxWorktreesLessThanWorkers(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.Enabled = true
	cfg.Worktree.GC.Enabled = true
	cfg.Worktree.GC.TTLHours = ptr.Int(24)
	cfg.Worktree.GC.MaxWorktrees = ptr.Int(1) // less than workers.count=2
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error when gc.max_worktrees < workers.count")
	}
	if !strings.Contains(err.Error(), "worktree.gc.max_worktrees") {
		t.Fatalf("expected worktree.gc.max_worktrees in error, got: %v", err)
	}
}

func TestValidate_Worktree_GCMaxWorktreesEqualWorkers_OK(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.Enabled = true
	cfg.Worktree.GC.Enabled = true
	cfg.Worktree.GC.TTLHours = ptr.Int(24)
	cfg.Worktree.GC.MaxWorktrees = ptr.Int(2) // equal to workers.count=2
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidate_Worktree_BothTimeoutsDisabled(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.Enabled = true
	cfg.Worktree.StallTimeoutMinutes = ptr.Int(0)
	cfg.Worktree.FallbackMergeTimeoutMinutes = ptr.Int(0)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error when both stall and fallback_merge timeouts are disabled")
	}
	if !strings.Contains(err.Error(), "stall_timeout_minutes") {
		t.Fatalf("expected stall_timeout_minutes in error, got: %v", err)
	}
}

func TestValidate_Worktree_OneTimeoutEnabled_OK(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.Enabled = true
	cfg.Worktree.StallTimeoutMinutes = ptr.Int(0)          // disabled
	cfg.Worktree.FallbackMergeTimeoutMinutes = ptr.Int(60) // enabled
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error when one timeout is enabled, got: %v", err)
	}
}

func TestValidate_Worktree_Disabled_IgnoresCrossField(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.Enabled = false
	cfg.Worktree.StallTimeoutMinutes = ptr.Int(0)
	cfg.Worktree.FallbackMergeTimeoutMinutes = ptr.Int(0)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error when worktree disabled, got: %v", err)
	}
}

func TestValidate_RetryVsCircuitBreaker_Interaction(t *testing.T) {
	cfg := validConfig()
	cfg.CircuitBreaker.Enabled = true
	cfg.CircuitBreaker.MaxConsecutiveFailures = ptr.Int(3)
	cfg.Retry.TaskExecution.Enabled = true
	cfg.Retry.TaskExecution.MaxRetries = 5 // >= circuit_breaker threshold
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error when task retries >= circuit_breaker threshold")
	}
	if !strings.Contains(err.Error(), "retry.task_execution.max_retries") {
		t.Fatalf("expected retry.task_execution.max_retries in error, got: %v", err)
	}
}

func TestValidate_RetryVsCircuitBreaker_BelowThreshold_OK(t *testing.T) {
	cfg := validConfig()
	cfg.CircuitBreaker.Enabled = true
	cfg.CircuitBreaker.MaxConsecutiveFailures = ptr.Int(5)
	cfg.Retry.TaskExecution.Enabled = true
	cfg.Retry.TaskExecution.MaxRetries = 2 // below threshold
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error when retries < threshold, got: %v", err)
	}
}

func TestValidate_RetryVsCircuitBreaker_RetryDisabled_OK(t *testing.T) {
	cfg := validConfig()
	cfg.CircuitBreaker.Enabled = true
	cfg.CircuitBreaker.MaxConsecutiveFailures = ptr.Int(3)
	cfg.Retry.TaskExecution.Enabled = false
	cfg.Retry.TaskExecution.MaxRetries = 10 // doesn't matter when disabled
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error when retry disabled, got: %v", err)
	}
}

func TestValidate_ComplexityThresholdOrdering(t *testing.T) {
	cfg := validConfig()
	cfg.Complexity.Thresholds.SimpleMaxFiles = ptr.Int(10)
	cfg.Complexity.Thresholds.StandardMaxFiles = ptr.Int(3)
	cfg.Complexity.Thresholds.ComplexMaxFiles = ptr.Int(30)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for unordered complexity thresholds")
	}
	if !strings.Contains(err.Error(), "complexity.thresholds") {
		t.Fatalf("expected complexity.thresholds in error, got: %v", err)
	}
}

// --- NaN/Inf rejection tests ---

func TestValidate_DebounceSec_NaN(t *testing.T) {
	cfg := validConfig()
	cfg.Watcher.DebounceSec = math.NaN()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for NaN debounce_sec")
	}
	if !strings.Contains(err.Error(), "watcher.debounce_sec") {
		t.Fatalf("expected watcher.debounce_sec in error, got: %v", err)
	}
}

func TestValidate_DebounceSec_Inf(t *testing.T) {
	cfg := validConfig()
	cfg.Watcher.DebounceSec = math.Inf(1)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for +Inf debounce_sec")
	}
	if !strings.Contains(err.Error(), "watcher.debounce_sec") {
		t.Fatalf("expected watcher.debounce_sec in error, got: %v", err)
	}
}

func TestValidate_Float64Ptr_NaN_Inf(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	tests := []struct {
		name     string
		setup    func(*Config)
		errField string
	}{
		{"evolution novelty NaN", func(c *Config) { c.Evolution.NoveltyThreshold = &nan }, "evolution.novelty_threshold"},
		{"evolution novelty Inf", func(c *Config) { c.Evolution.NoveltyThreshold = &inf }, "evolution.novelty_threshold"},
		{"bandit exploration NaN", func(c *Config) { c.Bandit.ExplorationCoeff = &nan }, "bandit.exploration_coefficient"},
		{"search prune NaN", func(c *Config) { c.Search.PruneThreshold = &nan }, "search.prune_threshold"},
		{"search alpha Inf", func(c *Config) { c.Search.ThompsonAlpha = &inf }, "search.thompson_alpha"},
		{"search beta NaN", func(c *Config) { c.Search.ThompsonBeta = &nan }, "search.thompson_beta"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.setup(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected error for NaN/Inf float64 field")
			}
			if !strings.Contains(err.Error(), tt.errField) {
				t.Errorf("expected %q in error, got: %v", tt.errField, err)
			}
		})
	}
}

// --- NormalizeExperimentalConfig tests ---

func TestNormalizeExperimentalConfig(t *testing.T) {
	t.Parallel()
	cfg := &Config{}
	NormalizeExperimentalConfig(cfg)

	// C-1 Evolution
	if cfg.Evolution.Enabled == nil || *cfg.Evolution.Enabled != false {
		t.Error("Evolution.Enabled should default to false")
	}
	if cfg.Evolution.MaxMutationsPerRound == nil || *cfg.Evolution.MaxMutationsPerRound != DefaultMaxMutationsPerRound {
		t.Errorf("Evolution.MaxMutationsPerRound = %v, want %d", cfg.Evolution.MaxMutationsPerRound, DefaultMaxMutationsPerRound)
	}
	if len(cfg.Evolution.Strategies) == 0 {
		t.Error("Evolution.Strategies should have defaults")
	}

	// C-2 Bandit
	if cfg.Bandit.Enabled == nil || *cfg.Bandit.Enabled != false {
		t.Error("Bandit.Enabled should default to false")
	}
	if cfg.Bandit.ExplorationCoeff == nil || *cfg.Bandit.ExplorationCoeff != DefaultExplorationCoeff {
		t.Errorf("Bandit.ExplorationCoeff = %v, want %f", cfg.Bandit.ExplorationCoeff, DefaultExplorationCoeff)
	}

	// C-3 Extended Verification
	if cfg.ExtendedVerification.Enabled == nil || *cfg.ExtendedVerification.Enabled != false {
		t.Error("ExtendedVerification.Enabled should default to false")
	}

	// C-4 Search
	if cfg.Search.Enabled == nil || *cfg.Search.Enabled != false {
		t.Error("Search.Enabled should default to false")
	}
	if cfg.Search.MaxDepth == nil || *cfg.Search.MaxDepth != DefaultSearchMaxDepth {
		t.Errorf("Search.MaxDepth = %v, want %d", cfg.Search.MaxDepth, DefaultSearchMaxDepth)
	}

	// C-5 Self-Improvement
	if cfg.SelfImprovement.Enabled == nil || *cfg.SelfImprovement.Enabled != false {
		t.Error("SelfImprovement.Enabled should default to false")
	}
	if len(cfg.SelfImprovement.Targets) == 0 {
		t.Error("SelfImprovement.Targets should have defaults")
	}

	// C-6 Complexity
	if cfg.Complexity.Enabled == nil || *cfg.Complexity.Enabled != false {
		t.Error("Complexity.Enabled should default to false")
	}
	if cfg.Complexity.Thresholds.SimpleMaxFiles == nil || *cfg.Complexity.Thresholds.SimpleMaxFiles != DefaultSimpleMaxFiles {
		t.Errorf("Complexity.SimpleMaxFiles = %v, want %d", cfg.Complexity.Thresholds.SimpleMaxFiles, DefaultSimpleMaxFiles)
	}
}

func TestNormalizeExperimentalConfig_Idempotent(t *testing.T) {
	t.Parallel()
	cfg := &Config{}
	NormalizeExperimentalConfig(cfg)
	NormalizeExperimentalConfig(cfg)

	// Should not change values on second call
	if cfg.Evolution.MaxMutationsPerRound == nil || *cfg.Evolution.MaxMutationsPerRound != DefaultMaxMutationsPerRound {
		t.Error("second normalization changed Evolution.MaxMutationsPerRound")
	}
}

// --- New validation rule tests ---

func TestValidate_QueuePriorityAgingSec_UpperBound(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxPriorityAgingSec, false},
		{"exceeds_max", MaxPriorityAgingSec + 1, true},
		{"negative", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Queue.PriorityAgingSec = tt.val
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "queue.priority_aging_sec") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_RetryCommandDispatch_UpperBound(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxCommandDispatchRetries, false},
		{"exceeds_max", MaxCommandDispatchRetries + 1, true},
		{"negative", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Retry.CommandDispatch = tt.val
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "retry.command_dispatch") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_RetryTaskDispatch_UpperBound(t *testing.T) {
	tests := []struct {
		name    string
		val     int
		wantErr bool
	}{
		{"at_max", MaxTaskDispatchRetries, false},
		{"exceeds_max", MaxTaskDispatchRetries + 1, true},
		{"negative", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Retry.TaskDispatch = tt.val
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "retry.task_dispatch") {
				t.Fatalf("expected field path in error, got: %v", err)
			}
		})
	}
}

func TestValidate_ContinuousMaxIterations_AlwaysValidated(t *testing.T) {
	cfg := validConfig()
	cfg.Continuous.Enabled = false
	cfg.Continuous.MaxIterations = -5
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative max_iterations even when disabled")
	}
	if !strings.Contains(err.Error(), "continuous.max_iterations") {
		t.Fatalf("expected continuous.max_iterations in error, got: %v", err)
	}
}

func TestValidate_DispatchLeaseVsMaxInProgress(t *testing.T) {
	tests := []struct {
		name             string
		dispatchLeaseSec int
		maxInProgressMin *int
		wantErr          bool
	}{
		{"lease_exceeds_runtime", 3600, ptr.Int(30), true},   // 3600 >= 30*60=1800 -> true (3600 >= 1800)
		{"lease_equals_runtime", 1800, ptr.Int(30), true},    // 1800 >= 1800 -> error
		{"lease_below_runtime", 1799, ptr.Int(30), false},    // 1799 < 1800 -> ok
		{"lease_zero_skips_check", 0, ptr.Int(1), false},     // dispatch_lease_sec=0 -> skip
		{"runtime_zero_skips_check", 300, ptr.Int(0), false}, // maxInProgressSec=0 -> skip
		{"nil_runtime_uses_default", 300, nil, false},        // default=60min=3600s, 300<3600 -> ok
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Watcher.DispatchLeaseSec = tt.dispatchLeaseSec
			cfg.Watcher.MaxInProgressMin = tt.maxInProgressMin
			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), "watcher.dispatch_lease_sec") {
					t.Fatalf("expected watcher.dispatch_lease_sec in error, got: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

// --- worktree.base_branch validation tests ---

func TestValidate_WorktreeBaseBranch_Valid(t *testing.T) {
	valid := []string{
		"", // empty uses default
		"main",
		"develop",
		"feature/foo",
		"release/v1.2.3",
		"my-branch",
		"my_branch",
		"v1.0",
	}
	for _, branch := range valid {
		t.Run("valid_"+branch, func(t *testing.T) {
			cfg := validConfig()
			cfg.Worktree.BaseBranch = branch
			if err := cfg.Validate(); err != nil {
				t.Fatalf("expected no error for base_branch=%q, got: %v", branch, err)
			}
		})
	}
}

func TestValidate_WorktreeBaseBranch_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		branch string
		errMsg string
	}{
		{"double_dot", "main..develop", "must not contain \"..\""},
		{"space", "my branch", "contains invalid characters"},
		{"control_char", "main\x00", "contains invalid characters"},
		{"tilde", "main~1", "contains invalid characters"},
		{"caret", "main^2", "contains invalid characters"},
		{"colon", "refs:heads", "contains invalid characters"},
		{"question_mark", "main?", "contains invalid characters"},
		{"asterisk", "main*", "contains invalid characters"},
		{"backslash", "main\\dev", "contains invalid characters"},
		{"trailing_dot", "main.", "must not end with \".\" or \"/\""},
		{"trailing_slash", "feature/", "must not end with \".\" or \"/\""},
		{"dot_lock_suffix", "main.lock", "must not end with \".lock\""},
		{"consecutive_slashes", "feature//bar", "must not contain consecutive slashes"},
		{"starts_with_hyphen", "-branch", "contains invalid characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Worktree.BaseBranch = tt.branch
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for base_branch=%q", tt.branch)
			}
			if !strings.Contains(err.Error(), "worktree.base_branch") {
				t.Fatalf("expected worktree.base_branch in error, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("expected %q in error, got: %v", tt.errMsg, err)
			}
		})
	}
}

// --- worktree.path_prefix validation tests ---

func TestValidate_WorktreePathPrefix_Valid(t *testing.T) {
	valid := []string{
		"", // empty uses default
		".maestro/worktrees",
		"worktrees",
		"build/output",
		"my-prefix",
		"my_prefix",
		".hidden/dir",
	}
	for _, prefix := range valid {
		t.Run("valid_"+prefix, func(t *testing.T) {
			cfg := validConfig()
			cfg.Worktree.PathPrefix = prefix
			if err := cfg.Validate(); err != nil {
				t.Fatalf("expected no error for path_prefix=%q, got: %v", prefix, err)
			}
		})
	}
}

func TestValidate_WorktreePathPrefix_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		errMsg string
	}{
		{"absolute_path", "/tmp/worktrees", "must be a relative path"},
		{"traversal_prefix", "../escape", "must not contain path traversal"},
		{"traversal_middle", "foo/../bar", "must not contain path traversal"},
		{"traversal_suffix", "foo/..", "must not contain path traversal"},
		{"space", "my prefix", "contains invalid characters"},
		{"control_char", "dir\x00name", "contains invalid characters"},
		{"trailing_slash", "worktrees/", "must not end with \"/\""},
		{"starts_with_hyphen", "-prefix", "contains invalid characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Worktree.PathPrefix = tt.prefix
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error for path_prefix=%q", tt.prefix)
			}
			if !strings.Contains(err.Error(), "worktree.path_prefix") {
				t.Fatalf("expected worktree.path_prefix in error, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("expected %q in error, got: %v", tt.errMsg, err)
			}
		})
	}
}

// TestFeatureProfile_Defaults pins the zero-value FeatureProfile semantics:
// every feature toggle defaults to disabled.
func TestFeatureProfile_Defaults(t *testing.T) {
	fp := FeatureProfile{}
	if fp.EffectiveCrossAgentReview() {
		t.Errorf("default CrossAgentReview = %v, want false", fp.EffectiveCrossAgentReview())
	}
	if fp.EffectiveExploratoryOptimization() {
		t.Error("default ExploratoryOptimization should be false")
	}
	if fp.EffectiveEvolutionaryQuality() {
		t.Error("default EvolutionaryQuality should be false")
	}
	if fp.EffectiveAdaptiveModelSelection() {
		t.Error("default AdaptiveModelSelection should be false")
	}
	if fp.EffectiveSelfImprovement() {
		t.Error("default SelfImprovement should be false")
	}
	if fp.EffectiveAdaptiveDepth() {
		t.Error("default AdaptiveDepth should be false")
	}
}
