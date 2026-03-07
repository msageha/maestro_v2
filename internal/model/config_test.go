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

func TestValidate_QualityGateRateOutOfRange(t *testing.T) {
	cfg := validConfig()
	cfg.QualityGates.Thresholds.MaxTaskFailureRate = 1.5
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for failure rate > 1.0")
	}
	if !strings.Contains(err.Error(), "quality_gates.thresholds.max_task_failure_rate") {
		t.Fatalf("expected quality_gates.thresholds.max_task_failure_rate in error, got: %v", err)
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
	cfg.Daemon.ShutdownTimeoutSec = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative daemon timeout")
	}
	if !strings.Contains(err.Error(), "daemon.shutdown_timeout_sec") {
		t.Fatalf("expected daemon.shutdown_timeout_sec in error, got: %v", err)
	}
}

func TestValidate_NegativeCircuitBreaker(t *testing.T) {
	cfg := validConfig()
	cfg.CircuitBreaker.MaxConsecutiveFailures = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative circuit breaker field")
	}
	if !strings.Contains(err.Error(), "circuit_breaker.max_consecutive_failures") {
		t.Fatalf("expected circuit_breaker.max_consecutive_failures in error, got: %v", err)
	}
}

func TestValidate_NegativeVerification(t *testing.T) {
	cfg := validConfig()
	cfg.Verification.TimeoutSeconds = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative verification timeout")
	}
	if !strings.Contains(err.Error(), "verification.timeout_seconds") {
		t.Fatalf("expected verification.timeout_seconds in error, got: %v", err)
	}
}

func TestValidate_NegativeLearnings(t *testing.T) {
	cfg := validConfig()
	cfg.Learnings.MaxEntries = -1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for negative learnings max_entries")
	}
	if !strings.Contains(err.Error(), "learnings.max_entries") {
		t.Fatalf("expected learnings.max_entries in error, got: %v", err)
	}
}

func TestValidate_PersonaValid(t *testing.T) {
	cfg := validConfig()
	cfg.Personas = map[string]PersonaConfig{
		"code-reviewer": {Description: "Reviews code", Prompt: "You are a code reviewer."},
		"architect":     {Description: "System architect", Prompt: "You are a system architect."},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected no error for valid personas, got: %v", err)
	}
}

func TestValidate_PersonaInvalidName(t *testing.T) {
	cfg := validConfig()
	cfg.Personas = map[string]PersonaConfig{
		"invalid name with spaces": {Prompt: "some prompt"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for invalid persona name")
	}
	if !strings.Contains(err.Error(), "personas.invalid name with spaces") {
		t.Fatalf("expected persona name in error, got: %v", err)
	}
}

func TestValidate_PersonaEmptyPrompt(t *testing.T) {
	cfg := validConfig()
	cfg.Personas = map[string]PersonaConfig{
		"valid-name": {Description: "desc", Prompt: ""},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for empty persona prompt")
	}
	if !strings.Contains(err.Error(), "personas.valid-name.prompt") {
		t.Fatalf("expected personas.valid-name.prompt in error, got: %v", err)
	}
}

func TestValidate_PersonaWhitespaceOnlyPrompt(t *testing.T) {
	cfg := validConfig()
	cfg.Personas = map[string]PersonaConfig{
		"valid-name": {Description: "desc", Prompt: "   \t\n  "},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for whitespace-only persona prompt")
	}
	if !strings.Contains(err.Error(), "personas.valid-name.prompt") {
		t.Fatalf("expected personas.valid-name.prompt in error, got: %v", err)
	}
}

func TestValidate_PersonaMultipleErrors(t *testing.T) {
	cfg := validConfig()
	cfg.Personas = map[string]PersonaConfig{
		"valid-persona":   {Prompt: "valid prompt"},
		"../bad-traverse": {Prompt: "prompt"},
		"ok-name":         {Prompt: ""},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected errors for invalid personas")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "personas.../bad-traverse") {
		t.Errorf("expected invalid name error, got: %v", err)
	}
	if !strings.Contains(errStr, "personas.ok-name.prompt") {
		t.Errorf("expected empty prompt error, got: %v", err)
	}
}
