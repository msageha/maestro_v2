package model

import (
	"strings"
	"testing"
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
	cfg.CircuitBreaker.MaxConsecutiveFailures = IntPtr(-1)
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
	cfg.Learnings.MaxEntries = IntPtr(-1)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative learnings max_entries")
	}
	if !strings.Contains(err.Error(), "learnings.max_entries") {
		t.Fatalf("expected learnings.max_entries in error, got: %v", err)
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
	if v := ac.EffectiveMaxConcurrentRollout(); v != 1 {
		t.Errorf("expected default rollout=1, got %d", v)
	}
}

func TestValidate_AdmissionControl_Configured(t *testing.T) {
	ac := AdmissionControl{
		MaxConcurrentVerify:  4,
		MaxConcurrentRepair:  2,
		MaxConcurrentRollout: 3,
	}
	if v := ac.EffectiveMaxConcurrentVerify(); v != 4 {
		t.Errorf("expected verify=4, got %d", v)
	}
	if v := ac.EffectiveMaxConcurrentRepair(); v != 2 {
		t.Errorf("expected repair=2, got %d", v)
	}
	if v := ac.EffectiveMaxConcurrentRollout(); v != 3 {
		t.Errorf("expected rollout=3, got %d", v)
	}
}

func TestValidate_AdmissionControl_NegativeValues(t *testing.T) {
	tests := []struct {
		name    string
		cfg     func(*Config)
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
		{
			"negative rollout",
			func(c *Config) { c.AdmissionControl.MaxConcurrentRollout = -1 },
			"admission_control.max_concurrent_rollout",
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
		Enabled:              BoolPtr(true),
		MaxMutationsPerRound: IntPtr(5),
		NoveltyThreshold:     Float64Ptr(0.8),
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
	if v := bc.EffectiveDecayFactor(); v != 0.95 {
		t.Errorf("EffectiveDecayFactor() = %v, want 0.95", v)
	}
	if v := bc.EffectiveTraceDataRequirement(); v != 50 {
		t.Errorf("EffectiveTraceDataRequirement() = %d, want 50", v)
	}
}

func TestBanditConfig_Configured(t *testing.T) {
	bc := BanditConfig{
		Enabled:              BoolPtr(true),
		ExplorationCoeff:     Float64Ptr(2.0),
		MinSamplesBeforeUse:  IntPtr(20),
		DecayFactor:          Float64Ptr(0.9),
		TraceDataRequirement: IntPtr(100),
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
	if v := bc.EffectiveDecayFactor(); v != 0.9 {
		t.Errorf("EffectiveDecayFactor() = %v, want 0.9", v)
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
	if ev.EffectiveSecurityCheck() {
		t.Error("default SecurityCheck should be false")
	}
	if ev.EffectivePerformanceBench() {
		t.Error("default PerformanceBench should be false")
	}
	weights := ev.EffectivePerspectiveWeights()
	if weights["build"] != 1.0 || weights["test"] != 1.0 || weights["security"] != 0.5 {
		t.Errorf("unexpected default weights: %v", weights)
	}
	if v := ev.EffectiveMaxAutoRetries(); v != 2 {
		t.Errorf("EffectiveMaxAutoRetries() = %d, want 2", v)
	}
}

func TestExtendedVerificationConfig_Configured(t *testing.T) {
	ev := ExtendedVerificationConfig{
		Enabled:            BoolPtr(true),
		SecurityCheck:      BoolPtr(true),
		PerformanceBench:   BoolPtr(true),
		PerspectiveWeights: map[string]float64{"build": 2.0},
		MaxAutoRetries:     IntPtr(5),
	}
	if !ev.EffectiveEnabled() {
		t.Error("Enabled should be true")
	}
	if !ev.EffectiveSecurityCheck() {
		t.Error("SecurityCheck should be true")
	}
	if !ev.EffectivePerformanceBench() {
		t.Error("PerformanceBench should be true")
	}
	weights := ev.EffectivePerspectiveWeights()
	if weights["build"] != 2.0 {
		t.Errorf("weights[build] = %v, want 2.0", weights["build"])
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
		Enabled:        BoolPtr(true),
		MaxDepth:       IntPtr(5),
		MaxBranching:   IntPtr(8),
		PruneThreshold: Float64Ptr(0.5),
		ThompsonAlpha:  Float64Ptr(2.0),
		ThompsonBeta:   Float64Ptr(3.0),
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
		Enabled:        BoolPtr(true),
		Targets:        []string{"persona"},
		ExcludeTargets: []string{"fitness"},
		ArchiveMaxSize: IntPtr(50),
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

func TestValidate_ContinuousDisabled_NegativeFieldsOK(t *testing.T) {
	cfg := validConfig()
	cfg.Continuous.Enabled = false
	cfg.Continuous.MaxIterations = -1
	cfg.Continuous.MaxConsecutiveFailures = -1
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("expected no error when continuous disabled, got: %v", err)
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
	cfg.Worktree.GC.TTLHours = IntPtr(0)
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
	cfg.Worktree.GC.MaxWorktrees = IntPtr(-1)
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
	cfg.Worktree.GC.TTLHours = IntPtr(0)
	cfg.Worktree.GC.MaxWorktrees = IntPtr(-1)
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("expected no error when gc disabled, got: %v", err)
	}
}

func TestValidate_WorktreeGC_Enabled_ValidFields(t *testing.T) {
	cfg := validConfig()
	cfg.Worktree.GC.Enabled = true
	cfg.Worktree.GC.TTLHours = IntPtr(24)
	cfg.Worktree.GC.MaxWorktrees = IntPtr(5)
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
			cfg.Worktree.GitTimeoutSec = IntPtr(tt.val)
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
			cfg.Watcher.MaxInProgressMin = IntPtr(tt.val)
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
			cfg.Limits.MaxDeadLetterArchiveFiles = IntPtr(tt.val)
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
			cfg.Limits.MaxQuarantineFiles = IntPtr(tt.val)
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
			cfg.Worktree.GC.TTLHours = IntPtr(24)
			cfg.Worktree.GC.MaxWorktrees = IntPtr(tt.val)
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

// --- C-6 ComplexityConfig tests ---

func TestComplexityConfig_Defaults(t *testing.T) {
	cc := ComplexityConfig{}
	if cc.EffectiveEnabled() {
		t.Error("default Enabled should be false")
	}
}

