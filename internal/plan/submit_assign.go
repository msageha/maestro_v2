package plan

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/model"
)

// concretePhaseData holds the aggregated results of processing concrete phases.
type concretePhaseData struct {
	nameToID     map[string]string
	assignMap    map[string]WorkerAssignment
	tasks        []TaskInput
	assignments  []WorkerAssignment
	workerStates []WorkerState
}

// validateCrossPhaseTaskNames checks for task name collisions across concrete phases.
func validateCrossPhaseTaskNames(phases []PhaseInput) error {
	globalTaskNames := make(map[string]string) // name → phase name
	for _, phase := range phases {
		if phase.Type != "concrete" {
			continue
		}
		for _, task := range phase.Tasks {
			if existingPhase, exists := globalTaskNames[task.Name]; exists {
				return fmt.Errorf("task name %q appears in both phase %q and %q; task names must be unique across all phases",
					task.Name, existingPhase, phase.Name)
			}
			globalTaskNames[task.Name] = phase.Name
		}
	}
	return nil
}

// processConcretePhases resolves names and assigns workers for all concrete phases.
func processConcretePhases(opts SubmitOptions, phases []PhaseInput) (*concretePhaseData, error) {
	workerStates, err := BuildWorkerStates(opts.MaestroDir, opts.Config.Agents.Workers)
	if err != nil {
		return nil, fmt.Errorf("build worker states: %w", err)
	}

	cpd := &concretePhaseData{
		nameToID:     make(map[string]string),
		assignMap:    make(map[string]WorkerAssignment),
		workerStates: workerStates,
	}

	for _, phase := range phases {
		if phase.Type != "concrete" {
			continue
		}

		nameToID, err := resolveNames(phase.Tasks)
		if err != nil {
			return nil, fmt.Errorf("resolve names for phase %s: %w", phase.Name, err)
		}
		for k, v := range nameToID {
			cpd.nameToID[k] = v
		}

		var assignReqs []TaskAssignmentRequest
		for _, t := range phase.Tasks {
			assignReqs = append(assignReqs, TaskAssignmentRequest{
				Name:           t.Name,
				BloomLevel:     t.BloomLevel,
				PinnedWorkerID: t.WorkerID,
			})
		}

		assignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, cpd.workerStates, assignReqs, WithModelSelector(opts.ModelSelector))
		if err != nil {
			return nil, fmt.Errorf("worker assignment for phase %s: %w", phase.Name, err)
		}
		cpd.assignments = append(cpd.assignments, assignments...)
		for _, a := range assignments {
			cpd.assignMap[a.TaskName] = a
		}
		cpd.tasks = append(cpd.tasks, phase.Tasks...)

		// Update workerStates for next phase assignment
		for _, a := range assignments {
			for i := range cpd.workerStates {
				if cpd.workerStates[i].WorkerID == a.WorkerID {
					cpd.workerStates[i].PendingCount++
					break
				}
			}
		}
	}

	return cpd, nil
}

// addSystemCommitForPhases creates a system commit task that depends on all concrete-phase tasks.
func addSystemCommitForPhases(opts SubmitOptions, cpd *concretePhaseData) (*string, error) {
	allConcreteTaskNames := make([]string, 0, len(cpd.tasks))
	for _, t := range cpd.tasks {
		allConcreteTaskNames = append(allConcreteTaskNames, t.Name)
	}

	commitTask := buildSystemCommitTask(allConcreteTaskNames)
	if err := validateSystemTask(commitTask); err != nil {
		return nil, fmt.Errorf("validate system commit task: %w", err)
	}

	commitID, err := model.NewTaskID(model.TaskIDCallerPlannerSystemCommit)
	if err != nil {
		return nil, fmt.Errorf("generate system commit ID: %w", err)
	}
	cpd.nameToID[commitTask.Name] = commitID

	assignReqs := []TaskAssignmentRequest{{Name: commitTask.Name, BloomLevel: commitTask.BloomLevel}}
	commitAssignments, err := AssignWorkers(opts.Config.Agents.Workers, opts.Config.Limits, cpd.workerStates, assignReqs, WithModelSelector(opts.ModelSelector))
	if err != nil {
		return nil, fmt.Errorf("worker assignment for system commit: %w", err)
	}
	cpd.assignments = append(cpd.assignments, commitAssignments...)
	for _, a := range commitAssignments {
		cpd.assignMap[a.TaskName] = a
	}
	cpd.tasks = append(cpd.tasks, commitTask)

	return &commitID, nil
}
