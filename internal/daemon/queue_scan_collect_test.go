package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestDetachTaskSlices_BreaksBackingArraySharing(t *testing.T) {
	original := model.Task{
		DefinitionOfDone: []string{"done1", "done2"},
		Constraints:      []string{"c1"},
		BlockedBy:        []string{"task-1", "task-2"},
		ToolsHint:        []string{"codex"},
		SkillRefs:        []string{"skill-a", "skill-b"},
		ExpectedPaths:    []string{"internal/foo.go"},
		DefinitionOfAbort: &model.DefinitionOfAbort{
			MaxRepairCount:            3,
			MaxWallClockSec:           1800,
			ExplicitFailureConditions: []string{"timeout", "crash"},
		},
	}

	// Keep references to original backing arrays.
	origDone := original.DefinitionOfDone
	origConstraints := original.Constraints
	origBlocked := original.BlockedBy
	origTools := original.ToolsHint
	origSkills := original.SkillRefs
	origPaths := original.ExpectedPaths
	origAbortConds := original.DefinitionOfAbort.ExplicitFailureConditions

	detachTaskSlices(&original)

	// Values must be preserved.
	if len(original.BlockedBy) != 2 || original.BlockedBy[0] != "task-1" || original.BlockedBy[1] != "task-2" {
		t.Fatalf("BlockedBy values changed: %v", original.BlockedBy)
	}

	// Backing arrays must be different (mutation must not propagate).
	original.BlockedBy[0] = "MUTATED"
	if origBlocked[0] == "MUTATED" {
		t.Error("BlockedBy still shares backing array after detach")
	}

	original.DefinitionOfDone[0] = "MUTATED"
	if origDone[0] == "MUTATED" {
		t.Error("DefinitionOfDone still shares backing array after detach")
	}

	original.Constraints[0] = "MUTATED"
	if origConstraints[0] == "MUTATED" {
		t.Error("Constraints still shares backing array after detach")
	}

	original.ToolsHint[0] = "MUTATED"
	if origTools[0] == "MUTATED" {
		t.Error("ToolsHint still shares backing array after detach")
	}

	original.SkillRefs[0] = "MUTATED"
	if origSkills[0] == "MUTATED" {
		t.Error("SkillRefs still shares backing array after detach")
	}

	original.ExpectedPaths[0] = "MUTATED"
	if origPaths[0] == "MUTATED" {
		t.Error("ExpectedPaths still shares backing array after detach")
	}

	original.DefinitionOfAbort.ExplicitFailureConditions[0] = "MUTATED"
	if origAbortConds[0] == "MUTATED" {
		t.Error("DefinitionOfAbort.ExplicitFailureConditions still shares backing array after detach")
	}
}

func TestDetachTaskSlices_NilSlicesPreserved(t *testing.T) {
	task := model.Task{}
	detachTaskSlices(&task)

	if task.BlockedBy != nil {
		t.Error("nil BlockedBy became non-nil")
	}
	if task.Constraints != nil {
		t.Error("nil Constraints became non-nil")
	}
	if task.ToolsHint != nil {
		t.Error("nil ToolsHint became non-nil")
	}
	if task.SkillRefs != nil {
		t.Error("nil SkillRefs became non-nil")
	}
	if task.ExpectedPaths != nil {
		t.Error("nil ExpectedPaths became non-nil")
	}
	if task.DefinitionOfDone != nil {
		t.Error("nil DefinitionOfDone became non-nil")
	}
}

func TestDetachTaskSlices_NilDefinitionOfAbort(t *testing.T) {
	task := model.Task{DefinitionOfAbort: nil}
	detachTaskSlices(&task) // must not panic
	if task.DefinitionOfAbort != nil {
		t.Error("nil DefinitionOfAbort became non-nil")
	}
}

func TestDetachTaskSlices_EmptyExplicitFailureConditions(t *testing.T) {
	task := model.Task{
		DefinitionOfAbort: &model.DefinitionOfAbort{
			MaxRepairCount: 3,
		},
	}
	origPtr := task.DefinitionOfAbort
	detachTaskSlices(&task)
	// When ExplicitFailureConditions is empty, DefinitionOfAbort pointer should be unchanged.
	if task.DefinitionOfAbort != origPtr {
		t.Error("DefinitionOfAbort pointer changed when ExplicitFailureConditions was empty")
	}
}
