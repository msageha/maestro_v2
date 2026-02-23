package plan

import (
	"fmt"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

func ValidateTasksInput(tasks []TaskInput) *ValidationErrors {
	errs := &ValidationErrors{}

	if len(tasks) == 0 {
		errs.Add("tasks", "at least one task is required")
		return errs
	}

	names := make([]string, 0, len(tasks))
	nameSet := make(map[string]bool, len(tasks))
	blockedBy := make(map[string][]string)

	for i, task := range tasks {
		prefix := fmt.Sprintf("tasks[%d]", i)
		validateTaskFields(task, prefix, errs)

		if task.Name != "" {
			names = append(names, task.Name)
			nameSet[task.Name] = true
			if len(task.BlockedBy) > 0 {
				blockedBy[task.Name] = task.BlockedBy
			}
		}
	}

	validateNameUniqueness(names, "tasks", errs)
	validateSystemReservedNames(names, "tasks", errs)

	// Validate blocked_by references within same scope
	if refErrs := ValidateSamePhaseRefs(blockedBy, nameSet); refErrs != nil {
		for _, e := range refErrs.Errors {
			errs.Add("tasks."+e.FieldPath, e.Message)
		}
	}

	// Validate self-references
	if selfErrs := ValidateNoSelfReference(blockedBy); selfErrs != nil {
		for _, e := range selfErrs.Errors {
			errs.Add("tasks."+e.FieldPath, e.Message)
		}
	}

	// DAG validation
	if !errs.HasErrors() && len(blockedBy) > 0 {
		if _, err := ValidateTaskDAG(names, blockedBy); err != nil {
			errs.Add("tasks", err.Error())
		}
	}

	if errs.HasErrors() {
		return errs
	}
	return nil
}

func ValidatePhasesInput(phases []PhaseInput) *ValidationErrors {
	errs := &ValidationErrors{}

	if len(phases) == 0 {
		errs.Add("phases", "at least one phase is required")
		return errs
	}

	phaseNames := make([]string, 0, len(phases))
	phaseNameSet := make(map[string]bool)
	hasConcretePhase := false
	phaseDependsOn := make(map[string][]string)

	for i, phase := range phases {
		prefix := fmt.Sprintf("phases[%d]", i)

		if phase.Name == "" {
			errs.Add(prefix+".name", "required field is missing")
		} else {
			phaseNames = append(phaseNames, phase.Name)
			phaseNameSet[phase.Name] = true
		}

		if phase.Type != "concrete" && phase.Type != "deferred" {
			errs.Add(prefix+".type", fmt.Sprintf("must be 'concrete' or 'deferred', got %q", phase.Type))
		}

		if phase.Type == "concrete" {
			hasConcretePhase = true
			if len(phase.DependsOnPhases) > 0 {
				errs.Add(prefix+".depends_on_phases", "concrete phases must have empty depends_on_phases")
			}
			if phase.Constraints != nil {
				errs.Add(prefix+".constraints", "concrete phases must not have constraints")
			}

			// Validate tasks within concrete phase
			taskNames := make(map[string]bool)
			taskBlockedBy := make(map[string][]string)
			var taskNameList []string
			for j, task := range phase.Tasks {
				taskPrefix := fmt.Sprintf("%s.tasks[%d]", prefix, j)
				validateTaskFields(task, taskPrefix, errs)
				if task.Name != "" {
					taskNames[task.Name] = true
					taskNameList = append(taskNameList, task.Name)
					if len(task.BlockedBy) > 0 {
						taskBlockedBy[task.Name] = task.BlockedBy
					}
				}
			}

			// Validate within-phase blocked_by
			if refErrs := ValidateSamePhaseRefs(taskBlockedBy, taskNames); refErrs != nil {
				for _, e := range refErrs.Errors {
					errs.Add(prefix+".tasks."+e.FieldPath, e.Message)
				}
			}
			if selfErrs := ValidateNoSelfReference(taskBlockedBy); selfErrs != nil {
				for _, e := range selfErrs.Errors {
					errs.Add(prefix+".tasks."+e.FieldPath, e.Message)
				}
			}
			// Task-level DAG within phase
			if len(taskBlockedBy) > 0 {
				if _, err := ValidateTaskDAG(taskNameList, taskBlockedBy); err != nil {
					errs.Add(prefix+".tasks", err.Error())
				}
			}

			validateNameUniqueness(taskNameList, prefix+".tasks", errs)
			validateSystemReservedNames(taskNameList, prefix+".tasks", errs)
		}

		if phase.Type == "deferred" {
			if phase.Constraints == nil {
				errs.Add(prefix+".constraints", "deferred phases must have constraints")
			} else {
				validatePhaseConstraints(phase.Constraints, prefix+".constraints", errs)
			}
			if len(phase.Tasks) > 0 {
				errs.Add(prefix+".tasks", "deferred phases must not have tasks at submit time")
			}
		}

		if len(phase.DependsOnPhases) > 0 {
			phaseDependsOn[phase.Name] = phase.DependsOnPhases
		}
	}

	if !hasConcretePhase {
		errs.Add("phases", "at least one concrete phase is required")
	}

	validateNameUniqueness(phaseNames, "phases", errs)
	validateSystemReservedNames(phaseNames, "phases", errs)

	// Validate depends_on_phases references
	for i, phase := range phases {
		for j, dep := range phase.DependsOnPhases {
			if !phaseNameSet[dep] {
				errs.Add(fmt.Sprintf("phases[%d].depends_on_phases[%d]", i, j),
					fmt.Sprintf("references unknown phase %q", dep))
			}
		}
	}

	// Phase-level DAG
	if !errs.HasErrors() && len(phaseDependsOn) > 0 {
		if _, err := ValidatePhaseDAG(phaseNames, phaseDependsOn); err != nil {
			errs.Add("phases", err.Error())
		}
	}

	if errs.HasErrors() {
		return errs
	}
	return nil
}

func ValidatePhaseFillInput(tasks []TaskInput, phase model.Phase) *ValidationErrors {
	errs := &ValidationErrors{}

	if len(tasks) == 0 {
		errs.Add("tasks", "at least one task is required")
		return errs
	}

	// Validate against phase constraints
	if phase.Constraints != nil {
		if len(tasks) > phase.Constraints.MaxTasks {
			errs.Add("tasks", fmt.Sprintf("task count %d exceeds phase constraint max_tasks %d",
				len(tasks), phase.Constraints.MaxTasks))
		}

		allowedBloom := make(map[int]bool)
		if len(phase.Constraints.AllowedBloomLevels) > 0 {
			for _, l := range phase.Constraints.AllowedBloomLevels {
				allowedBloom[l] = true
			}
		} else {
			// Default: all levels allowed
			for l := 1; l <= 6; l++ {
				allowedBloom[l] = true
			}
		}

		for i, task := range tasks {
			if task.BloomLevel > 0 && !allowedBloom[task.BloomLevel] {
				errs.Add(fmt.Sprintf("tasks[%d].bloom_level", i),
					fmt.Sprintf("bloom_level %d not in allowed levels for phase %q",
						task.BloomLevel, phase.Name))
			}
		}
	}

	// Validate task fields
	names := make([]string, 0, len(tasks))
	nameSet := make(map[string]bool)
	blockedBy := make(map[string][]string)

	for i, task := range tasks {
		prefix := fmt.Sprintf("tasks[%d]", i)
		validateTaskFields(task, prefix, errs)

		if task.Name != "" {
			names = append(names, task.Name)
			nameSet[task.Name] = true
			if len(task.BlockedBy) > 0 {
				blockedBy[task.Name] = task.BlockedBy
			}
		}
	}

	validateNameUniqueness(names, "tasks", errs)
	validateSystemReservedNames(names, "tasks", errs)

	if refErrs := ValidateSamePhaseRefs(blockedBy, nameSet); refErrs != nil {
		for _, e := range refErrs.Errors {
			errs.Add("tasks."+e.FieldPath, e.Message)
		}
	}
	if selfErrs := ValidateNoSelfReference(blockedBy); selfErrs != nil {
		for _, e := range selfErrs.Errors {
			errs.Add("tasks."+e.FieldPath, e.Message)
		}
	}

	if !errs.HasErrors() && len(blockedBy) > 0 {
		if _, err := ValidateTaskDAG(names, blockedBy); err != nil {
			errs.Add("tasks", err.Error())
		}
	}

	if errs.HasErrors() {
		return errs
	}
	return nil
}

func validateTaskFields(task TaskInput, fieldPrefix string, errs *ValidationErrors) {
	if task.Name == "" {
		errs.Add(fieldPrefix+".name", "required field is missing")
	}
	if task.Purpose == "" {
		errs.Add(fieldPrefix+".purpose", "required field is missing")
	}
	if task.Content == "" {
		errs.Add(fieldPrefix+".content", "required field is missing")
	}
	if task.AcceptanceCriteria == "" {
		errs.Add(fieldPrefix+".acceptance_criteria", "required field is missing")
	}
	validateBloomLevel(task.BloomLevel, fieldPrefix+".bloom_level", errs)
}

func validateNameUniqueness(names []string, fieldPrefix string, errs *ValidationErrors) {
	seen := make(map[string]bool)
	for _, name := range names {
		if seen[name] {
			errs.Add(fieldPrefix, fmt.Sprintf("duplicate name %q", name))
		}
		seen[name] = true
	}
}

func validateSystemReservedNames(names []string, fieldPrefix string, errs *ValidationErrors) {
	for _, name := range names {
		if strings.HasPrefix(name, "__") {
			errs.Add(fieldPrefix, fmt.Sprintf("name %q uses reserved prefix '__'", name))
		}
	}
}

func validateBloomLevel(level int, fieldPath string, errs *ValidationErrors) {
	if level < 1 || level > 6 {
		errs.Add(fieldPath, fmt.Sprintf("must be between 1 and 6, got %d", level))
	}
}

func validatePhaseConstraints(c *ConstraintInput, fieldPath string, errs *ValidationErrors) {
	if c.MaxTasks <= 0 {
		errs.Add(fieldPath+".max_tasks", "must be greater than 0")
	}
	if c.TimeoutMinutes <= 0 {
		errs.Add(fieldPath+".timeout_minutes", "must be greater than 0")
	}
	if len(c.AllowedBloomLevels) > 0 {
		for i, l := range c.AllowedBloomLevels {
			if l < 1 || l > 6 {
				errs.Add(fmt.Sprintf("%s.allowed_bloom_levels[%d]", fieldPath, i),
					fmt.Sprintf("must be between 1 and 6, got %d", l))
			}
		}
	}
}
