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

