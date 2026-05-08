package plan

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/validate"
)

// workerIDPattern matches the configured worker slot naming scheme
// (worker1, worker2, …). Used for fast-fail format validation of the
// optional worker_id pinning field on TaskInput; existence against
// agents.workers.count is checked downstream by AssignWorkers.
var workerIDPattern = regexp.MustCompile(`^worker[1-9][0-9]*$`)

// validateTaskWorkerPins runs the count-aware existence check for every
// task's worker_id pin. Format-only validation (workerN shape) already
// runs inside validateTaskFieldsCore; this layer additionally requires
// that the numeric suffix is in [1, workerCount] so dry-run cannot pass a
// typo like worker99 in a 2-worker config.
//
// fieldPrefix is the YAML path prefix used for error messages
// ("tasks" for flat submits, "phases[<name>].tasks" for phased ones)
// so validation errors point the operator at the offending entry.
func validateTaskWorkerPins(tasks []TaskInput, workerCount int, fieldPrefix string) *ValidationErrors {
	if workerCount <= 0 {
		// Caller has not configured workers at all — `plan submit`'s
		// downstream AssignWorkers path will surface the more useful
		// "no workers configured" error. Skip the pin check here so
		// the user does not receive a less-actionable secondary error.
		return nil
	}
	errs := &ValidationErrors{}
	for i, t := range tasks {
		if t.WorkerID == "" {
			continue
		}
		if !workerIDPattern.MatchString(t.WorkerID) {
			// Format error is already reported by validateTaskFieldsCore;
			// skip duplicate noise here.
			continue
		}
		// workerN pattern guarantees the suffix is a positive integer.
		var n int
		if _, err := fmt.Sscanf(t.WorkerID, "worker%d", &n); err != nil || n < 1 {
			continue
		}
		if n > workerCount {
			errs.Add(fmt.Sprintf("%s[%d].worker_id", fieldPrefix, i),
				fmt.Sprintf("worker_id %q references worker %d but only %d workers are configured (agents.workers.count=%d)",
					t.WorkerID, n, workerCount, workerCount))
		}
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

// BloomLevel range constants.
const (
	BloomLevelMin = 1
	BloomLevelMax = 6
)

// Maximum lengths for task fields.
const (
	MaxTaskNameRunes               = 128
	MaxTaskPurposeRunes            = 1024
	MaxTaskContentBytes            = 102400 // 100KB
	MaxTaskAcceptanceCriteriaRunes = 4096
	MaxTaskConstraintRunes         = 1024
)

// Limits for new schema fields.
const (
	MaxDefinitionOfDoneItems = 20
	MaxRepairCount           = 100
	MaxWallClockSec          = 86400 // 24 hours
)

// Minimum lengths enforced on add-task-injected fields. These defend
// against shell-quoting mishaps (e.g. backtick / `$()` expansion inside
// double quotes) that would otherwise let a malformed task land in the
// queue. Thresholds are intentionally very conservative — they catch
// obviously truncated payloads (1–3 byte leftovers from command
// substitution) without rejecting the kind of terse content that appears
// in legitimate unit test fixtures. Not applied to `plan submit` because
// submit flows aggregate many tasks where tersely-described fixtures are
// sometimes legitimate.
const (
	MinInjectedPurposeBytes            = 4
	MinInjectedContentBytes            = 4
	MinInjectedAcceptanceCriteriaBytes = 4
)

// validateTaskSetCommon validates a slice of task inputs for field integrity,
// name uniqueness, reserved-name prefixes, blocked_by references, self-references,
// and DAG constraints. It collects task names, name set, and blocked_by mappings
// into the provided errs. The fieldPrefix (e.g. "tasks") is used for error paths.
func validateTaskSetCommon(tasks []TaskInput, fieldPrefix string, errs *ValidationErrors) {
	names := make([]string, 0, len(tasks))
	nameSet := make(map[string]bool, len(tasks))
	blockedBy := make(map[string][]string)

	for i, task := range tasks {
		prefix := fmt.Sprintf("%s[%d]", fieldPrefix, i)
		validateTaskFields(task, prefix, errs)

		if task.Name != "" {
			names = append(names, task.Name)
			nameSet[task.Name] = true
			if len(task.BlockedBy) > 0 {
				blockedBy[task.Name] = task.BlockedBy
			}
		}
	}

	validateNameUniqueness(names, fieldPrefix, errs)

	// Reserved name prefix (__) is now checked per-task inside validateTaskFields,
	// so validateSystemReservedNames is no longer needed here for tasks.

	if refErrs := ValidateSamePhaseRefs(blockedBy, nameSet); refErrs != nil {
		for _, e := range refErrs.Errors {
			errs.Add(fieldPrefix+"."+e.FieldPath, e.Message)
		}
	}
	if selfErrs := ValidateNoSelfReference(blockedBy); selfErrs != nil {
		for _, e := range selfErrs.Errors {
			errs.Add(fieldPrefix+"."+e.FieldPath, e.Message)
		}
	}

	if !errs.HasErrors() && len(blockedBy) > 0 {
		if _, err := ValidateTaskDAG(names, blockedBy); err != nil {
			errs.Add(fieldPrefix, err.Error())
		}
	}
}

// ValidateTasksInput validates a slice of task inputs for field integrity, uniqueness, and DAG constraints.
func ValidateTasksInput(tasks []TaskInput) *ValidationErrors {
	errs := &ValidationErrors{}

	if len(tasks) == 0 {
		errs.Add("tasks", "at least one task is required")
		return errs
	}

	validateTaskSetCommon(tasks, "tasks", errs)

	if errs.HasErrors() {
		return errs
	}
	return nil
}

// ValidatePhasesInput validates a slice of phase inputs for structure, task integrity, and DAG constraints.
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

		switch phase.Type {
		case "concrete":
			hasConcretePhase = true
			validateConcretePhase(phase, prefix, errs)
		case "deferred":
			validateDeferredPhase(phase, prefix, errs)
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

// validateConcretePhase validates a single concrete phase's structure and tasks.
func validateConcretePhase(phase PhaseInput, prefix string, errs *ValidationErrors) {
	if len(phase.DependsOnPhases) > 0 {
		errs.Add(prefix+".depends_on_phases", "concrete phases must have empty depends_on_phases")
	}
	if phase.Constraints != nil {
		errs.Add(prefix+".constraints", "concrete phases must not have constraints")
	}

	if len(phase.Tasks) == 0 {
		errs.Add(prefix+".tasks", "concrete phases must have at least one task")
	} else {
		validateTaskSetCommon(phase.Tasks, prefix+".tasks", errs)
	}
}

// validateDeferredPhase validates a single deferred phase's structure and constraints.
func validateDeferredPhase(phase PhaseInput, prefix string, errs *ValidationErrors) {
	if phase.Constraints == nil {
		errs.Add(prefix+".constraints", "deferred phases must have constraints")
	} else {
		validatePhaseConstraints(phase.Constraints, prefix+".constraints", errs)
	}
	if len(phase.Tasks) > 0 {
		errs.Add(prefix+".tasks", "deferred phases must not have tasks at submit time")
	}
}

// ValidatePhaseFillInput validates tasks submitted to fill a deferred phase against its constraints.
func ValidatePhaseFillInput(tasks []TaskInput, phase model.Phase) *ValidationErrors {
	errs := &ValidationErrors{}

	if len(tasks) == 0 {
		errs.Add("tasks", "at least one task is required")
		return errs
	}

	// Validate against phase constraints (deferred phases must have constraints)
	if phase.Constraints == nil {
		errs.Add("constraints", fmt.Sprintf("phase %q has nil constraints; deferred phases require constraints for validation", phase.Name))
	} else {
		if len(tasks) > phase.Constraints.MaxTasks {
			errs.Add("tasks", fmt.Sprintf("task count %d exceeds phase constraint max_tasks %d",
				len(tasks), phase.Constraints.MaxTasks))
		}

		allowedBloom := buildAllowedBloomMap(phase.Constraints.AllowedBloomLevels)

		for i, task := range tasks {
			if task.BloomLevel > 0 && !allowedBloom[task.BloomLevel] {
				errs.Add(fmt.Sprintf("tasks[%d].bloom_level", i),
					fmt.Sprintf("bloom_level %d not in allowed levels for phase %q",
						task.BloomLevel, phase.Name))
			}
		}
	}

	// Validate task fields, uniqueness, references, and DAG
	validateTaskSetCommon(tasks, "tasks", errs)

	if errs.HasErrors() {
		return errs
	}
	return nil
}

// validateTaskFields validates a single task's fields including reserved name prefix check.
// For system-generated tasks that use the __ prefix, use validateTaskFieldsCore instead.
func validateTaskFields(task TaskInput, fieldPrefix string, errs *ValidationErrors) {
	validateTaskFieldsCore(task, fieldPrefix, errs)
	if task.Name != "" && strings.HasPrefix(task.Name, "__") {
		errs.Add(fieldPrefix+".name", fmt.Sprintf("name %q uses reserved prefix '__'", task.Name))
	}
}

// validateTaskFieldsCore validates a single task's field integrity (non-empty, length limits,
// bloom level range) without checking the reserved __ name prefix.
func validateTaskFieldsCore(task TaskInput, fieldPrefix string, errs *ValidationErrors) {
	if task.Name == "" {
		errs.Add(fieldPrefix+".name", "required field is missing")
	} else if len([]rune(task.Name)) > MaxTaskNameRunes {
		errs.Add(fieldPrefix+".name", fmt.Sprintf("exceeds maximum length of %d characters", MaxTaskNameRunes))
	}
	if task.Purpose == "" {
		errs.Add(fieldPrefix+".purpose", "required field is missing")
	} else if len([]rune(task.Purpose)) > MaxTaskPurposeRunes {
		errs.Add(fieldPrefix+".purpose", fmt.Sprintf("exceeds maximum length of %d characters", MaxTaskPurposeRunes))
	}
	if task.Content == "" {
		errs.Add(fieldPrefix+".content", "required field is missing")
	} else if len(task.Content) > MaxTaskContentBytes {
		errs.Add(fieldPrefix+".content", fmt.Sprintf("exceeds maximum size of %d bytes", MaxTaskContentBytes))
	}
	if task.AcceptanceCriteria == "" {
		errs.Add(fieldPrefix+".acceptance_criteria", "required field is missing")
	} else if len([]rune(task.AcceptanceCriteria)) > MaxTaskAcceptanceCriteriaRunes {
		errs.Add(fieldPrefix+".acceptance_criteria", fmt.Sprintf("exceeds maximum length of %d characters", MaxTaskAcceptanceCriteriaRunes))
	}
	for i, c := range task.Constraints {
		if len([]rune(c)) > MaxTaskConstraintRunes {
			errs.Add(fmt.Sprintf("%s.constraints[%d]", fieldPrefix, i), fmt.Sprintf("exceeds maximum length of %d characters", MaxTaskConstraintRunes))
		}
	}
	validateBloomLevel(task.BloomLevel, fieldPrefix+".bloom_level", errs)

	// Validate persona_hint: must be a safe identifier (no path traversal).
	if task.PersonaHint != "" {
		if !validate.IsValidIdentifier(task.PersonaHint) {
			errs.Add(fieldPrefix+".persona_hint", fmt.Sprintf("invalid persona_hint %q: must not contain '/', '\\', or null bytes", task.PersonaHint))
		}
		if strings.Contains(task.PersonaHint, "..") {
			errs.Add(fieldPrefix+".persona_hint", fmt.Sprintf("invalid persona_hint %q: must not contain '..'", task.PersonaHint))
		}
	}

	// Validate skill_refs: each must be a safe identifier.
	for i, ref := range task.SkillRefs {
		if !validate.IsValidIdentifier(ref) {
			errs.Add(fmt.Sprintf("%s.skill_refs[%d]", fieldPrefix, i), fmt.Sprintf("invalid skill_ref %q: must not contain '/', '\\', or null bytes", ref))
		}
	}

	// Validate expected_paths (required after auto-completion).
	if task.ExpectedPaths == nil {
		errs.Add(fieldPrefix+".expected_paths", "required field is missing")
	} else if len(task.ExpectedPaths) == 0 {
		errs.Add(fieldPrefix+".expected_paths", "must contain at least one path")
	} else {
		validateExpectedPaths(task.ExpectedPaths, fieldPrefix+".expected_paths", errs)
	}

	// Validate definition_of_abort (required after auto-completion).
	if task.DefinitionOfAbort == nil {
		errs.Add(fieldPrefix+".definition_of_abort", "required field is missing")
	} else {
		validateDefinitionOfAbort(task.DefinitionOfAbort, fieldPrefix+".definition_of_abort", errs)
	}

	// Validate definition_of_done.
	validateDefinitionOfDone(task.DefinitionOfDone, fieldPrefix+".definition_of_done", errs)

	validateOperationType(task.OperationType, fieldPrefix+".operation_type", errs)

	// run_on_main and run_on_integration are mutually exclusive since the
	// dispatcher selects exactly one target directory per task.
	if task.RunOnMain && task.RunOnIntegration {
		errs.Add(fieldPrefix, "run_on_main and run_on_integration are mutually exclusive; set at most one")
	}

	// worker_id pinning is optional, but when present it must look like a
	// configured worker slot (worker1, worker2, …). Existence is verified
	// later by AssignWorkers against the actual workers.count, so the
	// format check here is purely a fast-fail for typos like "wroker1" or
	// "worker_one" — same shape used by `plan add-task --worker-id`.
	if task.WorkerID != "" {
		if !workerIDPattern.MatchString(task.WorkerID) {
			errs.Add(fieldPrefix+".worker_id",
				fmt.Sprintf("invalid worker_id %q: must match workerN where N is a positive integer", task.WorkerID))
		}
	}
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
	if level < BloomLevelMin || level > BloomLevelMax {
		errs.Add(fieldPath, fmt.Sprintf("must be between %d and %d, got %d", BloomLevelMin, BloomLevelMax, level))
	}
}

func validateExpectedPaths(paths []string, fieldPath string, errs *ValidationErrors) {
	seen := make(map[string]bool, len(paths))
	for i, p := range paths {
		fp := fmt.Sprintf("%s[%d]", fieldPath, i)
		if p == "" {
			errs.Add(fp, "must not be empty")
			continue
		}
		if strings.HasPrefix(p, "/") {
			errs.Add(fp, fmt.Sprintf("must be a relative path, got %q", p))
		}
		if strings.Contains(p, "..") {
			errs.Add(fp, fmt.Sprintf("must not contain '..', got %q", p))
		}
		if seen[p] {
			errs.Add(fp, fmt.Sprintf("duplicate path %q", p))
		}
		seen[p] = true
	}
}

func validateDefinitionOfAbort(d *model.DefinitionOfAbort, fieldPath string, errs *ValidationErrors) {
	if d.MaxRepairCount <= 0 || d.MaxRepairCount > MaxRepairCount {
		errs.Add(fieldPath+".max_repair_count",
			fmt.Sprintf("must be between 1 and %d, got %d", MaxRepairCount, d.MaxRepairCount))
	}
	if d.MaxWallClockSec <= 0 || d.MaxWallClockSec > MaxWallClockSec {
		errs.Add(fieldPath+".max_wall_clock_sec",
			fmt.Sprintf("must be between 1 and %d, got %d", MaxWallClockSec, d.MaxWallClockSec))
	}
	for i, cond := range d.ExplicitFailureConditions {
		if strings.TrimSpace(cond) == "" {
			errs.Add(fmt.Sprintf("%s.explicit_failure_conditions[%d]", fieldPath, i),
				"must not be empty")
		}
	}
}

func validateDefinitionOfDone(items []string, fieldPath string, errs *ValidationErrors) {
	if len(items) > MaxDefinitionOfDoneItems {
		errs.Add(fieldPath,
			fmt.Sprintf("exceeds maximum of %d items, got %d", MaxDefinitionOfDoneItems, len(items)))
	}
	for i, item := range items {
		if strings.TrimSpace(item) == "" {
			errs.Add(fmt.Sprintf("%s[%d]", fieldPath, i), "must not be empty")
		}
	}
}

func validateOperationType(op string, fieldPath string, errs *ValidationErrors) {
	switch op {
	case "", model.OperationTypeVerify, model.OperationTypeRepair:
		return
	default:
		errs.Add(fieldPath, fmt.Sprintf("must be one of %q, %q, got %q",
			model.OperationTypeVerify, model.OperationTypeRepair, op))
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
			if l < BloomLevelMin || l > BloomLevelMax {
				errs.Add(fmt.Sprintf("%s.allowed_bloom_levels[%d]", fieldPath, i),
					fmt.Sprintf("must be between %d and %d, got %d", BloomLevelMin, BloomLevelMax, l))
			}
		}
	}
}
