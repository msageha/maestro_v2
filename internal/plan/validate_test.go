package plan

import (
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func validTask(name string) TaskInput {
	return TaskInput{
		Name:               name,
		Purpose:            "test purpose",
		Content:            "test content",
		AcceptanceCriteria: "test criteria",
		BloomLevel:         3,
		Required:           true,
	}
}

func TestValidateTasksInput_Valid(t *testing.T) {
	tasks := []TaskInput{
		validTask("task-a"),
		validTask("task-b"),
	}

	errs := ValidateTasksInput(tasks)
	if errs != nil {
		t.Errorf("ValidateTasksInput returned errors for valid input: %v", errs)
	}
}

func TestValidateTasksInput_MissingFields(t *testing.T) {
	tasks := []TaskInput{
		{
			BloomLevel: 3,
		},
	}

	errs := ValidateTasksInput(tasks)
	if errs == nil {
		t.Fatalf("ValidateTasksInput returned nil for missing required fields")
	}

	errStr := errs.Error()
	for _, field := range []string{"name", "purpose", "content", "acceptance_criteria"} {
		if !strings.Contains(errStr, field) {
			t.Errorf("expected error mentioning %q, got: %s", field, errStr)
		}
	}
}

func TestValidateTasksInput_DuplicateNames(t *testing.T) {
	tasks := []TaskInput{
		validTask("dup-name"),
		validTask("dup-name"),
	}

	errs := ValidateTasksInput(tasks)
	if errs == nil {
		t.Fatalf("ValidateTasksInput returned nil for duplicate names")
	}

	if !strings.Contains(errs.Error(), "duplicate") {
		t.Errorf("expected error mentioning 'duplicate', got: %s", errs.Error())
	}
}

func TestValidateTasksInput_SystemReservedName(t *testing.T) {
	tasks := []TaskInput{
		validTask("__system-internal"),
	}

	errs := ValidateTasksInput(tasks)
	if errs == nil {
		t.Fatalf("ValidateTasksInput returned nil for system reserved name")
	}

	if !strings.Contains(errs.Error(), "reserved") {
		t.Errorf("expected error mentioning 'reserved', got: %s", errs.Error())
	}
}

func TestValidateTasksInput_BloomLevelRange(t *testing.T) {
	t.Run("level 0 rejected", func(t *testing.T) {
		task := validTask("bloom-zero")
		task.BloomLevel = 0
		errs := ValidateTasksInput([]TaskInput{task})
		if errs == nil {
			t.Fatalf("ValidateTasksInput returned nil for bloom_level 0")
		}
		if !strings.Contains(errs.Error(), "bloom_level") {
			t.Errorf("expected error mentioning 'bloom_level', got: %s", errs.Error())
		}
	})

	t.Run("level 7 rejected", func(t *testing.T) {
		task := validTask("bloom-seven")
		task.BloomLevel = 7
		errs := ValidateTasksInput([]TaskInput{task})
		if errs == nil {
			t.Fatalf("ValidateTasksInput returned nil for bloom_level 7")
		}
		if !strings.Contains(errs.Error(), "bloom_level") {
			t.Errorf("expected error mentioning 'bloom_level', got: %s", errs.Error())
		}
	})
}

func TestValidateTasksInput_DAGCycle(t *testing.T) {
	tasks := []TaskInput{
		{
			Name:               "a",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
			BlockedBy:          []string{"b"},
		},
		{
			Name:               "b",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
			BlockedBy:          []string{"a"},
		},
	}

	errs := ValidateTasksInput(tasks)
	if errs == nil {
		t.Fatalf("ValidateTasksInput returned nil for circular dependency")
	}

	if !strings.Contains(errs.Error(), "circular") {
		t.Errorf("expected error mentioning 'circular', got: %s", errs.Error())
	}
}

func TestValidateTasksInput_CrossRef(t *testing.T) {
	tasks := []TaskInput{
		{
			Name:               "a",
			Purpose:            "p",
			Content:            "c",
			AcceptanceCriteria: "ac",
			BloomLevel:         1,
			BlockedBy:          []string{"nonexistent"},
		},
	}

	errs := ValidateTasksInput(tasks)
	if errs == nil {
		t.Fatalf("ValidateTasksInput returned nil for unknown blocked_by reference")
	}

	if !strings.Contains(errs.Error(), "unknown") {
		t.Errorf("expected error mentioning 'unknown', got: %s", errs.Error())
	}
}

func TestValidatePhasesInput_Valid(t *testing.T) {
	phases := []PhaseInput{
		{
			Name: "phase-concrete",
			Type: "concrete",
			Tasks: []TaskInput{
				validTask("t1"),
			},
		},
		{
			Name:            "phase-deferred",
			Type:            "deferred",
			DependsOnPhases: []string{"phase-concrete"},
			Constraints: &ConstraintInput{
				MaxTasks:       5,
				TimeoutMinutes: 30,
			},
		},
	}

	errs := ValidatePhasesInput(phases)
	if errs != nil {
		t.Errorf("ValidatePhasesInput returned errors for valid input: %v", errs)
	}
}

func TestValidatePhasesInput_ConcreteDependsOnPhases(t *testing.T) {
	phases := []PhaseInput{
		{
			Name:            "phase-concrete",
			Type:            "concrete",
			DependsOnPhases: []string{"some-phase"},
			Tasks: []TaskInput{
				validTask("t1"),
			},
		},
	}

	errs := ValidatePhasesInput(phases)
	if errs == nil {
		t.Fatalf("ValidatePhasesInput returned nil for concrete phase with depends_on_phases")
	}

	if !strings.Contains(errs.Error(), "depends_on_phases") {
		t.Errorf("expected error mentioning 'depends_on_phases', got: %s", errs.Error())
	}
}

func TestValidatePhasesInput_NoConcretePhase(t *testing.T) {
	phases := []PhaseInput{
		{
			Name: "phase-deferred",
			Type: "deferred",
			Constraints: &ConstraintInput{
				MaxTasks:       5,
				TimeoutMinutes: 30,
			},
		},
	}

	errs := ValidatePhasesInput(phases)
	if errs == nil {
		t.Fatalf("ValidatePhasesInput returned nil when no concrete phase exists")
	}

	if !strings.Contains(errs.Error(), "concrete") {
		t.Errorf("expected error mentioning 'concrete', got: %s", errs.Error())
	}
}

func TestValidatePhasesInput_DeferredMissingConstraints(t *testing.T) {
	phases := []PhaseInput{
		{
			Name: "phase-concrete",
			Type: "concrete",
			Tasks: []TaskInput{
				validTask("t1"),
			},
		},
		{
			Name:            "phase-deferred",
			Type:            "deferred",
			DependsOnPhases: []string{"phase-concrete"},
			Constraints:     nil,
		},
	}

	errs := ValidatePhasesInput(phases)
	if errs == nil {
		t.Fatalf("ValidatePhasesInput returned nil for deferred phase without constraints")
	}

	if !strings.Contains(errs.Error(), "constraints") {
		t.Errorf("expected error mentioning 'constraints', got: %s", errs.Error())
	}
}

func TestValidatePhaseFillInput_ExceedsMaxTasks(t *testing.T) {
	phase := model.Phase{
		Name: "fill-phase",
		Constraints: &model.PhaseConstraints{
			MaxTasks:       1,
			TimeoutMinutes: 10,
		},
	}

	tasks := []TaskInput{
		validTask("t1"),
		validTask("t2"),
	}

	errs := ValidatePhaseFillInput(tasks, phase)
	if errs == nil {
		t.Fatalf("ValidatePhaseFillInput returned nil when exceeding max_tasks")
	}

	if !strings.Contains(errs.Error(), "exceeds") {
		t.Errorf("expected error mentioning 'exceeds', got: %s", errs.Error())
	}
}

func TestValidatePhaseFillInput_DisallowedBloomLevel(t *testing.T) {
	phase := model.Phase{
		Name: "bloom-phase",
		Constraints: &model.PhaseConstraints{
			MaxTasks:           10,
			AllowedBloomLevels: []int{1, 2},
			TimeoutMinutes:     10,
		},
	}

	task := validTask("high-bloom")
	task.BloomLevel = 5

	errs := ValidatePhaseFillInput([]TaskInput{task}, phase)
	if errs == nil {
		t.Fatalf("ValidatePhaseFillInput returned nil for disallowed bloom level")
	}

	if !strings.Contains(errs.Error(), "bloom_level") {
		t.Errorf("expected error mentioning 'bloom_level', got: %s", errs.Error())
	}
}
