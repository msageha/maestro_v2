package plan

import (
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestValidateTaskDAG_LinearChain(t *testing.T) {
	names := []string{"A", "B", "C"}
	blockedBy := map[string][]string{
		"B": {"A"},
		"C": {"B"},
	}

	sorted, err := ValidateTaskDAG(names, blockedBy)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	idxA, idxB, idxC := indexOf(sorted, "A"), indexOf(sorted, "B"), indexOf(sorted, "C")
	if idxA < 0 || idxB < 0 || idxC < 0 {
		t.Fatalf("expected all nodes in sorted result, got %v", sorted)
	}
	if idxA >= idxB {
		t.Errorf("expected A before B, got A at %d, B at %d", idxA, idxB)
	}
	if idxB >= idxC {
		t.Errorf("expected B before C, got B at %d, C at %d", idxB, idxC)
	}
}

func TestValidateTaskDAG_Diamond(t *testing.T) {
	names := []string{"A", "B", "C", "D"}
	blockedBy := map[string][]string{
		"B": {"A"},
		"C": {"A"},
		"D": {"B", "C"},
	}

	sorted, err := ValidateTaskDAG(names, blockedBy)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(sorted) != 4 {
		t.Fatalf("expected 4 nodes, got %d: %v", len(sorted), sorted)
	}

	idxA := indexOf(sorted, "A")
	idxB := indexOf(sorted, "B")
	idxC := indexOf(sorted, "C")
	idxD := indexOf(sorted, "D")

	if idxA >= idxB {
		t.Errorf("expected A before B")
	}
	if idxA >= idxC {
		t.Errorf("expected A before C")
	}
	if idxB >= idxD {
		t.Errorf("expected B before D")
	}
	if idxC >= idxD {
		t.Errorf("expected C before D")
	}
}

func TestValidateTaskDAG_NoDependencies(t *testing.T) {
	names := []string{"X", "Y", "Z"}
	blockedBy := map[string][]string{}

	sorted, err := ValidateTaskDAG(names, blockedBy)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(sorted) != 3 {
		t.Fatalf("expected 3 nodes, got %d: %v", len(sorted), sorted)
	}

	seen := make(map[string]bool)
	for _, n := range sorted {
		seen[n] = true
	}
	for _, n := range names {
		if !seen[n] {
			t.Errorf("expected %q in sorted result", n)
		}
	}
}

func TestValidateTaskDAG_CycleDetection(t *testing.T) {
	names := []string{"A", "B"}
	blockedBy := map[string][]string{
		"A": {"B"},
		"B": {"A"},
	}

	_, err := ValidateTaskDAG(names, blockedBy)
	if err == nil {
		t.Fatal("expected error for cycle, got nil")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Errorf("expected error containing 'circular dependency', got %q", err.Error())
	}
}

func TestValidateTaskDAG_ThreeNodeCycle(t *testing.T) {
	names := []string{"A", "B", "C"}
	blockedBy := map[string][]string{
		"A": {"C"},
		"B": {"A"},
		"C": {"B"},
	}

	_, err := ValidateTaskDAG(names, blockedBy)
	if err == nil {
		t.Fatal("expected error for three-node cycle, got nil")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Errorf("expected error containing 'circular dependency', got %q", err.Error())
	}
}

func TestValidateTaskDAG_SelfReference(t *testing.T) {
	names := []string{"A"}
	blockedBy := map[string][]string{
		"A": {"A"},
	}

	_, err := ValidateTaskDAG(names, blockedBy)
	if err == nil {
		t.Fatal("expected error for self-reference cycle, got nil")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Errorf("expected error containing 'circular dependency', got %q", err.Error())
	}
}

func TestValidatePhaseDAG_Valid(t *testing.T) {
	phaseNames := []string{"phase1", "phase2", "phase3"}
	dependsOn := map[string][]string{
		"phase2": {"phase1"},
		"phase3": {"phase2"},
	}

	sorted, err := ValidatePhaseDAG(phaseNames, dependsOn)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	idx1 := indexOf(sorted, "phase1")
	idx2 := indexOf(sorted, "phase2")
	idx3 := indexOf(sorted, "phase3")
	if idx1 >= idx2 {
		t.Errorf("expected phase1 before phase2")
	}
	if idx2 >= idx3 {
		t.Errorf("expected phase2 before phase3")
	}
}

func TestValidatePhaseDAG_Cycle(t *testing.T) {
	phaseNames := []string{"alpha", "beta"}
	dependsOn := map[string][]string{
		"alpha": {"beta"},
		"beta":  {"alpha"},
	}

	_, err := ValidatePhaseDAG(phaseNames, dependsOn)
	if err == nil {
		t.Fatal("expected error for phase cycle, got nil")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Errorf("expected error containing 'circular dependency', got %q", err.Error())
	}
}

func TestValidateSamePhaseRefs_Valid(t *testing.T) {
	blockedBy := map[string][]string{
		"task_b": {"task_a"},
		"task_c": {"task_a", "task_b"},
	}
	validNames := map[string]bool{
		"task_a": true,
		"task_b": true,
		"task_c": true,
	}

	result := ValidateSamePhaseRefs(blockedBy, validNames)
	if result != nil {
		t.Errorf("expected nil (no errors), got %v", result)
	}
}

func TestValidateSamePhaseRefs_Unknown(t *testing.T) {
	blockedBy := map[string][]string{
		"task_b": {"task_a", "unknown_task"},
	}
	validNames := map[string]bool{
		"task_a": true,
		"task_b": true,
	}

	result := ValidateSamePhaseRefs(blockedBy, validNames)
	if result == nil {
		t.Fatal("expected validation errors, got nil")
	}
	if !result.HasErrors() {
		t.Fatal("expected HasErrors() to be true")
	}
	errMsg := result.Error()
	if !strings.Contains(errMsg, "unknown_task") {
		t.Errorf("expected error referencing 'unknown_task', got %q", errMsg)
	}
}

func TestValidateNoSelfReference_Detected(t *testing.T) {
	blockedBy := map[string][]string{
		"task_x": {"task_x"},
	}

	result := ValidateNoSelfReference(blockedBy)
	if result == nil {
		t.Fatal("expected validation errors for self-reference, got nil")
	}
	if !result.HasErrors() {
		t.Fatal("expected HasErrors() to be true")
	}
	errMsg := result.Error()
	if !strings.Contains(errMsg, "self-reference") {
		t.Errorf("expected error containing 'self-reference', got %q", errMsg)
	}
}

func TestValidateDAG_EmptyInput(t *testing.T) {
	sorted, err := ValidateTaskDAG(nil, nil)
	if err != nil {
		t.Fatalf("expected no error for empty input, got %v", err)
	}
	if sorted == nil {
		t.Errorf("expected non-nil empty slice for empty input, got nil")
	}
	if len(sorted) != 0 {
		t.Errorf("expected empty slice for empty input, got %v", sorted)
	}

	sorted2, err2 := ValidatePhaseDAG([]string{}, map[string][]string{})
	if err2 != nil {
		t.Fatalf("expected no error for empty slice, got %v", err2)
	}
	if sorted2 == nil {
		t.Errorf("expected non-nil empty slice for empty slice input, got nil")
	}
	if len(sorted2) != 0 {
		t.Errorf("expected empty slice for empty slice input, got %v", sorted2)
	}
}

func TestValidateTaskDAGAfterMutation_Valid(t *testing.T) {
	state := &model.CommandState{
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
				"t2": model.StatusPending,
				"t3": model.StatusPending,
			},
			TaskDependencies: map[string][]string{
				"t1": {},
				"t2": {"t1"},
				"t3": {"t2"},
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{Name: "phase1", TaskIDs: []string{"t1", "t2", "t3"}},
			},
		},
	}
	if err := ValidateTaskDAGAfterMutation(state); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateTaskDAGAfterMutation_Cycle(t *testing.T) {
	state := &model.CommandState{
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				"t1": model.StatusPending,
				"t2": model.StatusPending,
			},
			TaskDependencies: map[string][]string{
				"t1": {"t2"},
				"t2": {"t1"},
			},
		},
	}
	err := ValidateTaskDAGAfterMutation(state)
	if err == nil {
		t.Fatal("expected error for cycle, got nil")
	}
	if !strings.Contains(err.Error(), "circular dependency") {
		t.Errorf("expected 'circular dependency' in error, got %q", err.Error())
	}
}

