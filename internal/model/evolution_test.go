package model

import "testing"

func TestValidateMutationStrategy(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"diff", true},
		{"full", true},
		{"cross", true},
		{"unknown", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := validateMutationStrategy(tt.input); got != tt.want {
				t.Errorf("validateMutationStrategy(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestMutationRequest_Fields(t *testing.T) {
	req := mutationRequest{
		TaskID:   "task-1",
		Strategy: "diff",
		ParentFitness: &FitnessScore{
			Passed:      true,
			RepairCount: 1,
		},
		Constraints: []string{"no-breaking-changes"},
	}
	if req.TaskID != "task-1" {
		t.Errorf("TaskID = %q, want task-1", req.TaskID)
	}
	if req.Strategy != "diff" {
		t.Errorf("Strategy = %q, want diff", req.Strategy)
	}
	if req.ParentFitness == nil || !req.ParentFitness.Passed {
		t.Error("ParentFitness should be set and Passed=true")
	}
}

func TestEvolutionCycle_Fields(t *testing.T) {
	best := &FitnessScore{Passed: true, QualityScore: 0.9}
	cycle := evolutionCycle{
		Round: 1,
		Mutations: []mutationResult{
			{SlotIndex: 0, Fitness: best, IsNovel: true},
			{SlotIndex: 1, IsNovel: false},
		},
		BestFitness: best,
	}
	if cycle.Round != 1 {
		t.Errorf("Round = %d, want 1", cycle.Round)
	}
	if len(cycle.Mutations) != 2 {
		t.Errorf("Mutations count = %d, want 2", len(cycle.Mutations))
	}
	if !cycle.Mutations[0].IsNovel {
		t.Error("first mutation should be novel")
	}
	if cycle.BestFitness.QualityScore != 0.9 {
		t.Errorf("BestFitness.QualityScore = %v, want 0.9", cycle.BestFitness.QualityScore)
	}
}
