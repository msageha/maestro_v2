package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// --- validateRunOnMainIsolation (plan submit, tasks-only) ---

func TestValidateTasksInput_RunOnMainMixedWithNormalTask_Rejected(t *testing.T) {
	verify := validTask("verify-main")
	verify.RunOnMain = true
	tasks := []TaskInput{validTask("impl"), verify}

	errs := ValidateTasksInput(tasks)
	if errs == nil {
		t.Fatal("expected validation error for run_on_main mixed with a normal task")
	}
	if !strings.Contains(errs.Error(), "run_on_main tasks cannot be mixed") {
		t.Errorf("expected mixing rejection, got: %s", errs.Error())
	}
}

func TestValidateTasksInput_RunOnMainMixedWithRunOnIntegration_Rejected(t *testing.T) {
	verify := validTask("verify-main")
	verify.RunOnMain = true
	integ := validTask("verify-integration")
	integ.RunOnIntegration = true

	errs := ValidateTasksInput([]TaskInput{verify, integ})
	if errs == nil {
		t.Fatal("expected validation error: run_on_integration counts as 'other work' that must publish first")
	}
}

func TestValidateTasksInput_AllRunOnMain_Valid(t *testing.T) {
	a := validTask("verify-a")
	a.RunOnMain = true
	b := validTask("verify-b")
	b.RunOnMain = true

	if errs := ValidateTasksInput([]TaskInput{a, b}); errs != nil {
		t.Errorf("run_on_main-only verification command should be valid: %v", errs)
	}
}

// --- phased plans / phase fills reject run_on_main entirely ---

func TestValidatePhasesInput_RunOnMainInConcretePhase_Rejected(t *testing.T) {
	verify := validTask("verify-main")
	verify.RunOnMain = true
	phases := []PhaseInput{
		{Name: "impl", Type: "concrete", Tasks: []TaskInput{validTask("t1"), verify}},
	}

	errs := ValidatePhasesInput(phases)
	if errs == nil {
		t.Fatal("expected validation error for run_on_main inside a phased plan")
	}
	if !strings.Contains(errs.Error(), "not allowed in phased plans") {
		t.Errorf("expected phased-plan rejection, got: %s", errs.Error())
	}
}

func TestValidatePhaseFillInput_RunOnMain_Rejected(t *testing.T) {
	phase := model.Phase{
		Name: "fill-phase",
		Constraints: &model.PhaseConstraints{
			MaxTasks:       3,
			TimeoutMinutes: 10,
		},
	}
	verify := validTask("verify-main")
	verify.RunOnMain = true

	errs := ValidatePhaseFillInput([]TaskInput{verify}, phase)
	if errs == nil {
		t.Fatal("expected validation error for run_on_main inside a phase fill")
	}
	if !strings.Contains(errs.Error(), "not allowed in phase fills") {
		t.Errorf("expected phase-fill rejection, got: %s", errs.Error())
	}
}

func TestValidatePhaseFillInput_RunOnIntegration_StillAllowed(t *testing.T) {
	phase := model.Phase{
		Name: "fill-phase",
		Constraints: &model.PhaseConstraints{
			MaxTasks:       3,
			TimeoutMinutes: 10,
		},
	}
	integ := validTask("verify-integration")
	integ.RunOnIntegration = true

	if errs := ValidatePhaseFillInput([]TaskInput{integ}, phase); errs != nil {
		t.Errorf("run_on_integration in a phase fill should remain valid: %v", errs)
	}
}

// --- AssignWorkers claude-runtime requirement ---

func TestAssignWorkers_RequireClaudeRuntime_PinnedNonClaude_Rejected(t *testing.T) {
	states := []WorkerState{
		{WorkerID: "worker1", Model: "codex"},
		{WorkerID: "worker2", Model: "sonnet"},
	}
	reqs := []TaskAssignmentRequest{{
		Name:                 "verify-main",
		BloomLevel:           3,
		PinnedWorkerID:       "worker1",
		RequireClaudeRuntime: true,
	}}

	_, err := AssignWorkers(model.WorkerConfig{Count: 2}, model.LimitsConfig{}, states, reqs)
	if err == nil {
		t.Fatal("expected error pinning a run_on_main task to a codex worker")
	}
	if !strings.Contains(err.Error(), "non-claude runtime") {
		t.Errorf("expected non-claude runtime rejection, got: %v", err)
	}
}

func TestAssignWorkers_RequireClaudeRuntime_AutoAssignSkipsNonClaude(t *testing.T) {
	// Both workers satisfy the sonnet family requirement only via worker2;
	// worker1 (codex) must be skipped even if less loaded.
	states := []WorkerState{
		{WorkerID: "worker1", Model: "codex", PendingCount: 0},
		{WorkerID: "worker2", Model: "sonnet", PendingCount: 5},
	}
	reqs := []TaskAssignmentRequest{{
		Name:                 "verify-main",
		BloomLevel:           3,
		RequireClaudeRuntime: true,
	}}

	assignments, err := AssignWorkers(model.WorkerConfig{Count: 2}, model.LimitsConfig{}, states, reqs)
	if err != nil {
		t.Fatalf("AssignWorkers: %v", err)
	}
	if assignments[0].WorkerID != "worker2" {
		t.Errorf("assigned to %s, want worker2 (claude runtime)", assignments[0].WorkerID)
	}
}

func TestAssignWorkers_RequireClaudeRuntime_NoClaudeWorkers_ClearError(t *testing.T) {
	states := []WorkerState{
		{WorkerID: "worker1", Model: "codex"},
		{WorkerID: "worker2", Model: "gemini-2.5-pro"},
	}
	reqs := []TaskAssignmentRequest{{
		Name:                 "verify-main",
		BloomLevel:           3,
		RequireClaudeRuntime: true,
	}}

	_, err := AssignWorkers(model.WorkerConfig{Count: 2}, model.LimitsConfig{}, states, reqs)
	if err == nil {
		t.Fatal("expected error when no claude-code workers are configured")
	}
	if !strings.Contains(err.Error(), "no claude-code workers configured") {
		t.Errorf("expected no-claude-workers error, got: %v", err)
	}
}

// --- add-task publish gate ---

func writeRunOnMainGuardState(t *testing.T, dir, commandID string, status model.IntegrationStatus) string {
	t.Helper()
	stateDir := filepath.Join(dir, "state", "worktrees")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, commandID+".yaml")
	content := "integration:\n  status: " + string(status) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateRunOnMainPublishGate_NotPublished_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := writeRunOnMainGuardState(t, dir, "cmd1", model.IntegrationStatusMerged)

	err := validateRunOnMainPublishGate(path, "cmd1")
	if err == nil {
		t.Fatal("expected rejection while integration status is merged")
	}
	if !strings.Contains(err.Error(), "not published") {
		t.Errorf("expected not-published rejection, got: %v", err)
	}
}

func TestValidateRunOnMainPublishGate_Published_Allowed(t *testing.T) {
	dir := t.TempDir()
	path := writeRunOnMainGuardState(t, dir, "cmd1", model.IntegrationStatusPublished)

	if err := validateRunOnMainPublishGate(path, "cmd1"); err != nil {
		t.Errorf("published integration should allow run_on_main injection: %v", err)
	}
}

func TestValidateRunOnMainPublishGate_UnreadableState_FailsClosed(t *testing.T) {
	err := validateRunOnMainPublishGate(filepath.Join(t.TempDir(), "missing.yaml"), "cmd1")
	if err == nil {
		t.Fatal("expected fail-closed rejection for unreadable worktree state")
	}
}