func TestValidateTaskDAGAfterMutation_CrossPhaseViolation(t *testing.T) {
	// phase2 does NOT depend on phase1, but t3 (in phase1) depends on t2 (in phase2).
	state := &model.CommandState{
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				"t1": model.StatusPending,
				"t2": model.StatusPending,
				"t3": model.StatusPending,
			},
			TaskDependencies: map[string][]string{
				"t1": {},
				"t2": {},
				"t3": {"t2"}, // t3 in phase1 depends on t2 in phase2 — violation
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{Name: "phase1", TaskIDs: []string{"t1", "t3"}},
				{Name: "phase2", TaskIDs: []string{"t2"}},
			},
		},
	}
	err := ValidateTaskDAGAfterMutation(state)
	if err == nil {
		t.Fatal("expected cross-phase error, got nil")
	}
	if !strings.Contains(err.Error(), "cross-phase") {
		t.Errorf("expected 'cross-phase' in error, got %q", err.Error())
	}
}

func TestValidateTaskDAGAfterMutation_CrossPhaseValid(t *testing.T) {
	// phase2 depends on phase1, so t3 (in phase2) depending on t1 (in phase1) is valid.
	state := &model.CommandState{
		TaskTracking: model.TaskTracking{
			TaskStates: map[string]model.Status{
				"t1": model.StatusPending,
				"t2": model.StatusPending,
				"t3": model.StatusPending,
			},
			TaskDependencies: map[string][]string{
				"t1": {},
				"t2": {},
				"t3": {"t1"}, // t3 in phase2 depends on t1 in phase1 — valid
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{Name: "phase1", PhaseID: "p1", TaskIDs: []string{"t1", "t2"}},
				{Name: "phase2", PhaseID: "p2", DependsOnPhases: []string{"p1"}, TaskIDs: []string{"t3"}},
			},
		},
	}
	if err := ValidateTaskDAGAfterMutation(state); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestValidateCrossPhaseRefs_SinglePhase(t *testing.T) {
	state := &model.CommandState{
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{Name: "only-phase", TaskIDs: []string{"t1", "t2"}},
			},
		},
	}
	if err := validateCrossPhaseRefs(state); err != nil {
		t.Fatalf("single phase should have no cross-phase violations, got %v", err)
	}
}

func indexOf(slice []string, val string) int {
	for i, s := range slice {
		if s == val {
			return i
		}
	}
	return -1
}
