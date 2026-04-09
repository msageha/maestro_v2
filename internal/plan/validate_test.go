package plan

import (
	"fmt"
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

func TestValidateTasksInput_PersonaHintValidation(t *testing.T) {
	tests := []struct {
		name      string
		hint      string
		wantError bool
	}{
		{"valid hint", "implementer", false},
		{"path traversal", "../etc/passwd", true},
		{"slash", "foo/bar", true},
		{"backslash", "foo\\bar", true},
		{"null byte", "foo\x00bar", true},
		{"dot-dot embedded", "foo..bar", true},
		{"empty hint", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := validTask("test-task")
			task.PersonaHint = tt.hint
			errs := ValidateTasksInput([]TaskInput{task})
			if tt.wantError {
				if errs == nil {
					t.Fatalf("expected error for persona_hint %q", tt.hint)
				}
				if !strings.Contains(errs.Error(), "persona_hint") {
					t.Errorf("expected error mentioning 'persona_hint', got: %s", errs.Error())
				}
			} else {
				if errs != nil {
					t.Errorf("unexpected error for persona_hint %q: %v", tt.hint, errs)
				}
			}
		})
	}
}

func TestValidateTasksInput_SkillRefsValidation(t *testing.T) {
	task := validTask("test-task")
	task.SkillRefs = []string{"valid-skill", "../bad/skill"}
	errs := ValidateTasksInput([]TaskInput{task})
	if errs == nil {
		t.Fatal("expected error for invalid skill_ref")
	}
	if !strings.Contains(errs.Error(), "skill_refs") {
		t.Errorf("expected error mentioning 'skill_refs', got: %s", errs.Error())
	}
}

// --- expected_paths validation ---

func TestValidateTasksInput_ExpectedPaths_Valid(t *testing.T) {
	task := validTask("ep-valid")
	task.ExpectedPaths = []string{"src/main.go", "internal/handler.go"}
	errs := ValidateTasksInput([]TaskInput{task})
	if errs != nil {
		t.Errorf("unexpected error for valid expected_paths: %v", errs)
	}
}

func TestValidateTasksInput_ExpectedPaths_AbsolutePath(t *testing.T) {
	task := validTask("ep-abs")
	task.ExpectedPaths = []string{"/etc/passwd"}
	errs := ValidateTasksInput([]TaskInput{task})
	if errs == nil {
		t.Fatal("expected error for absolute path in expected_paths")
	}
	if !strings.Contains(errs.Error(), "relative path") {
		t.Errorf("expected 'relative path' in error, got: %s", errs.Error())
	}
}

func TestValidateTasksInput_ExpectedPaths_Traversal(t *testing.T) {
	task := validTask("ep-trav")
	task.ExpectedPaths = []string{"../secret/file"}
	errs := ValidateTasksInput([]TaskInput{task})
	if errs == nil {
		t.Fatal("expected error for path traversal in expected_paths")
	}
	if !strings.Contains(errs.Error(), "..") {
		t.Errorf("expected '..' in error, got: %s", errs.Error())
	}
}

func TestValidateTasksInput_ExpectedPaths_Empty(t *testing.T) {
	task := validTask("ep-empty")
	task.ExpectedPaths = []string{""}
	errs := ValidateTasksInput([]TaskInput{task})
	if errs == nil {
		t.Fatal("expected error for empty string in expected_paths")
	}
	if !strings.Contains(errs.Error(), "empty") {
		t.Errorf("expected 'empty' in error, got: %s", errs.Error())
	}
}

func TestValidateTasksInput_ExpectedPaths_Duplicate(t *testing.T) {
	task := validTask("ep-dup")
	task.ExpectedPaths = []string{"src/main.go", "src/main.go"}
	errs := ValidateTasksInput([]TaskInput{task})
	if errs == nil {
		t.Fatal("expected error for duplicate expected_paths")
	}
	if !strings.Contains(errs.Error(), "duplicate") {
		t.Errorf("expected 'duplicate' in error, got: %s", errs.Error())
	}
}

