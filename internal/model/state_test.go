package model

import (
	"strings"
	"testing"
)

func TestPhaseIndex_Found(t *testing.T) {
	t.Parallel()
	pt := PhaseTracking{
		Phases: []Phase{
			{PhaseID: "phase-a"},
			{PhaseID: "phase-b"},
			{PhaseID: "phase-c"},
		},
	}
	idx, ok := pt.PhaseIndex("phase-b")
	if !ok {
		t.Fatal("expected phase-b to be found")
	}
	if idx != 1 {
		t.Fatalf("expected index 1, got %d", idx)
	}
}

func TestPhaseIndex_NotFound(t *testing.T) {
	t.Parallel()
	pt := PhaseTracking{
		Phases: []Phase{{PhaseID: "phase-a"}},
	}
	idx, ok := pt.PhaseIndex("no-such-phase")
	if ok {
		t.Fatal("expected not found")
	}
	if idx != -1 {
		t.Fatalf("expected index -1, got %d", idx)
	}
}

func TestPhaseIndex_EmptyPhases(t *testing.T) {
	t.Parallel()
	pt := PhaseTracking{}
	idx, ok := pt.PhaseIndex("any")
	if ok {
		t.Fatal("expected not found on empty phases")
	}
	if idx != -1 {
		t.Fatalf("expected index -1, got %d", idx)
	}
}

func TestPhaseIndex_AfterPhasesModification(t *testing.T) {
	t.Parallel()
	pt := PhaseTracking{
		Phases: []Phase{
			{PhaseID: "phase-a"},
			{PhaseID: "phase-b"},
		},
	}

	// Initial lookup succeeds.
	idx, ok := pt.PhaseIndex("phase-b")
	if !ok || idx != 1 {
		t.Fatalf("initial lookup: expected (1, true), got (%d, %v)", idx, ok)
	}

	// Append a new phase — PhaseIndex must reflect the change.
	pt.Phases = append(pt.Phases, Phase{PhaseID: "phase-c"})
	idx, ok = pt.PhaseIndex("phase-c")
	if !ok || idx != 2 {
		t.Fatalf("after append: expected (2, true) for phase-c, got (%d, %v)", idx, ok)
	}

	// Remove phase-b by replacing the slice — PhaseIndex must not return stale data.
	pt.Phases = []Phase{
		{PhaseID: "phase-a"},
		{PhaseID: "phase-c"},
	}
	_, ok = pt.PhaseIndex("phase-b")
	if ok {
		t.Fatal("after removal: expected phase-b not found, but it was returned")
	}
	idx, ok = pt.PhaseIndex("phase-c")
	if !ok || idx != 1 {
		t.Fatalf("after removal: expected (1, true) for phase-c, got (%d, %v)", idx, ok)
	}
}

func TestPhaseConstraints_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pc      PhaseConstraints
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid",
			pc:      PhaseConstraints{MaxTasks: 5, TimeoutMinutes: 30},
			wantErr: false,
		},
		{
			name:    "zero MaxTasks",
			pc:      PhaseConstraints{MaxTasks: 0, TimeoutMinutes: 30},
			wantErr: true,
			errMsg:  "max_tasks",
		},
		{
			name:    "negative MaxTasks",
			pc:      PhaseConstraints{MaxTasks: -1, TimeoutMinutes: 30},
			wantErr: true,
			errMsg:  "max_tasks",
		},
		{
			name:    "zero TimeoutMinutes",
			pc:      PhaseConstraints{MaxTasks: 5, TimeoutMinutes: 0},
			wantErr: true,
			errMsg:  "timeout_minutes",
		},
		{
			name:    "negative TimeoutMinutes",
			pc:      PhaseConstraints{MaxTasks: 5, TimeoutMinutes: -10},
			wantErr: true,
			errMsg:  "timeout_minutes",
		},
		{
			name:    "both zero",
			pc:      PhaseConstraints{MaxTasks: 0, TimeoutMinutes: 0},
			wantErr: true,
			errMsg:  "max_tasks",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.pc.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}
