package daemon

import (
	"path/filepath"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
	yamlutil "github.com/msageha/maestro_v2/internal/yaml"
)

// TestIsCommandPlannerIdle_AllPausedForReplan covers the unblocking case:
// every required task is paused_for_replan (waiting for daemon-side R10 or
// operator action). The Planner pane is genuinely idle and a new command
// must be allowed to dispatch.
func TestIsCommandPlannerIdle_AllPausedForReplan(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs: []string{"t1"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusPausedForReplan,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusActive},
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "commands", "cmd1.yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if !qh.isCommandPlannerIdle("cmd1") {
		t.Error("expected isCommandPlannerIdle=true when every required task is paused_for_replan")
	}
}

// TestIsCommandPlannerIdle_LiveTaskRunning covers the negative case: a
// required task is still in_progress. The Planner needs to observe its
// outcome; new commands must NOT preempt the queue.
func TestIsCommandPlannerIdle_LiveTaskRunning(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd2",
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs: []string{"t1"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusInProgress,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusActive},
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "commands", "cmd2.yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if qh.isCommandPlannerIdle("cmd2") {
		t.Error("expected isCommandPlannerIdle=false when a required task is still in flight")
	}
}

// TestIsCommandPlannerIdle_AwaitingFillPhase covers a phase awaiting Planner
// fill: the Planner is being asked to author next-phase tasks, so the
// command is not idle even though all *current* tasks may be terminal.
func TestIsCommandPlannerIdle_AwaitingFillPhase(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd3",
		PlanStatus:    model.PlanStatusSealed,
		TaskTracking: model.TaskTracking{
			RequiredTaskIDs: []string{"t1"},
			TaskStates: map[string]model.Status{
				"t1": model.StatusCompleted,
			},
		},
		PhaseTracking: model.PhaseTracking{
			Phases: []model.Phase{
				{PhaseID: "p1", Name: "phase1", Status: model.PhaseStatusCompleted},
				{PhaseID: "p2", Name: "phase2", Status: model.PhaseStatusAwaitingFill},
			},
		},
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "commands", "cmd3.yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if qh.isCommandPlannerIdle("cmd3") {
		t.Error("expected isCommandPlannerIdle=false when a phase is awaiting Planner fill")
	}
}

// TestIsCommandPlannerIdle_NoStateFile covers the conservative default: a
// brand-new in_progress command without a state file is Planner-engaged.
func TestIsCommandPlannerIdle_NoStateFile(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	if qh.isCommandPlannerIdle("cmd_unknown") {
		t.Error("expected isCommandPlannerIdle=false when state file is missing (conservative default)")
	}
}

// TestIsCommandPlannerIdle_PlanningStillEngaged covers the planning phase:
// even with no required tasks yet, a command at PlanStatus=planning is
// definitely Planner-engaged (Planner is mid-authoring).
func TestIsCommandPlannerIdle_PlanningStillEngaged(t *testing.T) {
	t.Parallel()
	maestroDir := setupScanPhaseTestDir(t)
	qh := newScanPhaseTestQueueHandler(t, maestroDir, model.WorktreeConfig{Enabled: true})

	state := model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd4",
		PlanStatus:    model.PlanStatusPlanning,
		CreatedAt:     "2026-01-01T00:00:00Z",
		UpdatedAt:     "2026-01-01T00:00:00Z",
	}
	statePath := filepath.Join(maestroDir, "state", "commands", "cmd4.yaml")
	if err := yamlutil.AtomicWrite(statePath, state); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if qh.isCommandPlannerIdle("cmd4") {
		t.Error("expected isCommandPlannerIdle=false at PlanStatusPlanning")
	}
}
