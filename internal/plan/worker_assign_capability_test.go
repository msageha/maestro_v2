package plan

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// Mixed-runtime fleet used by most capability routing tests: worker1 is
// claude (sonnet), worker2 is codex, worker3 is gemini. Capabilities mirror
// the runtime defaults, the same values BuildWorkerStates would populate.
func capabilityFleet() []WorkerState {
	return []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", Capabilities: model.DefaultCapabilitiesForRuntime(model.RuntimeClaudeCode)},
		{WorkerID: "worker2", Model: "codex", Capabilities: model.DefaultCapabilitiesForRuntime(model.RuntimeCodex)},
		{WorkerID: "worker3", Model: "gemini", Capabilities: model.DefaultCapabilitiesForRuntime(model.RuntimeGemini)},
	}
}

func capabilityFleetConfig() model.WorkerConfig {
	return model.WorkerConfig{
		Count:        3,
		DefaultModel: "sonnet",
		Models:       map[string]string{"worker2": "codex", "worker3": "gemini"},
	}
}

func TestAssignWorkers_RequiredCapabilityRoutesToMatchingWorker(t *testing.T) {
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	// research is a gemini default capability; bloom 2 statically maps to
	// sonnet, so this also exercises the family fallback inside the
	// capability-filtered candidate set (gemini is the only candidate).
	tasks := []TaskAssignmentRequest{
		{Name: "investigate", BloomLevel: 2, RequiredCapabilities: []string{model.CapabilityResearch}},
	}

	got, err := AssignWorkers(capabilityFleetConfig(), limits, capabilityFleet(), tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker3" {
		t.Fatalf("assignment = %+v, want worker3 (only research-capable worker)", got)
	}
	if got[0].Model != "gemini" {
		t.Errorf("model = %q, want gemini", got[0].Model)
	}
}

func TestAssignWorkers_RequiredCapabilityMultipleTagsAllMustMatch(t *testing.T) {
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	// code_quality + design_review are both claude defaults; codex/gemini
	// each hold neither, so worker1 is the only candidate even though it has
	// higher load than the others.
	fleet := capabilityFleet()
	fleet[0].PendingCount = 5
	tasks := []TaskAssignmentRequest{
		{Name: "review-arch", BloomLevel: 5, RequiredCapabilities: []string{
			model.CapabilityCodeQuality, model.CapabilityDesignReview,
		}},
	}

	got, err := AssignWorkers(capabilityFleetConfig(), limits, fleet, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker1" {
		t.Fatalf("assignment = %+v, want worker1 (only worker with both capabilities)", got)
	}
}

func TestAssignWorkers_RequiredCapabilityUnmetFailsLoudly(t *testing.T) {
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	tasks := []TaskAssignmentRequest{
		{Name: "video-analysis", BloomLevel: 3, RequiredCapabilities: []string{"no_such_capability"}},
	}

	_, err := AssignWorkers(capabilityFleetConfig(), limits, capabilityFleet(), tasks)
	if err == nil {
		t.Fatal("expected error when no worker advertises the required capability")
	}
	if !errors.Is(err, ErrNoAvailableWorker) {
		t.Errorf("err = %v, want ErrNoAvailableWorker", err)
	}
	if !strings.Contains(err.Error(), "no_such_capability") {
		t.Errorf("error %q does not name the missing capability", err.Error())
	}
	if !strings.Contains(err.Error(), "agents.workers.capabilities") {
		t.Errorf("error %q does not point at the config remedy", err.Error())
	}
}

func TestAssignWorkers_PreferredCapabilityBiasesSelection(t *testing.T) {
	// Two sonnet workers with equal load: worker1 would win the lexicographic
	// tie-break, but worker2 advertises the preferred capability and must win.
	config := model.WorkerConfig{Count: 2, DefaultModel: "sonnet"}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", Capabilities: []string{model.CapabilityCodeQuality}},
		{WorkerID: "worker2", Model: "sonnet", Capabilities: []string{model.CapabilityCodeQuality, model.CapabilityRefactor}},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "mass-rename", BloomLevel: 2, PreferredCapabilities: []string{model.CapabilityRefactor}},
	}

	got, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker2" {
		t.Fatalf("assignment = %+v, want worker2 (preferred capability match)", got)
	}
}

func TestAssignWorkers_PreferredCapabilityOutranksLoad(t *testing.T) {
	// Issue #34 ordering: (b) preferred scoring → (c) least loaded → (d)
	// lexicographic. A loaded worker matching the preferred tag beats an idle
	// worker without it (capacity limits still apply).
	config := model.WorkerConfig{Count: 2, DefaultModel: "sonnet"}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", PendingCount: 0, Capabilities: nil},
		{WorkerID: "worker2", Model: "sonnet", PendingCount: 3, Capabilities: []string{model.CapabilityRefactor}},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "refactor-pkg", BloomLevel: 2, PreferredCapabilities: []string{model.CapabilityRefactor}},
	}

	got, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker2" {
		t.Fatalf("assignment = %+v, want worker2 (preferred match outranks load)", got)
	}
}

