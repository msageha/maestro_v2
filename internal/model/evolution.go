package model

// mutationStrategy defines the strategy used for evolutionary mutation.
type mutationStrategy string

const (
	// MutationStrategyDiff applies mutations as diffs between versions.
	MutationStrategyDiff mutationStrategy = "diff"
	// MutationStrategyFull replaces the entire implementation as a mutation.
	MutationStrategyFull mutationStrategy = "full"
	// MutationStrategyCross creates mutations by combining multiple parent versions.
	MutationStrategyCross mutationStrategy = "cross"
)

// validateMutationStrategy reports whether s is a known mutation strategy.
func validateMutationStrategy(s string) bool {
	switch mutationStrategy(s) {
	case MutationStrategyDiff, MutationStrategyFull, MutationStrategyCross:
		return true
	}
	return false
}

// mutationRequest represents a request to generate a mutation for a task.
type mutationRequest struct {
	TaskID        string        `yaml:"task_id" json:"task_id"`
	Strategy      string        `yaml:"strategy" json:"strategy"`
	ParentFitness *FitnessScore `yaml:"parent_fitness,omitempty" json:"parent_fitness,omitempty"`
	Constraints   []string      `yaml:"constraints,omitempty" json:"constraints,omitempty"`
}

// mutationResult represents the outcome of a single mutation attempt.
type mutationResult struct {
	SlotIndex int           `yaml:"slot_index" json:"slot_index"`
	Fitness   *FitnessScore `yaml:"fitness,omitempty" json:"fitness,omitempty"`
	IsNovel   bool          `yaml:"is_novel" json:"is_novel"`
}

// evolutionCycle captures one round of evolutionary improvement.
type evolutionCycle struct {
	Round       int              `yaml:"round" json:"round"`
	Mutations   []mutationResult `yaml:"mutations" json:"mutations"`
	BestFitness *FitnessScore    `yaml:"best_fitness,omitempty" json:"best_fitness,omitempty"`
}
