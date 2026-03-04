package plan

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestFindPhaseForTask(t *testing.T) {
	tests := []struct {
		name       string
		state      *model.CommandState
		taskID     string
		wantPhase  string
		wantIdx    int
	}{
		{
			name: "task in first phase",
			state: &model.CommandState{
				Phases: []model.Phase{
					{Name: "phase1", TaskIDs: []string{"t1", "t2"}},
					{Name: "phase2", TaskIDs: []string{"t3"}},
				},
			},
			taskID:    "t2",
			wantPhase: "phase1",
			wantIdx:   0,
		},
		{
			name: "task in second phase",
			state: &model.CommandState{
				Phases: []model.Phase{
					{Name: "phase1", TaskIDs: []string{"t1"}},
					{Name: "phase2", TaskIDs: []string{"t2", "t3"}},
				},
			},
			taskID:    "t3",
			wantPhase: "phase2",
			wantIdx:   1,
		},
		{
			name: "task not in any phase",
			state: &model.CommandState{
				Phases: []model.Phase{
					{Name: "phase1", TaskIDs: []string{"t1"}},
				},
			},
			taskID:    "t99",
			wantPhase: "",
			wantIdx:   -1,
		},
		{
			name: "no phases",
			state: &model.CommandState{
				Phases: nil,
			},
			taskID:    "t1",
			wantPhase: "",
			wantIdx:   -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase, idx := findPhaseForTask(tt.state, tt.taskID)
			if idx != tt.wantIdx {
				t.Errorf("idx = %d, want %d", idx, tt.wantIdx)
			}
			if tt.wantPhase == "" {
				if phase != nil {
					t.Errorf("phase = %+v, want nil", phase)
				}
			} else {
				if phase == nil || phase.Name != tt.wantPhase {
					t.Errorf("phase.Name = %v, want %q", phase, tt.wantPhase)
				}
			}
		})
	}
}

func TestReplaceInRequiredOrOptional(t *testing.T) {
	tests := []struct {
		name            string
		requiredIDs     []string
		optionalIDs     []string
		oldID           string
		newID           string
		wantRequired    []string
		wantOptional    []string
	}{
		{
			name:         "replace in required",
			requiredIDs:  []string{"t1", "t2", "t3"},
			optionalIDs:  []string{"t4"},
			oldID:        "t2",
			newID:        "t2_retry",
			wantRequired: []string{"t1", "t2_retry", "t3"},
			wantOptional: []string{"t4"},
		},
		{
			name:         "replace in optional",
			requiredIDs:  []string{"t1"},
			optionalIDs:  []string{"t4", "t5"},
			oldID:        "t5",
			newID:        "t5_retry",
			wantRequired: []string{"t1"},
			wantOptional: []string{"t4", "t5_retry"},
		},
		{
			name:         "not found (no-op)",
			requiredIDs:  []string{"t1"},
			optionalIDs:  []string{"t2"},
			oldID:        "t99",
			newID:        "t99_retry",
			wantRequired: []string{"t1"},
			wantOptional: []string{"t2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &model.CommandState{
				RequiredTaskIDs: append([]string{}, tt.requiredIDs...),
				OptionalTaskIDs: append([]string{}, tt.optionalIDs...),
			}
			replaceInRequiredOrOptional(state, tt.oldID, tt.newID)

			if !sliceEqual(state.RequiredTaskIDs, tt.wantRequired) {
				t.Errorf("RequiredTaskIDs = %v, want %v", state.RequiredTaskIDs, tt.wantRequired)
			}
			if !sliceEqual(state.OptionalTaskIDs, tt.wantOptional) {
				t.Errorf("OptionalTaskIDs = %v, want %v", state.OptionalTaskIDs, tt.wantOptional)
			}
		})
	}
}

func TestRewriteDependencies(t *testing.T) {
	state := &model.CommandState{
		TaskDependencies: map[string][]string{
			"t2": {"t1"},
			"t3": {"t1", "t2"},
			"t4": {"t3"},
		},
	}

	rewriteDependencies(state, "t1", "t1_v2")

	if !sliceEqual(state.TaskDependencies["t2"], []string{"t1_v2"}) {
		t.Errorf("t2 deps = %v, want [t1_v2]", state.TaskDependencies["t2"])
	}
	if !sliceEqual(state.TaskDependencies["t3"], []string{"t1_v2", "t2"}) {
		t.Errorf("t3 deps = %v, want [t1_v2, t2]", state.TaskDependencies["t3"])
	}
	// t4 should be unchanged (doesn't depend on t1)
	if !sliceEqual(state.TaskDependencies["t4"], []string{"t3"}) {
		t.Errorf("t4 deps = %v, want [t3]", state.TaskDependencies["t4"])
	}
}