func TestAssignWorkers_PreferredCapabilityUnmetStillAssigns(t *testing.T) {
	// No worker matches the preferred tag → scoring is uniformly zero and the
	// legacy least-loaded + lexicographic selection applies unchanged.
	config := model.WorkerConfig{Count: 2, DefaultModel: "sonnet"}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", PendingCount: 2},
		{WorkerID: "worker2", Model: "sonnet", PendingCount: 1},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "t1", BloomLevel: 2, PreferredCapabilities: []string{model.CapabilityMultimodal}},
	}

	got, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker2" {
		t.Fatalf("assignment = %+v, want worker2 (least loaded; preferred is soft)", got)
	}
}

func TestAssignWorkers_NoCapabilitiesIdenticalToLegacy(t *testing.T) {
	// Backward compatibility: tasks without capability declarations must land
	// exactly where the pre-capability logic put them, regardless of what the
	// workers advertise.
	config := model.WorkerConfig{Count: 3, DefaultModel: "sonnet"}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", PendingCount: 5, Capabilities: model.DefaultCapabilitiesForRuntime(model.RuntimeClaudeCode)},
		{WorkerID: "worker2", Model: "sonnet", PendingCount: 2, Capabilities: model.DefaultCapabilitiesForRuntime(model.RuntimeClaudeCode)},
		{WorkerID: "worker3", Model: "sonnet", PendingCount: 2, Capabilities: nil},
	}
	tasks := []TaskAssignmentRequest{{Name: "t1", BloomLevel: 2}}

	got, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Same load on worker2/worker3 → lexicographic tie-break → worker2,
	// exactly as before capabilities existed.
	if len(got) != 1 || got[0].WorkerID != "worker2" {
		t.Fatalf("assignment = %+v, want worker2 (legacy least-loaded + lexicographic)", got)
	}
}

func TestAssignWorkers_RequiredCapabilityWithRunOnMainGuard(t *testing.T) {
	// RequireClaudeRuntime (run_on_main) and required capabilities combine:
	// worker2 (codex) advertises the tag too, but only claude-code satisfies
	// the runtime guard.
	config := model.WorkerConfig{Count: 2, DefaultModel: "sonnet", Models: map[string]string{"worker2": "codex"}}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", Capabilities: []string{"verify_main"}},
		{WorkerID: "worker2", Model: "codex", PendingCount: 0, Capabilities: []string{"verify_main"}},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "main-verify", BloomLevel: 3, RequireClaudeRuntime: true, RequiredCapabilities: []string{"verify_main"}},
	}

	got, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker1" {
		t.Fatalf("assignment = %+v, want worker1 (claude runtime required)", got)
	}
}

func TestAssignWorkers_RequiredCapabilityCandidatesAtCapacity(t *testing.T) {
	// The only capable worker is at capacity → error must mention capacity
	// and the capability restriction, not claim the capability is missing.
	config := model.WorkerConfig{Count: 2, DefaultModel: "sonnet"}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 1}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", PendingCount: 1, Capabilities: []string{model.CapabilityResearch}},
		{WorkerID: "worker2", Model: "sonnet", PendingCount: 0},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "t1", BloomLevel: 2, RequiredCapabilities: []string{model.CapabilityResearch}},
	}

	_, err := AssignWorkers(config, limits, workers, tasks)
	if err == nil {
		t.Fatal("expected capacity error")
	}
	if !errors.Is(err, ErrNoAvailableWorker) {
		t.Errorf("err = %v, want ErrNoAvailableWorker", err)
	}
	if !strings.Contains(err.Error(), "capacity") {
		t.Errorf("error %q should mention capacity", err.Error())
	}
	if !strings.Contains(err.Error(), "required_capabilities") {
		t.Errorf("error %q should mention the capability restriction", err.Error())
	}
}

func TestAssignWorkers_PinnedWorkerOverridesRequiredCapabilities(t *testing.T) {
	// An explicit worker_id pin bypasses capability matching (WARN only):
	// pinning is a deterministic contract and must stay authoritative.
	config := model.WorkerConfig{Count: 2, DefaultModel: "sonnet"}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", Capabilities: nil},
		{WorkerID: "worker2", Model: "sonnet", Capabilities: []string{model.CapabilityResearch}},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "t1", BloomLevel: 2, PinnedWorkerID: "worker1", RequiredCapabilities: []string{model.CapabilityResearch}},
	}

	got, err := AssignWorkers(config, limits, workers, tasks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker1" {
		t.Fatalf("assignment = %+v, want pinned worker1 despite capability mismatch", got)
	}
}

