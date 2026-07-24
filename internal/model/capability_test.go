package model

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultCapabilitiesForRuntime(t *testing.T) {
	tests := []struct {
		runtime string
		want    []string
	}{
		{RuntimeClaudeCode, []string{
			CapabilityCodeQuality, CapabilityDesignReview,
			CapabilityInstructionFollowing, CapabilityRunOnMain,
		}},
		{RuntimeCodex, []string{
			CapabilityBulkImplementation, CapabilityLongHorizonAutonomy,
			CapabilityRefactor,
		}},
		{RuntimeGemini, []string{
			CapabilityMultimodal, CapabilityLongContextIngest,
			CapabilityResearch, CapabilityCostEfficient,
		}},
		{"unknown-runtime", nil},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.runtime, func(t *testing.T) {
			got := DefaultCapabilitiesForRuntime(tt.runtime)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DefaultCapabilitiesForRuntime(%q) = %v, want %v", tt.runtime, got, tt.want)
			}
		})
	}
}

func TestDefaultCapabilitiesForRuntime_ReturnsFreshSlice(t *testing.T) {
	a := DefaultCapabilitiesForRuntime(RuntimeClaudeCode)
	a[0] = "mutated"
	b := DefaultCapabilitiesForRuntime(RuntimeClaudeCode)
	if b[0] != CapabilityCodeQuality {
		t.Errorf("mutation of a returned slice leaked into subsequent calls: got %v", b)
	}
}

func TestWorkerConfigCapabilitiesFor(t *testing.T) {
	config := WorkerConfig{
		Count:        4,
		DefaultModel: "sonnet",
		Models: map[string]string{
			"worker2": "codex",
			"worker3": "gemini-2.5-pro",
		},
		Capabilities: map[string][]string{
			"worker4": {"custom_tag", CapabilityResearch},
		},
	}

	tests := []struct {
		workerID string
		want     []string
	}{
		// worker1 → default_model sonnet → claude-code defaults
		{"worker1", DefaultCapabilitiesForRuntime(RuntimeClaudeCode)},
		// worker2 → codex → codex defaults
		{"worker2", DefaultCapabilitiesForRuntime(RuntimeCodex)},
		// worker3 → gemini-2.5-pro → gemini defaults
		{"worker3", DefaultCapabilitiesForRuntime(RuntimeGemini)},
		// worker4 → explicit override wins over runtime defaults
		{"worker4", []string{"custom_tag", CapabilityResearch}},
	}
	for _, tt := range tests {
		t.Run(tt.workerID, func(t *testing.T) {
			got := config.CapabilitiesFor(tt.workerID)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CapabilitiesFor(%q) = %v, want %v", tt.workerID, got, tt.want)
			}
		})
	}
}

func TestWorkerConfigCapabilitiesFor_ExplicitEmptyListMeansNoCapabilities(t *testing.T) {
	config := WorkerConfig{
		Count:        1,
		DefaultModel: "sonnet",
		Capabilities: map[string][]string{"worker1": {}},
	}
	got := config.CapabilitiesFor("worker1")
	if len(got) != 0 {
		t.Errorf("explicit empty capability list must not fall back to runtime defaults, got %v", got)
	}
}

func TestWorkerConfigCapabilitiesFor_BoostUsesClaudeDefaults(t *testing.T) {
	// Boost forces every worker to opus (claude-code runtime), so the implied
	// default capabilities must be the claude-code set even when the per-worker
	// model would otherwise be codex.
	config := WorkerConfig{
		Count:        1,
		DefaultModel: "sonnet",
		Models:       map[string]string{"worker1": "codex"},
		Boost:        true,
	}
	got := config.CapabilitiesFor("worker1")
	want := DefaultCapabilitiesForRuntime(RuntimeClaudeCode)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CapabilitiesFor under boost = %v, want claude defaults %v", got, want)
	}
}

func TestWorkerConfigCapabilitiesYAMLDecode_NonStrict(t *testing.T) {
	// capabilities parses from YAML, and unknown sibling fields must not
	// break decoding (non-strict policy: unknown fields never stop the daemon).
	src := `
count: 2
default_model: "sonnet"
models:
  worker2: "codex"
capabilities:
  worker1: ["code_quality", "run_on_main"]
  worker2: []
some_future_unknown_field: true
`
	var w WorkerConfig
	if err := yaml.Unmarshal([]byte(src), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := w.CapabilitiesFor("worker1"); !reflect.DeepEqual(got, []string{"code_quality", "run_on_main"}) {
		t.Errorf("worker1 capabilities = %v", got)
	}
	if got := w.CapabilitiesFor("worker2"); len(got) != 0 {
		t.Errorf("worker2 explicit empty list = %v, want empty", got)
	}
}

func TestTaskCapabilityFieldsYAMLRoundTrip(t *testing.T) {
	task := Task{
		ID:                    "task_1",
		CommandID:             "cmd_1",
		Purpose:               "p",
		Content:               "c",
		AcceptanceCriteria:    "a",
		Status:                StatusPending,
		RequiredCapabilities:  []string{CapabilityResearch},
		PreferredCapabilities: []string{CapabilityCostEfficient},
	}
	data, err := yaml.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Task
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.RequiredCapabilities, task.RequiredCapabilities) {
		t.Errorf("required_capabilities round-trip = %v, want %v", got.RequiredCapabilities, task.RequiredCapabilities)
	}
	if !reflect.DeepEqual(got.PreferredCapabilities, task.PreferredCapabilities) {
		t.Errorf("preferred_capabilities round-trip = %v, want %v", got.PreferredCapabilities, task.PreferredCapabilities)
	}
}

func TestTaskCapabilityFieldsOmittedWhenEmpty(t *testing.T) {
	task := Task{ID: "task_1", CommandID: "cmd_1", Status: StatusPending}
	data, err := yaml.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "required_capabilities") ||
		strings.Contains(string(data), "preferred_capabilities") {
		t.Errorf("empty capability fields must be omitted (omitempty), got:\n%s", data)
	}
}

func TestConfigValidate_EmptyCapabilityTagRejected(t *testing.T) {
	cfg := validConfigForCapabilityTest()
	cfg.Agents.Workers.Capabilities = map[string][]string{
		"worker1": {"code_quality", "   "},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for whitespace-only capability tag")
	}
	if !strings.Contains(err.Error(), "agents.workers.capabilities.worker1[1]") {
		t.Errorf("error should name the offending entry, got: %v", err)
	}
}

func TestConfigValidate_CapabilityTagsAccepted(t *testing.T) {
	cfg := validConfigForCapabilityTest()
	cfg.Agents.Workers.Capabilities = map[string][]string{
		"worker1": {"code_quality", "custom_domain_tag"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

// validConfigForCapabilityTest builds a minimal Config that passes Validate,
// as a base for capability-specific validation cases.
func validConfigForCapabilityTest() Config {
	return Config{
		Project: ProjectConfig{Name: "test"},
		Maestro: MaestroConfig{Version: "2.0.0"},
		Agents: AgentsConfig{
			Orchestrator: AgentConfig{ID: "orchestrator", Model: "opus"},
			Planner:      AgentConfig{ID: "planner", Model: "opus"},
			Workers:      WorkerConfig{Count: 2, DefaultModel: "sonnet"},
		},
	}
}
