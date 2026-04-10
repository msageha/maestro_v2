package model

// MutationStrategy defines the strategy used for evolutionary mutation.
type MutationStrategy string

const (
	// MutationStrategyDiff applies mutations as diffs between versions.
	MutationStrategyDiff MutationStrategy = "diff"
	// MutationStrategyFull replaces the entire implementation as a mutation.
	MutationStrategyFull MutationStrategy = "full"
	// MutationStrategyCross creates mutations by combining multiple parent versions.
	MutationStrategyCross MutationStrategy = "cross"
)

// ValidateMutationStrategy reports whether s is a known mutation strategy.
func ValidateMutationStrategy(s string) bool {
	switch MutationStrategy(s) {
	case MutationStrategyDiff, MutationStrategyFull, MutationStrategyCross:
		return true
	}
	return false
}

// MutationRequest represents a request to generate a mutation for a task.
type MutationRequest struct {
	TaskID        string        `yaml:"task_id" json:"task_id"`
	Strategy      string        `yaml:"strategy" json:"strategy"`
	ParentFitness *FitnessScore `yaml:"parent_fitness,omitempty" json:"parent_fitness,omitempty"`
	Constraints   []string      `yaml:"constraints,omitempty" json:"constraints,omitempty"`
}

// MutationResult represents the outcome of a single mutation attempt.
type MutationResult struct {
	SlotIndex int           `yaml:"slot_index" json:"slot_index"`
	Fitness   *FitnessScore `yaml:"fitness,omitempty" json:"fitness,omitempty"`
	IsNovel   bool          `yaml:"is_novel" json:"is_novel"`
}

// EvolutionCycle captures one round of evolutionary improvement.
type EvolutionCycle struct {
	Round       int              `yaml:"round" json:"round"`
	Mutations   []MutationResult `yaml:"mutations" json:"mutations"`
	BestFitness *FitnessScore    `yaml:"best_fitness,omitempty" json:"best_fitness,omitempty"`
}