func TestAssignWorkers_ModelSelectorPickOutsideCapabilitySetIgnored(t *testing.T) {
	// The bandit picks "opus" but the only opus worker lacks the required
	// capability → the pick is infeasible within the candidate set and the
	// static (fallback) path must select the capable worker instead.
	config := model.WorkerConfig{Count: 2, DefaultModel: "sonnet", Models: map[string]string{"worker2": "opus"}}
	limits := model.LimitsConfig{MaxPendingTasksPerWorker: 10}
	workers := []WorkerState{
		{WorkerID: "worker1", Model: "sonnet", Capabilities: []string{model.CapabilityResearch}},
		{WorkerID: "worker2", Model: "opus", Capabilities: nil},
	}
	tasks := []TaskAssignmentRequest{
		{Name: "t1", BloomLevel: 2, RequiredCapabilities: []string{model.CapabilityResearch}},
	}

	got, err := AssignWorkers(config, limits, workers, tasks, WithModelSelector(stubModelSelector{choice: "opus"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker1" {
		t.Fatalf("assignment = %+v, want worker1 (selector pick outside capability set)", got)
	}
}

func TestBuildWorkerStates_PopulatesCapabilities(t *testing.T) {
	dir := t.TempDir()
	config := model.WorkerConfig{
		Count:        3,
		DefaultModel: "sonnet",
		Models:       map[string]string{"worker2": "codex"},
		Capabilities: map[string][]string{"worker3": {"custom_tag"}},
	}

	states, err := BuildWorkerStates(dir, config)
	if err != nil {
		t.Fatalf("BuildWorkerStates: %v", err)
	}
	if len(states) != 3 {
		t.Fatalf("expected 3 states, got %d", len(states))
	}
	byID := make(map[string]WorkerState, len(states))
	for _, s := range states {
		byID[s.WorkerID] = s
	}
	if got, want := byID["worker1"].Capabilities, model.DefaultCapabilitiesForRuntime(model.RuntimeClaudeCode); !reflect.DeepEqual(got, want) {
		t.Errorf("worker1 capabilities = %v, want claude defaults %v", got, want)
	}
	if got, want := byID["worker2"].Capabilities, model.DefaultCapabilitiesForRuntime(model.RuntimeCodex); !reflect.DeepEqual(got, want) {
		t.Errorf("worker2 capabilities = %v, want codex defaults %v", got, want)
	}
	if got, want := byID["worker3"].Capabilities, []string{"custom_tag"}; !reflect.DeepEqual(got, want) {
		t.Errorf("worker3 capabilities = %v, want explicit override %v", got, want)
	}
}

func TestBuildQueueTasksByWorker_PropagatesCapabilities(t *testing.T) {
	tasks := []TaskInput{{
		Name:                  "t1",
		Purpose:               "p",
		Content:               "c",
		AcceptanceCriteria:    "a",
		BloomLevel:            2,
		RequiredCapabilities:  []string{model.CapabilityResearch},
		PreferredCapabilities: []string{model.CapabilityCostEfficient},
	}}
	assignments := []WorkerAssignment{{TaskName: "t1", WorkerID: "worker1", Model: "sonnet"}}
	nameToID := map[string]string{"t1": "task_x"}

	workerTasks, err := buildQueueTasksByWorker(assignments, tasks, nameToID, "cmd_1", "2026-07-23T00:00:00Z")
	if err != nil {
		t.Fatalf("buildQueueTasksByWorker: %v", err)
	}
	got := workerTasks["worker1"]
	if len(got) != 1 {
		t.Fatalf("expected 1 queue task, got %d", len(got))
	}
	if !reflect.DeepEqual(got[0].RequiredCapabilities, tasks[0].RequiredCapabilities) {
		t.Errorf("queue required_capabilities = %v, want %v", got[0].RequiredCapabilities, tasks[0].RequiredCapabilities)
	}
	if !reflect.DeepEqual(got[0].PreferredCapabilities, tasks[0].PreferredCapabilities) {
		t.Errorf("queue preferred_capabilities = %v, want %v", got[0].PreferredCapabilities, tasks[0].PreferredCapabilities)
	}
}

func TestValidateCapabilityTag(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		wantErr bool
	}{
		{"valid known tag", "code_quality", false},
		{"valid custom tag", "custom_domain_tag", false},
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"path separator", "a/b", true},
		{"backslash", `a\b`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := &ValidationErrors{}
			validateCapabilityTag(tt.tag, "tasks[0].required_capabilities[0]", errs)
			if got := errs.HasErrors(); got != tt.wantErr {
				t.Errorf("validateCapabilityTag(%q) hasErrors = %v, want %v (errs: %+v)", tt.tag, got, tt.wantErr, errs)
			}
		})
	}
}