// --- definition_of_abort validation ---

func TestValidateTasksInput_DefinitionOfAbort_Valid(t *testing.T) {
	task := validTask("doa-valid")
	task.DefinitionOfAbort = &model.DefinitionOfAbort{
		MaxRepairCount:  5,
		MaxWallClockSec: 3600,
	}
	errs := ValidateTasksInput([]TaskInput{task})
	if errs != nil {
		t.Errorf("unexpected error for valid definition_of_abort: %v", errs)
	}
}

func TestValidateTasksInput_DefinitionOfAbort_Boundaries(t *testing.T) {
	tests := []struct {
		name      string
		repair    int
		wall      int
		wantError bool
		errField  string
	}{
		{"repair=0", 0, 3600, true, "max_repair_count"},
		{"repair=100", 100, 3600, false, ""},
		{"repair=101", 101, 3600, true, "max_repair_count"},
		{"wall=0", 5, 0, true, "max_wall_clock_sec"},
		{"wall=86400", 5, 86400, false, ""},
		{"wall=86401", 5, 86401, true, "max_wall_clock_sec"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := validTask("doa-boundary")
			task.DefinitionOfAbort = &model.DefinitionOfAbort{
				MaxRepairCount:  tt.repair,
				MaxWallClockSec: tt.wall,
			}
			errs := ValidateTasksInput([]TaskInput{task})
			if tt.wantError {
				if errs == nil {
					t.Fatalf("expected error for %s", tt.name)
				}
				if !strings.Contains(errs.Error(), tt.errField) {
					t.Errorf("expected %q in error, got: %s", tt.errField, errs.Error())
				}
			} else {
				if errs != nil {
					t.Errorf("unexpected error: %v", errs)
				}
			}
		})
	}
}

func TestValidateTasksInput_DefinitionOfAbort_EmptyCondition(t *testing.T) {
	task := validTask("doa-empty-cond")
	task.DefinitionOfAbort = &model.DefinitionOfAbort{
		MaxRepairCount:            5,
		MaxWallClockSec:           3600,
		ExplicitFailureConditions: []string{"valid condition", "  "},
	}
	errs := ValidateTasksInput([]TaskInput{task})
	if errs == nil {
		t.Fatal("expected error for empty failure condition")
	}
	if !strings.Contains(errs.Error(), "explicit_failure_conditions") {
		t.Errorf("expected 'explicit_failure_conditions' in error, got: %s", errs.Error())
	}
}

// --- definition_of_done validation ---

func TestValidateTasksInput_DefinitionOfDone_Valid(t *testing.T) {
	task := validTask("dod-valid")
	task.DefinitionOfDone = []string{"tests pass", "no lint errors"}
	errs := ValidateTasksInput([]TaskInput{task})
	if errs != nil {
		t.Errorf("unexpected error for valid definition_of_done: %v", errs)
	}
}

func TestValidateTasksInput_DefinitionOfDone_EmptyItem(t *testing.T) {
	task := validTask("dod-empty")
	task.DefinitionOfDone = []string{"valid", ""}
	errs := ValidateTasksInput([]TaskInput{task})
	if errs == nil {
		t.Fatal("expected error for empty item in definition_of_done")
	}
	if !strings.Contains(errs.Error(), "definition_of_done") {
		t.Errorf("expected 'definition_of_done' in error, got: %s", errs.Error())
	}
}

func TestValidateTasksInput_DefinitionOfDone_ExceedsMax(t *testing.T) {
	task := validTask("dod-max")
	items := make([]string, 21)
	for i := range items {
		items[i] = fmt.Sprintf("condition %d", i)
	}
	task.DefinitionOfDone = items
	errs := ValidateTasksInput([]TaskInput{task})
	if errs == nil {
		t.Fatal("expected error for exceeding max definition_of_done items")
	}
	if !strings.Contains(errs.Error(), "exceeds maximum") {
		t.Errorf("expected 'exceeds maximum' in error, got: %s", errs.Error())
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