func TestFindCascadeCandidates(t *testing.T) {
	state := &model.CommandState{
		CancelledReasons: map[string]string{
			"t2": "blocked_dependency_terminal:t1",
			"t3": "blocked_dependency_terminal:t1",
			"t4": "blocked_dependency_terminal:t5",
			"t5": "command_cancel_requested",
		},
	}

	candidates := findCascadeCandidates(state, "t1")

	candidateSet := make(map[string]bool)
	for _, c := range candidates {
		candidateSet[c] = true
	}

	if !candidateSet["t2"] || !candidateSet["t3"] {
		t.Errorf("candidates = %v, want t2 and t3", candidates)
	}
	if candidateSet["t4"] || candidateSet["t5"] {
		t.Errorf("candidates should not include t4 or t5, got %v", candidates)
	}
}

func TestResolveBlockedByViaLineage(t *testing.T) {
	tests := []struct {
		name      string
		blockedBy []string
		lineage   map[string]string
		want      []string
	}{
		{
			name:      "no lineage",
			blockedBy: []string{"t1", "t2"},
			lineage:   map[string]string{},
			want:      []string{"t1", "t2"},
		},
		{
			name:      "single hop",
			blockedBy: []string{"t1"},
			lineage:   map[string]string{"t1_v2": "t1"},
			want:      []string{"t1_v2"},
		},
		{
			name:      "multi hop",
			blockedBy: []string{"t1"},
			lineage:   map[string]string{"t1_v2": "t1", "t1_v3": "t1_v2"},
			want:      []string{"t1_v3"},
		},
		{
			name:      "mixed resolved and unresolved",
			blockedBy: []string{"t1", "t2"},
			lineage:   map[string]string{"t1_v2": "t1"},
			want:      []string{"t1_v2", "t2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveBlockedByViaLineage(tt.blockedBy, tt.lineage)
			if !sliceEqual(got, tt.want) {
				t.Errorf("resolveBlockedByViaLineage = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetLatestDescendant(t *testing.T) {
	tests := []struct {
		name    string
		taskID  string
		lineage map[string]string
		want    string
	}{
		{
			name:    "no descendants",
			taskID:  "t1",
			lineage: map[string]string{},
			want:    "t1",
		},
		{
			name:    "one descendant",
			taskID:  "t1",
			lineage: map[string]string{"t1_v2": "t1"},
			want:    "t1_v2",
		},
		{
			name:    "chain of descendants",
			taskID:  "t1",
			lineage: map[string]string{"t1_v2": "t1", "t1_v3": "t1_v2"},
			want:    "t1_v3",
		},
		{
			name:    "cycle protection",
			taskID:  "t1",
			lineage: map[string]string{"t2": "t1", "t1": "t2"},
			want:    "t1", // cycle detected on revisit, returns current
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getLatestDescendant(tt.taskID, tt.lineage)
			if got != tt.want {
				t.Errorf("getLatestDescendant(%q) = %q, want %q", tt.taskID, got, tt.want)
			}
		})
	}
}

func TestReopenPhase(t *testing.T) {
	now := "2024-01-01T00:00:00Z"

	tests := []struct {
		name      string
		status    model.PhaseStatus
		wantErr   bool
		wantStatus model.PhaseStatus
	}{
		{
			name:       "reopen failed phase",
			status:     model.PhaseStatusFailed,
			wantErr:    false,
			wantStatus: model.PhaseStatusActive,
		},
		{
			name:    "cannot reopen active phase",
			status:  model.PhaseStatusActive,
			wantErr: true,
		},
		{
			name:    "cannot reopen completed phase",
			status:  model.PhaseStatusCompleted,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &model.CommandState{
				Phases: []model.Phase{
					{Name: "p1", Status: tt.status},
				},
			}
			err := reopenPhase(state, 0, now)
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if state.Phases[0].Status != tt.wantStatus {
					t.Errorf("status = %s, want %s", state.Phases[0].Status, tt.wantStatus)
				}
				if state.Phases[0].ReopenedAt == nil || *state.Phases[0].ReopenedAt != now {
					t.Error("ReopenedAt should be set")
				}
				if state.Phases[0].CompletedAt != nil {
					t.Error("CompletedAt should be nil after reopen")
				}
			}
		})
	}
}

func TestCopyAndRestoreState(t *testing.T) {
	state := &model.CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd1",
		TaskStates:    map[string]model.Status{"t1": model.StatusPending},
	}

	backup, err := copyState(state)
	if err != nil {
		t.Fatalf("copyState error: %v", err)
	}

	// Mutate state
	state.TaskStates["t1"] = model.StatusCompleted
	state.TaskStates["t2"] = model.StatusFailed

	// Restore
	restoreState(state, backup)

	if state.TaskStates["t1"] != model.StatusPending {
		t.Errorf("t1 status = %s after restore, want pending", state.TaskStates["t1"])
	}
	if _, exists := state.TaskStates["t2"]; exists {
		t.Error("t2 should not exist after restore")
	}
}

func TestAddRetryTask_NilLockMap(t *testing.T) {
	_, err := AddRetryTask(RetryOptions{
		CommandID:  "cmd1",
		RetryOf:    "t1",
		MaestroDir: t.TempDir(),
		LockMap:    nil,
	})
	if err == nil {
		t.Fatal("expected error for nil LockMap")
	}
}

// sliceEqual compares two string slices for equality.
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
