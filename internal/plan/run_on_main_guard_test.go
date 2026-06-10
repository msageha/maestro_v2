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

// fixedPickSelector always returns the same model, simulating a warmed-up
// bandit whose UCB1 winner is a non-claude arm.
type fixedPickSelector struct{ pick string }

func (s fixedPickSelector) SelectModel(int, string) string { return s.pick }

func TestAssignWorkers_RequireClaudeRuntime_NonClaudeSelectorPick_Ignored(t *testing.T) {
	// Bandit arms are built from worker-configured models, so a mixed fleet
	// can warm up a "codex" arm. Honoring that pick for a run_on_main task
	// would make the eligibility loop skip every worker (codex fails the
	// runtime check, sonnet fails the family match) and fail the assignment
	// with a misleading "at capacity" error despite the idle claude worker.
	states := []WorkerState{
		{WorkerID: "worker1", Model: "codex", PendingCount: 0},
		{WorkerID: "worker2", Model: "sonnet", PendingCount: 3},
	}
	reqs := []TaskAssignmentRequest{{
		Name:                 "verify-main",
		BloomLevel:           3,
		RequireClaudeRuntime: true,
	}}

	assignments, err := AssignWorkers(model.WorkerConfig{Count: 2}, model.LimitsConfig{}, states, reqs,
		WithModelSelector(fixedPickSelector{pick: "codex"}))
	if err != nil {
		t.Fatalf("AssignWorkers must ignore the non-claude selector pick: %v", err)
	}
	if assignments[0].WorkerID != "worker2" {
		t.Errorf("assigned to %s, want worker2 (claude runtime)", assignments[0].WorkerID)
	}
}

func TestAssignWorkers_NoClaudeRequirement_SelectorPickStillHonored(t *testing.T) {
	states := []WorkerState{
		{WorkerID: "worker1", Model: "codex", PendingCount: 0},
		{WorkerID: "worker2", Model: "sonnet", PendingCount: 0},
	}
	reqs := []TaskAssignmentRequest{{Name: "impl", BloomLevel: 3}}

	assignments, err := AssignWorkers(model.WorkerConfig{Count: 2}, model.LimitsConfig{}, states, reqs,
		WithModelSelector(fixedPickSelector{pick: "codex"}))
	if err != nil {
		t.Fatalf("AssignWorkers: %v", err)
	}
	if assignments[0].WorkerID != "worker1" {
		t.Errorf("assigned to %s, want worker1 (selector pick honored for normal tasks)", assignments[0].WorkerID)
	}
}

func TestChooseFallbackFamily_RequireClaude_SkipsNonClaudeLastResort(t *testing.T) {
	// Mixed fleet where the only claude worker uses a custom model name
	// outside the known sonnet/opus/haiku families. The last-resort scan
	// iterates a map, so without the requireClaude filter the result is
	// nondeterministic and can hand a RequireClaudeRuntime task the codex
	// family.
	sm := map[string]*WorkerState{
		"w1": {WorkerID: "w1", Model: "codex"},
		"w2": {WorkerID: "w2", Model: "claude-custom-1"},
	}
	for range 20 {
		if got := chooseFallbackFamily(sm, "sonnet", true); got != "claude-custom-1" {
			t.Fatalf("chooseFallbackFamily(requireClaude) = %q, want claude-custom-1", got)
		}
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

// --- shared worktree-state fixture ---

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

// --- cross-command ordering advisory at submit ---

func TestRunOnMainCrossCommandWarnings_OtherCommandUnpublished_Warns(t *testing.T) {
	dir := t.TempDir()
	writeRunOnMainGuardState(t, dir, "cmd_other", model.IntegrationStatusMerged)
	verify := validTask("verify-main")
	verify.RunOnMain = true

	warnings := runOnMainCrossCommandWarnings(dir, "cmd_verify", []TaskInput{verify})
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one cross-command advisory", warnings)
	}
	if !strings.Contains(warnings[0], "cmd_other (integration merged)") {
		t.Errorf("warning should name the unpublished command and status, got: %s", warnings[0])
	}
}

func TestRunOnMainCrossCommandWarnings_NoWarningCases(t *testing.T) {
	verify := validTask("verify-main")
	verify.RunOnMain = true
	normal := validTask("impl")

	t.Run("other command already published", func(t *testing.T) {
		dir := t.TempDir()
		writeRunOnMainGuardState(t, dir, "cmd_other", model.IntegrationStatusPublished)
		if w := runOnMainCrossCommandWarnings(dir, "cmd_verify", []TaskInput{verify}); w != nil {
			t.Errorf("published siblings must not warn, got: %v", w)
		}
	})
	t.Run("own state file excluded", func(t *testing.T) {
		dir := t.TempDir()
		writeRunOnMainGuardState(t, dir, "cmd_verify", model.IntegrationStatusMerged)
		if w := runOnMainCrossCommandWarnings(dir, "cmd_verify", []TaskInput{verify}); w != nil {
			t.Errorf("own command state must not warn (own ordering is enforced elsewhere), got: %v", w)
		}
	})
	t.Run("normal command never warns", func(t *testing.T) {
		dir := t.TempDir()
		writeRunOnMainGuardState(t, dir, "cmd_other", model.IntegrationStatusMerged)
		if w := runOnMainCrossCommandWarnings(dir, "cmd_impl", []TaskInput{normal}); w != nil {
			t.Errorf("non-run_on_main submissions must not warn, got: %v", w)
		}
	})
	t.Run("missing state dir is silent", func(t *testing.T) {
		if w := runOnMainCrossCommandWarnings(t.TempDir(), "cmd_verify", []TaskInput{verify}); w != nil {
			t.Errorf("absent state dir must not warn, got: %v", w)
		}
	})
}

// --- add-task publish gate ---

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
